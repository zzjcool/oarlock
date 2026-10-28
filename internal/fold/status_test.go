package fold

import (
	"encoding/json"
	"testing"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/kernel"
)

// TestEmptyStreamIsIdle pins issue #7's empty-stream acceptance criterion for
// the status fold: an empty stream (and a stream carrying only idle evidence)
// is idle with Seq 0, never running and never exited.
func TestEmptyStreamIsIdle(t *testing.T) {
	for _, records := range [][]oarlock.Record{nil, {}} {
		got := StatusOf(records)
		if got.Seq != 0 {
			t.Fatalf("Status Seq on an empty stream = %d, want 0", got.Seq)
		}
		if got.Value.Kind != StatusIdle {
			t.Fatalf("Status on an empty stream = %q, want idle", got.Value.Kind)
		}
		if got.Value.SinceSeq != 0 {
			t.Fatalf("SinceSeq on an empty stream = %d, want 0", got.Value.SinceSeq)
		}
		if got.Value.LastTurnOutcome != nil {
			t.Fatalf("LastTurnOutcome on an empty stream = %+v, want nil", got.Value.LastTurnOutcome)
		}
	}

	// Idle evidence only: accounting, model, queue and task frames say nothing
	// about a turn, so the idle span still starts at 0.
	idleOnly := []oarlock.Record{
		mkUsageFrame(1, oarlock.UsageReport{Tokens: mkTokens(10, 2)}),
		mkFrame(2, oarlock.Event{Kind: oarlock.EventModel, Model: "some-model"}),
		mkFrame(3, oarlock.Event{Kind: oarlock.EventEffort, Effort: "high"}),
		mkFrame(4, oarlock.Event{Kind: oarlock.EventInputQueued, InputID: "input-9"}),
		mkFrame(5, oarlock.Event{Kind: oarlock.EventTaskStarted, Task: &oarlock.TaskEvent{TaskID: "task-1"}}),
		mkFrame(6, oarlock.Event{Kind: oarlock.EventSessionLink, Parent: testRootSessionID, Child: testChildSessionID}),
	}
	got := StatusOf(idleOnly).Value
	if got.Kind != StatusIdle {
		t.Fatalf("Status after idle-only frames = %q, want idle", got.Kind)
	}
	if got.SinceSeq != 0 {
		t.Fatalf("SinceSeq after idle-only frames = %d, want 0", got.SinceSeq)
	}
}

// TestRunningPromptIsBusyAndIdlePromptIsNotBusy pins issue #7's status
// invariant through the kernel: a prompt recorded while the fold says
// StatusRunning is answered rejected{busy}, a prompt recorded while the fold
// says StatusIdle is never answered busy, and a turn_ended frame reopens the
// window for the next prompt.
func TestRunningPromptIsBusyAndIdlePromptIsNotBusy(t *testing.T) {
	k := kernel.New("s1", nil)

	// seen records the status the decide saw, so a failure names the fold
	// answer instead of only the kernel's verdict.
	var seen []StatusKind

	// decide reads this fold over the records retained before the request
	// being decided: exactly what a host observing the stream would have
	// folded at that moment.
	decide := func(request kernel.RequestRecord) oarlock.ResponseBody {
		var before []oarlock.Record
		for _, record := range k.Records(kernel.Cursor{}) {
			if record.Seq < request.Seq {
				before = append(before, record)
			}
		}
		status := StatusOf(before).Value
		seen = append(seen, status.Kind)
		if status.Kind == StatusRunning {
			return mkRejectedBody(oarlock.Busy)
		}
		return mkAcceptedBody()
	}

	prompt := func(text string) oarlock.ControlOutcome {
		return k.Control(oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: text}, decide, kernel.RecordAt{})
	}

	first := prompt("first")
	if first.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt on an idle stream: got %q (code %q, reason %q), want accepted", first.Kind, first.Code, first.Reason)
	}

	// One running turn: the runtime adopts it and streams an answer.
	k.Frame(kernel.FrameBody{
		Native: json.RawMessage(`{"type":"turn_start"}`),
		Events: []oarlock.Event{evTurnActive()},
	}, kernel.RecordAt{})
	k.Frame(kernel.FrameBody{
		Native: json.RawMessage(`{"type":"text_delta"}`),
		Events: []oarlock.Event{evTextDelta("working", "m-1")},
	}, kernel.RecordAt{})
	if status := StatusOf(k.Records(kernel.Cursor{})).Value; status.Kind != StatusRunning {
		t.Fatalf("status after turn_active + text_delta = %q, want running", status.Kind)
	}

	second := prompt("second")
	if second.Kind != oarlock.ControlRejected || second.Code != oarlock.Busy {
		t.Fatalf("prompt while running: got %q (code %q), want rejected/busy", second.Kind, second.Code)
	}

	// The runtime ends the turn; the next prompt is taken over again.
	k.Frame(kernel.FrameBody{
		Native: json.RawMessage(`{"type":"turn_end"}`),
		Events: []oarlock.Event{evTurnEnded(oarlock.OutcomeCompleted)},
	}, kernel.RecordAt{})
	if status := StatusOf(k.Records(kernel.Cursor{})).Value; status.Kind != StatusIdle {
		t.Fatalf("status after turn_ended = %q, want idle", status.Kind)
	}

	third := prompt("third")
	if third.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt after turn_ended: got %q (code %q), want accepted", third.Kind, third.Code)
	}

	want := []StatusKind{StatusIdle, StatusRunning, StatusIdle}
	if len(seen) != len(want) {
		t.Fatalf("decide folded %d times (%v), want 3", len(seen), seen)
	}
	for index := range want {
		if seen[index] != want[index] {
			t.Fatalf("decide %d folded status %q, want %q: a running fold means busy, an idle fold never does", index, seen[index], want[index])
		}
	}
}

// TestChildAgentTurnEndedDoesNotEndRootTurn pins issue #7's root-scope rule: a
// sub-agent's or a child session's turn_ended never ends the root turn, and
// its outcome never lands in the root Status.
func TestChildAgentTurnEndedDoesNotEndRootTurn(t *testing.T) {
	records := []oarlock.Record{
		mkPrompt(1, "req-1", "input-1", "delegate"),
		mkRespond(2, "req-1", mkAcceptedBody()),
		mkFrame(3, evTurnActive()),
		// A sub-agent inside the root session ends its own turn.
		mkFrameAt(4, testRootSessionID, []string{"sub"}, evTurnEnded(oarlock.OutcomeFailed)),
		// A derived child session ends its turn too.
		mkFrameAt(5, testChildSessionID, nil, evTurnEnded(oarlock.OutcomeAborted)),
		// And a child session keeps streaming while the root turn runs.
		mkFrameAt(6, testChildSessionID, nil, evTextDelta("child output", "m-c1")),
		mkFrameAt(7, testChildSessionID, nil, oarlock.Event{Kind: oarlock.EventToolCallStarted, CallID: "call-child", Tool: "bash"}),
		mkFrame(8, evTextDelta("still the root turn", "m-1")),
	}

	got := StatusOf(records).Value
	if got.Kind != StatusRunning {
		t.Fatalf("status after child turn_ended = %q, want running: a child never ends the root turn", got.Kind)
	}
	if got.LastTurnOutcome != nil {
		t.Fatalf("LastTurnOutcome = %+v, want nil: a child's outcome is not the root's", got.LastTurnOutcome)
	}

	// The root's own turn_ended is the one that closes it, and its outcome is
	// the one recorded.
	ended := StatusOf(withAppended(records, mkFrame(9, evTurnEnded(oarlock.OutcomeCompleted)))).Value
	if ended.Kind != StatusIdle {
		t.Fatalf("status after the root turn_ended = %q, want idle", ended.Kind)
	}
	if ended.LastTurnOutcome == nil || ended.LastTurnOutcome.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("LastTurnOutcome = %+v, want the root turn's completed outcome", ended.LastTurnOutcome)
	}
}

// TestRejectedPromptReturnsToIdle pins issue #7's Status = idle|running|exited
// criterion for a refusal: a rejected response whose RequestID is the running
// request's id ends the turn (the turn never began), so the next prompt opens a
// fresh running span.
func TestRejectedPromptReturnsToIdle(t *testing.T) {
	refused := []oarlock.Record{
		mkPrompt(1, "req-1", "input-1", "first"),
		mkRespond(2, "req-1", mkRejectedBody(oarlock.Busy)),
	}

	got := StatusOf(refused).Value
	if got.Kind != StatusIdle {
		t.Fatalf("status after the running request was rejected = %q, want idle", got.Kind)
	}

	// A turn can only be opened from idle, so a prompt now opens a new span.
	next := StatusOf(withAppended(refused, mkPrompt(3, "req-3", "input-3", "second"))).Value
	if next.Kind != StatusRunning {
		t.Fatalf("status after a prompt following the rejection = %q, want running", next.Kind)
	}
	if next.RequestID != "req-3" {
		t.Fatalf("RequestID = %q, want the new prompt's req-3", next.RequestID)
	}
	if next.InputID != "input-3" {
		t.Fatalf("InputID = %q, want the new prompt's input-3", next.InputID)
	}
	if next.Phase.Kind != PhaseWaitingModel {
		t.Fatalf("Phase = %q, want waiting_model", next.Phase.Kind)
	}
	if next.SinceSeq != 3 {
		t.Fatalf("SinceSeq = %d, want 3 (the record that opened the turn)", next.SinceSeq)
	}
}

// TestForeignRejectionDoesNotEndTurn pins issue #7's criterion that a recorded
// prompt while running is busy: a rejection for some other request changes
// nothing, so it must not end the running turn either.
func TestForeignRejectionDoesNotEndTurn(t *testing.T) {
	records := []oarlock.Record{
		mkPrompt(1, "req-1", "input-1", "first"),
		mkFrame(2, evTurnActive()),
		mkRespond(3, "req-other", mkRejectedBody(oarlock.Busy)),
	}

	got := StatusOf(records).Value
	if got.Kind != StatusRunning {
		t.Fatalf("status after a foreign rejection = %q, want running (the response answers another request)", got.Kind)
	}
	if got.RequestID != "req-1" {
		t.Fatalf("RequestID = %q, want the running span's req-1", got.RequestID)
	}
}

// TestHalfToolCallWithExitIsExited pins issue #7's crash criterion for the
// combined case: a trailing half tool call leaves the phase determinable, and
// an exited response anywhere in the stream makes the status exited while the
// turn's start is preserved.
func TestHalfToolCallWithExitIsExited(t *testing.T) {
	half := []oarlock.Record{
		mkPrompt(1, "req-1", "input-1", "run ls"),
		mkRespond(2, "req-1", mkAcceptedBody()),
		mkFrame(3, evTurnActive()),
		mkFrame(4, oarlock.Event{Kind: oarlock.EventToolCallStarted, CallID: "call-1", Tool: "bash"}),
		mkFrame(5, oarlock.Event{Kind: oarlock.EventToolCallInputDelta, CallID: "call-1", Delta: `{"command":"ls`}),
	}

	running := StatusOf(half).Value
	if running.Kind != StatusRunning {
		t.Fatalf("status with a half tool call = %q, want running", running.Kind)
	}
	if running.Phase.Kind != PhaseTool || running.Phase.Tool != "bash" || running.Phase.CallID != "call-1" {
		t.Fatalf("phase with a half tool call = %+v, want the tool and call id", running.Phase)
	}

	exited := StatusOf(withAppended(half, mkRespond(6, "", mkExitedBody()))).Value
	if exited.Kind != StatusExited {
		t.Fatalf("status after an exited response = %q, want exited", exited.Kind)
	}
	if exited.SinceSeq != running.SinceSeq {
		t.Fatalf("SinceSeq after exit = %d, want the turn's start %d", exited.SinceSeq, running.SinceSeq)
	}
}

// TestHalfToolCallWithoutExitStaysRunningWithToolPhase pins issue #7's crash
// criterion: a stream that ends in the middle of a tool call is still a
// determinable status-running with a tool phase, not an unknown state and not
// a premature idle.
func TestHalfToolCallWithoutExitStaysRunningWithToolPhase(t *testing.T) {
	records := []oarlock.Record{
		mkPrompt(1, "req-1", "input-1", "run ls"),
		mkRespond(2, "req-1", mkAcceptedBody()),
		mkFrame(3, evTurnActive()),
		mkFrame(4, oarlock.Event{Kind: oarlock.EventToolCallStarted, CallID: "call-1", Tool: "bash"}),
		mkFrame(5, oarlock.Event{Kind: oarlock.EventToolCallInputDelta, CallID: "call-1", Delta: `{"command":"ls`}),
	}

	got := StatusOf(records).Value
	if got.Kind != StatusRunning {
		t.Fatalf("status with a trailing half tool call = %q, want running", got.Kind)
	}
	if got.Phase.Kind != PhaseTool {
		t.Fatalf("phase = %q, want tool", got.Phase.Kind)
	}
	if got.Phase.Tool != "bash" || got.Phase.CallID != "call-1" {
		t.Fatalf("phase = %+v, want tool bash at call-1", got.Phase)
	}
	if !got.Phase.Writing {
		t.Fatal("Writing = false after an input delta, want true: the arguments are still streaming")
	}
}

// TestToolWritingFlagFollowsInputDeltas pins issue #7's Status = idle|running
// criterion for the tool phase: the phase says which tool and call is running
// and whether its arguments are still streaming, while progress and other
// calls' deltas cannot move it.
func TestToolWritingFlagFollowsInputDeltas(t *testing.T) {
	records := []oarlock.Record{
		mkPrompt(1, "req-1", "input-1", "run ls"),
		mkFrame(2, evTurnActive()),
		mkFrame(3, oarlock.Event{Kind: oarlock.EventToolCallStarted, CallID: "call-1", Tool: "bash"}),
	}
	if got := StatusOf(records).Value; got.Phase.Kind != PhaseTool || got.Phase.Writing {
		t.Fatalf("phase after tool_call_started = %+v, want tool with Writing false", got.Phase)
	}

	records = withAppended(records, mkFrame(4, oarlock.Event{Kind: oarlock.EventToolCallInputDelta, CallID: "call-1", Delta: "{}"}))
	if got := StatusOf(records).Value; !got.Phase.Writing || got.Phase.CallID != "call-1" {
		t.Fatalf("phase after input_delta = %+v, want Writing true at call-1", got.Phase)
	}

	records = withAppended(records, mkFrame(5, oarlock.Event{Kind: oarlock.EventToolCallInput, CallID: "call-1", Input: "{}"}))
	if got := StatusOf(records).Value; got.Phase.Writing {
		t.Fatalf("phase after tool_call_input = %+v, want Writing false", got.Phase)
	}

	// Progress leaves the phase where it was; a delta for a different call
	// cannot move the phase either.
	records = withAppended(records,
		mkFrame(6, oarlock.Event{Kind: oarlock.EventToolCallProgress, CallID: "call-1", Output: "out"}),
		mkFrame(7, oarlock.Event{Kind: oarlock.EventToolCallInputDelta, CallID: "call-2", Delta: "{}"}),
	)
	got := StatusOf(records).Value
	if got.Phase.Kind != PhaseTool || got.Phase.CallID != "call-1" || got.Phase.Writing {
		t.Fatalf("phase after progress + a foreign delta = %+v, want tool call-1 with Writing false", got.Phase)
	}
}

// TestExitIsFinalDespiteLateFrames pins issue #7's Status = idle|running|exited
// criterion read off the records alone: once the stream holds an exited
// response the status is exited for the rest of the stream, unaffected by late
// frames, late prompts or a late turn_ended.
func TestExitIsFinalDespiteLateFrames(t *testing.T) {
	records := []oarlock.Record{
		mkPrompt(1, "req-1", "input-1", "hi"),
		mkRespond(2, "req-1", mkAcceptedBody()),
		mkFrame(3, evTurnActive()),
		mkRespond(4, "", mkExitedBody()),
	}
	// Everything below lands after the process exit: none of it reopens the
	// session.
	late := []oarlock.Record{
		mkFrame(5, evTurnActive()),
		mkFrame(6, evTextDelta("late text", "m-late")),
		mkFrame(7, oarlock.Event{Kind: oarlock.EventCompactionStarted, Trigger: "auto"}),
		mkFrame(8, evTurnEnded(oarlock.OutcomeCompleted)),
		mkPrompt(9, "req-9", "input-9", "another prompt"),
		mkRespond(10, "req-9", mkAcceptedBody()),
	}

	f := New()
	for _, record := range records {
		f.Feed(record)
	}
	if got := f.Status().Value; got.Kind != StatusExited {
		t.Fatalf("status after the exited response = %q, want exited", got.Kind)
	}
	for _, record := range late {
		f.Feed(record)
		if got := f.Status().Value; got.Kind != StatusExited {
			t.Fatalf("status after late record %d = %q, want exited (exit is final)", record.Seq, got.Kind)
		}
	}
	if got := f.Status().Value; got.LastTurnOutcome != nil {
		t.Fatalf("LastTurnOutcome = %+v, want nil: the late turn_ended came after the exit", got.LastTurnOutcome)
	}
}

// TestCompactionEndedOnlyClosesARunningPhase pins issue #7's Status =
// idle|running criterion for the compaction phase: compaction_started opens a
// compacting phase, compaction_ended returns to waiting_model only when a turn
// is running, and idle stays idle.
func TestCompactionEndedOnlyClosesARunningPhase(t *testing.T) {
	idle := []oarlock.Record{
		mkFrame(1, oarlock.Event{Kind: oarlock.EventCompactionEnded, CompactionResult: oarlock.CompactionCompleted}),
	}
	if got := StatusOf(idle).Value; got.Kind != StatusIdle {
		t.Fatalf("status after compaction_ended on an idle stream = %q, want idle", got.Kind)
	}

	records := []oarlock.Record{
		mkPrompt(1, "req-1", "input-1", "hi"),
		mkFrame(2, evTurnActive()),
		mkFrame(3, oarlock.Event{Kind: oarlock.EventCompactionStarted, Trigger: "auto"}),
	}
	if got := StatusOf(records).Value; got.Kind != StatusRunning || got.Phase.Kind != PhaseCompacting {
		t.Fatalf("status after compaction_started = %q/%q, want running/compacting", got.Kind, got.Phase.Kind)
	}
	ended := StatusOf(withAppended(records, mkFrame(4, oarlock.Event{Kind: oarlock.EventCompactionEnded, CompactionResult: oarlock.CompactionCompleted}))).Value
	if ended.Kind != StatusRunning || ended.Phase.Kind != PhaseWaitingModel {
		t.Fatalf("status after compaction_ended = %q/%q, want running/waiting_model", ended.Kind, ended.Phase.Kind)
	}
}

// TestInputDroppedEndsTurnOnlyForTheCurrentInput pins issue #7's Status =
// idle|running criterion for dropped input: the turn ends only when the
// dropped input is the one the running turn was opened with; any other dropped
// input changes nothing.
func TestInputDroppedEndsTurnOnlyForTheCurrentInput(t *testing.T) {
	records := []oarlock.Record{
		mkPrompt(1, "req-1", "input-1", "first"),
		mkFrame(2, oarlock.Event{Kind: oarlock.EventInputDropped, InputID: "input-other", Dropped: oarlock.DroppedTurnInterrupted}),
	}
	got := StatusOf(records).Value
	if got.Kind != StatusRunning {
		t.Fatalf("status after dropping a foreign input = %q, want running", got.Kind)
	}

	dropped := StatusOf(withAppended(records, mkFrame(3, oarlock.Event{Kind: oarlock.EventInputDropped, InputID: "input-1", Dropped: oarlock.DroppedTurnInterrupted}))).Value
	if dropped.Kind != StatusIdle {
		t.Fatalf("status after dropping the running input = %q, want idle", dropped.Kind)
	}
}

// evInputHandled is the runtime's own "this prompt was handled without
// opening a turn" (pi: prompt response disposition "handled"). The adapter
// names the prompt by its request id and, when the host gave one, its input id.
func evInputHandled(requestID, inputID string) oarlock.Event {
	return oarlock.Event{Kind: oarlock.EventInputHandled, RequestID: requestID, InputID: inputID}
}

// TestInputHandledReturnsOwnPromptToIdleWithoutAnOutcome pins issue #10's
// handled-prompt rule: the runtime reporting the running span's own prompt as
// handled returns the status to idle, and since no turn ended it leaves no
// LastTurnOutcome behind.
func TestInputHandledReturnsOwnPromptToIdleWithoutAnOutcome(t *testing.T) {
	records := []oarlock.Record{mkPrompt(1, "req-1", "in-1", "/compact")}
	if got := StatusOf(records).Value; got.Kind != StatusRunning || got.InputID != "in-1" {
		t.Fatalf("status after the prompt request = %q (input %q), want running for in-1", got.Kind, got.InputID)
	}

	handled := StatusOf(withAppended(records, mkFrame(2, evInputHandled("req-1", "in-1"))))
	if handled.Value.Kind != StatusIdle {
		t.Fatalf("status after input_handled for the running prompt = %q, want idle", handled.Value.Kind)
	}
	if handled.Value.LastTurnOutcome != nil {
		t.Fatalf("LastTurnOutcome = %+v, want nil: a handled prompt ended no turn", handled.Value.LastTurnOutcome)
	}
	if handled.Value.SinceSeq != 0 || handled.Value.RequestID != "" || handled.Value.InputID != "" {
		t.Fatalf("idle status after input_handled carries a span: %+v", handled.Value)
	}
	if handled.Seq != 2 {
		t.Fatalf("Status Seq = %d, want 2 (the input_handled frame)", handled.Seq)
	}

	// Either name is enough: the input id alone (no request id on the event)
	// also returns the running prompt to idle.
	if got := StatusOf(withAppended(records, mkFrame(2, evInputHandled("", "in-1")))).Value; got.Kind != StatusIdle {
		t.Fatalf("status after input_handled naming only the input id = %q, want idle", got.Kind)
	}

	// The same fact in the order a runtime may answer in: after the adapter's
	// accepted response.
	answered := StatusOf([]oarlock.Record{
		mkPrompt(1, "req-1", "in-1", "/compact"),
		mkRespond(2, "req-1", mkAcceptedBody()),
		mkFrame(3, evInputHandled("req-1", "in-1")),
	}).Value
	if answered.Kind != StatusIdle || answered.LastTurnOutcome != nil {
		t.Fatalf("status after accepted + input_handled = %q (outcome %+v), want idle with no outcome", answered.Kind, answered.LastTurnOutcome)
	}

	// Idle is a real idle: the next prompt opens a fresh span.
	next := StatusOf(withAppended(records, mkFrame(2, evInputHandled("req-1", "in-1")), mkPrompt(3, "req-3", "in-3", "hello"))).Value
	if next.Kind != StatusRunning || next.RequestID != "req-3" || next.InputID != "in-3" || next.SinceSeq != 3 {
		t.Fatalf("status after the prompt following a handled one = %+v, want running for req-3/in-3 since 3", next)
	}
}

// TestInputHandledWithoutAnInputIDMatchesByRequestID pins that the host's
// InputID is optional: a handled prompt that has none is still named by its
// request id, so it cannot leave the status running forever, and a request id
// that is not the running prompt's names some other prompt and changes nothing.
func TestInputHandledWithoutAnInputIDMatchesByRequestID(t *testing.T) {
	records := []oarlock.Record{mkPrompt(1, "req-1", "", "/compact")}
	if got := StatusOf(records).Value; got.Kind != StatusRunning || got.InputID != "" || got.RequestID != "req-1" {
		t.Fatalf("status after a prompt with no input id = %+v, want running for req-1 with no input id", got)
	}

	handled := StatusOf(withAppended(records, mkFrame(2, evInputHandled("req-1", "")))).Value
	if handled.Kind != StatusIdle {
		t.Fatalf("status after input_handled naming only the request id = %q, want idle", handled.Kind)
	}
	if handled.LastTurnOutcome != nil {
		t.Fatalf("LastTurnOutcome = %+v, want nil: a handled prompt ended no turn", handled.LastTurnOutcome)
	}

	foreign := StatusOf(withAppended(records, mkFrame(2, evInputHandled("req-9", "")))).Value
	if foreign.Kind != StatusRunning {
		t.Fatalf("status after input_handled for another request id and no input id = %q, want running", foreign.Kind)
	}
	if foreign.RequestID != "req-1" || foreign.SinceSeq != 1 {
		t.Fatalf("running span after a foreign input_handled = %+v, want req-1 since 1 untouched", foreign)
	}
}

// TestInputHandledForAnotherPromptLeavesTheTurnRunning pins that input_handled
// is matched, not broadcast: another prompt's ids, or no ids at all, change
// nothing, and an empty id never matches an adopted turn that has none.
func TestInputHandledForAnotherPromptLeavesTheTurnRunning(t *testing.T) {
	records := []oarlock.Record{
		mkPrompt(1, "req-1", "in-1", "first"),
		mkFrame(2, evInputHandled("req-other", "in-other")),
	}
	got := StatusOf(records).Value
	if got.Kind != StatusRunning {
		t.Fatalf("status after input_handled for a foreign prompt = %q, want running", got.Kind)
	}
	if got.RequestID != "req-1" || got.InputID != "in-1" || got.SinceSeq != 1 {
		t.Fatalf("running span after a foreign input_handled = %+v, want req-1/in-1 since 1 untouched", got)
	}

	// An input_handled naming neither a request nor an input matches nobody.
	if got := StatusOf(withAppended(records, mkFrame(3, evInputHandled("", "")))).Value; got.Kind != StatusRunning {
		t.Fatalf("status after input_handled with no ids = %q, want running", got.Kind)
	}

	// An adopted turn has neither a request id nor an input id of ours, so an
	// empty id must not match it and a request id cannot name it.
	adopted := []oarlock.Record{
		mkFrame(1, evTurnActive()),
		mkFrame(2, evInputHandled("", "")),
		mkFrame(3, evInputHandled("req-1", "in-1")),
	}
	if got := StatusOf(adopted).Value; got.Kind != StatusRunning {
		t.Fatalf("adopted turn after input_handled with no matching ids = %q, want running", got.Kind)
	}
}

// TestInputHandledWhileIdleChangesNothing pins that input_handled is a no-op
// when no span is running, and in particular that it does not erase the last
// real turn outcome.
func TestInputHandledWhileIdleChangesNothing(t *testing.T) {
	onEmpty := StatusOf([]oarlock.Record{mkFrame(1, evInputHandled("req-1", "in-1"))}).Value
	if onEmpty.Kind != StatusIdle || onEmpty.SinceSeq != 0 || onEmpty.LastTurnOutcome != nil {
		t.Fatalf("status after input_handled on an empty stream = %+v, want a plain idle", onEmpty)
	}

	ended := []oarlock.Record{
		mkPrompt(1, "req-1", "in-1", "hi"),
		mkFrame(2, evTurnActive()),
		mkFrame(3, evTurnEnded(oarlock.OutcomeCompleted)),
	}
	before := StatusOf(ended).Value
	after := StatusOf(withAppended(ended, mkFrame(4, evInputHandled("req-1", "in-1")))).Value
	if after.Kind != StatusIdle {
		t.Fatalf("status after a late input_handled = %q, want idle", after.Kind)
	}
	if after.LastTurnOutcome == nil || after.LastTurnOutcome.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("LastTurnOutcome = %+v, want the earlier completed outcome untouched", after.LastTurnOutcome)
	}
	if after.SinceSeq != before.SinceSeq || after.RequestID != before.RequestID || after.InputID != before.InputID {
		t.Fatalf("idle status moved: before %+v, after %+v", before, after)
	}
}

// TestInputHandledAfterExitKeepsExited pins that exit stays final: an
// input_handled fact that lands after the exited response reopens nothing.
func TestInputHandledAfterExitKeepsExited(t *testing.T) {
	records := []oarlock.Record{
		mkPrompt(1, "req-1", "in-1", "/compact"),
		mkRespond(2, "", mkExitedBody()),
		mkFrame(3, evInputHandled("req-1", "in-1")),
	}
	got := StatusOf(records).Value
	if got.Kind != StatusExited {
		t.Fatalf("status after exit + input_handled = %q, want exited", got.Kind)
	}
	if got.SinceSeq != 1 {
		t.Fatalf("SinceSeq = %d, want the turn's start 1 kept across the exit", got.SinceSeq)
	}
	if got.LastTurnOutcome != nil {
		t.Fatalf("LastTurnOutcome = %+v, want nil", got.LastTurnOutcome)
	}
}

// TestHandledPromptThroughTheKernelIsIdleWithoutATurnEnd pins issue #7's
// invariant 1 test through the real kernel: a prompt the runtime reports as
// handled leaves Status idle, a dense Seq and no turn_active or turn_ended
// event in any record. The frame lands where the Pi adapter records it, between
// the request and its accepted response, and also after both; the host's
// InputID is given once and left out once.
func TestHandledPromptThroughTheKernelIsIdleWithoutATurnEnd(t *testing.T) {
	handledFrame := func(requestID, inputID string) kernel.FrameBody {
		return kernel.FrameBody{
			Native: json.RawMessage(`{"type":"response","command":"prompt","success":true,"data":{"disposition":"handled"}}`),
			Events: []oarlock.Event{evInputHandled(requestID, inputID)},
		}
	}

	for _, inputID := range []string{"in-1", ""} {
		prompt := oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "/compact", InputID: inputID}
		cases := []struct {
			name string
			run  func(k kernel.Kernel) oarlock.ControlOutcome
		}{
			{"frame between request and response", func(k kernel.Kernel) oarlock.ControlOutcome {
				return k.Control(prompt, func(request kernel.RequestRecord) oarlock.ResponseBody {
					k.Frame(handledFrame(request.ID, inputID), kernel.RecordAt{})
					return mkAcceptedBody()
				}, kernel.RecordAt{})
			}},
			{"frame after the response", func(k kernel.Kernel) oarlock.ControlOutcome {
				outcome := k.Control(prompt, func(kernel.RequestRecord) oarlock.ResponseBody {
					return mkAcceptedBody()
				}, kernel.RecordAt{})
				k.Frame(handledFrame(outcome.RequestID, inputID), kernel.RecordAt{})
				return outcome
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name+" input="+inputID, func(t *testing.T) {
				k := kernel.New("s1", nil)
				outcome := tc.run(k)
				if outcome.Kind != oarlock.ControlAccepted {
					t.Fatalf("handled prompt: got %q (code %q, reason %q), want accepted", outcome.Kind, outcome.Code, outcome.Reason)
				}

				records := k.Records(kernel.Cursor{})
				if len(records) != 3 {
					t.Fatalf("handled prompt left %d records, want 3 (request, frame, response)", len(records))
				}
				for index, record := range records {
					if record.Seq != uint64(index+1) {
						t.Fatalf("record %d has Seq %d, want dense 1..%d", index, record.Seq, len(records))
					}
					for _, ev := range record.Events {
						if ev.Kind == oarlock.EventTurnEnded || ev.Kind == oarlock.EventTurnActive {
							t.Fatalf("record %d carries %q: a handled prompt opens and ends no turn", record.Seq, ev.Kind)
						}
					}
				}

				status := StatusOf(records)
				if status.Value.Kind != StatusIdle {
					t.Fatalf("status after a handled prompt = %q, want idle", status.Value.Kind)
				}
				if status.Value.LastTurnOutcome != nil {
					t.Fatalf("LastTurnOutcome = %+v, want nil: nothing ended", status.Value.LastTurnOutcome)
				}
				if status.Seq != uint64(len(records)) {
					t.Fatalf("Status Seq = %d, want %d", status.Seq, len(records))
				}

				// Idle means the next prompt is not busy.
				next := k.Control(oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "hello", InputID: "in-2"},
					func(request kernel.RequestRecord) oarlock.ResponseBody {
						var before []oarlock.Record
						for _, record := range k.Records(kernel.Cursor{}) {
							if record.Seq < request.Seq {
								before = append(before, record)
							}
						}
						if StatusOf(before).Value.Kind == StatusRunning {
							return mkRejectedBody(oarlock.Busy)
						}
						return mkAcceptedBody()
					}, kernel.RecordAt{})
				if next.Kind != oarlock.ControlAccepted {
					t.Fatalf("prompt after a handled one: got %q (code %q), want accepted", next.Kind, next.Code)
				}
			})
		}
	}
}
