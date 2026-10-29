package pi

// The control plane of one Pi RPC session (issue #10): oarlock.Session over a
// Conn, recorded on a real kernel.
//
//   - Every message Pi writes becomes exactly one frame, in wire order, Native
//     verbatim. Responses are frames too: a late native reply to an abort
//     must stay a frame (docs/spec/errors.md), and the "handled" fact lives on
//     the prompt response.
//   - Reading a frame is project.go's pure foldPiEvent (issue #11), the same
//     function for the live pump and for any replay: this file only supplies
//     what a pure fold cannot know, namely the prompt a response answers (the
//     "handled" disposition → input_handled) and the run gate. After an
//     agent_settled it asks Pi for get_session_stats; the answer arrives as a
//     frame and is read like any other, so context is in the stream or absent,
//     never invented.
//   - Status is never cached: busy, no_active_turn and the queue drain all
//     fold the retained records (internal/fold), so the answer agrees with
//     what a host reading the stream sees.
//   - Steer is implemented (issue #45) and gated on a running turn: the command
//     is sent only while the stream says a turn is running, and only Pi's own
//     success answer accepts it. Pi answers data.disposition "queued", which
//     means "taken into the current turn's steering buffer", not "applied to
//     the request": delivery is a separate native frame pair inside the same
//     run, so no turn end is synthesized here and the runtime's own
//     agent_settled stays the only turn end. An idle steer is refused locally
//     with no command on the wire, because Pi would answer success, hold it and
//     inject it into whatever turn comes next.
//
// Known gaps, on purpose: images are not sent yet (Capabilities.Images is
// false and an input with images is rejected unsupported); held queue input
// does not survive an exit or a dispose; a cancelled ctx does not abandon a
// prompt that is already on the wire (the prompt command is bounded by the
// Conn's command timeout instead, because abandoning it would leave the stream
// saying idle while Pi runs); and Events callbacks run inline on the pump, so
// they must not call controls.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
	"github.com/zzjcool/oarlock/internal/kernel"
)

const (
	// DefaultAbortGrace is how long an accepted abort may leave the turn
	// running before oarlock takes over and kills the process group. It is
	// OAR's createAbortFallback default.
	DefaultAbortGrace = 10 * time.Second

	// recordedWait bounds the wait for the pump to record a prompt response
	// the Conn has already delivered to Do. A conforming Conn also delivers it
	// to Events, so this only guards a Conn that does not.
	recordedWait = 10 * time.Second

	// statsWait bounds the post-settlement get_session_stats read. It is below
	// the Conn's own command timeout on purpose: ctx abandons only this wait and
	// never kills the child, whereas the Conn's timeout would.
	statsWait = 10 * time.Second

	// exitRecordedWait bounds how long Abort (after a take-over) and Dispose
	// wait for the exited response once they have ended the process.
	exitRecordedWait = 30 * time.Second
)

// SessionOptions configures NewSession.
type SessionOptions struct {
	// SessionID is the runtime-native session id (get_state.data.sessionId).
	// It is required: it is the stream instance's identity.
	SessionID string

	// AbortGrace is how long a turn may outlive an accepted abort before the
	// process group is killed. 0 = DefaultAbortGrace; negative is an error.
	AbortGrace time.Duration

	// Redact is the kernel's credential hook; nil retains records as built.
	Redact kernel.Redactor

	// PiVersion is the Pi version the gate verified before the process started
	// (issue #15). It is informational: PiVersion() returns it, and it never
	// becomes a record (docs/runtimes/pi.md §13.3).
	PiVersion string
}

// Session is a Pi RPC session. It implements oarlock.Session and
// oarlock.Steerer; it is not an oarlock.Withdrawer. Steer needs a turn the
// stream says is running, and only Pi's own success answer accepts the input.
type Session struct {
	conn  Conn
	k     kernel.Kernel
	grace time.Duration

	piVersion string // the version the gate verified; "" without a gate

	pumpDone     chan struct{} // closed when Conn.Events has been drained
	exitRecorded chan struct{} // closed once the exited response is in the stream

	mu          sync.Mutex
	changed     chan struct{} // closed and replaced whenever the stream changed in a way waiters care about
	started     bool          // agent_start seen for the current run
	proj        piState       // the fold's state; only the pump goroutine writes it
	stats       *statsCall    // the post-settlement get_session_stats read in flight
	inflight    *promptCall
	held        []heldInput
	draining    bool
	again       bool
	fb          *fallbackState
	disposeReq  string
	disposeDone chan struct{}
	disposeErr  error
}

var (
	_ oarlock.Session = (*Session)(nil)
	_ oarlock.Steerer = (*Session)(nil)
)

// promptCall is the prompt command on the wire. Pi answers it before it starts
// the turn, and the Conn matches the answer to Do by an id this layer never
// sees, so the pump learns which request a prompt response belongs to from here:
// busy keeps at most one prompt in flight.
type promptCall struct {
	requestID string
	inputID   string
	recorded  chan struct{} // closed by the pump after it recorded the response frame
}

// statsCall is the post-settlement get_session_stats read on the wire. recorded
// is closed by the pump once it recorded the response frame, so the queue drain
// can be held back until the stats frame precedes the next prompt in the stream.
type statsCall struct {
	recorded chan struct{}
}

// heldInput is one queued input. Pi has no queue of its own that oarlock
// relies on, so this list is the adapter's (Capabilities.Queue.Durable=false).
type heldInput struct {
	input   string
	inputID string
	origin  string
	queueID string // id of the queue request that holds it
}

// fallbackState is the take-over timer of an accepted abort. Repeated aborts
// share one deadline (they cannot postpone it); a refusal withdraws only its
// own attempt; a turn end or the process exit clears every attempt.
type fallbackState struct {
	timer    *time.Timer
	fired    chan struct{}
	attempts int
}

// NewSession wraps an already-handshaken Conn. It takes ownership of conn:
// Dispose (or a take-over) closes it. The pump starts reading conn.Events()
// immediately, so frames Pi wrote before this call become the first records.
func NewSession(conn Conn, opt SessionOptions) (*Session, error) {
	switch {
	case conn == nil:
		return nil, errors.New("pi: NewSession(nil Conn)")
	case opt.SessionID == "":
		return nil, errors.New("pi: SessionOptions.SessionID is required")
	case opt.AbortGrace < 0:
		return nil, fmt.Errorf("pi: SessionOptions.AbortGrace is negative (%v)", opt.AbortGrace)
	}
	grace := opt.AbortGrace
	if grace == 0 {
		grace = DefaultAbortGrace
	}
	s := &Session{
		conn:         conn,
		k:            kernel.New(opt.SessionID, opt.Redact),
		grace:        grace,
		piVersion:    opt.PiVersion,
		pumpDone:     make(chan struct{}),
		exitRecorded: make(chan struct{}),
		changed:      make(chan struct{}),
	}
	go s.pump()
	go s.watchExit()
	return s, nil
}

// Capabilities declares what a host must know before acting: the queue lives in
// the adapter, Pi has no sub-agents, and images are not sent yet.
func (s *Session) Capabilities() oarlock.Capabilities {
	return oarlock.Capabilities{
		Queue:       oarlock.QueueCapability{Durable: false},
		Attribution: oarlock.AttributionNone,
		Images:      false,
	}
}

// Records returns the retained records after the cursor.
func (s *Session) Records(cursor oarlock.Cursor) []oarlock.Record { return s.k.Records(cursor) }

// Events replays the retained records after *cursor and then delivers live
// ones until the returned function is called. A nil cursor is live only. A
// cursor that names another stream instance registers nothing and returns a
// no-op unsubscribe. The callback runs inline on the goroutine that appends, so
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

// Unknown is a copy of the fold's tally of native message types (and
// message_update sub-types, as "message_update/<sub>") that are not in the
// projection's known set (docs/runtimes/pi.md §9). The frames themselves are
// retained verbatim and produce no event; a non-zero tally is a test signal.
func (s *Session) Unknown() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.proj.unknown))
	for k, v := range s.proj.unknown {
		out[k] = v
	}
	return out
}

// Prompt starts a turn. It is rejected busy while the stream says a turn is
// running (never queued implicitly) and answered accepted once Pi accepted the
// command. Whether a turn opened is the stream's business: a "handled" prompt
// opens none and the status returns to idle without a turn_ended.
func (s *Session) Prompt(ctx context.Context, input string, opt oarlock.PromptOptions) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	out := s.prompt(ctx, oarlock.RequestBody{
		Action:  oarlock.ActionPrompt,
		Input:   input,
		InputID: opt.InputID,
		Images:  opt.Images,
		Origin:  opt.Origin,
		Carries: opt.Carries,
	})
	s.kick()
	return out, nil
}

// Queue holds input in the adapter and sends it as an ordinary prompt of its own
// once nothing is running. The answer only says the adapter took it over; each
// held input later shows up as its own prompt request, with its own turn.
func (s *Session) Queue(ctx context.Context, input string, opt oarlock.InputOptions) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	out := s.k.Control(oarlock.RequestBody{
		Action:  oarlock.ActionQueue,
		Input:   input,
		InputID: opt.InputID,
		Images:  opt.Images,
		Origin:  opt.Origin,
	}, func(req oarlock.Record) oarlock.ResponseBody {
		if len(req.Request.Images) > 0 {
			return sessRejected(oarlock.Unsupported, "the Pi session does not send images yet")
		}
		s.mu.Lock()
		s.held = append(s.held, heldInput{
			input:   req.Request.Input,
			inputID: req.Request.InputID,
			origin:  req.Request.Origin,
			queueID: req.ID,
		})
		s.mu.Unlock()
		return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
	}, kernel.RecordAt{})
	s.kick()
	return out, nil
}

// Steer adds input to the running turn. It is accepted only when Pi answers the
// steer command with success: a refusal, an exit or a transport fault is
// rejected, and nothing is held or retried. Pi's acknowledgement is
// disposition "queued" — the input sits in this turn's steering buffer — so the
// adapter never claims more than the runtime said; the delivery is a later
// native queue_update draining "steering" plus a role:user message pair inside
// the same run, and neither is projected into a turn boundary. With no running
// turn the input is refused no_active_turn before anything is written to the
// Conn: Pi would otherwise answer success, keep it in steering and inject it
// into whatever turn comes next.
//
// Evidence: pi 1.4.0 on this host, --mode rpc, one agent_start/agent_settled
// pair spanning the exchange (issue #45 notes, frames steer-in-turn.log).
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

// Abort asks Pi to stop the running turn. With no turn it is rejected
// no_active_turn. Otherwise the abort command goes out and a take-over timer
// starts: if the turn has not ended when AbortGrace is up, the process group is
// killed and the abort is still accepted ("we took the stop"); the exit is
// recorded as the exited response, and no turn_ended is ever written for it.
func (s *Session) Abort(ctx context.Context) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	return s.k.Control(oarlock.RequestBody{Action: oarlock.ActionAbort}, func(oarlock.Record) oarlock.ResponseBody {
		return s.decideAbort(ctx)
	}, kernel.RecordAt{}), nil
}

// Dispose releases the session. It is idempotent: the first call records one
// dispose request, closes stdin (Pi has no dispose command), escalates to
// SIGTERM and SIGKILL of the group and waits for the exited response; later
// calls wait for and return the first call's result. ctx bounds only the wait.
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
	s.mu.Unlock()
	s.clearFallback()
	s.disposeErr = s.conn.Close()
	s.waitExitRecorded(context.Background())
}

// ---- prompt ----------------------------------------------------------------

func (s *Session) prompt(ctx context.Context, body oarlock.RequestBody) oarlock.ControlOutcome {
	return s.k.Control(body, func(req oarlock.Record) oarlock.ResponseBody {
		return s.decidePrompt(ctx, req)
	}, kernel.RecordAt{})
}

func (s *Session) decidePrompt(ctx context.Context, req oarlock.Record) oarlock.ResponseBody {
	switch s.statusBefore(req.Seq).Kind {
	case fold.StatusRunning:
		return sessRejected(oarlock.Busy, "a turn is already running; wait for its end or abort it")
	case fold.StatusExited:
		return sessRejected(oarlock.RuntimeExited, "runtime exited")
	}
	if len(req.Request.Images) > 0 {
		return sessRejected(oarlock.Unsupported, "the Pi session does not send images yet")
	}

	call := &promptCall{requestID: req.ID, inputID: req.Request.InputID, recorded: make(chan struct{})}
	s.mu.Lock()
	s.inflight = call
	s.started = false
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.inflight == call {
			s.inflight = nil
		}
		s.mu.Unlock()
	}()

	// Not ctx: an abandoned prompt may still start a turn, and the stream
	// would then say idle while Pi runs. The Conn's command timeout bounds it.
	err := s.conn.Do(context.WithoutCancel(ctx), Command{Type: "prompt", Params: map[string]any{"message": req.Request.Input}}, nil)
	var rpc *RPCError
	switch {
	case err == nil:
		s.awaitRecorded(call)
		return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
	case errors.As(err, &rpc):
		s.awaitRecorded(call)
		reason := rpc.Message
		if reason == "" {
			reason = rpc.Error()
		}
		return sessRejected(oarlock.RuntimeRefused, reason)
	case errors.Is(err, ErrExited):
		return sessRejected(oarlock.RuntimeExited, err.Error())
	default:
		return sessRejected(oarlock.ErrorCode, err.Error())
	}
}

// awaitRecorded waits until the pump recorded the response frame of call, so
// the stream reads request, frame(response), response.
func (s *Session) awaitRecorded(call *promptCall) {
	t := time.NewTimer(recordedWait)
	defer t.Stop()
	select {
	case <-call.recorded:
	case <-s.pumpDone:
	case <-t.C:
	}
}

// statusBefore folds the records that precede seq: what the stream said when
// the request was recorded, which is what busy must agree with.
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

// ---- steer -----------------------------------------------------------------

func (s *Session) decideSteer(ctx context.Context, req oarlock.Record) oarlock.ResponseBody {
	switch s.statusBefore(req.Seq).Kind {
	case fold.StatusRunning:
		// The only path that writes to the Conn.
	case fold.StatusExited:
		return sessRejected(oarlock.RuntimeExited, "runtime exited")
	default:
		// No turn: refused here with no steer command on the wire at all.
		return sessRejected(oarlock.NoActiveTurn, "no active turn")
	}
	if len(req.Request.Images) > 0 {
		return sessRejected(oarlock.Unsupported, "the Pi session does not send images yet")
	}

	// Not ctx: an abandoned steer has been handed to Pi either way, and the
	// stream must be able to say so. The Conn's command timeout bounds it.
	//
	// Unlike a prompt there is nothing to wait for here: no event is projected
	// from this response (projectResponse keys on command "prompt"), and the
	// Conn hands the response to Do before it pushes the same frame on Events,
	// so the accepted response record can precede Pi's own frame. The answer
	// says what Pi said and nothing more; the frames still arrive in wire order.
	err := s.conn.Do(context.WithoutCancel(ctx), Command{Type: "steer", Params: map[string]any{"message": req.Request.Input}}, nil)
	var rpc *RPCError
	switch {
	case err == nil:
		return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
	case errors.As(err, &rpc):
		reason := rpc.Message
		if reason == "" {
			reason = rpc.Error()
		}
		return sessRejected(oarlock.RuntimeRefused, reason)
	case errors.Is(err, ErrExited):
		return sessRejected(oarlock.RuntimeExited, err.Error())
	default:
		return sessRejected(oarlock.ErrorCode, err.Error())
	}
}

// ---- abort -----------------------------------------------------------------

func (s *Session) decideAbort(ctx context.Context) oarlock.ResponseBody {
	if s.status().Kind != fold.StatusRunning {
		return sessRejected(oarlock.NoActiveTurn, "no active turn")
	}
	fb := s.armFallback()
	var ack chan error
	sent := false
	for {
		// changed is read before the stream is, so a change in between is never lost.
		s.mu.Lock()
		started, changed := s.started, s.changed
		s.mu.Unlock()

		switch s.status().Kind {
		case fold.StatusIdle:
			if !sent {
				s.disarm(fb)
				return sessRejected(oarlock.NoActiveTurn, "the turn ended before the abort could be sent")
			}
			return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
		case fold.StatusExited:
			if fb.isFired() {
				return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
			}
			return sessRejected(oarlock.RuntimeExited, "runtime exited before it answered the abort")
		}

		// Pi answers an abort sent before agent_start with success and then runs
		// the turn anyway, so the command waits for the run to have started.
		if started && !sent {
			sent = true
			ack = make(chan error, 1)
			go func() {
				ack <- s.conn.Do(context.WithoutCancel(ctx), Command{Type: "abort"}, nil)
			}()
		}

		select {
		case err := <-ack:
			ack = nil
			var rpc *RPCError
			switch {
			case err == nil:
				return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
			case errors.As(err, &rpc):
				// A refusal cancels only this attempt and kills nothing.
				s.disarm(fb)
				reason := rpc.Message
				if reason == "" {
					reason = rpc.Error()
				}
				return sessRejected(oarlock.RuntimeRefused, reason)
			}
			// An exit or a transport fault: the exited record or the grace timer decides.
		case <-changed:
		case <-fb.fired:
			// Taken over: the group is being killed. Still accepted, and no turn
			// end is invented for it.
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
	s.fb.attempts++
	return s.fb
}

func (s *Session) disarm(fb *fallbackState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fb != fb {
		return
	}
	fb.attempts--
	if fb.attempts <= 0 && !fb.isFired() {
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

// fireFallback is the grace expiry: if the stream still says a turn is running,
// the process group is closed (stdin EOF, SIGTERM, SIGKILL). Nothing is
// recorded here; the exit watcher records the exit.
func (s *Session) fireFallback(fb *fallbackState) {
	s.mu.Lock()
	current := s.fb == fb
	s.mu.Unlock()
	if !current || s.status().Kind != fold.StatusRunning {
		return
	}
	close(fb.fired)
	go func() { _ = s.conn.Close() }()
	s.signal()
}

func (fb *fallbackState) isFired() bool {
	select {
	case <-fb.fired:
		return true
	default:
		return false
	}
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

// ---- queue -----------------------------------------------------------------

// kick schedules a drain of the held input. It never blocks, so the pump can
// call it; a kick that lands while a drain runs is not lost (again).
func (s *Session) kick() {
	s.mu.Lock()
	if s.draining {
		s.again = true
		s.mu.Unlock()
		return
	}
	if len(s.held) == 0 {
		s.mu.Unlock()
		return
	}
	s.draining = true
	s.mu.Unlock()
	go s.drain()
}

func (s *Session) drain() {
	for {
		s.mu.Lock()
		s.again = false
		s.mu.Unlock()
		if s.drainOne() {
			continue
		}
		s.mu.Lock()
		if s.again {
			s.mu.Unlock()
			continue
		}
		s.draining = false
		s.mu.Unlock()
		return
	}
}

// drainOne sends the head of the held input as an ordinary prompt when the
// stream says nothing is running. It reports whether it consumed an input, so
// the loop goes on; a prompt that started a turn leaves the status running and
// the next turn_ended kicks again.
func (s *Session) drainOne() bool {
	if s.k.Unreachable() != nil {
		s.mu.Lock()
		s.held = nil
		s.mu.Unlock()
		return false
	}
	if s.status().Kind != fold.StatusIdle {
		return false
	}
	s.mu.Lock()
	// The runtime's turn_end is not the settlement boundary: agent_settled is.
	// started is cleared only after the post-settlement stats read is installed,
	// and stats stays set until its response frame is recorded, so a queued
	// prompt cannot overtake the context figure.
	started := s.started
	stats := s.stats
	if len(s.held) == 0 || started || stats != nil {
		s.mu.Unlock()
		return false
	}
	h := s.held[0]
	s.held = s.held[1:]
	s.mu.Unlock()

	carries := h.inputID
	if carries == "" {
		carries = h.queueID
	}
	out := s.prompt(context.Background(), oarlock.RequestBody{
		Action:  oarlock.ActionPrompt,
		Input:   h.input,
		InputID: h.inputID,
		Origin:  h.origin,
		Carries: []string{carries},
	})
	if out.Kind == oarlock.ControlRejected {
		switch out.Code {
		case oarlock.Busy:
			// Someone else's prompt got in first: keep the place in line.
			s.mu.Lock()
			s.held = append([]heldInput{h}, s.held...)
			s.mu.Unlock()
			return false
		case oarlock.RuntimeExited, oarlock.Disposed:
			s.mu.Lock()
			s.held = nil
			s.mu.Unlock()
			return false
		}
		// Any other refusal stays recorded on that prompt request, code and
		// reason included, and does not block the input behind it.
	}
	return true
}

// ---- pump and exit ---------------------------------------------------------

// pump records every message Pi wrote, in order. It never waits for a control.
func (s *Session) pump() {
	defer close(s.pumpDone)
	for m := range s.conn.Events() {
		s.ingest(m)
	}
}

func (s *Session) ingest(m Message) {
	var (
		call      *promptCall
		turnEnded bool
		handled   bool
		extra     []oarlock.Event
	)
	// One frame, one fold step. The fold is pure; the state lives here because
	// the pump is its only writer and Unknown() reads it under mu.
	s.mu.Lock()
	state := s.proj
	s.mu.Unlock()
	next, events := foldPiEvent(state, m.Raw)

	switch m.Type {
	case "response":
		// The prompt response is the one frame whose reading needs the request
		// that sent it, which only the control plane knows.
		extra, call, handled = s.projectResponse(m)
	case "agent_start":
		s.mu.Lock()
		s.started = true
		s.mu.Unlock()
	case "agent_settled":
		turnEnded = true
		// started stays true until the stats read has been installed below, so
		// a pending queue drain cannot slip in between the runtime's turn_end
		// and its settlement.
	}
	events = append(events, extra...)

	s.mu.Lock()
	s.proj = next
	s.mu.Unlock()
	s.k.Frame(kernel.FrameBody{Native: m.Raw, Events: events}, kernel.RecordAt{})

	if call != nil {
		close(call.recorded)
	}
	if turnEnded {
		s.clearFallback()
	}
	if len(events) > 0 || call != nil {
		s.signal()
	}
	if m.Type == "response" {
		s.noteStatsRecorded(m)
	}
	if handled {
		s.kick()
	}
	if turnEnded {
		s.afterSettle()
		s.mu.Lock()
		s.started = false
		s.mu.Unlock()
	}
}

// noteStatsRecorded releases the drain that waits for the stats frame: called by
// the pump right after it recorded a response.
func (s *Session) noteStatsRecorded(m Message) {
	var r struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(m.Raw, &r) != nil || r.Command != "get_session_stats" {
		return
	}
	s.mu.Lock()
	call := s.stats
	s.stats = nil
	s.mu.Unlock()
	if call != nil {
		close(call.recorded)
	}
}

// afterSettle runs once agent_settled is recorded: it asks Pi for its own context
// fullness (issue #11 item 6; pi is authoritative, and the figure read after
// agent_settled is post-compaction) and only then lets the queue drain, so the
// stats frame precedes the next prompt in the stream and the figure is not a
// mid-run estimate of the next turn.
//
// The command's response comes back as a frame through the pump like every other
// message and is read by foldPiEvent; nothing is written here, and a failure
// leaves context unreported (nil), never a guess. It runs on its own goroutine so
// the pump never waits for it; the wait for the recorded frame is bounded, and
// ctx abandons only this wait (it never kills the child, unlike the Conn's own
// command timeout). Once the session is unreachable there is nothing to ask.
func (s *Session) afterSettle() {
	if s.k.Unreachable() != nil {
		s.kick()
		return
	}
	call := &statsCall{recorded: make(chan struct{})}
	s.mu.Lock()
	if old := s.stats; old != nil {
		close(old.recorded) // a second settlement: the older read has nothing left to hold back
	}
	s.stats = call
	s.mu.Unlock()
	go func() {
		defer s.kick()
		ctx, cancel := context.WithTimeout(context.Background(), statsWait)
		defer cancel()
		err := s.conn.Do(ctx, Command{Type: "get_session_stats"}, nil)
		var rpc *RPCError
		if err == nil || errors.As(err, &rpc) {
			// Pi answered, so a response frame is on its way to the pump.
			t := time.NewTimer(recordedWait)
			defer t.Stop()
			select {
			case <-call.recorded:
			case <-s.pumpDone:
			case <-t.C:
			}
		}
		// Any other error (an exit, a transport fault, the bound) has no frame
		// to wait for; an exit is recorded by watchExit.
		s.mu.Lock()
		if s.stats == call {
			s.stats = nil
		}
		s.mu.Unlock()
	}()
}

// projectResponse reads the one native response that carries a fact oarlock
// needs: the prompt response. Only an explicit disposition "handled" becomes
// input_handled; the event is the runtime's own report, never a guess.
func (s *Session) projectResponse(m Message) (events []oarlock.Event, call *promptCall, handled bool) {
	var r struct {
		Command string `json:"command"`
		Success bool   `json:"success"`
		Data    struct {
			Disposition string `json:"disposition"`
		} `json:"data"`
	}
	_ = json.Unmarshal(m.Raw, &r)
	if r.Command != "prompt" {
		return nil, nil, false
	}
	s.mu.Lock()
	call = s.inflight
	s.inflight = nil
	s.mu.Unlock()
	if call == nil {
		return nil, nil, false
	}
	if r.Success && r.Data.Disposition == "handled" {
		events = []oarlock.Event{{Kind: oarlock.EventInputHandled, InputID: call.inputID, RequestID: call.requestID}}
		handled = true
	}
	return events, call, handled
}

// watchExit records the process exit once everything Pi wrote has been
// recorded: the exited response answers the dispose request when oarlock ended
// the process that way, and stands alone otherwise (a crash, a take-over).
func (s *Session) watchExit() {
	st := <-s.conn.Exit()
	<-s.pumpDone

	s.mu.Lock()
	requestID := s.disposeReq
	s.held = nil
	s.mu.Unlock()
	s.clearFallback()

	body := oarlock.ResponseBody{
		Kind:             oarlock.ResponseExited,
		Reason:           st.StderrTail,
		ExitCodeReported: true,
	}
	if st.Signal == nil {
		code := st.Code
		body.ExitCode = &code
	}
	if native, err := json.Marshal(map[string]any{"cause": st.Cause, "signal": signalName(st)}); err == nil {
		body.Native = native
	}
	s.k.Respond(requestID, body, kernel.RecordAt{})
	close(s.exitRecorded)
	s.signal()
}

func signalName(st ExitStatus) string {
	if st.Signal == nil {
		return ""
	}
	return st.Signal.String()
}

// signal wakes every waiter that re-reads the stream.
func (s *Session) signal() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

func sessRejected(code oarlock.Code, reason string) oarlock.ResponseBody {
	return oarlock.ResponseBody{Kind: oarlock.ResponseRejected, Code: code, Reason: reason}
}
