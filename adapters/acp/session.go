package acp

// The control plane of one ACP session: oarlock.Session over the SDK
// connection, recorded on a real kernel.
//
//   - The record stream is the wire (sink.go): every agent line is one frame
//     or one toApp request, in wire order, Native verbatim.
//   - Status is never cached: busy, no_active_turn and the queue drain fold
//     the retained records (internal/fold).
//   - Steer exists only for a profile with SteerMeta: the type returned by
//     open for such a profile is *steerSession, which adds the method, so a
//     kimi/antigravity session does not satisfy oarlock.Steerer.
//   - The turn ends only on the agent's own session/prompt answer. Nothing
//     here synthesizes a turn_ended; a process that dies mid-turn leaves the
//     turn open and the exited response says why.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/exec"
	"github.com/zzjcool/oarlock/internal/fold"
	"github.com/zzjcool/oarlock/internal/kernel"
)

// DefaultAbortGrace is how long an accepted abort may leave the turn running
// before oarlock takes over and kills the process tree.
const DefaultAbortGrace = 10 * time.Second

// defaultGrace is what new sessions use; tests shorten it.
var defaultGrace = DefaultAbortGrace

// Session is one ACP agent process and its session.
type Session struct {
	prof   Profile
	opt    oarlock.Options
	k      kernel.Kernel
	sink   *sink
	conn   *sdk.ClientSideConnection
	cmd    *exec.Cmd
	id     string
	cwd    string
	images bool
	term   *terminals
	grace  time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	cancelling atomic.Bool
	sendMu     sync.Mutex

	readDone     chan struct{}
	exitRecorded chan struct{}
	stderr       *tail

	mu          sync.Mutex
	held        []heldInput
	draining    bool
	again       bool
	disposeReq  string
	disposeDone chan struct{}
	disposeErr  error
	graceTimer  *time.Timer
}

type heldInput struct {
	input, inputID, origin string
	images                 []oarlock.InputImage
}

var _ oarlock.Session = (*Session)(nil)

// steerSession is a Session that can steer. Only profiles with SteerMeta are
// opened as this type.
type steerSession struct{ *Session }

var _ oarlock.Steerer = steerSession{}

// ID is the runtime-native session id.
func (s *Session) ID() string { return s.id }

// Unknown tallies native session/update kinds the projection does not read.
func (s *Session) Unknown() map[string]int { return s.sink.proj.tally() }

func (s *Session) Capabilities() oarlock.Capabilities {
	return oarlock.Capabilities{Queue: oarlock.QueueCapability{Durable: false}, Attribution: s.prof.Attribution, Images: s.images}
}

func (s *Session) Records(c oarlock.Cursor) []oarlock.Record { return s.k.Records(c) }

func (s *Session) Events(c *oarlock.Cursor, fn func(oarlock.Record)) (unsubscribe func()) {
	if c == nil {
		return s.k.Observe(fn)
	}
	cancel, err := s.k.ObserveFrom(*c, fn)
	if err != nil {
		return func() {}
	}
	return cancel
}

func (s *Session) status() fold.Status { return fold.StatusOf(s.k.Records(oarlock.Cursor{})).Value }

func (s *Session) statusBefore(seq uint64) fold.Status {
	records := s.k.Records(oarlock.Cursor{})
	if n := int(seq) - 1; n >= 0 && n < len(records) {
		records = records[:n]
	}
	return fold.StatusOf(records).Value
}

func rejected(code oarlock.Code, reason string) oarlock.ResponseBody {
	return oarlock.ResponseBody{Kind: oarlock.ResponseRejected, Code: code, Reason: reason}
}

// ---- prompt -----------------------------------------------------------------

func (s *Session) Prompt(ctx context.Context, input string, opt oarlock.PromptOptions) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	out := s.k.Control(oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: input, InputID: opt.InputID,
		Images: opt.Images, Origin: opt.Origin, Carries: opt.Carries},
		func(req oarlock.Record) oarlock.ResponseBody {
			switch s.statusBefore(req.Seq).Kind {
			case fold.StatusRunning:
				return rejected(oarlock.Busy, "a turn is already running; wait for its end or abort it")
			case fold.StatusExited:
				return rejected(oarlock.RuntimeExited, "runtime exited")
			}
			return s.send(req.Request, kindTurn, s.prof.PromptMeta)
		}, kernel.RecordAt{})
	s.kick()
	return out, nil
}

// send puts a session/prompt on the wire and returns once it was written. The
// answer arrives as a frame; it is never awaited here.
func (s *Session) send(req *oarlock.RequestBody, kind promptKind, meta func(oarlock.Options) map[string]any) oarlock.ResponseBody {
	blocks, err := s.blocks(req)
	if err != nil {
		var rej *oarlock.Rejected
		if errors.As(err, &rej) {
			return rejected(rej.Code, rej.Reason)
		}
		return rejected(oarlock.ErrorCode, err.Error())
	}
	params := sdk.PromptRequest{SessionId: sdk.SessionId(s.id), Prompt: blocks}
	if kind == kindSteer {
		params.Meta = s.prof.SteerMeta()
	} else if meta != nil {
		params.Meta = meta(s.opt)
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	call := &promptCall{kind: kind, written: make(chan struct{})}
	s.sink.arm(call)
	early := make(chan error, 1)
	go func() {
		_, err := s.conn.Prompt(s.ctx, params)
		if kind == kindTurn || s.prof.SteerSupersedes {
			s.cancelling.Store(false)
		}
		early <- err
		s.kick()
	}()
	select {
	case <-call.written:
		return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
	case err := <-early:
		s.sink.disarm(call)
		if err == nil {
			return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
		}
		return rejected(oarlock.RuntimeExited, err.Error())
	case <-s.readDone:
		s.sink.disarm(call)
		return rejected(oarlock.RuntimeExited, "runtime exited")
	}
}

func (s *Session) blocks(req *oarlock.RequestBody) ([]sdk.ContentBlock, error) {
	var out []sdk.ContentBlock
	if req.Input != "" {
		out = append(out, sdk.TextBlock(req.Input))
	}
	if len(req.Images) > 0 && !s.images {
		return nil, &oarlock.Rejected{Code: oarlock.Unsupported, Reason: "the agent did not advertise promptCapabilities.image"}
	}
	for _, img := range req.Images {
		data, err := os.ReadFile(img.Path)
		if err != nil {
			return nil, &oarlock.Rejected{Code: oarlock.Unsupported, Reason: "cannot read image: " + err.Error()}
		}
		mt := img.MediaType
		if mt == "" {
			mt = "image/png"
		}
		out = append(out, sdk.ImageBlock(base64.StdEncoding.EncodeToString(data), mt))
	}
	return out, nil
}

// ---- steer ------------------------------------------------------------------

// Steer adds input to the running turn through the vendor's own channel
// (Profile.SteerMeta). Without a running turn it is rejected no_active_turn.
func (s steerSession) Steer(ctx context.Context, input string, opt oarlock.InputOptions) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	return s.k.Control(oarlock.RequestBody{Action: oarlock.ActionSteer, Input: input, InputID: opt.InputID, Images: opt.Images, Origin: opt.Origin},
		func(req oarlock.Record) oarlock.ResponseBody {
			switch s.statusBefore(req.Seq).Kind {
			case fold.StatusRunning:
				return s.send(req.Request, kindSteer, nil)
			case fold.StatusExited:
				return rejected(oarlock.RuntimeExited, "runtime exited")
			}
			return rejected(oarlock.NoActiveTurn, "no active turn")
		}, kernel.RecordAt{}), nil
}

// ---- queue ------------------------------------------------------------------

// Queue holds input in the adapter and sends it as an ordinary prompt once the
// stream says idle.
func (s *Session) Queue(ctx context.Context, input string, opt oarlock.InputOptions) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	out := s.k.Control(oarlock.RequestBody{Action: oarlock.ActionQueue, Input: input, InputID: opt.InputID, Images: opt.Images, Origin: opt.Origin},
		func(req oarlock.Record) oarlock.ResponseBody {
			if len(req.Request.Images) > 0 && !s.images {
				return rejected(oarlock.Unsupported, "the agent did not advertise promptCapabilities.image")
			}
			s.mu.Lock()
			s.held = append(s.held, heldInput{req.Request.Input, req.Request.InputID, req.Request.Origin, req.Request.Images})
			s.mu.Unlock()
			return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
		}, kernel.RecordAt{})
	s.kick()
	return out, nil
}

// kick drains held input while the stream is idle. One drainer at a time.
func (s *Session) kick() {
	s.mu.Lock()
	if s.draining {
		s.again = true
		s.mu.Unlock()
		return
	}
	s.draining = true
	s.mu.Unlock()
	go func() {
		for {
			s.mu.Lock()
			if len(s.held) == 0 || s.k.Unreachable() != nil {
				s.draining, s.again = false, false
				s.mu.Unlock()
				return
			}
			s.again = false
			s.mu.Unlock()
			if s.status().Kind != fold.StatusIdle {
				s.mu.Lock()
				if !s.again {
					s.draining = false
					s.mu.Unlock()
					return
				}
				s.mu.Unlock()
				time.Sleep(5 * time.Millisecond)
				continue
			}
			s.mu.Lock()
			h := s.held[0]
			s.held = s.held[1:]
			s.mu.Unlock()
			_, _ = s.Prompt(context.Background(), h.input, oarlock.PromptOptions{InputOptions: oarlock.InputOptions{InputID: h.inputID, Images: h.images, Origin: h.origin}})
		}
	}()
}

// ---- abort ------------------------------------------------------------------

// Abort sends session/cancel. With no turn it is rejected no_active_turn.
// After the grace period a turn that still runs is taken over: the process
// tree is killed, the abort stays accepted, the exit is recorded as exited and
// no turn_ended is written for it.
func (s *Session) Abort(ctx context.Context) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	return s.k.Control(oarlock.RequestBody{Action: oarlock.ActionAbort}, func(oarlock.Record) oarlock.ResponseBody {
		if s.status().Kind != fold.StatusRunning {
			return rejected(oarlock.NoActiveTurn, "no active turn")
		}
		s.cancelling.Store(true)
		if err := s.conn.Cancel(context.WithoutCancel(ctx), sdk.CancelNotification{SessionId: sdk.SessionId(s.id)}); err != nil {
			s.cancelling.Store(false)
			return rejected(oarlock.ErrorCode, err.Error())
		}
		s.mu.Lock()
		if s.graceTimer == nil {
			s.graceTimer = time.AfterFunc(s.grace, func() {
				if s.status().Kind == fold.StatusRunning {
					_ = s.cmd.Close(context.Background())
				}
			})
		}
		s.mu.Unlock()
		return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
	}, kernel.RecordAt{}), nil
}

// ---- dispose and exit -------------------------------------------------------

// Dispose is idempotent: the first call records one dispose request, ends the
// process tree and waits for the exited response; later calls share its result.
func (s *Session) Dispose(ctx context.Context) error {
	s.mu.Lock()
	if s.disposeDone == nil {
		s.disposeDone = make(chan struct{})
		go s.runDispose(s.disposeDone)
	}
	done := s.disposeDone
	s.mu.Unlock()
	select {
	case <-done:
		return s.disposeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Session) runDispose(done chan struct{}) {
	defer close(done)
	s.k.Control(oarlock.RequestBody{Action: oarlock.ActionDispose}, func(req oarlock.Record) oarlock.ResponseBody {
		s.mu.Lock()
		s.disposeReq = req.ID
		s.mu.Unlock()
		return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
	}, kernel.RecordAt{})
	s.mu.Lock()
	s.held = nil
	if s.graceTimer != nil {
		s.graceTimer.Stop()
	}
	s.mu.Unlock()
	s.cancel()
	s.term.closeAll()
	_ = s.cmd.Stdin().Close()
	s.disposeErr = s.cmd.Close(context.Background())
	<-s.exitRecorded
}

func (s *Session) watchExit() {
	st, _ := s.cmd.Wait(context.Background())
	select {
	case <-s.readDone:
	case <-time.After(5 * time.Second):
	}
	s.mu.Lock()
	requestID := s.disposeReq
	s.held = nil
	if s.graceTimer != nil {
		s.graceTimer.Stop()
	}
	s.mu.Unlock()
	body := oarlock.ResponseBody{Kind: oarlock.ResponseExited, Reason: s.stderr.String(), ExitCodeReported: true}
	if st.Signal == nil {
		code := st.Code
		body.ExitCode = &code
	}
	s.k.Respond(requestID, body, kernel.RecordAt{})
	close(s.exitRecorded)
	s.cancel()
}

// tail keeps the end of stderr.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4096 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

var _ = fmt.Sprintf
