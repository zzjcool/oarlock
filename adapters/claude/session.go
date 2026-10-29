package claude

// The control plane of one claude stream-json session: oarlock.Session (and
// oarlock.Steerer, oarlock.ContextBreakdownReader) over a streamjson.Conn,
// recorded on a real kernel.
//
//   - Every stdout line becomes exactly one frame, in wire order, Native
//     verbatim. Reading it is internal/streamjson's Folder with this package's
//     profile (project.go): the same function for the live pump and for a replay.
//   - Reading and writing are decoupled (the Conn's writer goroutine), and no
//     control holds a lock while it waits, so a large stdin line cannot dead-lock
//     against a child that is busy writing stdout.
//   - Status is never cached: busy, no_active_turn and the queue drain all fold
//     the retained records, so the answer agrees with what a host reading the
//     stream sees.
//   - Steer writes a user message with a uuid and is accepted ONLY when claude
//     echoes it (--replay-user-messages: a user frame with isReplay and the same
//     uuid). Without the echo it is rejected; the rejection reason says who owns
//     the input, read off claude's own command_lifecycle frames for that uuid.
//   - Abort sends a control_request interrupt, arms the take-over timer and
//     answers accepted; the turn's end is claude's own result frame (aborted,
//     because an abort was requested). If the turn has not ended when the grace
//     is up, the process group gets SIGTERM and, after the kill grace, SIGKILL.
//
// Known gaps, on purpose: images are not sent (Capabilities.Images=false); there
// is no Withdrawer; held queue input does not survive an exit or a dispose; and
// Events callbacks run inline on the pump, so they must not call controls.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
	"github.com/zzjcool/oarlock/internal/kernel"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

const (
	// DefaultAbortGrace is how long an accepted abort may leave the turn running
	// before oarlock takes over and kills the process group (OAR's default).
	DefaultAbortGrace = 10 * time.Second

	// DefaultEchoTimeout is how long a steer waits for claude's echo of its
	// user message before it is rejected.
	DefaultEchoTimeout = 10 * time.Second

	// privatePrefix marks control_request ids of private readbacks (context
	// breakdown, usage baseline, resume initialize): their answers can carry
	// account details or local paths, so they are consumed, never recorded.
	privatePrefix = "oar-"

	exitRecordedWait = 30 * time.Second
	recordedWait     = 10 * time.Second
)

// SessionOptions configures NewSession.
type SessionOptions struct {
	// SessionID is claude's session id: the stream instance's identity. Required.
	SessionID string
	// AbortGrace: 0 = DefaultAbortGrace; negative is an error.
	AbortGrace time.Duration
	// EchoTimeout: 0 = DefaultEchoTimeout; negative is an error.
	EchoTimeout time.Duration
	// Redact is the kernel's credential hook; nil retains records as built.
	Redact kernel.Redactor
	// Resumed starts the usage ledger without a baseline (see usage.go).
	Resumed bool
}

// Session is a claude stream-json session.
type Session struct {
	conn sj.Conn
	k    kernel.Kernel
	fold *sj.Folder
	prof *profile

	grace time.Duration
	echo  time.Duration

	pumpDone     chan struct{}
	exitRecorded chan struct{}
	exitStatus   sj.ExitStatus

	mu          sync.Mutex
	changed     chan struct{}
	pending     map[string]chan pendingAnswer
	echoes      map[string]*pendingEcho
	opening     chan sj.Rec // resume: a startup failure result
	held        []heldInput
	nextID      uint64
	fb          *fallbackState
	disposeReq  string
	disposeDone chan struct{}
	disposeErr  error
	goneForRead bool
	breakdown   *breakdownCall
	drainMu     sync.Mutex
}

var (
	_ oarlock.Session                = (*Session)(nil)
	_ oarlock.Steerer                = (*Session)(nil)
	_ oarlock.ContextBreakdownReader = (*Session)(nil)
)

type pendingAnswer struct {
	rec sj.Rec
	raw json.RawMessage
	err error // the process ended before it answered
}

// pendingEcho is one steer waiting for claude's echo. landed closes on the
// echoed user frame (the only acceptance evidence); lifecycle is the last
// command_lifecycle state claude reported for the same uuid (msg_lifecycle_v1:
// queued / started / completed / cancelled), which states who owns the input
// when the echo never comes.
type pendingEcho struct {
	landed    chan struct{}
	lifecycle string
}

// lifecycleCancelled is the one command_lifecycle state that hands the input
// back: claude dropped it, so it is still the caller's.
const lifecycleCancelled = "cancelled"

type heldInput struct {
	input, inputID, origin, queueID string
}

type fallbackState struct {
	timer *time.Timer
	fired chan struct{}
}

func (fb *fallbackState) isFired() bool {
	select {
	case <-fb.fired:
		return true
	default:
		return false
	}
}

// NewSession wraps a started Conn. It takes ownership: Dispose (or a take-over)
// closes it. The pump starts reading conn.Events() immediately.
func NewSession(conn sj.Conn, opt SessionOptions) (*Session, error) {
	switch {
	case conn == nil:
		return nil, errors.New("claude: NewSession(nil Conn)")
	case opt.SessionID == "":
		return nil, errors.New("claude: SessionOptions.SessionID is required")
	case opt.AbortGrace < 0:
		return nil, fmt.Errorf("claude: SessionOptions.AbortGrace is negative (%v)", opt.AbortGrace)
	case opt.EchoTimeout < 0:
		return nil, fmt.Errorf("claude: SessionOptions.EchoTimeout is negative (%v)", opt.EchoTimeout)
	}
	grace, echo := opt.AbortGrace, opt.EchoTimeout
	if grace == 0 {
		grace = DefaultAbortGrace
	}
	if echo == 0 {
		echo = DefaultEchoTimeout
	}
	prof := newProfile(opt.Resumed)
	s := &Session{
		conn:         conn,
		k:            kernel.New(opt.SessionID, opt.Redact),
		fold:         sj.NewFolder(prof),
		prof:         prof,
		grace:        grace,
		echo:         echo,
		pumpDone:     make(chan struct{}),
		exitRecorded: make(chan struct{}),
		changed:      make(chan struct{}),
		pending:      map[string]chan pendingAnswer{},
		echoes:       map[string]*pendingEcho{},
	}
	go s.pump()
	go s.watchExit()
	return s, nil
}

// Capabilities: the queue lives in the adapter; sub-agent frames carry their
// AgentPath; images are not sent.
func (s *Session) Capabilities() oarlock.Capabilities {
	return oarlock.Capabilities{
		Queue:       oarlock.QueueCapability{Durable: false},
		Attribution: oarlock.AttributionAttributed,
		Images:      false,
	}
}

// Records returns the retained records after the cursor.
func (s *Session) Records(cursor oarlock.Cursor) []oarlock.Record { return s.k.Records(cursor) }

// Events replays the retained records after *cursor, then delivers live ones. A
// nil cursor is live only. The callback runs inline on the appending goroutine:
// it must be cheap and must not call a control.
func (s *Session) Events(cursor *oarlock.Cursor, fn func(oarlock.Record)) (unsubscribe func()) {
	if cursor == nil {
		return s.k.Observe(fn)
	}
	cancel, err := s.k.ObserveFrom(*cursor, fn)
	if err != nil {
		return func() {}
	}
	return cancel
}

// Unknown is a copy of the tally of top-level frame types the fold does not
// know. A non-zero tally is a test signal; the frames stay in Native.
func (s *Session) Unknown() map[string]int { return s.fold.Unknown() }

// ---- prompt ------------------------------------------------------------------

// Prompt starts a turn: rejected busy while the stream says one is running,
// accepted once the user message is on claude's stdin.
func (s *Session) Prompt(ctx context.Context, input string, opt oarlock.PromptOptions) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	out := s.prompt(ctx, oarlock.RequestBody{
		Action: oarlock.ActionPrompt, Input: input, InputID: opt.InputID,
		Images: opt.Images, Origin: opt.Origin, Carries: opt.Carries,
	})
	return out, nil
}

func (s *Session) prompt(ctx context.Context, body oarlock.RequestBody) oarlock.ControlOutcome {
	return s.k.Control(body, func(req oarlock.Record) oarlock.ResponseBody {
		switch s.statusBefore(req.Seq).Kind {
		case fold.StatusRunning:
			return rejected(oarlock.Busy, "a turn is already running; wait for its end or abort it")
		case fold.StatusExited:
			return rejected(oarlock.RuntimeExited, "runtime exited")
		}
		if len(req.Request.Images) > 0 {
			return rejected(oarlock.Unsupported, "the Claude session does not send images yet")
		}
		s.fold.Prompted()
		if err := s.write(ctx, userMessage(req.Request.Input, req.Request.InputID)); err != nil {
			return writeRejected(err)
		}
		return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
	}, kernel.RecordAt{})
}

// userMessage is one stream-json user message line.
func userMessage(text, uuid string) []byte {
	msg := map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}},
	}
	if uuid != "" {
		msg["uuid"] = uuid
	}
	b, _ := json.Marshal(msg)
	return append(b, '\n')
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (s *Session) write(ctx context.Context, line []byte) error {
	// Not ctx: a prompt abandoned half-way could still start a turn while the
	// stream says idle. The Conn's write timeout bounds it.
	return s.conn.Write(context.WithoutCancel(ctx), line)
}

func writeRejected(err error) oarlock.ResponseBody {
	if errors.Is(err, sj.ErrExited) {
		return rejected(oarlock.RuntimeExited, err.Error())
	}
	return rejected(oarlock.ErrorCode, err.Error())
}

func (s *Session) statusBefore(seq uint64) fold.Status {
	records := s.k.Records(oarlock.Cursor{})
	if n := int(seq) - 1; n >= 0 && n < len(records) {
		records = records[:n]
	}
	return fold.StatusOf(records).Value
}

func (s *Session) status() fold.Status {
	return fold.StatusOf(s.k.Records(oarlock.Cursor{})).Value
}

// ---- steer -------------------------------------------------------------------

// Steer writes a user message into the running turn. It is accepted only when
// claude echoes the message back (the replayed user frame with the same uuid):
// that is the evidence it landed. Without the echo it is rejected; the reason
// then says who owns the input — claude's own command_lifecycle frames decide.
// A state other than "cancelled" means claude took the input over (on 2.1.312 a
// mid-turn steer with no request boundary is queued and applied as a follow-up
// turn), so the caller must not resend it. With no such frame, or after
// "cancelled", the input is still the caller's, because a very late echo is
// possible and is then an ordinary user_message frame.
func (s *Session) Steer(ctx context.Context, input string, opt oarlock.InputOptions) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	return s.k.Control(oarlock.RequestBody{
		Action: oarlock.ActionSteer, Input: input, InputID: opt.InputID, Images: opt.Images, Origin: opt.Origin,
	}, func(req oarlock.Record) oarlock.ResponseBody {
		return s.decideSteer(ctx, req)
	}, kernel.RecordAt{}), nil
}

func (s *Session) decideSteer(ctx context.Context, req oarlock.Record) oarlock.ResponseBody {
	if st := s.status(); st.Kind != fold.StatusRunning {
		if st.Kind == fold.StatusExited {
			return rejected(oarlock.RuntimeExited, "runtime exited")
		}
		return rejected(oarlock.NoActiveTurn, "not_steerable: no active turn")
	}
	if len(req.Request.Images) > 0 {
		return rejected(oarlock.Unsupported, "the Claude session does not send images yet")
	}
	uuid := req.Request.InputID
	if uuid == "" {
		uuid = newUUID()
	}
	landed := make(chan struct{})
	s.mu.Lock()
	s.echoes[uuid] = &pendingEcho{landed: landed}
	changed := s.changed
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.echoes, uuid)
		s.mu.Unlock()
	}()

	if err := s.write(ctx, userMessage(req.Request.Input, uuid)); err != nil {
		return writeRejected(err)
	}
	timer := time.NewTimer(s.echo)
	defer timer.Stop()
	for {
		select {
		case <-landed:
			return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
		default:
		}
		switch s.status().Kind {
		case fold.StatusExited:
			return rejected(oarlock.RuntimeExited, "runtime exited before it echoed the steer; "+s.steerOwnershipAfterExit(uuid))
		case fold.StatusIdle:
			select {
			case <-landed:
				return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
			default:
			}
			return rejected(oarlock.NoActiveTurn, "the turn ended before claude echoed the steer; "+s.steerOwnership(uuid, false))
		}
		select {
		case <-landed:
			return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
		case <-changed:
			s.mu.Lock()
			changed = s.changed
			s.mu.Unlock()
		case <-timer.C:
			return rejected(oarlock.ErrorCode, "claude did not echo the steer within "+s.echo.String()+"; "+s.steerOwnership(uuid, true))
		case <-ctx.Done():
			return rejected(oarlock.ErrorCode, "steer abandoned before claude echoed it: "+ctx.Err().Error()+"; "+s.steerOwnership(uuid, false))
		}
	}
}

// steerOwnership is the honest half of a steer rejection: it says who holds the
// input now, never that claude accepted it. command_lifecycle is claude's own
// receipt (msg_lifecycle_v1); any state but "cancelled" means claude received
// the steer and owns it from here (on 2.1.312 a steer inside a turn with no
// request boundary is queued and applied as a follow-up turn after the result),
// so a second write would duplicate it. No lifecycle frame — or a cancelled one
// — leaves the input with the caller; lateEcho notes the late-echo possibility
// the timeout reason always had.
func (s *Session) steerOwnership(uuid string, lateEcho bool) string {
	state := s.echoLifecycle(uuid)
	if state != "" && state != lifecycleCancelled {
		return "claude received the steer (command_lifecycle " + state + ") and owns it from here; do not resend it"
	}
	if lateEcho {
		return "the input is still yours (a late echo would appear as a user_message frame)"
	}
	return "the input is still yours"
}

// steerOwnershipAfterExit is the exit path's statement: a lifecycle receipt
// proves claude saw the steer, but the process is gone, so it may never have
// been applied and a resend into a new session can be the right move.
func (s *Session) steerOwnershipAfterExit(uuid string) string {
	state := s.echoLifecycle(uuid)
	if state != "" && state != lifecycleCancelled {
		return "claude received the steer (command_lifecycle " + state + ") before the exit; it may not have been applied"
	}
	return "the input is still yours"
}

// echoLifecycle is the last lifecycle state claude reported for a pending steer
// uuid; empty when it never said anything (or the steer is no longer pending).
func (s *Session) echoLifecycle(uuid string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.echoes[uuid]; e != nil {
		return e.lifecycle
	}
	return ""
}

// ---- queue -------------------------------------------------------------------

// Queue holds input in the adapter and sends it as an ordinary prompt of its
// own once nothing is running.
func (s *Session) Queue(ctx context.Context, input string, opt oarlock.InputOptions) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	out := s.k.Control(oarlock.RequestBody{
		Action: oarlock.ActionQueue, Input: input, InputID: opt.InputID, Images: opt.Images, Origin: opt.Origin,
	}, func(req oarlock.Record) oarlock.ResponseBody {
		if len(req.Request.Images) > 0 {
			return rejected(oarlock.Unsupported, "the Claude session does not send images yet")
		}
		s.mu.Lock()
		s.held = append(s.held, heldInput{input: req.Request.Input, inputID: req.Request.InputID, origin: req.Request.Origin, queueID: req.ID})
		s.mu.Unlock()
		return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
	}, kernel.RecordAt{})
	s.kick()
	return out, nil
}

func (s *Session) kick() {
	s.mu.Lock()
	n := len(s.held)
	s.mu.Unlock()
	if n == 0 {
		return
	}
	go s.drain()
}

// drain sends held input while the stream says idle. Drains are serialised, so
// two kicks never send the same input twice.
func (s *Session) drain() {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	for {
		if s.k.Unreachable() != nil {
			s.mu.Lock()
			s.held = nil
			s.mu.Unlock()
			return
		}
		if s.status().Kind != fold.StatusIdle {
			return
		}
		s.mu.Lock()
		if len(s.held) == 0 {
			s.mu.Unlock()
			return
		}
		h := s.held[0]
		s.held = s.held[1:]
		s.mu.Unlock()

		carries := h.inputID
		if carries == "" {
			carries = h.queueID
		}
		out := s.prompt(context.Background(), oarlock.RequestBody{
			Action: oarlock.ActionPrompt, Input: h.input, InputID: h.inputID, Origin: h.origin, Carries: []string{carries},
		})
		if out.Kind == oarlock.ControlRejected {
			switch out.Code {
			case oarlock.Busy:
				s.mu.Lock()
				s.held = append([]heldInput{h}, s.held...)
				s.mu.Unlock()
				return
			case oarlock.RuntimeExited, oarlock.Disposed:
				s.mu.Lock()
				s.held = nil
				s.mu.Unlock()
				return
			}
		}
	}
}

// ---- abort -------------------------------------------------------------------

// Abort asks claude to interrupt the running turn. With no turn it is rejected
// no_active_turn. Otherwise the interrupt goes out and the take-over timer
// starts; the turn then ends with claude's own result, read as aborted. If it
// has not ended when AbortGrace is up the process group is closed (SIGTERM,
// then SIGKILL after the kill grace), the abort is still accepted ("we took the
// stop") and the exit is recorded as the exited response; no turn_ended is
// invented for it.
func (s *Session) Abort(ctx context.Context) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	return s.k.Control(oarlock.RequestBody{Action: oarlock.ActionAbort}, func(oarlock.Record) oarlock.ResponseBody {
		return s.decideAbort(ctx)
	}, kernel.RecordAt{}), nil
}

func (s *Session) decideAbort(ctx context.Context) oarlock.ResponseBody {
	if s.status().Kind != fold.StatusRunning {
		return rejected(oarlock.NoActiveTurn, "no active turn")
	}
	s.mu.Lock()
	s.nextID++
	id := fmt.Sprintf("interrupt-%d", s.nextID)
	ch := make(chan pendingAnswer, 1)
	s.pending[id] = ch
	changed := s.changed
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	fb := s.armFallback()
	s.fold.AbortRequested()
	req, _ := json.Marshal(map[string]any{"type": "control_request", "request_id": id, "request": map[string]any{"subtype": "interrupt"}})
	if err := s.write(ctx, append(req, '\n')); err != nil {
		s.disarm(fb)
		s.fold.AbortCleared()
		return writeRejected(err)
	}
	for {
		select {
		case a := <-ch:
			if a.err != nil {
				// The process ended before it answered: the exited record or the
				// fired fallback decides.
				if fb.isFired() {
					return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
				}
				return rejected(oarlock.RuntimeExited, "runtime exited before it answered the abort")
			}
			resp := sj.AsRec(a.rec["response"])
			if sj.Str(resp["subtype"]) == "error" || sj.Str(resp["error"]) != "" {
				// A refusal cancels only this attempt and kills nothing.
				s.disarm(fb)
				s.fold.AbortCleared()
				reason := sj.Str(resp["error"])
				if reason == "" {
					reason = "interrupt refused"
				}
				return oarlock.ResponseBody{Kind: oarlock.ResponseRejected, Code: oarlock.RuntimeRefused, Reason: reason, Native: a.raw}
			}
			return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted, Native: a.raw}
		case <-changed:
			s.mu.Lock()
			changed = s.changed
			s.mu.Unlock()
			switch s.status().Kind {
			case fold.StatusIdle:
				return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
			case fold.StatusExited:
				if fb.isFired() {
					return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
				}
				return rejected(oarlock.RuntimeExited, "runtime exited before it answered the abort")
			}
		case <-fb.fired:
			s.waitExitRecorded(ctx)
			return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
		case <-ctx.Done():
			// The take-over timer stays armed, so the stop is ours either way.
			return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
		}
	}
}

func (s *Session) armFallback() *fallbackState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fb == nil {
		fb := &fallbackState{fired: make(chan struct{})}
		fb.timer = time.AfterFunc(s.grace, func() { s.fireFallback(fb) })
		s.fb = fb
	}
	return s.fb
}

// disarm withdraws a refused attempt's timer unless it already fired.
func (s *Session) disarm(fb *fallbackState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fb == fb && !fb.isFired() {
		fb.timer.Stop()
		s.fb = nil
	}
}

func (s *Session) clearFallback() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fb != nil {
		s.fb.timer.Stop()
		s.fb = nil
	}
}

func (s *Session) fireFallback(fb *fallbackState) {
	s.mu.Lock()
	current := s.fb == fb
	s.mu.Unlock()
	if !current || s.status().Kind != fold.StatusRunning {
		return
	}
	close(fb.fired)
	s.markGone()
	go func() { _ = s.conn.Close() }()
	s.signal()
}

func (s *Session) waitExitRecorded(ctx context.Context) {
	t := time.NewTimer(exitRecordedWait)
	defer t.Stop()
	select {
	case <-s.exitRecorded:
	case <-ctx.Done():
	case <-t.C:
	}
}

// ---- dispose -----------------------------------------------------------------

// Dispose releases the session: idempotent; the first call records one dispose
// request, closes stdin, escalates SIGTERM then SIGKILL of the group and waits
// for the exited response. ctx bounds only the wait.
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
	s.markGone()
	s.k.Control(oarlock.RequestBody{Action: oarlock.ActionDispose}, func(req oarlock.Record) oarlock.ResponseBody {
		s.mu.Lock()
		s.disposeReq = req.ID
		s.mu.Unlock()
		return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
	}, kernel.RecordAt{})
	s.mu.Lock()
	s.held = nil
	s.mu.Unlock()
	s.clearFallback()
	s.disposeErr = s.conn.Close()
	s.waitExitRecorded(context.Background())
}

// ---- pump and exit -------------------------------------------------------------

func (s *Session) pump() {
	defer close(s.pumpDone)
	for m := range s.conn.Events() {
		s.ingest(m)
	}
}

func (s *Session) ingest(m sj.Message) {
	fr := s.fold.Fold(m.Raw)

	// command_lifecycle is claude's own receipt of one input, keyed by the uuid
	// the host wrote. For a pending steer it is the only statement about who
	// owns the input when the echo never arrives; it is never acceptance
	// evidence — that stays the echoed user frame below.
	if fr.Type == "command_lifecycle" {
		s.mu.Lock()
		if e := s.echoes[sj.Str(fr.Rec["command_uuid"])]; e != nil {
			e.lifecycle = sj.Str(fr.Rec["state"])
		}
		s.mu.Unlock()
	}

	// A private readback's answer is consumed, never recorded.
	var answer *pendingAnswer
	var answerCh chan pendingAnswer
	if fr.Response != nil {
		s.mu.Lock()
		answerCh = s.pending[fr.Response.RequestID]
		s.mu.Unlock()
		if answerCh != nil {
			answer = &pendingAnswer{rec: fr.Rec, raw: m.Raw}
		}
		if hasPrefix(fr.Response.RequestID, privatePrefix) {
			if answer != nil {
				answerCh <- *answer
			}
			return
		}
	}
	// A startup failure (a resume that found nothing) is also consumed by open.
	if fr.Type == "result/error_during_execution" {
		s.mu.Lock()
		o := s.opening
		s.mu.Unlock()
		if o != nil {
			select {
			case o <- fr.Rec:
			default:
			}
		}
	}

	s.k.Frame(kernel.FrameBody{Native: m.Raw, Events: fr.Events}, kernel.RecordAt{AgentPath: fr.AgentPath})
	if fr.Request != nil {
		// The runtime asks US something: always visible, never answered here
		// (headless runs skip permissions; recorded, not answered).
		s.k.Request(oarlock.ToApp, oarlock.RequestBody{
			Action: oarlock.ActionNative, NativeType: fr.Request.Subtype, Native: m.Raw,
		}, kernel.RecordAt{ID: fr.Request.ID})
	}
	for _, ev := range fr.Events {
		if ev.Kind == oarlock.EventUserMessage && ev.InputID != "" {
			s.mu.Lock()
			if e := s.echoes[ev.InputID]; e != nil {
				delete(s.echoes, ev.InputID)
				close(e.landed)
			}
			s.mu.Unlock()
		}
	}
	if answer != nil {
		answerCh <- *answer // after the frame: the abort response follows its frame
	}
	if fr.TurnEnded {
		s.clearFallback()
	}
	s.signal()
	if fr.TurnEnded {
		s.kick()
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

// watchExit records the process exit once everything claude wrote has been
// recorded: the exited response answers the dispose request when oarlock ended
// the process that way, and stands alone otherwise.
func (s *Session) watchExit() {
	st := <-s.conn.Exit()
	<-s.pumpDone
	s.mu.Lock()
	s.exitStatus = st
	requestID := s.disposeReq
	s.held = nil
	s.goneForRead = true
	pend := s.pending
	s.pending = map[string]chan pendingAnswer{}
	s.mu.Unlock()
	s.clearFallback()
	for _, ch := range pend {
		select {
		case ch <- pendingAnswer{err: sj.ErrExited}:
		default:
		}
	}

	body := oarlock.ResponseBody{Kind: oarlock.ResponseExited, Reason: st.StderrTail, ExitCodeReported: true}
	if st.Signal == nil {
		code := st.Code
		body.ExitCode = &code
	}
	sig := ""
	if st.Signal != nil {
		sig = st.Signal.String()
	}
	if native, err := json.Marshal(map[string]any{"cause": st.Cause, "signal": sig}); err == nil {
		body.Native = native
	}
	s.k.Respond(requestID, body, kernel.RecordAt{})
	close(s.exitRecorded)
	s.signal()
}

func (s *Session) markGone() {
	s.mu.Lock()
	s.goneForRead = true
	s.mu.Unlock()
}

func (s *Session) signal() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

func rejected(code oarlock.Code, reason string) oarlock.ResponseBody {
	return oarlock.ResponseBody{Kind: oarlock.ResponseRejected, Code: code, Reason: reason}
}

// request writes a private control_request (id must carry privatePrefix) and
// waits for its answer, the process's end, the timeout or ctx.
func (s *Session) request(ctx context.Context, id string, request map[string]any, timeout time.Duration) (sj.Rec, error) {
	ch := make(chan pendingAnswer, 1)
	s.mu.Lock()
	if s.goneForRead {
		s.mu.Unlock()
		return nil, sj.ErrExited
	}
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()
	line, _ := json.Marshal(map[string]any{"type": "control_request", "request_id": id, "request": request})
	if err := s.conn.Write(ctx, append(line, '\n')); err != nil {
		return nil, err
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case a := <-ch:
		if a.err != nil {
			return nil, a.err
		}
		return a.rec, nil
	case <-t.C:
		return nil, fmt.Errorf("claude did not answer %v within %v", request["subtype"], timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Session) nextPrivateID(kind string) string {
	s.mu.Lock()
	s.nextID++
	n := s.nextID
	s.mu.Unlock()
	return fmt.Sprintf("%s%s-%d-%s", privatePrefix, kind, n, newUUID()[:8])
}
