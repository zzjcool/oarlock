package oarlock_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
)

// fakeStream is a fake session's record stream: the retained records, the live
// observers, and the ONE mutex that guards both.
//
// Its Events is the kernel's ObserveFrom seam (internal/kernel): the replay of
// the retained records after the cursor and the registration of the live
// observer happen in the same critical section, so the hand-over has no gap and
// no duplicate. The callback is invoked while that mutex is held, exactly as the
// kernel dispatches inline on the appending goroutine, so it must never block.
type fakeStream struct {
	mu      sync.Mutex
	records []oarlock.Record
	// subs holds the live observers in registration order; unsubscribing nils
	// its own slot so a later record never reaches it.
	subs []func(oarlock.Record)
}

// emitLocked stamps the next Seq, retains rec and dispatches it to every live
// observer. The caller holds mu.
func (f *fakeStream) emitLocked(rec oarlock.Record) {
	rec.Seq = uint64(len(f.records) + 1)
	f.records = append(f.records, rec)
	for _, fn := range f.subs {
		if fn != nil {
			fn(rec)
		}
	}
}

func (f *fakeStream) emit(rec oarlock.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emitLocked(rec)
}

// promptRequest retains a prompt request and returns it. sessionID is the id the
// runtime records the request under: the kernel's session, or the empty id on a
// fresh Cursor chat where the chat id is not known yet.
func (f *fakeStream) promptRequest(sessionID, input string) oarlock.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	req := oarlock.Record{
		// emitLocked stamps its own copy, so the caller's request carries the
		// same position.
		Seq:       uint64(len(f.records) + 1),
		Kind:      oarlock.KindRequest,
		SessionID: sessionID,
		ID:        "r1",
		Direction: oarlock.ToRuntime,
		Request:   &oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: input},
	}
	f.emitLocked(req)
	return req
}

// accepted is the ControlOutcome of a prompt the runtime accepted.
func accepted(req oarlock.Record) oarlock.ControlOutcome {
	return oarlock.ControlOutcome{Kind: oarlock.ControlAccepted, Seq: req.Seq, RequestID: "r1", Request: req}
}

// Events replays and registers in one critical section, like ObserveFrom. The
// cursor filter here is the fake's simplification: the kernel's cursor names one
// stream instance, while these fakes keep every session of the fixture in one
// slice, so they filter per record.
func (f *fakeStream) Events(cursor *oarlock.Cursor, fn func(oarlock.Record)) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cursor != nil {
		for _, rec := range f.records {
			if rec.Seq > cursor.AfterSeq && (cursor.SessionID == "" || cursor.SessionID == rec.SessionID) {
				fn(rec)
			}
		}
	}
	f.subs = append(f.subs, fn)
	id := len(f.subs) - 1
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.subs[id] = nil
	}
}

func (f *fakeStream) Records(cursor oarlock.Cursor) []oarlock.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []oarlock.Record
	for _, rec := range f.records {
		if rec.Seq > cursor.AfterSeq && (cursor.SessionID == "" || cursor.SessionID == rec.SessionID) {
			out = append(out, rec)
		}
	}
	return out
}

func (f *fakeStream) lastSeq() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return uint64(len(f.records))
}

// count is how many retained records match pred.
func (f *fakeStream) count(pred func(oarlock.Record) bool) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, rec := range f.records {
		if pred(rec) {
			n++
		}
	}
	return n
}

// emitLater retains a frame after d, from its own goroutine: the late record a
// wait that already started still has to see.
func emitLater(f *fakeStream, d time.Duration, sessionID string, events ...oarlock.Event) {
	go func() {
		time.Sleep(d)
		f.emit(oarlock.Record{Kind: oarlock.KindFrame, SessionID: sessionID, Events: events})
	}()
}

type fakeWaitSession struct {
	st      fakeStream
	handled bool
	reject  *oarlock.ControlOutcome
	timeout bool
	// exit appends the process-exit response oarlock's kernel records: the one
	// outcome the runtime cannot state itself. It lands after the turn's frames
	// in the same batch, so a waiter that drains before deciding sees both.
	exit   bool
	aborts int
}

func (s *fakeWaitSession) append(rec oarlock.Record) {
	rec.Kind = oarlock.KindFrame
	rec.SessionID = "s"
	s.st.emit(rec)
}

// exited retains the process-exit response the kernel appends when the runtime
// dies: a response record, never a frame.
func (s *fakeWaitSession) exited() {
	s.st.emit(oarlock.Record{
		Kind:      oarlock.KindResponse,
		SessionID: "s",
		RequestID: "r1",
		Response:  &oarlock.ResponseBody{Kind: oarlock.ResponseExited},
	})
}

func (s *fakeWaitSession) Prompt(_ context.Context, input string, _ oarlock.PromptOptions) (oarlock.ControlOutcome, error) {
	if s.reject != nil {
		return *s.reject, nil
	}
	req := s.st.promptRequest("s", input)
	switch {
	case s.handled:
		s.append(oarlock.Record{Events: []oarlock.Event{{Kind: oarlock.EventInputHandled, RequestID: "r1"}}})
	case s.timeout:
		s.append(oarlock.Record{Events: []oarlock.Event{{Kind: oarlock.EventTurnActive}}})
	default:
		s.append(oarlock.Record{Events: []oarlock.Event{{Kind: oarlock.EventTextDelta, Text: "hello ", MessageID: "m"}}})
		s.append(oarlock.Record{Events: []oarlock.Event{{Kind: oarlock.EventTextDelta, Text: "world", MessageID: "m"}}})
		s.append(oarlock.Record{Events: []oarlock.Event{{Kind: oarlock.EventTurnEnded, Outcome: &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted}}}})
	}
	if s.exit {
		s.exited()
	}
	return accepted(req), nil
}

func (s *fakeWaitSession) Queue(context.Context, string, oarlock.InputOptions) (oarlock.ControlOutcome, error) {
	return oarlock.ControlOutcome{}, nil
}

func (s *fakeWaitSession) Abort(context.Context) (oarlock.ControlOutcome, error) {
	s.aborts++
	s.append(oarlock.Record{Events: []oarlock.Event{{Kind: oarlock.EventTurnEnded, Outcome: &oarlock.TurnOutcome{Kind: oarlock.OutcomeAborted}}}})
	return oarlock.ControlOutcome{Kind: oarlock.ControlAccepted}, nil
}

func (s *fakeWaitSession) Dispose(context.Context) error { return nil }

func (s *fakeWaitSession) Records(c oarlock.Cursor) []oarlock.Record { return s.st.Records(c) }

func (s *fakeWaitSession) Events(c *oarlock.Cursor, fn func(oarlock.Record)) func() {
	return s.st.Events(c, fn)
}

func (s *fakeWaitSession) Capabilities() oarlock.Capabilities { return oarlock.Capabilities{} }

func TestPromptAndWait_CompletedCollectsTextAndTheRuntimeOutcome(t *testing.T) {
	s := &fakeWaitSession{}
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != oarlock.RunCompleted || run.Text != "hello world" || run.Turn == nil || run.Turn.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("run = %+v", run)
	}
	if run.Seq != 4 {
		t.Fatalf("seq = %d, want the last frame (4)", run.Seq)
	}
}

func TestPromptAndWait_RejectedKeepsTheBranchWord(t *testing.T) {
	s := &fakeWaitSession{reject: &oarlock.ControlOutcome{Kind: oarlock.ControlRejected, Code: oarlock.Busy, Reason: "busy", Seq: 1}}
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != oarlock.RunRejected || run.Code != oarlock.Busy || run.Reason != "busy" {
		t.Fatalf("run = %+v", run)
	}
}

func TestPromptAndWait_HandledDoesNotInventATurn(t *testing.T) {
	s := &fakeWaitSession{handled: true}
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != oarlock.RunCompleted || run.Turn != nil {
		t.Fatalf("run = %+v, want completed with no turn", run)
	}
}

// isExitedTurn reports whether turn is exactly what PromptAndWait summarizes
// from a process-exit record. TurnOutcome holds a slice, so it is not
// comparable as a whole.
func isExitedTurn(turn *oarlock.TurnOutcome) bool {
	return turn != nil &&
		turn.Kind == oarlock.OutcomeFailed &&
		turn.Failure == oarlock.FailureRuntimeExited &&
		turn.Reason == "runtime exited"
}

// TestPromptAndWait_TurnEndBeatsExitedInTheSameBatch pins the decision priority
// on one drained batch: the batch carries both the runtime's turn end and the
// process-exit response, and the wait drains before it decides, so the priority
// is decided()'s order rather than the record order. The turn end is the
// runtime's own end; the exit must not downgrade it to RunFailed.
func TestPromptAndWait_TurnEndBeatsExitedInTheSameBatch(t *testing.T) {
	s := &fakeWaitSession{exit: true}
	// The deadline only bounds a regression: both records arrive before the wait.
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi", oarlock.WithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != oarlock.RunCompleted {
		t.Fatalf("run = %+v, want completed: the turn end is the runtime's own end", run)
	}
	if run.Turn == nil || run.Turn.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("run.Turn = %+v, want the runtime's own completed turn", run.Turn)
	}
	if run.Text != "hello world" {
		t.Fatalf("run.Text = %q, want the turn's text", run.Text)
	}
	if s.aborts != 0 {
		t.Fatalf("Abort called %d times, want 0", s.aborts)
	}
}

// TestPromptAndWait_ExitedWithoutATurnEndIsFailed is the other half of that
// priority: no turn end in the batch, so the process exit is the outcome that
// is left. RunFailed carries the runtime-exited turn PromptAndWait summarizes
// from the exit record.
func TestPromptAndWait_ExitedWithoutATurnEndIsFailed(t *testing.T) {
	s := &fakeWaitSession{timeout: true, exit: true} // a turn active, never ended
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi", oarlock.WithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != oarlock.RunFailed {
		t.Fatalf("run = %+v, want failed", run)
	}
	if !isExitedTurn(run.Turn) {
		t.Fatalf("run.Turn = %+v, want the runtime-exited turn", run.Turn)
	}
	if s.aborts != 0 {
		t.Fatalf("Abort called %d times, want 0", s.aborts)
	}
}

// freshChatSession is a fresh Cursor chat (adapters/cursor): the chat id is not
// known when the prompt goes in, so the prompt request is recorded under the
// kernel's empty session id and every later frame is stamped with the id the
// runtime reported. A child session's frames carry their own id.
type freshChatSession struct {
	st fakeStream
	// quiet suppresses the turn's frames until Abort, so the chat id is only
	// learned while the abort is already in flight.
	quiet bool
}

const freshChatID = "chat-1"

func (s *freshChatSession) append(sessionID string, agentPath []string, events ...oarlock.Event) {
	s.st.emit(oarlock.Record{
		Kind:      oarlock.KindFrame,
		SessionID: sessionID,
		AgentPath: agentPath,
		Events:    events,
	})
}

func (s *freshChatSession) Prompt(_ context.Context, input string, _ oarlock.PromptOptions) (oarlock.ControlOutcome, error) {
	req := s.st.promptRequest("", input) // a fresh chat: no chat id yet
	if s.quiet {
		return accepted(req), nil
	}
	s.append(freshChatID, nil, oarlock.Event{Kind: oarlock.EventTurnActive})
	s.append(freshChatID, nil, oarlock.Event{Kind: oarlock.EventTextDelta, Text: "hello ", MessageID: "m"})
	// A child session's frame and a sub-agent's frame carry text the root
	// run must never see.
	s.append("child", nil, oarlock.Event{Kind: oarlock.EventTextDelta, Text: "child ", MessageID: "c"})
	s.append(freshChatID, []string{"sub"}, oarlock.Event{Kind: oarlock.EventTextDelta, Text: "sub ", MessageID: "s"})
	s.append(freshChatID, nil, oarlock.Event{Kind: oarlock.EventTextDelta, Text: "world", MessageID: "m"})
	s.append(freshChatID, nil, oarlock.Event{Kind: oarlock.EventTurnEnded, Outcome: &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted}})
	return accepted(req), nil
}

func (s *freshChatSession) Queue(context.Context, string, oarlock.InputOptions) (oarlock.ControlOutcome, error) {
	return oarlock.ControlOutcome{}, nil
}

// Abort ends the turn the way the runtime does: with its own frames, stamped
// with the chat id.
func (s *freshChatSession) Abort(context.Context) (oarlock.ControlOutcome, error) {
	s.append(freshChatID, nil, oarlock.Event{Kind: oarlock.EventTextDelta, Text: "late ", MessageID: "m"})
	s.append(freshChatID, nil, oarlock.Event{Kind: oarlock.EventTurnEnded, Outcome: &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted}})
	return oarlock.ControlOutcome{Kind: oarlock.ControlAccepted}, nil
}

func (s *freshChatSession) Dispose(context.Context) error { return nil }

func (s *freshChatSession) Records(c oarlock.Cursor) []oarlock.Record { return s.st.Records(c) }

func (s *freshChatSession) Events(c *oarlock.Cursor, fn func(oarlock.Record)) func() {
	return s.st.Events(c, fn)
}

func (s *freshChatSession) Capabilities() oarlock.Capabilities { return oarlock.Capabilities{} }

func (s *freshChatSession) lastSeq() uint64 { return s.st.lastSeq() }

// TestPromptAndWait_FreshChatLearnsTheRootSessionID pins the bug the Cursor live
// run exposed: a fresh chat's prompt request has no session id, every later
// frame is stamped with the chat id learned from system/init, and filtering by
// the prompt-time root alone dropped the text and the runtime's turn_ended.
func TestPromptAndWait_FreshChatLearnsTheRootSessionID(t *testing.T) {
	s := &freshChatSession{}
	// The deadline only bounds a regression: the runtime ends the turn at once.
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi", oarlock.WithTimeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != oarlock.RunCompleted {
		t.Fatalf("run = %+v, want completed", run)
	}
	if run.Text != "hello world" {
		t.Fatalf("run.Text = %q, want the root agent's text only", run.Text)
	}
	if run.Turn == nil || run.Turn.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("run.Turn = %+v, want one completed turn", run.Turn)
	}
	// Seq still tracks the last record read, child frames included.
	if run.Seq != s.lastSeq() {
		t.Fatalf("run.Seq = %d, want the last frame (%d)", run.Seq, s.lastSeq())
	}
}

// TestPromptAndWait_FreshChatAbortUsesTheLearnedRoot: the abort wait reads the
// same root, so a runtime that ends the turn with the chat id while the abort
// is in flight still reports its own completion.
func TestPromptAndWait_FreshChatAbortUsesTheLearnedRoot(t *testing.T) {
	s := &freshChatSession{quiet: true}
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi", oarlock.WithTimeout(30*time.Millisecond))
	if err != nil {
		t.Fatalf("a WaitOption deadline is not a caller error: %v", err)
	}
	if run.Outcome != oarlock.RunCompleted || run.Text != "late " {
		t.Fatalf("run = %+v, want the runtime's own completion after the abort", run)
	}
	if run.Turn == nil || run.Turn.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("run.Turn = %+v, want one completed turn", run.Turn)
	}
}

func TestPromptAndWait_TimeoutAbortsAndReportsInterrupted(t *testing.T) {
	s := &fakeWaitSession{timeout: true}
	started := time.Now()
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi", oarlock.WithTimeout(30*time.Millisecond))
	if err != nil {
		t.Fatalf("a WaitOption deadline is not a caller error: %v", err)
	}
	if run.Outcome != oarlock.RunInterrupted || run.Turn == nil || run.Turn.Kind != oarlock.OutcomeAborted {
		t.Fatalf("run = %+v", run)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatalf("timeout took %v", time.Since(started))
	}
}

// asyncWaitSession is the fake for the waits a poll resolved by luck of timing:
// frames that arrive after the wait started, and a runtime that answers Abort
// with its own turn end a little later, or never at all.
type asyncWaitSession struct {
	st fakeStream

	// Prompt retains turn_active, closes started, then emits the turn's text and
	// end turnDelay later (nothing when turnDelay is zero: the turn never ends by
	// itself).
	turnDelay  time.Duration
	promptText string
	promptEnd  *oarlock.TurnOutcome

	// Abort emits abortText/abortEnd, abortDelay later when it is positive.
	abortDelay time.Duration
	abortText  string
	abortEnd   *oarlock.TurnOutcome

	// abortHandled adds an input_handled event for the prompt's own request id to
	// the abort's batch, so the prompt is only learned to be handled after the
	// deadline already fired.
	abortHandled bool
	// abortExit makes Abort answer with the process-exit response the kernel
	// records and no turn frames, like a runtime that is already gone.
	abortExit bool

	// brokenTap: Events registers no observer and returns a noop unsubscribe,
	// like a stream that refused the cursor.
	brokenTap bool

	started chan struct{}
	aborts  int
}

func turnEvents(text string, end *oarlock.TurnOutcome) []oarlock.Event {
	var evs []oarlock.Event
	if text != "" {
		evs = append(evs, oarlock.Event{Kind: oarlock.EventTextDelta, Text: text, MessageID: "m"})
	}
	if end != nil {
		evs = append(evs, oarlock.Event{Kind: oarlock.EventTurnEnded, Outcome: end})
	}
	return evs
}

func (s *asyncWaitSession) Prompt(_ context.Context, input string, _ oarlock.PromptOptions) (oarlock.ControlOutcome, error) {
	req := s.st.promptRequest("s", input)
	s.st.emit(oarlock.Record{Kind: oarlock.KindFrame, SessionID: "s", Events: []oarlock.Event{{Kind: oarlock.EventTurnActive}}})
	if s.started != nil {
		close(s.started)
	}
	if evs := turnEvents(s.promptText, s.promptEnd); len(evs) != 0 {
		emitLater(&s.st, s.turnDelay, "s", evs...)
	}
	return accepted(req), nil
}

func (s *asyncWaitSession) Queue(context.Context, string, oarlock.InputOptions) (oarlock.ControlOutcome, error) {
	return oarlock.ControlOutcome{}, nil
}

func (s *asyncWaitSession) Abort(context.Context) (oarlock.ControlOutcome, error) {
	s.aborts++
	if s.abortExit {
		s.st.emit(oarlock.Record{
			Kind:      oarlock.KindResponse,
			SessionID: "s",
			RequestID: "r1",
			Response:  &oarlock.ResponseBody{Kind: oarlock.ResponseExited},
		})
	}
	evs := turnEvents(s.abortText, s.abortEnd)
	if s.abortHandled {
		evs = append(evs, oarlock.Event{Kind: oarlock.EventInputHandled, RequestID: "r1"})
	}
	if len(evs) != 0 {
		if s.abortDelay > 0 {
			emitLater(&s.st, s.abortDelay, "s", evs...)
		} else {
			s.st.emit(oarlock.Record{Kind: oarlock.KindFrame, SessionID: "s", Events: evs})
		}
	}
	return oarlock.ControlOutcome{Kind: oarlock.ControlAccepted}, nil
}

func (s *asyncWaitSession) Dispose(context.Context) error { return nil }

func (s *asyncWaitSession) Records(c oarlock.Cursor) []oarlock.Record { return s.st.Records(c) }

func (s *asyncWaitSession) Events(c *oarlock.Cursor, fn func(oarlock.Record)) func() {
	if s.brokenTap {
		return func() {}
	}
	return s.st.Events(c, fn)
}

func (s *asyncWaitSession) Capabilities() oarlock.Capabilities { return oarlock.Capabilities{} }

// turnEnds counts the turn_ended events in the retained stream: the wait must
// never add one of its own.
func (s *asyncWaitSession) turnEnds() int {
	return s.st.count(func(rec oarlock.Record) bool {
		for _, ev := range rec.Events {
			if ev.Kind == oarlock.EventTurnEnded {
				return true
			}
		}
		return false
	})
}

// promptRequests counts the prompt requests in the retained stream: a second
// turn would show up here.
func (s *asyncWaitSession) promptRequests() int {
	return s.st.count(func(rec oarlock.Record) bool {
		return rec.Kind == oarlock.KindRequest && rec.Request != nil && rec.Request.Action == oarlock.ActionPrompt
	})
}

// TestPromptAndWait_CallerCancelAbortsOnce: a cancelled caller context aborts
// the turn, waits for the runtime's own end, and reports the caller's error with
// RunInterrupted. The stream keeps exactly one turn_ended and exactly one prompt
// request: the wait neither invents nor repeats a turn.
func TestPromptAndWait_CallerCancelAbortsOnce(t *testing.T) {
	s := &asyncWaitSession{
		started:  make(chan struct{}),
		abortEnd: &oarlock.TurnOutcome{Kind: oarlock.OutcomeAborted},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel only once the turn is active, so the wait is really cancelled
	// mid-turn rather than before the prompt opened one.
	go func() {
		<-s.started
		cancel()
	}()
	run, err := oarlock.PromptAndWait(ctx, s, "hi")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if run.Outcome != oarlock.RunInterrupted {
		t.Fatalf("run = %+v, want interrupted", run)
	}
	if run.Turn == nil || run.Turn.Kind != oarlock.OutcomeAborted {
		t.Fatalf("run.Turn = %+v, want the runtime's own aborted turn", run.Turn)
	}
	if s.aborts != 1 {
		t.Fatalf("Abort called %d times, want 1", s.aborts)
	}
	if n := s.turnEnds(); n != 1 {
		t.Fatalf("turn_ended events = %d, want exactly 1", n)
	}
	if n := s.promptRequests(); n != 1 {
		t.Fatalf("prompt requests = %d, want exactly 1 (no second turn)", n)
	}
}

// TestPromptAndWait_LateCompletedAfterAbortWins: a real completion that raced
// the abort is the runtime's own outcome, so it beats the interruption.
func TestPromptAndWait_LateCompletedAfterAbortWins(t *testing.T) {
	s := &asyncWaitSession{
		started:    make(chan struct{}),
		abortDelay: 20 * time.Millisecond,
		abortText:  "late ",
		abortEnd:   &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted},
	}
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi", oarlock.WithTimeout(30*time.Millisecond))
	if err != nil {
		t.Fatalf("a WaitOption deadline is not a caller error: %v", err)
	}
	if run.Outcome != oarlock.RunCompleted || run.Text != "late " {
		t.Fatalf("run = %+v, want the runtime's own completion after the abort", run)
	}
	if run.Turn == nil || run.Turn.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("run.Turn = %+v, want one completed turn", run.Turn)
	}
	if s.aborts != 1 {
		t.Fatalf("Abort called %d times, want 1", s.aborts)
	}
}

// TestPromptAndWait_LateAbortedAfterAbortIsInterrupted: the runtime's own late
// end is an abort, and an accepted abort never reads as completed.
func TestPromptAndWait_LateAbortedAfterAbortIsInterrupted(t *testing.T) {
	s := &asyncWaitSession{
		started:    make(chan struct{}),
		abortDelay: 20 * time.Millisecond,
		abortEnd:   &oarlock.TurnOutcome{Kind: oarlock.OutcomeAborted},
	}
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi", oarlock.WithTimeout(30*time.Millisecond))
	if err != nil {
		t.Fatalf("a WaitOption deadline is not a caller error: %v", err)
	}
	if run.Outcome != oarlock.RunInterrupted {
		t.Fatalf("run = %+v, want interrupted", run)
	}
	if run.Turn == nil || run.Turn.Kind != oarlock.OutcomeAborted {
		t.Fatalf("run.Turn = %+v, want the runtime's own aborted turn", run.Turn)
	}
	if s.aborts != 1 {
		t.Fatalf("Abort called %d times, want 1", s.aborts)
	}
}

// TestPromptAndWait_HandledDuringTheAbortGraceIsCompleted pins the handled
// priority inside the abort wait: the prompt turns out to be handled only after
// the deadline already fired. A handled prompt never opened a turn, so it is
// completed with no turn even though the wait was interrupted on the way.
func TestPromptAndWait_HandledDuringTheAbortGraceIsCompleted(t *testing.T) {
	s := &asyncWaitSession{
		abortDelay:   10 * time.Millisecond, // well inside the abort grace
		abortHandled: true,
	}
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi", oarlock.WithTimeout(30*time.Millisecond))
	if err != nil {
		t.Fatalf("a WaitOption deadline is not a caller error: %v", err)
	}
	if run.Outcome != oarlock.RunCompleted || run.Turn != nil {
		t.Fatalf("run = %+v, want completed with no turn: a handled prompt opened none", run)
	}
	if run.Text != "" {
		t.Fatalf("run.Text = %q, want no text", run.Text)
	}
	if s.aborts != 1 {
		t.Fatalf("Abort called %d times, want 1", s.aborts)
	}
}

// TestPromptAndWait_ExitInsideTheAbortGraceKeepsTheRuntimeFact decides the
// exit-during-the-grace case explicitly: the runtime is already gone when the
// abort goes in, so nothing but the process-exit response answers it. That exit
// is a real fact, so the result keeps it as the turn's summary. The synthesized
// aborted turn is only for a runtime that reported nothing at all, and no turn
// end is written to the stream in either case.
func TestPromptAndWait_ExitInsideTheAbortGraceKeepsTheRuntimeFact(t *testing.T) {
	s := &asyncWaitSession{abortExit: true} // no turn end, ever
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi", oarlock.WithTimeout(30*time.Millisecond))
	if err != nil {
		t.Fatalf("a WaitOption deadline is not a caller error: %v", err)
	}
	if run.Outcome != oarlock.RunInterrupted {
		t.Fatalf("run = %+v, want interrupted", run)
	}
	if !isExitedTurn(run.Turn) {
		t.Fatalf("run.Turn = %+v, want the runtime's own exit, not the synthesized aborted turn", run.Turn)
	}
	if n := s.turnEnds(); n != 0 {
		t.Fatalf("turn_ended events = %d, want 0: the exit is the last word", n)
	}
	if s.aborts != 1 {
		t.Fatalf("Abort called %d times, want 1", s.aborts)
	}
}

// TestPromptAndWait_TapLatencyBeatsTheSafetySweep: a turn end that arrives 10ms
// after the wait started must decide it in well under the one-second safety
// sweep, which is only possible through the event tap.
func TestPromptAndWait_TapLatencyBeatsTheSafetySweep(t *testing.T) {
	s := &asyncWaitSession{
		started:   make(chan struct{}),
		turnDelay: 10 * time.Millisecond,
		promptEnd: &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted},
		abortEnd:  &oarlock.TurnOutcome{Kind: oarlock.OutcomeAborted},
	}
	started := time.Now()
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi", oarlock.WithTimeout(5*time.Second))
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != oarlock.RunCompleted || run.Turn == nil || run.Turn.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("run = %+v, want the runtime's own completion", run)
	}
	if s.aborts != 0 {
		t.Fatalf("Abort called %d times, want 0", s.aborts)
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("wait took %v, want the tap (< 500ms), not the 1s safety sweep", elapsed)
	}
}

// TestPromptAndWait_BrokenTapFallsBackToTheSafetySweep: a session whose Events
// never delivers still completes, because the sweep re-reads Records when the
// tap has been silent.
func TestPromptAndWait_BrokenTapFallsBackToTheSafetySweep(t *testing.T) {
	s := &asyncWaitSession{
		started:   make(chan struct{}),
		turnDelay: 10 * time.Millisecond,
		promptEnd: &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted},
		abortEnd:  &oarlock.TurnOutcome{Kind: oarlock.OutcomeAborted},
		brokenTap: true,
	}
	started := time.Now()
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi", oarlock.WithTimeout(5*time.Second))
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != oarlock.RunCompleted || run.Turn == nil || run.Turn.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("run = %+v, want the runtime's own completion", run)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("wait took %v, want the safety net well inside 5s", elapsed)
	}
}
