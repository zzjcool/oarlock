package pi

// The control plane of one Pi RPC session (issue #10): oarlock.Session over a
// Conn, recorded on a real kernel.
//
//   - Every message Pi writes becomes exactly one frame, in wire order, Native
//     verbatim. Responses are frames too: a late native reply to an abort
//     must stay a frame (docs/spec/errors.md), and the "handled" fact lives on
//     the prompt response.
//   - Only the minimum turn-boundary projection exists here, so the state
//     machine is testable: agent_start → turn_active, agent_settled →
//     turn_ended (the runtime's own stopReason / aborted flag) and a prompt
//     response with disposition "handled" → input_handled. Text, tools and
//     usage are #11's.
//   - Status is never cached: busy, no_active_turn and the queue drain all
//     fold the retained records (internal/fold), so the answer agrees with
//     what a host reading the stream sees.
//   - Steer is deliberately NOT implemented (issue #10 item 5): *Session does
//     not satisfy oarlock.Steerer, so the absence is a compile-time fact.
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
}

// Session is a Pi RPC session. It implements oarlock.Session and nothing more:
// no Steerer, no Withdrawer.
type Session struct {
	conn  Conn
	k     kernel.Kernel
	grace time.Duration

	pumpDone     chan struct{} // closed when Conn.Events has been drained
	exitRecorded chan struct{} // closed once the exited response is in the stream

	mu          sync.Mutex
	changed     chan struct{} // closed and replaced whenever the stream changed in a way waiters care about
	started     bool          // agent_start seen for the current run
	lastStop    string        // stopReason of the last turn_end of this run
	lastErr     string        // its errorMessage
	inflight    *promptCall
	held        []heldInput
	draining    bool
	again       bool
	fb          *fallbackState
	disposeReq  string
	disposeDone chan struct{}
	disposeErr  error
	unknown     map[string]int
}

var _ oarlock.Session = (*Session)(nil)

// promptCall is the prompt command on the wire. Pi answers it before it starts
// the turn, and the Conn matches the answer to Do by an id this layer never
// sees, so the pump learns which request a prompt response belongs to from here:
// busy keeps at most one prompt in flight.
type promptCall struct {
	requestID string
	inputID   string
	recorded  chan struct{} // closed by the pump after it recorded the response frame
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
		pumpDone:     make(chan struct{}),
		exitRecorded: make(chan struct{}),
		changed:      make(chan struct{}),
		unknown:      make(map[string]int),
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

// Unknown is a copy of the tally of native message types that are neither a
// response nor in the evidenced set (docs/runtimes/pi.md §9). The frames
// themselves are retained verbatim; a non-zero tally is a test signal.
func (s *Session) Unknown() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.unknown))
	for k, v := range s.unknown {
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
	if len(s.held) == 0 {
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
		events    []oarlock.Event
		call      *promptCall
		turnEnded bool
		handled   bool
	)
	switch m.Type {
	case "response":
		events, call, handled = s.projectResponse(m)
	case "agent_start":
		events = []oarlock.Event{{Kind: oarlock.EventTurnActive}}
		s.mu.Lock()
		s.started = true
		s.mu.Unlock()
	case "turn_end":
		// Not a turn end: a run with tools has several. It only supplies the
		// runtime's own stopReason for the settlement that follows.
		var v struct {
			Message struct {
				StopReason   string `json:"stopReason"`
				ErrorMessage string `json:"errorMessage"`
			} `json:"message"`
		}
		_ = json.Unmarshal(m.Raw, &v)
		s.mu.Lock()
		s.lastStop, s.lastErr = v.Message.StopReason, v.Message.ErrorMessage
		s.mu.Unlock()
	case "agent_settled":
		events = []oarlock.Event{{Kind: oarlock.EventTurnEnded, Outcome: s.settle(m.Raw)}}
		turnEnded = true
	default:
		if _, known := knownTypes[m.Type]; !known {
			s.mu.Lock()
			s.unknown[m.Type]++
			s.mu.Unlock()
		}
	}

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
	if turnEnded || handled {
		s.kick()
	}
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

// settle builds the turn outcome from the runtime's own words: the aborted flag
// of agent_settled, else the stopReason of the run's last turn_end. It never
// comes from a timer or an exit.
func (s *Session) settle(raw json.RawMessage) *oarlock.TurnOutcome {
	var v struct {
		Aborted bool `json:"aborted"`
	}
	_ = json.Unmarshal(raw, &v)
	s.mu.Lock()
	stop, msg := s.lastStop, s.lastErr
	s.lastStop, s.lastErr = "", ""
	s.started = false
	s.mu.Unlock()
	switch {
	case v.Aborted || stop == "aborted":
		return &oarlock.TurnOutcome{Kind: oarlock.OutcomeAborted}
	case stop == "error":
		return &oarlock.TurnOutcome{Kind: oarlock.OutcomeFailed, Failure: oarlock.FailureUnknown, Reason: msg}
	default:
		return &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted}
	}
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
