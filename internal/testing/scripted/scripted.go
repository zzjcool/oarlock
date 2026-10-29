package scripted

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
	"github.com/zzjcool/oarlock/internal/kernel"
)

// DefaultAbortGrace is how long an accepted abort may leave the turn running
// before the session takes over and kills the fake process.
const DefaultAbortGrace = time.Second

// exitRecordedWait bounds the wait for the exited response once a take-over or
// a dispose has ended the process.
const exitRecordedWait = 10 * time.Second

// Options configures NewWithOptions. The zero value is usable.
type Options struct {
	// SessionID is the stream instance's session id; "" = "scripted-session".
	SessionID string
	// Model is reported by a model frame when the session opens; "" = "scripted-1".
	Model string
	// AbortGrace is how long a turn may outlive an accepted abort before the
	// process is killed. 0 = DefaultAbortGrace; negative is an error.
	AbortGrace time.Duration
	// Images makes the session accept images (Capabilities.Images).
	Images bool
	// Redact is the kernel's credential hook; nil retains records as built.
	Redact kernel.Redactor
}

// knownTypes are the native frame types the session itself writes. Any other
// type that a script injects is kept verbatim and counted by Unknown.
var knownTypes = map[string]struct{}{
	"scripted/model":          {},
	"scripted/turn_active":    {},
	"scripted/text":           {},
	"scripted/reasoning":      {},
	"scripted/tool_start":     {},
	"scripted/tool_end":       {},
	"scripted/turn_end":       {},
	"scripted/prompt_handled": {},
}

// Session is a scripted oarlock.Session. It implements oarlock.Session and
// nothing more: no Steerer, no Withdrawer. The Pi session does implement
// oarlock.Steerer (issue #45); this scripted runtime does not model steering at
// all, so the absence here stays a compile-time fact.
//
// Everything a host can observe is real: Seq, the cursor, reachability, the
// empty-input refusal and the request/response pairs all come from an
// internal/kernel Kernel. Only the runtime behind it is a script. The session
// holds no goroutine while it is idle; a turn is one goroutine that lives from
// the moment its prompt is accepted to the moment the runtime ends it.
type Session struct {
	k      kernel.Kernel
	opt    Options
	grace  time.Duration
	script []Turn

	live sync.WaitGroup // every goroutine the session started

	// dead is closed when the fake process is gone (dispose, take-over or a
	// Die step). It stops goroutines and, together with emit, guarantees that
	// no scripted frame follows the exited response. It never decides a
	// control: reachability is read off the stream by the kernel.
	dead     chan struct{}
	deadOnce sync.Once
	emit     sync.Mutex // serialises frame emission with the exit record

	exitRecorded chan struct{} // closed once the exited response is in the stream

	mu          sync.Mutex
	changed     chan struct{} // closed and replaced whenever the stream changed in a way waiters care about
	next        int           // index of the script entry the next turn runs
	run         *turnRun      // latest accepted turn that has not finished
	held        []heldInput
	draining    bool
	again       bool
	closing     bool // Dispose started: no goroutine may be started any more
	exitDone    bool // the exit is being / has been recorded (a once guard, never a control gate)
	disposeReq  string
	disposeDone chan struct{}
	unknown     map[string]int
	inTokens    int64
	outTokens   int64
}

var _ oarlock.Session = (*Session)(nil)

// heldInput is one queued input. The queue lives in the session
// (Capabilities.Queue.Durable=false).
type heldInput struct {
	input   string
	inputID string
	origin  string
	images  []oarlock.InputImage
	queueID string // id of the queue request that holds it
}

// fallbackState is the take-over timer of the aborts aimed at one turn. Repeated
// aborts of that turn share one deadline; the turn's end stops it. It belongs to
// the turn, so a late timer can never kill a later turn.
type fallbackState struct {
	fired    chan struct{}
	cancel   chan struct{}
	stopOnce sync.Once
}

func (fb *fallbackState) stop() { fb.stopOnce.Do(func() { close(fb.cancel) }) }

func (fb *fallbackState) isFired() bool {
	select {
	case <-fb.fired:
		return true
	default:
		return false
	}
}

// New returns a scripted session. Turn n of the script runs the n-th turn the
// session starts (a prompt, or a queued input when its turn starts). A turn
// past the end of the script is empty and completes at once.
func New(script ...Turn) *Session { return newSession(Options{}, script) }

// NewWithOptions is New with options. It fails only on an invalid option.
func NewWithOptions(opt Options, script ...Turn) (*Session, error) {
	if opt.AbortGrace < 0 {
		return nil, fmt.Errorf("scripted: Options.AbortGrace is negative (%v)", opt.AbortGrace)
	}
	return newSession(opt, script), nil
}

func newSession(opt Options, script []Turn) *Session {
	if opt.SessionID == "" {
		opt.SessionID = "scripted-session"
	}
	if opt.Model == "" {
		opt.Model = "scripted-1"
	}
	grace := opt.AbortGrace
	if grace == 0 {
		grace = DefaultAbortGrace
	}
	s := &Session{
		k:            kernel.New(opt.SessionID, opt.Redact),
		opt:          opt,
		grace:        grace,
		script:       append([]Turn(nil), script...),
		dead:         make(chan struct{}),
		exitRecorded: make(chan struct{}),
		changed:      make(chan struct{}),
		unknown:      make(map[string]int),
	}
	s.emitFrame(jsonOf(map[string]any{"type": "scripted/model", "model": opt.Model}),
		[]oarlock.Event{{Kind: oarlock.EventModel, Model: opt.Model}})
	return s
}

// Capabilities declares what a host must know before acting: the queue lives
// in the session, there are no sub-agents, and images only when asked for.
func (s *Session) Capabilities() oarlock.Capabilities {
	return oarlock.Capabilities{
		Queue:       oarlock.QueueCapability{Durable: false},
		Attribution: oarlock.AttributionNone,
		Images:      s.opt.Images,
	}
}

// Records returns the retained records after the cursor.
func (s *Session) Records(cursor oarlock.Cursor) []oarlock.Record { return s.k.Records(cursor) }

// Events replays the retained records after *cursor and then delivers live ones
// until the returned function is called. A nil cursor is live only. A cursor
// that names another stream instance registers nothing. The callback runs
// inline on the goroutine that appends, so it must be cheap and must not call a
// control.
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

// Unknown is a copy of the tally of injected native frame types the session
// does not know. The frames themselves are retained verbatim; a non-zero tally
// is a test signal, never a coerced event.
func (s *Session) Unknown() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.unknown))
	for k, v := range s.unknown {
		out[k] = v
	}
	return out
}

// ---- controls --------------------------------------------------------------

// Prompt starts a turn. It is rejected busy while the stream says a turn is
// running and answered accepted once the session took it over; the turn's own
// frames follow the accepted response.
func (s *Session) Prompt(ctx context.Context, input string, opt oarlock.PromptOptions) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	out := s.prompt(oarlock.RequestBody{
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

// Queue holds input in the session and sends it as an ordinary prompt of its
// own once nothing is running. Each held input later shows up as its own prompt
// request, with its own turn.
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
		if len(req.Request.Images) > 0 && !s.opt.Images {
			return rejected(oarlock.Unsupported, "the scripted session was opened without Options.Images")
		}
		s.mu.Lock()
		s.held = append(s.held, heldInput{
			input:   req.Request.Input,
			inputID: req.Request.InputID,
			origin:  req.Request.Origin,
			images:  req.Request.Images,
			queueID: req.ID,
		})
		s.mu.Unlock()
		return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
	}, kernel.RecordAt{})
	s.kick()
	return out, nil
}

// Abort asks the running turn to stop. With no turn it is rejected
// no_active_turn. Otherwise the turn is told, and a take-over timer starts: if
// the turn has not ended when AbortGrace is up the fake process is killed and
// the abort is still accepted ("we took the stop"); the exit is recorded as
// the exited response and no turn_ended is invented for it.
func (s *Session) Abort(ctx context.Context) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	return s.k.Control(oarlock.RequestBody{Action: oarlock.ActionAbort}, func(oarlock.Record) oarlock.ResponseBody {
		return s.decideAbort(ctx)
	}, kernel.RecordAt{}), nil
}

// Dispose releases the session. It is idempotent: the first call records one
// dispose request, ends the fake process (a running turn gets no turn_ended),
// records one exited response that answers the request and waits for every
// goroutine the session started; later calls wait for and return the first
// call's result. ctx bounds only the wait.
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
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Session) runDispose(done chan struct{}) {
	defer close(done)
	_ = s.k.Control(oarlock.RequestBody{Action: oarlock.ActionDispose}, func(req oarlock.Record) oarlock.ResponseBody {
		s.mu.Lock()
		s.disposeReq = req.ID
		s.mu.Unlock()
		return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
	}, kernel.RecordAt{})

	s.mu.Lock()
	s.closing = true
	s.held = nil
	requestID := s.disposeReq
	s.mu.Unlock()

	zero := 0
	s.recordExit(requestID, &zero, "disposed")
	s.waitExitRecorded(nil)
	s.live.Wait()
}

// ---- prompt ----------------------------------------------------------------

func (s *Session) prompt(body oarlock.RequestBody) oarlock.ControlOutcome {
	var run *turnRun
	out := s.k.Control(body, func(req oarlock.Record) oarlock.ResponseBody {
		switch s.statusBefore(req.Seq).Kind {
		case fold.StatusRunning:
			return rejected(oarlock.Busy, "a turn is already running; wait for its end or abort it")
		case fold.StatusExited:
			return rejected(oarlock.RuntimeExited, "runtime exited")
		}
		if len(req.Request.Images) > 0 && !s.opt.Images {
			return rejected(oarlock.Unsupported, "the scripted session was opened without Options.Images")
		}
		run = s.newRun(req)
		return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
	}, kernel.RecordAt{})
	if run != nil {
		if out.Kind == oarlock.ControlAccepted {
			// The turn starts only after the response that accepted it is in
			// the stream: request, response, then the turn's first frame.
			s.launch(run)
		} else {
			s.release(run)
		}
	}
	// A prompt request opens a running span in the status fold and its rejection
	// closes it again, whoever rejected it (the kernel's empty-input and
	// reachability gates never reach decide). An abort that looked at the stream
	// in between must be woken, or it would wait for a change that never comes.
	s.signal()
	return out
}

// newRun registers the turn an accepted prompt starts. It consumes the next
// script entry.
func (s *Session) newRun(req oarlock.Record) *turnRun {
	s.mu.Lock()
	var steps Turn
	if s.next < len(s.script) {
		steps = s.script[s.next]
	}
	s.next++
	c := &turnRun{
		s:       s,
		steps:   steps,
		input:   req.Request.Input,
		reqID:   req.ID,
		inputID: req.Request.InputID,
		started: make(chan struct{}),
		abort:   make(chan struct{}),
		done:    make(chan struct{}),

		ignoreAbort: hasIgnoreAbort(steps),
	}
	s.run = c
	s.mu.Unlock()
	s.signal()
	return c
}

func (s *Session) launch(c *turnRun) {
	s.mu.Lock()
	ok := s.goLocked(func() { s.runTurn(c) })
	s.mu.Unlock()
	if !ok { // Dispose has started: the accepted turn never runs
		s.release(c)
		close(c.done)
	}
}

// release forgets c as the running turn.
func (s *Session) release(c *turnRun) {
	s.mu.Lock()
	if s.run == c {
		s.run = nil
	}
	s.mu.Unlock()
	s.signal()
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

// ---- the turn --------------------------------------------------------------

// turnRun is one turn of the script, run by one goroutine.
type turnRun struct {
	s       *Session
	steps   Turn
	input   string
	reqID   string
	inputID string

	started   chan struct{} // closed once the turn is active (its turn_active is in the stream)
	abort     chan struct{} // closed when an abort is delivered to the turn
	abortOnce sync.Once
	done      chan struct{} // closed when the goroutine has finished

	// guarded by Session.mu
	fb   *fallbackState // take-over timer of the aborts aimed at this turn
	over bool           // the turn has ended: no timer may be armed for it any more

	// owned by the turn goroutine
	ignoreAbort bool
	outcome     *oarlock.TurnOutcome
	callSeq     int
}

func (c *turnRun) requestAbort() { c.abortOnce.Do(func() { close(c.abort) }) }

// aborted reports whether an abort the runtime honours is pending.
func (c *turnRun) aborted() bool {
	if c.ignoreAbort {
		return false
	}
	select {
	case <-c.abort:
		return true
	default:
		return false
	}
}

// sleep waits d (forever when d < 0). An honoured abort ends the turn aborted,
// a dead process ends it silently.
func (c *turnRun) sleep(d time.Duration) action {
	var fire <-chan time.Time
	if d >= 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		fire = t.C
	}
	var abort <-chan struct{}
	if !c.ignoreAbort {
		abort = c.abort
	}
	select {
	case <-fire:
		return actNext
	case <-abort:
		c.outcome = &oarlock.TurnOutcome{Kind: oarlock.OutcomeAborted}
		return actEnd
	case <-c.s.dead:
		return actGone
	}
}

func (c *turnRun) frame(typ string, payload map[string]any, events ...oarlock.Event) {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["type"] = typ
	c.rawFrame(jsonOf(payload), events)
}

func (c *turnRun) rawFrame(native json.RawMessage, events []oarlock.Event) {
	c.s.emitFrame(native, events)
}

func (c *turnRun) addOutput(text string) { c.s.addTokens(0, tokensOf(text)) }

// hasIgnoreAbort reports whether the turn marks itself as one whose runtime does
// not respond to abort. It is a property of the whole turn, so an abort cannot
// race the step that says so.
func hasIgnoreAbort(steps Turn) bool {
	for _, st := range steps {
		if _, ok := st.(ignoreAbortStep); ok {
			return true
		}
	}
	return false
}

func tokensOf(text string) int64 { return int64((len(text) + 3) / 4) }

func (s *Session) addTokens(in, out int64) {
	s.mu.Lock()
	s.inTokens += in
	s.outTokens += out
	s.mu.Unlock()
}

// runTurn is the whole life of one turn. It ends without a turn_ended when the
// process is gone (dispose, take-over, Die): the stream never gets a completion
// the runtime did not report.
func (s *Session) runTurn(c *turnRun) {
	defer close(c.done)
	defer s.release(c)
	defer s.endTurnTimer(c)

	if len(c.steps) > 0 {
		if _, handled := c.steps[0].(handledStep); handled {
			s.emitFrame(jsonOf(map[string]any{"type": "scripted/prompt_handled"}),
				[]oarlock.Event{{Kind: oarlock.EventInputHandled, InputID: c.inputID, RequestID: c.reqID}})
			s.kick()
			return
		}
	}

	if !s.emitFrame(jsonOf(map[string]any{"type": "scripted/turn_active"}),
		[]oarlock.Event{{Kind: oarlock.EventTurnActive, InputID: c.inputID}}) {
		return
	}
	close(c.started)
	s.signal()
	s.addTokens(tokensOf(c.input), 0)

loop:
	for _, st := range c.steps {
		select {
		case <-s.dead:
			return
		default:
		}
		if c.aborted() {
			c.outcome = &oarlock.TurnOutcome{Kind: oarlock.OutcomeAborted}
			break loop
		}
		switch st.apply(c) {
		case actEnd:
			break loop
		case actGone, actDied:
			return
		}
	}
	if c.outcome == nil {
		kind := oarlock.OutcomeCompleted
		if c.aborted() {
			kind = oarlock.OutcomeAborted
		}
		c.outcome = &oarlock.TurnOutcome{Kind: kind}
	}

	// The runtime is ending the turn by its own report: no take-over may follow.
	s.endTurnTimer(c)
	if !s.emitFrame(jsonOf(map[string]any{"type": "scripted/turn_end", "outcome": string(c.outcome.Kind)}),
		[]oarlock.Event{{Kind: oarlock.EventTurnEnded, Outcome: c.outcome}, s.usageEvent()}) {
		return
	}
	s.kick()
}

func (s *Session) usageEvent() oarlock.Event {
	s.mu.Lock()
	in, out := s.inTokens, s.outTokens
	s.mu.Unlock()
	total, window := in+out, int64(200_000)
	percent := float64(total) / float64(window) * 100
	return oarlock.Event{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{
		Tokens:  &oarlock.TokenTotals{Input: in, Output: out},
		Context: &oarlock.ContextUsage{Tokens: &total, ContextWindow: &window, Percent: &percent},
	}}
}

// emitFrame appends one frame the fake runtime wrote. A dead process writes
// nothing, and the lock makes that exact: no scripted frame can land after the
// exited response. It reports whether the frame was written.
func (s *Session) emitFrame(native json.RawMessage, events []oarlock.Event) bool {
	s.emit.Lock()
	defer s.emit.Unlock()
	select {
	case <-s.dead:
		return false
	default:
	}
	s.countUnknown(native)
	s.k.Frame(kernel.FrameBody{Native: native, Events: events}, kernel.RecordAt{})
	s.signal()
	return true
}

func (s *Session) countUnknown(native json.RawMessage) {
	var h struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(native, &h)
	if _, known := knownTypes[h.Type]; known {
		return
	}
	s.mu.Lock()
	s.unknown[h.Type]++
	s.mu.Unlock()
}

// recordExit records the process exit once. The exited response answers the
// dispose request when the session ended the process that way, and stands alone
// (requestID "") when the process died on its own or was taken over.
func (s *Session) recordExit(requestID string, code *int, cause string) {
	s.emit.Lock()
	s.exitLocked(requestID, code, cause)
	s.emit.Unlock()
	s.signal()
}

// exitLocked is recordExit for a caller that holds emit. Closing dead and
// appending the exited response happen under that one lock, which is what makes
// "no scripted frame follows the exit" exact. It reports whether this call
// recorded the exit.
func (s *Session) exitLocked(requestID string, code *int, cause string) bool {
	s.mu.Lock()
	if s.exitDone {
		s.mu.Unlock()
		return false
	}
	s.exitDone = true
	s.held = nil
	s.mu.Unlock()

	s.deadOnce.Do(func() { close(s.dead) })
	s.k.Respond(requestID, oarlock.ResponseBody{
		Kind:             oarlock.ResponseExited,
		ExitCode:         code,
		ExitCodeReported: true,
		Native:           jsonOf(map[string]string{"cause": cause}),
	}, kernel.RecordAt{})
	close(s.exitRecorded)
	return true
}

// waitExitRecorded waits for the exited response, for at most exitRecordedWait
// and until cancel (nil = no cancel) is closed.
func (s *Session) waitExitRecorded(cancel <-chan struct{}) {
	t := time.NewTimer(exitRecordedWait)
	defer t.Stop()
	select {
	case <-s.exitRecorded:
	case <-cancel:
	case <-t.C:
	}
}

// ---- abort -----------------------------------------------------------------

func (s *Session) decideAbort(ctx context.Context) oarlock.ResponseBody {
	if s.status().Kind != fold.StatusRunning {
		return rejected(oarlock.NoActiveTurn, "no active turn")
	}
	var (
		target    *turnRun // the turn this abort is aimed at
		fb        *fallbackState
		delivered bool
	)
	for {
		// changed is read before the stream is, so a change in between is never lost.
		s.mu.Lock()
		cur, changed := s.run, s.changed
		s.mu.Unlock()

		if target == nil && cur != nil {
			target = cur
			fb = s.armFallback(target)
		}
		// Like Pi, which runs a turn anyway when an abort beats agent_start, the
		// abort is delivered only once the turn is active.
		if target != nil && !delivered {
			select {
			case <-target.started:
				delivered = true
				target.requestAbort()
			default:
			}
		}

		switch s.status().Kind {
		case fold.StatusExited:
			if fb != nil && fb.isFired() {
				return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
			}
			return rejected(oarlock.RuntimeExited, "runtime exited before it answered the abort")
		case fold.StatusIdle:
			if !delivered {
				return rejected(oarlock.NoActiveTurn, "the turn ended before the abort could be delivered")
			}
			return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
		}
		if delivered {
			select {
			case <-target.done:
				// The turn this abort was aimed at is over by the runtime's own
				// report. A later turn started from the queue is not ours.
				return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
			default:
			}
		}

		var fired <-chan struct{}
		if fb != nil {
			fired = fb.fired
		}
		select {
		case <-changed:
		case <-fired:
			// Taken over: the process is being killed. Still accepted, and no
			// turn end is invented for it.
			s.waitExitRecorded(ctx.Done())
			return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
		case <-ctx.Done():
			if fb == nil {
				return rejected(oarlock.ErrorCode, ctx.Err().Error())
			}
			// The take-over timer stays armed, so the stop is ours either way.
			return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
		}
	}
}

// armFallback returns the take-over timer of the aborts aimed at c, starting it
// on the first one. A turn that is already over gets a stopped timer.
func (s *Session) armFallback(c *turnRun) *fallbackState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.fb != nil {
		return c.fb
	}
	fb := &fallbackState{fired: make(chan struct{}), cancel: make(chan struct{})}
	if c.over || !s.goLocked(func() { s.watchGrace(c, fb) }) {
		fb.stop() // the turn is over or Dispose has started: nothing is left to take over
		return fb
	}
	c.fb = fb
	return fb
}

// endTurnTimer stops the take-over timer of c and forbids a new one.
func (s *Session) endTurnTimer(c *turnRun) {
	s.mu.Lock()
	c.over = true
	fb := c.fb
	c.fb = nil
	s.mu.Unlock()
	if fb != nil {
		fb.stop()
	}
}

// watchGrace is the take-over timer: one goroutine for as long as an abort of c
// is outstanding, gone when the turn ends, the process exits or the timer fires.
func (s *Session) watchGrace(c *turnRun, fb *fallbackState) {
	t := time.NewTimer(s.grace)
	defer t.Stop()
	select {
	case <-t.C:
		s.fireFallback(c, fb)
	case <-fb.cancel:
	case <-s.dead:
	}
}

// fireFallback is the grace expiry: if the turn the aborts were aimed at is
// still running, the fake process is killed. The abort is told first (fired),
// so it answers accepted rather than runtime_exited; the exit is then recorded
// standing alone.
//
// The decision and the kill happen under emit, the lock every frame of the turn
// is written under. So either the turn's own turn_ended is already in the
// stream (the turn is over, nothing is taken over) or it never will be (the
// process is dead): a take-over can never land after a completed turn.
func (s *Session) fireFallback(c *turnRun, fb *fallbackState) {
	s.emit.Lock()
	s.mu.Lock()
	current := c.fb == fb && !c.over
	s.mu.Unlock()
	if !current || s.status().Kind != fold.StatusRunning {
		s.emit.Unlock()
		return
	}
	close(fb.fired)
	s.exitLocked("", nil, "taken_over")
	s.emit.Unlock()
	s.signal()
}

// ---- queue -----------------------------------------------------------------

// kick drains the held input. A kick that lands while a drain runs is not lost.
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
// stream says nothing is running. It reports whether it consumed an input.
func (s *Session) drainOne() bool {
	if s.k.Unreachable() != nil {
		s.dropHeld()
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
	out := s.prompt(oarlock.RequestBody{
		Action:  oarlock.ActionPrompt,
		Input:   h.input,
		InputID: h.inputID,
		Images:  h.images,
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
			s.dropHeld()
			return false
		}
		// Any other refusal stays recorded on that prompt request and does not
		// block the input behind it.
	}
	return true
}

func (s *Session) dropHeld() {
	s.mu.Lock()
	s.held = nil
	s.mu.Unlock()
}

// ---- plumbing --------------------------------------------------------------

// goLocked starts a tracked goroutine unless Dispose has started. The caller
// holds mu, which is what keeps Add from racing the Wait in Dispose.
func (s *Session) goLocked(fn func()) bool {
	if s.closing {
		return false
	}
	s.live.Add(1)
	go func() {
		defer s.live.Done()
		fn()
	}()
	return true
}

// signal wakes every waiter that re-reads the stream.
func (s *Session) signal() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

func rejected(code oarlock.Code, reason string) oarlock.ResponseBody {
	return oarlock.ResponseBody{Kind: oarlock.ResponseRejected, Code: code, Reason: reason}
}

// jsonOf marshals the strings, numbers and maps of them this package builds,
// which cannot fail; the null it falls back to is unreachable.
func jsonOf(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}
