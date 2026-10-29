package cursor

// session.go — the control plane of one cursor-agent chat: oarlock.Session over
// the shared stream-json transport, recorded on a real kernel.
//
// The CLI is one-shot, so the lifetime model differs from Claude's in exactly
// one place: a turn owns a process. `Prompt` starts a fresh cursor-agent with
// LaunchArgs (BaseArgs + --model + --resume <chat id> once known), writes the
// prompt as free text on stdin and closes stdin, which starts the run. The
// process emits `system/init` … `result` and exits; the next prompt starts the
// next process on the same chat.
//
//   - Every stdout line becomes exactly one frame, in wire order, Native
//     verbatim. Reading it is internal/streamjson's Folder with this package's
//     profile (project.go): the same function the offline replay uses.
//   - A fresh Folder per process. Folder.Prompted() is never called, so the
//     runtime's own `system/init` is what states turn_active.
//   - The chat id is whatever `system/init` says (`session_id`). It is stamped
//     on every later record through kernel.RecordAt.SessionID, and the kernel's
//     own session id is Options.Resume at open ("" for a fresh run).
//   - A turn runs from the prompt to `result` or to the process exit. Busy and
//     idle are read from the session's own state, never from the record fold:
//     the fold reads a process exit as session death, and here an exit is the
//     normal end of a turn.
//   - A turn that reached its `result` frame ends with `turn_ended`; the
//     process exit that follows is that turn's lifecycle, not the session's
//     outcome, so no `exited` response is recorded for it (the host would be
//     told a session that can still `--resume` is over). The status is kept for
//     the adapter's own evidence: LastExit.
//   - An exit WITHOUT a `result` for the running turn — abort, crash, a startup
//     failure such as a bad `--resume` id — IS the session's outcome: it is
//     recorded as an `exited` response (exit code, signal, stderr tail as
//     Reason, cause in Native), and the kernel then refuses every later control
//     `runtime_exited`. Nothing that ends a turn is invented: a process that
//     died without `result` gets no `turn_ended`. The chat can be resumed from
//     ChatID in a new session.
//   - Abort closes the process group (SIGTERM, then SIGKILL after AbortGrace)
//     and waits for the exit. The turn ends with the runtime's own frames, or
//     with the exit when the process died first; the abort record itself is
//     accepted, never a fabricated turn end.
//   - Queue is held in the adapter (Capabilities.Queue.Durable = false) and
//     drained as an ordinary prompt once nothing is running; a drain starts its
//     own process.
//   - No Steer: the CLI has no mid-run input channel, and absence of the
//     interface is the capability (oarlock.Steerer). No Withdrawer either.
//
// Known gaps, on purpose: images are refused (the CLI takes text on stdin);
// held queue input does not survive a dispose; and Events callbacks run inline
// on the pump, so they must not call controls.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/kernel"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

const (
	// DefaultAbortGrace is how long a closed process group gets between SIGTERM
	// and SIGKILL (sj.Options.KillGrace), and how long an accepted abort may
	// wait for the exit before answering anyway (OAR's default).
	DefaultAbortGrace = 10 * time.Second

	defaultEventBuffer = 4096
	exitRecordedWait   = 30 * time.Second
)

// SessionOptions configures a Cursor session.
type SessionOptions struct {
	// ChatID is the CLI chat the session opens on. Open takes Options.Resume
	// instead when it is non-empty; "" starts a fresh chat whose id is learned
	// from system/init.
	ChatID string
	// AbortGrace is the SIGTERM→SIGKILL window of a closed process group, and
	// the bound on waiting for the exit: 0 = DefaultAbortGrace; negative is
	// refused by Open.
	AbortGrace time.Duration
	// Redact is the kernel's credential hook; nil retains records as built.
	Redact kernel.Redactor
}

// sessionConfig is everything newSession needs. Tests build it directly to
// point command at a fake cursor-agent; open fills it from the Installation and
// the Options.
type sessionConfig struct {
	command string
	args    func(chatID string) []string // argv of one turn
	dir     string
	env     []string
	chatID  string // the chat the session opens on, "" for a fresh one
	redact  kernel.Redactor

	abortGrace time.Duration
	// newProcess overrides the real sj.Launch of command/args/dir/env. nil
	// means: start cursor-agent.
	newProcess func(ctx context.Context) (*sj.Process, error)
}

// Session is one Cursor chat: an ordered record stream over a kernel, and a
// process per turn.
type Session struct {
	k kernel.Kernel

	cfg   sessionConfig
	grace time.Duration

	mu       sync.Mutex
	chatID   string
	cur      *turnProc // the process of the turn in flight; nil when idle
	starting bool      // a process is being launched
	running  bool      // a turn runs from its prompt to result-or-exit
	held     []heldInput
	gone     bool             // disposed: no new process, held input dropped
	tallies  []map[string]int // unknown-frame tallies of finished processes
	dispose  *disposeState
	lastExit sj.ExitStatus // the most recent process exit
	haveExit bool          // lastExit is set

	drainMu sync.Mutex
}

// turnProc is one cursor-agent process and the fold state over its stream.
type turnProc struct {
	p    *sj.Process
	fold *sj.Folder
	prof *profile

	pumpDone chan struct{} // closed when Events() is drained
	exited   chan struct{} // closed once the turn's exit has been handled
	ended    bool          // the runtime's own result frame ended this turn
}

type heldInput struct {
	input, inputID, origin, queueID string
}

type disposeState struct {
	done  chan struct{}
	reqID string
	err   error
}

var _ oarlock.Session = (*Session)(nil)

// The absence of oarlock.Steerer and oarlock.Withdrawer is the capability:
// cursor-agent has no mid-run input channel and no queue of its own to withdraw
// from. Nothing below may add a Steer method.

// newSession builds the session; nothing starts until the first prompt. It does
// not fail: Open has already checked the options, and a bad grace is a negative
// duration the caller can see.
func newSession(cfg sessionConfig) *Session {
	grace := cfg.abortGrace
	if grace <= 0 {
		grace = DefaultAbortGrace
	}
	return &Session{
		k:      kernel.New(cfg.chatID, cfg.redact),
		cfg:    cfg,
		grace:  grace,
		chatID: cfg.chatID,
	}
}

// ChatID is the CLI chat this session is bound to: Options.Resume at open,
// learned from the first system/init otherwise. "" until the runtime says.
func (s *Session) ChatID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chatID
}

// Capabilities: the queue lives in the adapter; the CLI shows only the root
// agent (docs/adr/0004: attribution is opaque until measured); no images.
func (s *Session) Capabilities() oarlock.Capabilities {
	return oarlock.Capabilities{
		Queue:       oarlock.QueueCapability{Durable: false},
		Attribution: oarlock.AttributionOpaque,
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

// Unknown is the tally of frames this adapter did not understand, merged over
// every turn: the shared fold's unknown top-level types plus the profile's
// unhandled subtypes (MUL-5231: an unhandled frame is counted, never silently
// absorbed). The frames themselves stay in Native.
func (s *Session) Unknown() map[string]int {
	s.mu.Lock()
	out := make(map[string]int, len(s.tallies))
	for _, tally := range s.tallies {
		mergeTally(out, tally)
	}
	cur := s.cur
	s.mu.Unlock()
	if cur != nil {
		mergeTally(out, cur.fold.Unknown())
		// The profile's counters are only safe to read under the fold's lock.
		cur.fold.Do(func() { mergeTally(out, cur.prof.unknownSubtypes()) })
	}
	return out
}

func mergeTally(into, from map[string]int) {
	for k, v := range from {
		into[k] += v
	}
}

// ---- turns -------------------------------------------------------------------

// Prompt starts a turn: rejected busy while one runs, rejected runtime_exited
// or disposed once the stream holds that answer, unsupported with images, and
// accepted once the prompt is on the new process's stdin (EOF starts the run).
func (s *Session) Prompt(ctx context.Context, input string, opt oarlock.PromptOptions) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	return s.prompt(ctx, oarlock.RequestBody{
		Action: oarlock.ActionPrompt, Input: input, InputID: opt.InputID,
		Images: opt.Images, Origin: opt.Origin, Carries: opt.Carries,
	}), nil
}

func (s *Session) prompt(ctx context.Context, body oarlock.RequestBody) oarlock.ControlOutcome {
	return s.k.Control(body, func(req oarlock.Record) oarlock.ResponseBody {
		return s.startTurn(ctx, req)
	}, s.at(nil))
}

// startTurn launches the process of one turn and writes the prompt into it.
func (s *Session) startTurn(ctx context.Context, req oarlock.Record) oarlock.ResponseBody {
	if len(req.Request.Images) > 0 {
		return rejected(oarlock.Unsupported, "cursor-agent takes a text prompt on stdin: the CLI has no image input")
	}
	s.mu.Lock()
	switch {
	case s.running || s.starting:
		s.mu.Unlock()
		return rejected(oarlock.Busy, "a turn is already running; wait for its end or abort it")
	case s.gone:
		s.mu.Unlock()
		return rejected(oarlock.RuntimeExited, "session disposed")
	}
	chatID := s.chatID
	s.starting = true
	s.mu.Unlock()

	// Not ctx: the process lives until its turn ends (or Dispose), not until the
	// caller's prompt context does.
	p, err := s.launch(context.WithoutCancel(ctx), chatID)
	if err != nil {
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
		return rejected(oarlock.ErrorCode, fmt.Sprintf("cursor-agent did not start: %v", err))
	}
	// The pump and the exit watcher start first: a child that writes while the
	// prompt is still going in must never block on a full stdout pipe.
	s.begin(p)

	// The prompt is free text on stdin; EOF starts the run (a multi-line or
	// megabyte prompt is ordinary text, never a framed line).
	if err := p.WriteRaw(context.WithoutCancel(ctx), []byte(req.Request.Input)); err != nil {
		_ = p.Close()
		return writeRejected(err)
	}
	if err := p.CloseStdin(); err != nil {
		return writeRejected(err)
	}
	return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
}

// launch starts one cursor-agent process.
func (s *Session) launch(ctx context.Context, chatID string) (*sj.Process, error) {
	if s.cfg.newProcess != nil {
		return s.cfg.newProcess(ctx)
	}
	return sj.Launch(ctx, sj.Options{
		Command:     s.cfg.command,
		Args:        s.cfg.args(chatID),
		Dir:         s.cfg.dir,
		Env:         s.cfg.env,
		KillGrace:   s.grace,
		EventBuffer: defaultEventBuffer,
	})
}

// begin registers the process of a turn and starts its pump and exit watcher.
func (s *Session) begin(p *sj.Process) {
	prof := newProfile()
	turn := &turnProc{
		p:        p,
		prof:     prof,
		fold:     sj.NewFolder(prof),
		pumpDone: make(chan struct{}),
		exited:   make(chan struct{}),
	}
	s.mu.Lock()
	s.cur = turn
	s.starting = false
	s.running = true
	s.mu.Unlock()
	go s.pump(turn)
	go s.watchExit(turn)
}

func (s *Session) pump(turn *turnProc) {
	defer close(turn.pumpDone)
	for m := range turn.p.Events() {
		s.ingest(turn, m)
	}
}

// ingest records one frame. The chat id is learned before the frame that
// carries it is recorded, so the init record is already stamped with it.
func (s *Session) ingest(turn *turnProc, m sj.Message) {
	fr := turn.fold.Fold(m.Raw)
	if fr.Type == "system/init" {
		if id := sj.Str(fr.Rec["session_id"]); id != "" {
			s.learnChatID(id)
		}
	}
	s.k.Frame(kernel.FrameBody{Native: m.Raw, Events: fr.Events}, s.at(fr.AgentPath))
	if fr.TurnEnded {
		s.endTurn(turn)
		s.kick()
	}
}

func (s *Session) learnChatID(id string) {
	s.mu.Lock()
	s.chatID = id
	s.mu.Unlock()
}

// endTurn marks the turn's own end (the runtime's result frame). The process
// still exits afterwards; that exit is the next attempt's business.
func (s *Session) endTurn(turn *turnProc) {
	s.mu.Lock()
	turn.ended = true
	if s.cur == turn {
		s.running = false
	}
	s.mu.Unlock()
}

// watchExit handles the process exit once everything it wrote has been
// recorded. A turn that reached its result frame is over: its exit is the
// turn's lifecycle, remembered in LastExit but not recorded as the session's
// outcome. An exit without a result is the session's own end and is recorded as
// the `exited` response — answering the dispose request when oarlock ended the
// process that way, standing alone otherwise.
func (s *Session) watchExit(turn *turnProc) {
	st := <-turn.p.Exit()
	<-turn.pumpDone

	tally := turn.fold.Unknown()
	turn.fold.Do(func() { mergeTally(tally, turn.prof.unknownSubtypes()) })
	s.mu.Lock()
	if s.cur == turn {
		s.cur = nil
		s.running = false
	}
	s.tallies = append(s.tallies, tally)
	clean := turn.ended
	s.lastExit, s.haveExit = st, true
	gone := s.gone
	requestID := ""
	if s.dispose != nil {
		requestID = s.dispose.reqID
	}
	s.mu.Unlock()

	if !clean {
		// The turn never reached a result: this exit is the session's outcome,
		// and nothing this session holds can be sent after the kernel reads it.
		s.k.Respond(requestID, exitBody(st), s.at(nil))
		s.dropHeld()
	}
	close(turn.exited)
	if !gone && clean {
		// The chat is still open: the next held input may go now.
		s.kick()
	}
}

// LastExit is the most recent process exit this session observed, and whether
// one was observed at all. A clean turn's exit never enters the record stream
// (the turn ended with the runtime's own turn_ended), so this is where the
// adapter's evidence of it lives: exit code, signal, stderr tail and cause.
func (s *Session) LastExit() (sj.ExitStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastExit, s.haveExit
}

func exitBody(st sj.ExitStatus) oarlock.ResponseBody {
	body := oarlock.ResponseBody{Kind: oarlock.ResponseExited, Reason: st.StderrTail, ExitCodeReported: true}
	if st.Signal == nil {
		code := st.Code
		body.ExitCode = &code
	}
	signal := ""
	if st.Signal != nil {
		signal = st.Signal.String()
	}
	if native, err := json.Marshal(map[string]any{"cause": st.Cause, "signal": signal}); err == nil {
		body.Native = native
	}
	return body
}

// at stamps a record with the chat id once it is known.
func (s *Session) at(agentPath []string) kernel.RecordAt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return kernel.RecordAt{SessionID: s.chatID, AgentPath: agentPath}
}

// ---- queue -------------------------------------------------------------------

// Queue holds input in the adapter and sends it as an ordinary prompt of its own
// once nothing is running.
func (s *Session) Queue(ctx context.Context, input string, opt oarlock.InputOptions) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	out := s.k.Control(oarlock.RequestBody{
		Action: oarlock.ActionQueue, Input: input, InputID: opt.InputID, Images: opt.Images, Origin: opt.Origin,
	}, func(req oarlock.Record) oarlock.ResponseBody {
		if len(req.Request.Images) > 0 {
			return rejected(oarlock.Unsupported, "cursor-agent takes a text prompt on stdin: the CLI has no image input")
		}
		s.mu.Lock()
		s.held = append(s.held, heldInput{
			input: req.Request.Input, inputID: req.Request.InputID, origin: req.Request.Origin, queueID: req.ID,
		})
		s.mu.Unlock()
		return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
	}, s.at(nil))
	s.kick()
	return out, nil
}

func (s *Session) kick() {
	s.mu.Lock()
	n := len(s.held)
	s.mu.Unlock()
	if n > 0 {
		go s.drain()
	}
}

// drain sends held input while no turn is running. Drains are serialised, so
// two kicks never send the same input twice.
func (s *Session) drain() {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	for {
		s.mu.Lock()
		gone := s.gone
		s.mu.Unlock()
		if gone {
			s.dropHeld()
			return
		}
		if !s.idle() {
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
				s.dropHeld()
				return
			}
		}
	}
}

// idle reports whether a new turn may start: the session's own state, never the
// record fold (a per-turn exit reads as session death there).
func (s *Session) idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.running && !s.starting && s.cur == nil && !s.gone
}

func (s *Session) dropHeld() {
	s.mu.Lock()
	s.held = nil
	s.mu.Unlock()
}

// ---- abort -------------------------------------------------------------------

// Abort ends the running turn's process group and answers accepted; with no
// turn it is rejected no_active_turn. The group gets SIGTERM and, after
// AbortGrace, SIGKILL (sj.Options.KillGrace). The turn ends with the runtime's
// own frames or with the exited response; no turn_ended is invented.
func (s *Session) Abort(ctx context.Context) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	return s.k.Control(oarlock.RequestBody{Action: oarlock.ActionAbort}, func(oarlock.Record) oarlock.ResponseBody {
		return s.decideAbort(ctx)
	}, s.at(nil)), nil
}

func (s *Session) decideAbort(ctx context.Context) oarlock.ResponseBody {
	s.mu.Lock()
	turn := s.cur
	running := s.running
	s.mu.Unlock()
	if turn == nil || !running {
		return rejected(oarlock.NoActiveTurn, "no active turn")
	}
	// The fold reads the next end of this turn as aborted.
	turn.fold.AbortRequested()
	// Close is the whole escalation: SIGTERM to the group, then SIGKILL after
	// the kill grace, and it waits for the exit.
	_ = turn.p.Close()
	s.waitExited(ctx, turn)
	return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
}

// waitExited waits for the exited response to be recorded, bounded by ctx and a
// generous ceiling.
func (s *Session) waitExited(ctx context.Context, turn *turnProc) {
	timer := time.NewTimer(exitRecordedWait)
	defer timer.Stop()
	select {
	case <-turn.exited:
	case <-ctx.Done():
	case <-timer.C:
	}
}

// ---- dispose -----------------------------------------------------------------

// Dispose releases the session: idempotent; the first call records one dispose
// request, drops held input, closes the current process group and waits for it
// to be gone. A turn still running at that point has no result, so its exit is
// the session's and is recorded as the exited response. ctx bounds only the
// wait.
func (s *Session) Dispose(ctx context.Context) error {
	s.mu.Lock()
	if s.dispose == nil {
		s.dispose = &disposeState{done: make(chan struct{})}
		go s.runDispose(s.dispose)
	}
	d := s.dispose
	s.mu.Unlock()
	select {
	case <-d.done:
		s.mu.Lock()
		err := d.err
		s.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Session) runDispose(d *disposeState) {
	defer close(d.done)
	s.mu.Lock()
	s.gone = true
	turn := s.cur
	s.held = nil
	s.mu.Unlock()

	s.k.Control(oarlock.RequestBody{Action: oarlock.ActionDispose}, func(req oarlock.Record) oarlock.ResponseBody {
		s.mu.Lock()
		d.reqID = req.ID
		s.mu.Unlock()
		return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
	}, s.at(nil))

	if turn == nil {
		return
	}
	err := turn.p.Close()
	s.waitExited(context.Background(), turn)
	s.mu.Lock()
	d.err = err
	s.mu.Unlock()
}

func rejected(code oarlock.Code, reason string) oarlock.ResponseBody {
	return oarlock.ResponseBody{Kind: oarlock.ResponseRejected, Code: code, Reason: reason}
}

func writeRejected(err error) oarlock.ResponseBody {
	if errors.Is(err, sj.ErrExited) {
		return rejected(oarlock.RuntimeExited, err.Error())
	}
	return rejected(oarlock.ErrorCode, err.Error())
}
