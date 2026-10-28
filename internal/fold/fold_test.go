package fold

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/zzjcool/oarlock"
)

// This file holds the record builders every test in the package shares, plus
// the two tests that pin the fold as a whole. Records are built by hand: a
// fold only ever sees the stream, never a file or a socket.

const (
	// testRootSessionID is the session id of the first record of every
	// synthetic stream, so the fold's root scope is exactly this id.
	testRootSessionID = "root-session"
	// testChildSessionID is a derived child session. Records carrying it are
	// never root records.
	testChildSessionID = "child-session"
)

// mkPtr returns a pointer to value. The frozen shapes use a nil pointer for
// "the runtime did not report this number", which never means zero.
func mkPtr[T any](value T) *T { return &value }

// mkFrame builds a root frame record: root session, root agent.
func mkFrame(seq uint64, events ...oarlock.Event) oarlock.Record {
	return mkFrameAt(seq, testRootSessionID, nil, events...)
}

// mkFrameAt builds a frame record for an explicit session and agent path. A
// nil agentPath is the root agent.
func mkFrameAt(seq uint64, sessionID string, agentPath []string, events ...oarlock.Event) oarlock.Record {
	return oarlock.Record{
		Seq:       seq,
		Kind:      oarlock.KindFrame,
		SessionID: sessionID,
		AgentPath: agentPath,
		Native:    json.RawMessage(`{"type":"synthetic"}`),
		Events:    events,
	}
}

// mkPrompt builds a toRuntime prompt request in the root session.
func mkPrompt(seq uint64, id, inputID, text string) oarlock.Record {
	return oarlock.Record{
		Seq:       seq,
		Kind:      oarlock.KindRequest,
		SessionID: testRootSessionID,
		ID:        id,
		Direction: oarlock.ToRuntime,
		Request: &oarlock.RequestBody{
			Action:  oarlock.ActionPrompt,
			Input:   text,
			InputID: inputID,
		},
	}
}

// mkRespond builds a response in the root session pointing at requestID. The
// empty requestID is how a stream records a process exit oarlock observed
// itself.
func mkRespond(seq uint64, requestID string, body oarlock.ResponseBody) oarlock.Record {
	return oarlock.Record{
		Seq:       seq,
		Kind:      oarlock.KindResponse,
		SessionID: testRootSessionID,
		RequestID: requestID,
		Response:  &body,
	}
}

// mkAcceptedBody is the runtime taking a control over without naming a
// rejection.
func mkAcceptedBody() oarlock.ResponseBody {
	return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
}

// mkRejectedBody is a typed refusal: the caller still owns the input.
func mkRejectedBody(code oarlock.Code) oarlock.ResponseBody {
	return oarlock.ResponseBody{Kind: oarlock.ResponseRejected, Code: code}
}

// mkExitedBody is the one outcome the runtime cannot report itself.
func mkExitedBody() oarlock.ResponseBody {
	return oarlock.ResponseBody{
		Kind:             oarlock.ResponseExited,
		ExitCode:         mkPtr(137),
		ExitCodeReported: true,
	}
}

// mkTokens is a token total with only the always-present counts reported.
// Reasoning/cache stay nil: "not reported" is not zero.
func mkTokens(in, out int64) *oarlock.TokenTotals {
	return &oarlock.TokenTotals{Input: in, Output: out}
}

// evTurnActive is the runtime's own "a turn is now running".
func evTurnActive() oarlock.Event {
	return oarlock.Event{Kind: oarlock.EventTurnActive}
}

// evTurnEnded is the runtime's own "the turn ended this way".
func evTurnEnded(kind oarlock.OutcomeKind) oarlock.Event {
	return oarlock.Event{
		Kind:    oarlock.EventTurnEnded,
		Outcome: &oarlock.TurnOutcome{Kind: kind},
	}
}

// evTextDelta is one streamed assistant text chunk.
func evTextDelta(text, messageID string) oarlock.Event {
	return oarlock.Event{Kind: oarlock.EventTextDelta, Text: text, MessageID: messageID}
}

// withAppended returns records followed by more, in a fresh slice: a caller
// never aliases another case's backing array.
func withAppended(records []oarlock.Record, more ...oarlock.Record) []oarlock.Record {
	out := make([]oarlock.Record, 0, len(records)+len(more))
	out = append(out, records...)
	return append(out, more...)
}

// richStream is one synthetic session covering every fold: a prompt, its
// accepted response, a turn with reasoning, streamed text, a tool call with
// deltas, usage carrying both an agent figure and a session total, a
// self-attributing sub-agent, a linked child session, a busy second prompt,
// compaction and a completed turn.
func richStream() []oarlock.Record {
	return []oarlock.Record{
		mkFrame(1, oarlock.Event{
			Kind:            oarlock.EventUserMessage,
			Text:            "list the files",
			MessageID:       "msg-u1",
			NativeMessageID: "native-u1",
			TurnID:          "turn-1",
			Evidence:        oarlock.EvidenceTurnItem,
		}),
		mkPrompt(2, "req-2", "input-2", "list the files"),
		mkRespond(3, "req-2", mkAcceptedBody()),
		mkFrame(4, evTurnActive()),
		mkFrame(5, oarlock.Event{Kind: oarlock.EventReasoning, Text: "I should list", MessageID: "msg-r1"}),
		mkFrame(6, evTextDelta("Running ", "msg-a1")),
		mkFrame(7,
			evTextDelta("ls now.", "msg-a1"),
			oarlock.Event{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{
				Context: mkContextUsage(9000, 200000, 4.5),
				Tokens:  mkTokens(1200, 80),
			}},
		),
		mkFrame(8, oarlock.Event{Kind: oarlock.EventToolCallStarted, CallID: "call-1", Tool: "bash"}),
		mkFrame(9, oarlock.Event{Kind: oarlock.EventToolCallInputDelta, CallID: "call-1", Delta: `{"command":"ls`}),
		mkFrame(10, oarlock.Event{Kind: oarlock.EventToolCallInput, CallID: "call-1", Input: `{"command":"ls -la"}`}),
		mkFrame(11, oarlock.Event{
			Kind:        oarlock.EventToolCallProgress,
			CallID:      "call-1",
			Output:      "total 0",
			OutputDelta: "\ndrwxr-xr-x  2 root root",
		}),
		mkFrame(12, oarlock.Event{
			Kind:             oarlock.EventToolCallEnded,
			CallID:           "call-1",
			Result:           oarlock.ToolOK,
			Content:          []oarlock.ToolOutputPart{{Type: "text", Text: "total 0\ndrwxr-xr-x  2 root root"}},
			ExitCode:         mkPtr(0),
			ExitCodeReported: true,
		}),
		// A sub-agent inside the same session self-attributes its own usage.
		mkFrameAt(13, testRootSessionID, []string{"sub"}, oarlock.Event{
			Kind:  oarlock.EventUsage,
			Usage: &oarlock.UsageReport{Tokens: mkTokens(300, 40)},
		}),
		// The runtime also reports a session-wide total covering more than its
		// agents; it is a second authoritative figure, never one to add.
		mkFrame(14, oarlock.Event{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{Total: mkTokens(5000, 400)}}),
		// A derived child session with its own transcript and its own total.
		mkFrame(15, oarlock.Event{Kind: oarlock.EventSessionLink, Parent: testRootSessionID, Child: testChildSessionID, Via: "task"}),
		mkFrameAt(16, testChildSessionID, nil,
			oarlock.Event{Kind: oarlock.EventUserMessage, Text: "child task", MessageID: "msg-c1"},
			oarlock.Event{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{Total: mkTokens(700, 60)}},
		),
		mkFrame(17, oarlock.Event{Kind: oarlock.EventCompactionStarted, Trigger: "auto"}),
		mkFrame(18, oarlock.Event{Kind: oarlock.EventCompactionEnded, CompactionResult: oarlock.CompactionCompleted}),
		// A prompt recorded while the turn is running: the busy answer belongs
		// to this request and the running span must survive it.
		mkPrompt(19, "req-19", "input-19", "second prompt"),
		mkRespond(20, "req-19", mkRejectedBody(oarlock.Busy)),
		mkFrame(21, evTurnEnded(oarlock.OutcomeCompleted)),
	}
}

// TestFoldIncrementalFeedEqualsOneShot pins issue #7's "every fold is a pure
// function" acceptance criterion: fold(records) == fold(replay(records)). One
// fold over the whole stream and a fold fed record by record must answer all
// four queries identically, and the XOf wrappers must equal Of(records).X().
func TestFoldIncrementalFeedEqualsOneShot(t *testing.T) {
	records := richStream()

	oneShot := Of(records)
	replayed := New()
	for _, record := range records {
		replayed.Feed(record)
	}

	if got, want := replayed.Seq(), oneShot.Seq(); got != want {
		t.Fatalf("Seq after replay = %d, want %d", got, want)
	}
	if got, want := replayed.Status(), oneShot.Status(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Status differs between one-shot and replay:\n one-shot: %+v\n replay:   %+v", want, got)
	}
	if got, want := replayed.Usage(), oneShot.Usage(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Usage differs between one-shot and replay:\n one-shot: %+v\n replay:   %+v", want, got)
	}
	if got, want := replayed.ContextUsage(), oneShot.ContextUsage(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ContextUsage differs between one-shot and replay:\n one-shot: %+v\n replay:   %+v", want, got)
	}
	if got, want := replayed.Conversation(), oneShot.Conversation(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Conversation differs between one-shot and replay:\n one-shot: %+v\n replay:   %+v", want, got)
	}

	// XOf(records) == Of(records).X() for every fold.
	if got, want := StatusOf(records), oneShot.Status(); !reflect.DeepEqual(got, want) {
		t.Fatalf("StatusOf != Of(records).Status():\n StatusOf: %+v\n Of:       %+v", got, want)
	}
	if got, want := UsageOf(records), oneShot.Usage(); !reflect.DeepEqual(got, want) {
		t.Fatalf("UsageOf != Of(records).Usage():\n UsageOf: %+v\n Of:      %+v", got, want)
	}
	if got, want := ContextUsageOf(records), oneShot.ContextUsage(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ContextUsageOf != Of(records).ContextUsage():\n ContextUsageOf: %+v\n Of:             %+v", got, want)
	}
	if got, want := ConversationOf(records), oneShot.Conversation(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ConversationOf != Of(records).Conversation():\n ConversationOf: %+v\n Of:             %+v", got, want)
	}

	// The stream is rich enough that every fold has something to say; without
	// this the equality above could hold over four empty answers.
	if oneShot.Usage().Value.Total == nil {
		t.Fatal("rich stream reported no usage total")
	}
	if oneShot.ContextUsage().Value == nil {
		t.Fatal("rich stream reported no context usage")
	}
	if len(oneShot.Conversation().Value) == 0 {
		t.Fatal("rich stream produced no conversation messages")
	}
	if got := oneShot.Status().Value.Kind; got != StatusIdle {
		t.Fatalf("rich stream ends with a completed turn: status %q, want idle", got)
	}
}

// TestFoldReturnsLastSeq pins issue #7's "every answer carries a Seq, so it can
// be aligned and rebuilt": Seq is the Seq of the last consumed record (0 before
// any record), incrementally, one-shot, and on every QueryResult.
func TestFoldReturnsLastSeq(t *testing.T) {
	records := richStream()

	incremental := New()
	if got := incremental.Seq(); got != 0 {
		t.Fatalf("Seq of an empty fold = %d, want 0 (no record consumed yet)", got)
	}
	for _, record := range records {
		incremental.Feed(record)
		if got := incremental.Seq(); got != record.Seq {
			t.Fatalf("Seq after feeding record %d = %d, want the fed record's Seq", record.Seq, got)
		}
	}

	last := records[len(records)-1].Seq
	if got := Of(records).Seq(); got != last {
		t.Fatalf("Of(records).Seq() = %d, want %d", got, last)
	}
	if got := incremental.Status().Seq; got != last {
		t.Fatalf("Status().Seq = %d, want last consumed record %d", got, last)
	}
	if got := incremental.Usage().Seq; got != last {
		t.Fatalf("Usage().Seq = %d, want last consumed record %d", got, last)
	}
	if got := incremental.ContextUsage().Seq; got != last {
		t.Fatalf("ContextUsage().Seq = %d, want last consumed record %d", got, last)
	}
	if got := incremental.Conversation().Seq; got != last {
		t.Fatalf("Conversation().Seq = %d, want last consumed record %d", got, last)
	}
}
