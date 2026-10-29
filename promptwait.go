package oarlock

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"
)

// RunOutcome is how PromptAndWait's run ended. It is the small, host-facing
// summary of the turn; the record stream remains the source of truth.
type RunOutcome string

const (
	// RunCompleted: the runtime reported a completed turn.
	RunCompleted RunOutcome = "completed"
	// RunFailed: the runtime reported a failed turn, or the process exited.
	RunFailed RunOutcome = "failed"
	// RunAborted: the runtime reported an aborted turn.
	RunAborted RunOutcome = "aborted"
	// RunInterrupted: PromptAndWait's own deadline or the caller's context
	// caused an abort. It is not a vendor outcome.
	RunInterrupted RunOutcome = "interrupted"
	// RunRejected: the prompt itself was refused and never started a turn.
	RunRejected RunOutcome = "rejected"
)

// RunResult is what PromptAndWait saw. Text is the root agent's streamed text
// (child agents are not concatenated). Code/Reason are set when the prompt was
// rejected. Turn is the runtime's own completion when it reported one.
type RunResult struct {
	Outcome   RunOutcome
	Text      string
	Turn      *TurnOutcome
	Seq       uint64
	RequestID string
	Code      Code
	Reason    string
}

// WaitOption configures PromptAndWait.
type WaitOption func(*waitConfig)

type waitConfig struct {
	timeout time.Duration
}

// WithTimeout bounds the wait for the runtime's own turn end. On expiry
// PromptAndWait aborts the turn and reports RunInterrupted; it never invents a
// turn end. A non-positive duration means no extra deadline.
func WithTimeout(d time.Duration) WaitOption {
	return func(c *waitConfig) { c.timeout = d }
}

// waitSweep is the safety-net interval of the event-driven wait: when the side
// tap delivered nothing since the last tick, the watcher re-reads Records from
// its cursor so a session whose tap never fires (a Cursor the stream refused
// registers no observer) still cannot lose a turn end. With a working tap the
// sweep never reads Records; it is not the polling loop it replaced.
const waitSweep = time.Second

// eventTapBuffer bounds the watcher's side tap. It is a variable only so a test
// can shrink it and force the drop path; production code never writes it. A test
// that mutates it must not call t.Parallel: the seam is package-level.
var eventTapBuffer = 256

// abortWaitBound bounds the event-driven wait for the runtime's own turn end
// after an abort. It is a variable only so a test can shrink it; production
// code never writes it. A test that mutates it must not call t.Parallel: the
// seam is package-level.
var abortWaitBound = 10 * time.Second

// PromptAndWait sends one prompt and waits for the runtime's own completion. It
// is a convenience over Prompt + Records; it never synthesizes a turn end and
// never appends a record. It only calls Prompt and Abort, and the stream stays
// the source of truth.
//
// The wait is event-driven. It subscribes to Session.Events after Prompt
// returns and before the first wait: ObserveFrom replays the retained records
// after the cursor and registers the observer in one critical section, so
// nothing between Prompt returning and the subscription is missed. The observer
// is a side tap: its callback hands each record to a bounded 256-record buffer
// and never blocks the run. A burst larger than that buffer is caught up with
// one Records read from the same cursor, in Seq order. A one-second sweep
// re-reads Records only when the tap delivered nothing since the last tick, so
// a session whose tap never fires still cannot lose a turn end; it is a safety
// net, not a poll.
//
// A rejected prompt returns RunRejected with Code/Reason and no error. A
// cancelled caller context aborts the turn, waits for the runtime's own end
// inside a bounded grace, and reports RunInterrupted with the caller's context
// error. A WaitOption deadline is PromptAndWait's own interruption, not a caller
// error: it aborts the same way and reports RunInterrupted with a nil error. In
// both cases a real completion that raced the abort wins and reads as
// RunCompleted. When the runtime never reports its own end inside the grace,
// RunInterrupted carries Turn = aborted: that is the result's summary, not a
// record, and no turn end is written to the stream. The aborted summary is used
// only when the runtime reported nothing at all; a runtime exit inside the grace
// is kept as its own fact.
func PromptAndWait(ctx context.Context, sess Session, input string, opts ...WaitOption) (RunResult, error) {
	if ctx == nil {
		return RunResult{}, errors.New("oarlock: PromptAndWait(nil context)")
	}
	if sess == nil {
		return RunResult{}, errors.New("oarlock: PromptAndWait(nil session)")
	}
	var cfg waitConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	waitCtx := ctx
	if cfg.timeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, cfg.timeout)
		defer cancel()
	}

	out, err := sess.Prompt(ctx, input, PromptOptions{})
	if err != nil {
		return RunResult{RequestID: out.RequestID, Seq: out.Seq}, err
	}
	res := RunResult{RequestID: out.RequestID, Seq: out.Seq}
	if out.Kind == ControlRejected {
		res.Outcome = RunRejected
		res.Code = out.Code
		res.Reason = out.Reason
		return res, nil
	}
	root := newRunRoot(out.Request.SessionID)
	var text strings.Builder
	w := newRunWatcher(sess, root, out.Seq, &res, &text)
	// Subscribe after Prompt returned and before the first wait. No
	// subscription exists on the rejected branch above: nothing was started.
	cursor := root.cursor(out.Seq)
	w.stop = sess.Events(&cursor, w.tapUp)
	if w.stop == nil {
		w.stop = func() {}
	}
	defer w.stop()
	switch w.await(waitCtx, 0) {
	case waitDecided:
		return w.result(nil)
	default:
		// The caller's context, or the WaitOption deadline, ended the wait.
		return w.abort(ctx, waitCtx)
	}
}

// runRoot is the root-session filter of one PromptAndWait run.
//
// instance is the session id the prompt request itself was recorded with: the
// kernel's stream instance, and the session every read is applied to. root is
// the id root records are filtered with; it is instance except on a fresh
// Cursor chat (adapters/cursor), where the prompt request is recorded under the
// kernel's empty session id and every later frame is stamped with the chat id
// the runtime reported in system/init. There the first root record carrying a
// non-empty session id is that chat, and it becomes the root: without it the
// run's text and the runtime's own turn_ended look like a child session's and
// the wait would time out on a completed turn.
type runRoot struct {
	instance string
	root     string
}

func newRunRoot(instance string) *runRoot {
	return &runRoot{instance: instance, root: instance}
}

// cursor is the read cursor of the instance the prompt was recorded on. The
// learned root is never the cursor's session id: the kernel still knows this
// instance by the id it had at prompt time.
func (r *runRoot) cursor(after uint64) Cursor {
	return Cursor{SessionID: r.instance, AfterSeq: after}
}

// admit reports whether rec belongs to the root agent this run waits for. A
// child session's frames (its own session id) and a sub-agent's frames
// (AgentPath) are not the root's.
func (r *runRoot) admit(rec Record) bool {
	if len(rec.AgentPath) != 0 {
		return false
	}
	if r.instance != "" {
		return rec.SessionID == r.root
	}
	// A fresh Cursor chat: the empty id is the kernel's own root records, before
	// and after the switch, and the first non-empty id a root record carries is
	// the chat id learned from system/init.
	if rec.SessionID == "" {
		return true
	}
	if r.root == "" {
		r.root = rec.SessionID
	}
	return rec.SessionID == r.root
}

// waitOutcome is how one await step ended.
type waitOutcome int

const (
	// waitPending is the zero value: await never returns it.
	waitPending waitOutcome = iota
	// waitDecided: the drained records answered the wait.
	waitDecided
	// waitCancelled: the caller's context (or the WaitOption deadline) fired.
	waitCancelled
	// waitExpired: the bounded abort grace ran out.
	waitExpired
)

// runWatcher is the event-driven wait of one PromptAndWait run.
//
// It owns the read position (after), what the root's records said so far
// (text, Turn, handled, exited) and the side tap that feeds it. Everything
// except tapUp runs on the PromptAndWait goroutine, so no field here except the
// two atomics is shared with the appending goroutine: tapUp never touches the
// result, the text or the read position, and never blocks.
type runWatcher struct {
	sess  Session
	root  *runRoot
	after uint64
	res   *RunResult
	text  *strings.Builder

	handled bool
	exited  bool

	// tap is the bounded queue between the observer callback and this goroutine.
	tap chan Record
	// wake carries one pending "the tap overflowed" signal.
	wake chan struct{}
	// dropped says a record did not fit in tap and a catch-up read is owed.
	dropped atomic.Bool
	// sawTap says the tap delivered something since the last sweep tick.
	sawTap atomic.Bool

	stop func()
}

func newRunWatcher(sess Session, root *runRoot, after uint64, res *RunResult, text *strings.Builder) *runWatcher {
	return &runWatcher{
		sess:  sess,
		root:  root,
		after: after,
		res:   res,
		text:  text,
		tap:   make(chan Record, eventTapBuffer),
		wake:  make(chan struct{}, 1),
	}
}

// tapUp is the Session.Events callback. It runs inline on the goroutine that
// appends to the stream, so it must never block and must never call a control:
// it only hands the record to the bounded tap. A full tap drops the record and
// asks for one catch-up Records read, which is lossless because the cursor is
// the same position.
func (w *runWatcher) tapUp(rec Record) {
	w.sawTap.Store(true)
	select {
	case w.tap <- rec:
	default:
		w.dropped.Store(true)
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}

// consume applies one record to the wait and reports nothing: the decision is
// made on the whole drained batch.
func (w *runWatcher) consume(rec Record) {
	// The tap's replay and a Records catch-up read can overlap, so a record at
	// or before the read position is already applied: duplicated text deltas are
	// the bug this avoids.
	if rec.Seq <= w.after {
		return
	}
	// Seq tracks the last record read, child sessions' frames included, even
	// though the root filter below skips their events.
	w.after = rec.Seq
	w.res.Seq = rec.Seq
	if !w.root.admit(rec) {
		return
	}
	for _, ev := range rec.Events {
		switch ev.Kind {
		case EventTextDelta:
			w.text.WriteString(ev.Text)
		case EventTurnEnded:
			w.res.Turn = ev.Outcome
		case EventInputHandled:
			if ev.RequestID == w.res.RequestID {
				w.handled = true
			}
		}
	}
	if rec.Kind == KindResponse && rec.Response != nil && rec.Response.Kind == ResponseExited {
		w.exited = true
	}
}

// drain applies every queued tap record without blocking, then pays any owed
// catch-up read.
func (w *runWatcher) drain() {
drained:
	for {
		select {
		case rec := <-w.tap:
			w.consume(rec)
		default:
			break drained
		}
	}
	if w.dropped.Swap(false) {
		w.catchUp()
	}
}

// catchUp re-reads the stream from the current position. It is lossless and
// ordered because Records replays retained records with Seq > after in Seq
// order; consume's dedup makes the overlap with the tap harmless.
func (w *runWatcher) catchUp() {
	for _, rec := range w.sess.Records(w.root.cursor(w.after)) {
		w.consume(rec)
	}
}

// decided reports whether the drained batch already answers the wait, with the
// runtime's own priority: a handled prompt (no turn at all), then the runtime's
// turn end, then the process exiting.
func (w *runWatcher) decided() bool {
	if w.handled {
		w.res.Outcome = RunCompleted
		return true
	}
	if w.res.Turn != nil {
		w.res.Outcome = outcomeOfRun(w.res.Turn)
		return true
	}
	if w.exited {
		w.res.Outcome = RunFailed
		w.res.Turn = &TurnOutcome{Kind: OutcomeFailed, Failure: FailureRuntimeExited, Reason: "runtime exited"}
		return true
	}
	return false
}

// await waits for the runtime's own end, event-driven, for at most bound (zero
// means no bound). A nil ctx has no cancellation.
func (w *runWatcher) await(ctx context.Context, bound time.Duration) waitOutcome {
	var deadline <-chan time.Time
	if bound > 0 {
		timer := time.NewTimer(bound)
		defer timer.Stop()
		deadline = timer.C
	}
	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}
	sweep := time.NewTicker(waitSweep)
	defer sweep.Stop()
	for {
		w.drain()
		if w.decided() {
			return waitDecided
		}
		if ctx != nil && ctx.Err() != nil {
			return waitCancelled
		}
		select {
		case rec := <-w.tap:
			w.consume(rec)
		case <-w.wake:
			// The tap overflowed: the next loop's drain pays the catch-up read.
		case <-sweep.C:
			// Safety net only: a session whose tap never fires still gets a
			// lossless read, so a turn end is never lost.
			if !w.sawTap.Swap(false) {
				w.catchUp()
			}
		case <-deadline:
			return waitExpired
		case <-done:
			return waitCancelled
		}
	}
}

// abort asks the runtime to stop and waits, event-driven and bounded, for the
// runtime to end the turn itself.
func (w *runWatcher) abort(ctx, waitCtx context.Context) (RunResult, error) {
	// Ask the runtime to stop. A rejection is a normal race; the record stream
	// decides what actually happened.
	_, _ = w.sess.Abort(context.WithoutCancel(ctx))
	switch w.await(context.Background(), abortWaitBound) {
	case waitDecided:
		// A handled prompt still reads as completed, and a real completion that
		// raced the abort wins; an accepted abort is what ended the turn and does
		// not read as completed.
		if w.handled || (w.res.Turn != nil && w.res.Turn.Kind == OutcomeCompleted) {
			w.res.Outcome = RunCompleted
		} else {
			w.res.Outcome = RunInterrupted
		}
	default:
		// The runtime never reported its own end inside the grace: PromptAndWait
		// reports its own interruption. Turn is the result's summary when there is
		// none; no turn end is appended to the stream.
		w.res.Outcome = RunInterrupted
		if w.res.Turn == nil {
			w.res.Turn = &TurnOutcome{Kind: OutcomeAborted}
		}
	}
	return w.result(callerErr(ctx, waitCtx))
}

// result is the one return path: the text is always the builder's.
func (w *runWatcher) result(err error) (RunResult, error) {
	w.res.Text = w.text.String()
	return *w.res, err
}

func callerErr(ctx, waitCtx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// A WaitOption deadline is PromptAndWait's own interruption, not a caller
	// error; the result says RunInterrupted.
	_ = waitCtx
	return nil
}

func outcomeOfRun(t *TurnOutcome) RunOutcome {
	switch t.Kind {
	case OutcomeCompleted:
		return RunCompleted
	case OutcomeAborted:
		return RunAborted
	default:
		return RunFailed
	}
}
