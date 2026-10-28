package scripted

// The shared behaviour suite (issue #18, acceptance 1).
//
// RunSuite checks what every runtime's oarlock.Session must do, and only that,
// through the oarlock.Session interface alone: it never type-asserts to a
// concrete session, so the same sub-tests run against the scripted runtime
// (suite_test.go) and against a real adapter (pi_live_test.go). An optional
// operation (Steerer, Withdrawer) is the presence of an interface, not a
// capability flag, so nothing here requires or exercises one.
//
// Status is always read the way a host reads it: by folding Records
// (internal/fold), never from a flag the session keeps for itself. Waits poll
// the stream, so a runtime whose turn ends on its own goroutine is judged by
// what it recorded, not by when it returned.
//
// Everything unexported here carries a "suite" prefix: this file is part of the
// package proper, so a generic helper name would collide with the package's
// own files and its tests.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

// Behaviour is what the turn a sub-test starts is expected to do. Target.Open
// receives one per turn the sub-test will start, in order.
type Behaviour int

const (
	// Quick is a turn that ends by itself, soon, completed.
	Quick Behaviour = iota
	// Long is a turn that keeps running until it is aborted or the session is
	// disposed.
	Long
)

// Target is one runtime under the suite.
type Target struct {
	// Name labels failures.
	Name string

	// Open returns a fresh, idle session. plan is the behaviour of the first,
	// second, ... turn the sub-test is going to start; it is empty when the
	// sub-test starts none. RunSuite disposes the session when the sub-test
	// ends, so Open need not.
	Open func(t *testing.T, plan ...Behaviour) oarlock.Session

	// Prompt is the prompt text that makes the runtime show behaviour b. It
	// must not be empty.
	Prompt func(b Behaviour) string

	// Timeout bounds every wait and every control call. 0 means 10s.
	Timeout time.Duration
}

const (
	suiteDefaultTimeout = 10 * time.Second
	suitePollInterval   = 2 * time.Millisecond
	// suiteCleanupTimeout bounds the Dispose that ends every sub-test.
	suiteCleanupTimeout = 10 * time.Second
	// suiteSettleTime is how long the suite waits before it asserts that
	// something did NOT happen (a second turn_ended, a record on a tap that was
	// unsubscribed).
	suiteSettleTime = 25 * time.Millisecond
)

// RunSuite runs the shared behaviour sub-tests against target: the turn
// lifecycle, busy, abort, empty input, the queue, dispose (idle and mid-turn)
// and the Events replay. Every sub-test opens its own session.
func RunSuite(t *testing.T, target Target) {
	t.Helper()
	if target.Open == nil || target.Prompt == nil {
		t.Fatal("scripted.RunSuite: Target.Open and Target.Prompt are required")
	}
	r := &suiteRunner{target: target, timeout: target.Timeout}
	if r.timeout <= 0 {
		r.timeout = suiteDefaultTimeout
	}
	cases := []struct {
		name string
		run  func(*suiteRunner, *testing.T)
	}{
		{"prompt_turn_end_idle", (*suiteRunner).promptTurnEndIdle},
		{"busy", (*suiteRunner).busy},
		{"abort", (*suiteRunner).abort},
		{"abort_idle", (*suiteRunner).abortIdle},
		{"empty_input", (*suiteRunner).emptyInput},
		{"queue", (*suiteRunner).queue},
		{"dispose", (*suiteRunner).dispose},
		{"dispose_mid_turn", (*suiteRunner).disposeMidTurn},
		{"events_replay", (*suiteRunner).eventsReplay},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.run(r, t) })
	}
}

type suiteRunner struct {
	target  Target
	timeout time.Duration
}

func (r *suiteRunner) label() string {
	if r.target.Name == "" {
		return "target"
	}
	return r.target.Name
}

// open starts a session for one sub-test and disposes it when the sub-test
// ends, whatever the sub-test did to it.
func (r *suiteRunner) open(t *testing.T, plan ...Behaviour) oarlock.Session {
	t.Helper()
	sess := r.target.Open(t, plan...)
	if sess == nil {
		t.Fatalf("%s: Open returned a nil session", r.label())
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), suiteCleanupTimeout)
		defer cancel()
		if err := sess.Dispose(ctx); err != nil {
			t.Errorf("%s: cleanup Dispose = %v, want nil", r.label(), err)
		}
	})
	return sess
}

// ---- sub-tests -----------------------------------------------------------------

// prompt → accepted → turn_ended{completed} → idle.
func (r *suiteRunner) promptTurnEndIdle(t *testing.T) {
	sess := r.open(t, Quick)

	out := r.doPrompt(t, sess, r.target.Prompt(Quick))
	suiteWantAccepted(t, "Prompt on an idle session", out)
	r.recorded(t, sess, out, oarlock.ActionPrompt) // the request precedes its accepted response

	r.waitFor(t, sess, "the turn to end and the status to return to idle", suiteTurnsEnded(1))
	time.Sleep(suiteSettleTime) // a second turn_ended for the same prompt would show up now
	records := sess.Records(oarlock.Cursor{})

	if got := suiteTurnOutcomes(records); len(got) != 1 {
		t.Fatalf("turn_ended events = %d, want exactly 1 (%s)", len(got), suiteDescribe(records))
	} else if got[0].Kind != oarlock.OutcomeCompleted {
		t.Fatalf("turn outcome = %s (failure %q, reason %q), want completed", got[0].Kind, got[0].Failure, got[0].Reason)
	}
	st := suiteStatusOf(records)
	if st.Kind != fold.StatusIdle {
		t.Fatalf("status after the turn = %s, want idle", st.Kind)
	}
	if st.LastTurnOutcome == nil || st.LastTurnOutcome.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("status.LastTurnOutcome = %+v, want completed", st.LastTurnOutcome)
	}
	suiteAssertDense(t, records)
}

// A second prompt while a turn runs is rejected busy, never queued implicitly,
// and busy agrees with the status folded from the records before the request.
func (r *suiteRunner) busy(t *testing.T) {
	sess := r.open(t, Long)

	suiteWantAccepted(t, "first Prompt", r.doPrompt(t, sess, r.target.Prompt(Long)))

	second := r.doPrompt(t, sess, r.target.Prompt(Quick))
	suiteWantRejected(t, "Prompt while a turn is running", second, oarlock.Busy)
	req, _ := r.recorded(t, sess, second, oarlock.ActionPrompt)

	records := sess.Records(oarlock.Cursor{})
	if st := suiteStatusOf(suiteRecordsBefore(records, req.Seq)); st.Kind != fold.StatusRunning {
		t.Fatalf("busy was answered but the records before the request fold to status %s: busy must agree with the status", st.Kind)
	}
	if st := suiteStatusOf(records); st.Kind != fold.StatusRunning {
		t.Fatalf("status after a busy rejection = %s, want the first turn still running", st.Kind)
	}

	suiteWantAccepted(t, "Abort of the running turn", r.doAbort(t, sess))
	records = r.waitFor(t, sess, "the aborted turn to end and the status to return to idle", suiteTurnsEnded(1))

	if got := suiteTurnOutcomes(records); len(got) != 1 || got[0].Kind != oarlock.OutcomeAborted {
		t.Fatalf("turn outcomes = %v, want exactly [aborted]: the busy prompt must not have opened a turn (%s)",
			suiteOutcomeKinds(got), suiteDescribe(records))
	}
	suiteAssertDense(t, records)
}

// abort of a running turn: accepted, the runtime ends the turn aborted, and a
// later abort is the normal race: rejected no_active_turn.
func (r *suiteRunner) abort(t *testing.T) {
	sess := r.open(t, Long)

	suiteWantAccepted(t, "Prompt", r.doPrompt(t, sess, r.target.Prompt(Long)))
	abort := r.doAbort(t, sess)
	suiteWantAccepted(t, "Abort of the running turn", abort)
	r.recorded(t, sess, abort, oarlock.ActionAbort)

	records := r.waitFor(t, sess, "the aborted turn to end and the status to return to idle", suiteTurnsEnded(1))
	if got := suiteTurnOutcomes(records); len(got) != 1 || got[0].Kind != oarlock.OutcomeAborted {
		t.Fatalf("turn outcomes = %v, want exactly [aborted] (%s)", suiteOutcomeKinds(got), suiteDescribe(records))
	}

	again := r.doAbort(t, sess)
	suiteWantRejected(t, "Abort after the turn ended", again, oarlock.NoActiveTurn)
	r.recorded(t, sess, again, oarlock.ActionAbort)

	records = sess.Records(oarlock.Cursor{})
	if got := suiteTurnOutcomes(records); len(got) != 1 || got[0].Kind != oarlock.OutcomeAborted {
		t.Fatalf("a late abort changed the turn outcomes to %v, want [aborted] unchanged", suiteOutcomeKinds(got))
	}
	suiteAssertDense(t, records)
}

// abort with nothing running: rejected no_active_turn, and the request is still
// in the stream.
func (r *suiteRunner) abortIdle(t *testing.T) {
	sess := r.open(t)

	out := r.doAbort(t, sess)
	suiteWantRejected(t, "Abort on an idle session", out, oarlock.NoActiveTurn)
	r.recorded(t, sess, out, oarlock.ActionAbort)

	records := sess.Records(oarlock.Cursor{})
	if st := suiteStatusOf(records); st.Kind != fold.StatusIdle {
		t.Fatalf("status after a rejected abort = %s, want idle", st.Kind)
	}
	if n := suiteCountEvents(records, oarlock.EventTurnEnded); n != 0 {
		t.Fatalf("a rejected abort produced %d turn_ended", n)
	}
	suiteAssertDense(t, records)
}

// Empty text without images is refused unsupported before the runtime is asked,
// and the refused request is still recorded.
func (r *suiteRunner) emptyInput(t *testing.T) {
	sess := r.open(t)

	prompt := r.doPrompt(t, sess, "")
	suiteWantRejected(t, `Prompt("")`, prompt, oarlock.Unsupported)
	r.recorded(t, sess, prompt, oarlock.ActionPrompt)

	queue := r.doQueue(t, sess, "", "")
	suiteWantRejected(t, `Queue("")`, queue, oarlock.Unsupported)
	r.recorded(t, sess, queue, oarlock.ActionQueue)

	records := sess.Records(oarlock.Cursor{})
	if st := suiteStatusOf(records); st.Kind != fold.StatusIdle {
		t.Fatalf("status after refused empty inputs = %s, want idle (the turn never began)", st.Kind)
	}
	if n := suiteCountEvents(records, oarlock.EventTurnEnded) + suiteCountEvents(records, oarlock.EventTurnActive); n != 0 {
		t.Fatalf("refused empty inputs produced %d turn events", n)
	}
	suiteAssertDense(t, records)
}

// Queued input is held while a turn runs and then runs as a turn of its own.
func (r *suiteRunner) queue(t *testing.T) {
	sess := r.open(t, Long, Quick)

	suiteWantAccepted(t, "Prompt", r.doPrompt(t, sess, r.target.Prompt(Long)))
	queued := r.doQueue(t, sess, r.target.Prompt(Quick), "suite-queued-input")
	suiteWantAccepted(t, "Queue while a turn is running", queued)
	r.recorded(t, sess, queued, oarlock.ActionQueue)
	if st := suiteStatusOf(sess.Records(oarlock.Cursor{})); st.Kind != fold.StatusRunning {
		t.Fatalf("status after Queue = %s, want the first turn still running", st.Kind)
	}

	suiteWantAccepted(t, "Abort of the running turn", r.doAbort(t, sess))
	records := r.waitFor(t, sess, "the queued input to run as its own turn and the status to return to idle", suiteTurnsEnded(2))

	got := suiteTurnOutcomes(records)
	if len(got) != 2 || got[0].Kind != oarlock.OutcomeAborted || got[1].Kind != oarlock.OutcomeCompleted {
		t.Fatalf("turn outcomes = %v, want [aborted completed]: the queued input is a turn of its own (%s)",
			suiteOutcomeKinds(got), suiteDescribe(records))
	}
	suiteAssertDense(t, records)
}

// Dispose is idempotent, ends the stream with one exited response that answers
// the one dispose request, and every later control is rejected.
func (r *suiteRunner) dispose(t *testing.T) {
	sess := r.open(t)

	r.doDispose(t, sess, "first Dispose")
	r.doDispose(t, sess, "second Dispose")

	for _, c := range []struct {
		what string
		out  oarlock.ControlOutcome
	}{
		{"Prompt after Dispose", r.doPrompt(t, sess, r.target.Prompt(Quick))},
		{"Abort after Dispose", r.doAbort(t, sess)},
		{"Queue after Dispose", r.doQueue(t, sess, r.target.Prompt(Quick), "")},
	} {
		suiteWantRejected(t, c.what, c.out, oarlock.RuntimeExited, oarlock.Disposed)
	}

	records := sess.Records(oarlock.Cursor{})
	disposeID := r.checkDisposeRecords(t, records)
	if st := suiteStatusOf(records); st.Kind != fold.StatusExited {
		t.Fatalf("status after Dispose = %s, want exited", st.Kind)
	}
	suiteAssertDense(t, records)

	r.doDispose(t, sess, "Dispose after the rejected controls")
	records = sess.Records(oarlock.Cursor{})
	if again := r.checkDisposeRecords(t, records); again != disposeID {
		t.Fatalf("a later Dispose changed the dispose request from %q to %q", disposeID, again)
	}
	suiteAssertDense(t, records)
}

// Dispose while a turn runs releases the session without inventing a turn end.
func (r *suiteRunner) disposeMidTurn(t *testing.T) {
	sess := r.open(t, Long)

	prompt := r.doPrompt(t, sess, r.target.Prompt(Long))
	suiteWantAccepted(t, "Prompt", prompt)
	// Mid-turn for real: the runtime has said something about the turn.
	r.waitFor(t, sess, "the turn to be under way", func(rs []oarlock.Record) bool {
		for _, rec := range rs {
			if rec.Kind == oarlock.KindFrame && rec.Seq > prompt.Seq && len(rec.Events) > 0 {
				return true
			}
		}
		return false
	})

	r.doDispose(t, sess, "Dispose during a running turn")

	records := sess.Records(oarlock.Cursor{})
	if n := suiteCountEvents(records, oarlock.EventTurnEnded); n != 0 {
		t.Fatalf("Dispose mid-turn produced %d turn_ended; the stream must not invent one (%s)", n, suiteDescribe(records))
	}
	if st := suiteStatusOf(records); st.Kind != fold.StatusExited {
		t.Fatalf("status after Dispose mid-turn = %s, want exited", st.Kind)
	}
	r.checkDisposeRecords(t, records)
	suiteAssertDense(t, records)
}

// Events(nil, fn) is live only, Events(&cursor, fn) replays then goes live, both
// agree with Records, and unsubscribe stops delivery.
func (r *suiteRunner) eventsReplay(t *testing.T) {
	sess := r.open(t, Quick)
	before := uint64(len(sess.Records(oarlock.Cursor{})))

	var live, replay, late, control suiteTap
	unsubLive := suiteOnce(sess.Events(nil, live.add))
	unsubReplay := suiteOnce(sess.Events(&oarlock.Cursor{}, replay.add))
	t.Cleanup(unsubLive)
	t.Cleanup(unsubReplay)

	prompt := r.doPrompt(t, sess, r.target.Prompt(Quick))
	suiteWantAccepted(t, "Prompt", prompt)
	records := r.waitFor(t, sess, "the turn to end and the status to return to idle", suiteTurnsEnded(1))
	suiteAssertDense(t, records)
	last := records[len(records)-1].Seq
	want := suiteSeqsOf(records)

	r.waitTap(t, sess, "Events(&Cursor{})", &replay, last)
	if got := suiteUpTo(replay.snapshot(), last); !suiteEqualSeqs(got, want) {
		t.Fatalf("Events(&Cursor{}) delivered Seq %v, Records holds %v", got, want)
	}

	r.waitTap(t, sess, "Events(nil)", &live, last)
	got := suiteUpTo(live.snapshot(), last)
	switch {
	case len(got) == 0:
		t.Fatalf("Events(nil) delivered nothing up to Seq %d", last)
	case got[0] <= before:
		t.Fatalf("Events(nil) replayed history: first Seq %d, but %d records existed before it was registered", got[0], before)
	case got[0] > prompt.Seq:
		t.Fatalf("Events(nil) missed records: first Seq %d, the prompt request is Seq %d (delivered %v)", got[0], prompt.Seq, got)
	case !suiteEqualSeqs(got, want[got[0]-1:]):
		t.Fatalf("Events(nil) delivered Seq %v, want the tail of Records %v", got, want[got[0]-1:])
	}

	// A cursor in the middle replays exactly what follows it.
	unsubLate := suiteOnce(sess.Events(&oarlock.Cursor{AfterSeq: prompt.Seq}, late.add))
	t.Cleanup(unsubLate)
	r.waitTap(t, sess, "Events(&Cursor{AfterSeq})", &late, last)
	if got := suiteUpTo(late.snapshot(), last); !suiteEqualSeqs(got, want[prompt.Seq:]) {
		t.Fatalf("Events(&Cursor{AfterSeq: %d}) delivered Seq %v, want %v", prompt.Seq, got, want[prompt.Seq:])
	}

	// Unsubscribe. Only a callback that is already running may finish after the
	// unsubscribe returns, so let it, then count. An abort on the idle session
	// is recorded (and rejected): it must reach a tap that is still subscribed
	// and none of the three that are not.
	unsubLive()
	unsubReplay()
	unsubLate()
	time.Sleep(suiteSettleTime)
	nLive, nReplay, nLate := len(live.snapshot()), len(replay.snapshot()), len(late.snapshot())
	unsubControl := suiteOnce(sess.Events(nil, control.add))
	t.Cleanup(unsubControl)

	suiteWantRejected(t, "Abort on the idle session", r.doAbort(t, sess), oarlock.NoActiveTurn)
	after := sess.Records(oarlock.Cursor{})
	r.waitTap(t, sess, "the tap that stayed subscribed", &control, after[len(after)-1].Seq)
	time.Sleep(suiteSettleTime)
	if n := len(live.snapshot()); n != nLive {
		t.Fatalf("Events(nil) kept delivering after unsubscribe: %d records, was %d", n, nLive)
	}
	if n := len(replay.snapshot()); n != nReplay {
		t.Fatalf("Events(&Cursor{}) kept delivering after unsubscribe: %d records, was %d", n, nReplay)
	}
	if n := len(late.snapshot()); n != nLate {
		t.Fatalf("Events(&Cursor{AfterSeq}) kept delivering after unsubscribe: %d records, was %d", n, nLate)
	}
}

// ---- controls -------------------------------------------------------------------

func (r *suiteRunner) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), r.timeout)
}

// The do* helpers fail the sub-test on an operational error: a normal "no" is
// an outcome, never an error (docs/spec/errors.md).

func (r *suiteRunner) doPrompt(t *testing.T, sess oarlock.Session, text string) oarlock.ControlOutcome {
	t.Helper()
	ctx, cancel := r.ctx()
	defer cancel()
	out, err := sess.Prompt(ctx, text, oarlock.PromptOptions{})
	if err != nil {
		t.Fatalf("Prompt(%q) returned an error: %v", text, err)
	}
	return out
}

func (r *suiteRunner) doQueue(t *testing.T, sess oarlock.Session, text, inputID string) oarlock.ControlOutcome {
	t.Helper()
	ctx, cancel := r.ctx()
	defer cancel()
	out, err := sess.Queue(ctx, text, oarlock.InputOptions{InputID: inputID})
	if err != nil {
		t.Fatalf("Queue(%q) returned an error: %v", text, err)
	}
	return out
}

func (r *suiteRunner) doAbort(t *testing.T, sess oarlock.Session) oarlock.ControlOutcome {
	t.Helper()
	ctx, cancel := r.ctx()
	defer cancel()
	out, err := sess.Abort(ctx)
	if err != nil {
		t.Fatalf("Abort returned an error: %v", err)
	}
	return out
}

func (r *suiteRunner) doDispose(t *testing.T, sess oarlock.Session, label string) {
	t.Helper()
	ctx, cancel := r.ctx()
	defer cancel()
	if err := sess.Dispose(ctx); err != nil {
		t.Fatalf("%s = %v, want nil", label, err)
	}
}

func suiteWantAccepted(t *testing.T, what string, out oarlock.ControlOutcome) {
	t.Helper()
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("%s = %s (code %q, reason %q), want accepted", what, out.Kind, out.Code, out.Reason)
	}
}

func suiteWantRejected(t *testing.T, what string, out oarlock.ControlOutcome, codes ...oarlock.Code) {
	t.Helper()
	if out.Kind != oarlock.ControlRejected {
		t.Fatalf("%s = %s, want rejected %v", what, out.Kind, codes)
	}
	for _, c := range codes {
		if out.Code == c {
			return
		}
	}
	t.Fatalf("%s = rejected %q (reason %q), want rejected %v", what, out.Code, out.Reason, codes)
}

// recorded finds the two records of a control in the stream and checks they are
// what the outcome says: a toRuntime request of that action, then, later in the
// stream, a response carrying the same answer.
func (r *suiteRunner) recorded(t *testing.T, sess oarlock.Session, out oarlock.ControlOutcome, action oarlock.Action) (req, resp oarlock.Record) {
	t.Helper()
	if out.RequestID == "" {
		t.Fatalf("%s outcome carries no RequestID: %+v", action, out)
	}
	var haveReq, haveResp bool
	for _, rec := range sess.Records(oarlock.Cursor{}) {
		switch {
		case rec.Kind == oarlock.KindRequest && rec.ID == out.RequestID && !haveReq:
			req, haveReq = rec, true
		case rec.Kind == oarlock.KindResponse && rec.RequestID == out.RequestID && !haveResp:
			resp, haveResp = rec, true
		}
	}
	switch {
	case !haveReq:
		t.Fatalf("the %s request %q is not in the stream", action, out.RequestID)
	case req.Request == nil || req.Request.Action != action || req.Direction != oarlock.ToRuntime:
		t.Fatalf("record %q is %+v, want a toRuntime %s request", out.RequestID, req.Request, action)
	case !haveResp || resp.Response == nil:
		t.Fatalf("the %s request %q (Seq %d) has no response in the stream", action, out.RequestID, req.Seq)
	case req.Seq >= resp.Seq:
		t.Fatalf("the %s response (Seq %d) does not follow its request (Seq %d)", action, resp.Seq, req.Seq)
	case out.Seq != req.Seq:
		t.Fatalf("outcome Seq = %d, want the request's Seq %d", out.Seq, req.Seq)
	}
	switch out.Kind {
	case oarlock.ControlAccepted:
		if resp.Response.Kind != oarlock.ResponseAccepted {
			t.Fatalf("outcome is accepted but the %s response record is %s", action, resp.Response.Kind)
		}
	case oarlock.ControlRejected:
		if resp.Response.Kind != oarlock.ResponseRejected || resp.Response.Code != out.Code {
			t.Fatalf("outcome is rejected %q but the %s response record is %s/%q", out.Code, action, resp.Response.Kind, resp.Response.Code)
		}
	}
	return req, resp
}

// checkDisposeRecords asserts the dispose bookkeeping of a stream and returns
// the id of its one dispose request: one request, answered accepted, and one
// exited response that answers it.
func (r *suiteRunner) checkDisposeRecords(t *testing.T, records []oarlock.Record) string {
	t.Helper()
	var requests, accepted, exited int
	var disposeID, exitedFor string
	for _, rec := range records {
		if rec.Kind == oarlock.KindRequest && rec.Request != nil && rec.Request.Action == oarlock.ActionDispose {
			requests++
			disposeID = rec.ID
		}
	}
	for _, rec := range records {
		if rec.Kind != oarlock.KindResponse || rec.Response == nil {
			continue
		}
		switch rec.Response.Kind {
		case oarlock.ResponseAccepted:
			if disposeID != "" && rec.RequestID == disposeID {
				accepted++
			}
		case oarlock.ResponseExited:
			exited++
			exitedFor = rec.RequestID
		}
	}
	if requests != 1 || exited != 1 {
		t.Fatalf("dispose requests = %d, exited responses = %d, want 1 and 1 (%s)", requests, exited, suiteDescribe(records))
	}
	if accepted != 1 {
		t.Fatalf("the dispose request %q has %d accepted responses, want 1", disposeID, accepted)
	}
	if exitedFor != disposeID {
		t.Fatalf("the exited response answers request %q, want the dispose request %q", exitedFor, disposeID)
	}
	return disposeID
}

// ---- waiting --------------------------------------------------------------------

// waitFor polls the stream every 2ms until cond holds over the records, and
// fails the sub-test with the last status when the timeout passes.
func (r *suiteRunner) waitFor(t *testing.T, sess oarlock.Session, what string, cond func([]oarlock.Record) bool) []oarlock.Record {
	t.Helper()
	deadline := time.Now().Add(r.timeout)
	for {
		records := sess.Records(oarlock.Cursor{})
		if cond(records) {
			return records
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: timed out after %v waiting for %s; last status: %s", r.label(), r.timeout, what, suiteDescribe(records))
		}
		time.Sleep(suitePollInterval)
	}
}

// waitTap waits until an Events callback has delivered the record with Seq last.
func (r *suiteRunner) waitTap(t *testing.T, sess oarlock.Session, what string, p *suiteTap, last uint64) {
	t.Helper()
	deadline := time.Now().Add(r.timeout)
	for p.lastSeq() < last {
		if time.Now().After(deadline) {
			t.Fatalf("%s: timed out after %v waiting for %s to deliver Seq %d; delivered %v; last status: %s",
				r.label(), r.timeout, what, last, p.snapshot(), suiteDescribe(sess.Records(oarlock.Cursor{})))
		}
		time.Sleep(suitePollInterval)
	}
}

// suiteTurnsEnded holds when the status is idle and at least n turns have ended;
// the idle check keeps "n turns ended" from being true in the middle of the
// next turn.
func suiteTurnsEnded(n int) func([]oarlock.Record) bool {
	return func(records []oarlock.Record) bool {
		return suiteStatusOf(records).Kind == fold.StatusIdle && suiteCountEvents(records, oarlock.EventTurnEnded) >= n
	}
}

// ---- reading the stream -----------------------------------------------------------

func suiteStatusOf(records []oarlock.Record) fold.Status { return fold.StatusOf(records).Value }

func suiteCountEvents(records []oarlock.Record, kind oarlock.EventKind) (n int) {
	for _, rec := range records {
		for _, ev := range rec.Events {
			if ev.Kind == kind {
				n++
			}
		}
	}
	return n
}

// suiteTurnOutcomes are the outcomes of every turn_ended event, in stream order.
// A turn_ended without an outcome reads as the zero outcome, which no check
// accepts.
func suiteTurnOutcomes(records []oarlock.Record) (out []oarlock.TurnOutcome) {
	for _, rec := range records {
		for _, ev := range rec.Events {
			if ev.Kind != oarlock.EventTurnEnded {
				continue
			}
			if ev.Outcome == nil {
				out = append(out, oarlock.TurnOutcome{})
				continue
			}
			out = append(out, *ev.Outcome)
		}
	}
	return out
}

func suiteOutcomeKinds(outcomes []oarlock.TurnOutcome) []oarlock.OutcomeKind {
	kinds := make([]oarlock.OutcomeKind, len(outcomes))
	for i, o := range outcomes {
		kinds[i] = o.Kind
	}
	return kinds
}

// suiteRecordsBefore are the records that precede seq: what the stream said when
// the request at seq was recorded.
func suiteRecordsBefore(records []oarlock.Record, seq uint64) []oarlock.Record {
	n := 0
	for n < len(records) && records[n].Seq < seq {
		n++
	}
	return records[:n]
}

func suiteAssertDense(t *testing.T, records []oarlock.Record) {
	t.Helper()
	for i, rec := range records {
		if rec.Seq != uint64(i+1) {
			t.Fatalf("record %d has Seq %d: the stream must be dense from 1", i, rec.Seq)
		}
	}
}

// suiteDescribe is the one-line state of a stream for a failure message.
func suiteDescribe(records []oarlock.Record) string {
	st := suiteStatusOf(records)
	var b strings.Builder
	fmt.Fprintf(&b, "status=%s", st.Kind)
	if st.Kind == fold.StatusRunning {
		fmt.Fprintf(&b, "/%s", st.Phase.Kind)
	}
	fmt.Fprintf(&b, " records=%d turn_ended=%d", len(records), suiteCountEvents(records, oarlock.EventTurnEnded))
	if n := len(records); n > 0 {
		b.WriteString(" last=")
		b.WriteString(suiteBrief(records[n-1]))
	}
	return b.String()
}

func suiteBrief(rec oarlock.Record) string {
	switch rec.Kind {
	case oarlock.KindRequest:
		if rec.Request != nil {
			return fmt.Sprintf("#%d request(%s)", rec.Seq, rec.Request.Action)
		}
	case oarlock.KindResponse:
		if rec.Response != nil {
			return fmt.Sprintf("#%d response(%s %s)", rec.Seq, rec.Response.Kind, rec.Response.Code)
		}
	case oarlock.KindFrame:
		events := make([]string, len(rec.Events))
		for i, ev := range rec.Events {
			events[i] = string(ev.Kind)
		}
		return fmt.Sprintf("#%d frame%v", rec.Seq, events)
	}
	return fmt.Sprintf("#%d %s", rec.Seq, rec.Kind)
}

// ---- Events taps ----------------------------------------------------------------------

// suiteTap collects the Seq of every record an Events callback delivered. The
// callback runs on the goroutine that appends, so it only appends under a lock.
type suiteTap struct {
	mu   sync.Mutex
	seqs []uint64
}

func (p *suiteTap) add(rec oarlock.Record) {
	p.mu.Lock()
	p.seqs = append(p.seqs, rec.Seq)
	p.mu.Unlock()
}

func (p *suiteTap) snapshot() []uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]uint64(nil), p.seqs...)
}

func (p *suiteTap) lastSeq() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.seqs) == 0 {
		return 0
	}
	return p.seqs[len(p.seqs)-1]
}

// suiteOnce makes an unsubscribe safe to call from both the test and its cleanup.
func suiteOnce(f func()) func() {
	var o sync.Once
	return func() { o.Do(f) }
}

func suiteSeqsOf(records []oarlock.Record) []uint64 {
	seqs := make([]uint64, len(records))
	for i, rec := range records {
		seqs[i] = rec.Seq
	}
	return seqs
}

// suiteUpTo keeps the Seqs that are at most last: records appended after a
// snapshot of the stream are not a disagreement with it.
func suiteUpTo(seqs []uint64, last uint64) []uint64 {
	n := 0
	for n < len(seqs) && seqs[n] <= last {
		n++
	}
	return seqs[:n]
}

func suiteEqualSeqs(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
