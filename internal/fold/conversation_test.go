package fold

import (
	"slices"
	"testing"

	"github.com/zzjcool/oarlock"
)

// TestConversationMergesConsecutiveTextDeltas pins issue #7's conversation
// merge rule: text deltas carrying the same non-empty MessageID fold into one
// assistant message, and a new MessageID starts a new message.
func TestConversationMergesConsecutiveTextDeltas(t *testing.T) {
	records := []oarlock.Record{
		mkFrame(1, oarlock.Event{
			Kind:            oarlock.EventUserMessage,
			Text:            "list the files",
			MessageID:       "u-1",
			NativeMessageID: "native-u-1",
			TurnID:          "turn-1",
			Evidence:        oarlock.EvidenceAcknowledged,
		}),
		mkFrame(2, evTextDelta("Hello, ", "m-1")),
		mkFrame(3, evTextDelta("world", "m-1")),
		mkFrame(4, evTextDelta("Second message", "m-2")),
	}

	got := ConversationOf(records)
	if got.Seq != 4 {
		t.Fatalf("Conversation Seq = %d, want the last consumed record 4", got.Seq)
	}
	if len(got.Value) != 3 {
		t.Fatalf("Conversation = %+v, want user + two assistant messages", got.Value)
	}

	user := got.Value[0]
	if user.Role != RoleUser {
		t.Fatalf("message 0 role = %q, want user", user.Role)
	}
	if user.Text != "list the files" {
		t.Fatalf("message 0 text = %q, want %q", user.Text, "list the files")
	}
	if user.Seq != 1 || user.EndSeq != 1 {
		t.Fatalf("message 0 span = %d..%d, want 1..1", user.Seq, user.EndSeq)
	}
	if user.MessageID != "u-1" || user.NativeMessageID != "native-u-1" || user.TurnID != "turn-1" {
		t.Fatalf("message 0 ids = %q/%q/%q, want u-1/native-u-1/turn-1", user.MessageID, user.NativeMessageID, user.TurnID)
	}
	if user.Evidence != oarlock.EvidenceAcknowledged {
		t.Fatalf("message 0 evidence = %q, want acknowledged", user.Evidence)
	}

	merged := got.Value[1]
	if merged.Role != RoleAssistant {
		t.Fatalf("message 1 role = %q, want assistant", merged.Role)
	}
	if merged.Text != "Hello, world" {
		t.Fatalf("message 1 text = %q, want the deltas appended (%q)", merged.Text, "Hello, world")
	}
	if merged.Seq != 2 || merged.EndSeq != 3 {
		t.Fatalf("message 1 span = %d..%d, want 2..3 (first record .. last merged record)", merged.Seq, merged.EndSeq)
	}
	if merged.MessageID != "m-1" {
		t.Fatalf("message 1 MessageID = %q, want m-1", merged.MessageID)
	}

	fresh := got.Value[2]
	if fresh.Role != RoleAssistant || fresh.Text != "Second message" {
		t.Fatalf("message 2 = %+v, want a new assistant message %q", fresh, "Second message")
	}
	if fresh.Seq != 4 || fresh.EndSeq != 4 {
		t.Fatalf("message 2 span = %d..%d, want 4..4: a new MessageID starts a new message", fresh.Seq, fresh.EndSeq)
	}
	if fresh.MessageID != "m-2" {
		t.Fatalf("message 2 MessageID = %q, want m-2", fresh.MessageID)
	}
}

// TestConversationRolesInStreamOrder pins issue #7's message-view criterion:
// the transcript keeps user, reasoning, assistant and tool messages in stream
// order of first appearance.
func TestConversationRolesInStreamOrder(t *testing.T) {
	records := []oarlock.Record{
		mkFrame(1, oarlock.Event{Kind: oarlock.EventUserMessage, Text: "run it", MessageID: "u-1"}),
		mkFrame(2, oarlock.Event{Kind: oarlock.EventReasoning, Text: "I should run it", MessageID: "r-1"}),
		mkFrame(3, evTextDelta("Running it.", "a-1")),
		mkFrame(4, oarlock.Event{Kind: oarlock.EventToolCallStarted, CallID: "call-1", Tool: "bash"}),
		mkFrame(5, oarlock.Event{Kind: oarlock.EventToolCallEnded, CallID: "call-1", Result: oarlock.ToolOK}),
	}

	got := ConversationOf(records).Value
	want := []Role{RoleUser, RoleReasoning, RoleAssistant, RoleTool}
	if len(got) != len(want) {
		t.Fatalf("Conversation = %+v, want %d messages %v", got, len(want), want)
	}
	for index, role := range want {
		if got[index].Role != role {
			t.Fatalf("message %d role = %q, want %q (stream order of first appearance)", index, got[index].Role, role)
		}
	}
	if got[0].Seq != 1 || got[1].Seq != 2 || got[2].Seq != 3 || got[3].Seq != 4 {
		t.Fatalf("message Seq = %d/%d/%d/%d, want 1/2/3/4", got[0].Seq, got[1].Seq, got[2].Seq, got[3].Seq)
	}
	if got[3].Seq != 4 || got[3].EndSeq != 5 {
		t.Fatalf("tool message span = %d..%d, want 4..5 merged by CallID", got[3].Seq, got[3].EndSeq)
	}
}

// TestConversationToolCallMergesAcrossRecords pins issue #7's tool-view
// criterion: every tool event for one (AgentPath, CallID) folds into one
// message, with Input/Output/Result reflecting the last thing the runtime said.
func TestConversationToolCallMergesAcrossRecords(t *testing.T) {
	records := []oarlock.Record{
		mkFrame(1, oarlock.Event{Kind: oarlock.EventToolCallStarted, CallID: "call-1", Tool: "bash", Input: `{"command":"ls`}),
		mkFrame(2, oarlock.Event{Kind: oarlock.EventToolCallInputDelta, CallID: "call-1", Delta: ` -la"}`}),
	}

	// A streaming input: the delta is appended and the message says the input
	// is still partial.
	partial := ConversationOf(records).Value
	if len(partial) != 1 {
		t.Fatalf("Conversation after started+delta = %+v, want one tool message", partial)
	}
	if partial[0].Role != RoleTool || partial[0].Tool != "bash" || partial[0].CallID != "call-1" {
		t.Fatalf("tool message = %+v, want the started event's tool and call id", partial[0])
	}
	if want := `{"command":"ls -la"}`; partial[0].Input != want {
		t.Fatalf("Input after input_delta = %q, want the started input with the delta appended %q", partial[0].Input, want)
	}
	if !partial[0].InputPartial {
		t.Fatal("InputPartial = false after input_delta, want true (arguments still streaming)")
	}

	// The complete input replaces the streamed one and clears the partial flag.
	records = withAppended(records,
		mkFrame(3, oarlock.Event{Kind: oarlock.EventToolCallInput, CallID: "call-1", Input: `{"command":"ls -la"}`}),
		mkFrame(4, oarlock.Event{Kind: oarlock.EventToolCallProgress, CallID: "call-1", Output: "partial", OutputDelta: " more"}),
	)
	complete := ConversationOf(records).Value
	if len(complete) != 1 {
		t.Fatalf("Conversation after input+progress = %+v, want still one tool message", complete)
	}
	if want := `{"command":"ls -la"}`; complete[0].Input != want {
		t.Fatalf("Input after tool_call_input = %q, want the authoritative %q", complete[0].Input, want)
	}
	if complete[0].InputPartial {
		t.Fatal("InputPartial = true after tool_call_input, want false")
	}
	if want := "partial more"; complete[0].Output != want {
		t.Fatalf("Output after progress = %q, want Output then the appended delta %q", complete[0].Output, want)
	}
	if complete[0].Seq != 1 || complete[0].EndSeq != 4 {
		t.Fatalf("tool message span = %d..%d, want 1..4 merged by CallID", complete[0].Seq, complete[0].EndSeq)
	}

	// The ended event carries the explicit outcome and replaces the streamed
	// output with the reported text parts.
	records = withAppended(records,
		mkFrame(5, oarlock.Event{
			Kind:             oarlock.EventToolCallEnded,
			CallID:           "call-1",
			Result:           oarlock.ToolOK,
			Content:          []oarlock.ToolOutputPart{{Type: "text", Text: "final output"}},
			ExitCode:         mkPtr(0),
			ExitCodeReported: true,
		}),
	)
	ended := ConversationOf(records).Value
	if len(ended) != 1 {
		t.Fatalf("Conversation after tool_call_ended = %+v, want still one tool message", ended)
	}
	tool := ended[0]
	if tool.EndSeq != 5 {
		t.Fatalf("tool message EndSeq = %d, want 5", tool.EndSeq)
	}
	if tool.Result != oarlock.ToolOK {
		t.Fatalf("tool Result = %q, want ok", tool.Result)
	}
	if want := "final output"; tool.Output != want {
		t.Fatalf("Output after tool_call_ended = %q, want the reported text parts %q", tool.Output, want)
	}
	if len(tool.Content) != 1 || tool.Content[0].Text != "final output" {
		t.Fatalf("tool Content = %+v, want the reported text part", tool.Content)
	}
	if tool.ExitCode == nil || *tool.ExitCode != 0 || !tool.ExitCodeReported {
		t.Fatalf("tool ExitCode = %v (reported %v), want a reported 0", tool.ExitCode, tool.ExitCodeReported)
	}
}

// TestConversationSkipsChildSessionRecords pins issue #7's Conversation
// criterion (fold the records into a user/assistant/tool/reasoning message
// view) at its fixed root scope: a child session carries its own transcript,
// so its records never appear in the root conversation.
func TestConversationSkipsChildSessionRecords(t *testing.T) {
	records := []oarlock.Record{
		mkFrame(1, oarlock.Event{Kind: oarlock.EventUserMessage, Text: "root question", MessageID: "u-1"}),
		mkFrameAt(2, testChildSessionID, nil, oarlock.Event{Kind: oarlock.EventUserMessage, Text: "child question", MessageID: "u-2"}),
		mkFrameAt(3, testChildSessionID, nil, evTextDelta("child answer", "a-2")),
		mkFrameAt(4, testChildSessionID, nil, oarlock.Event{
			Kind:   oarlock.EventToolCallStarted,
			CallID: "call-child",
			Tool:   "bash",
		}),
		mkFrame(5, evTextDelta("root answer", "a-1")),
	}

	got := ConversationOf(records).Value
	if len(got) != 2 {
		t.Fatalf("Conversation = %+v, want only the root session's user and assistant messages", got)
	}
	if got[0].Role != RoleUser || got[0].Text != "root question" {
		t.Fatalf("message 0 = %+v, want the root user message", got[0])
	}
	if got[1].Role != RoleAssistant || got[1].Text != "root answer" {
		t.Fatalf("message 1 = %+v, want the root assistant message", got[1])
	}

	// The child session's own transcript is still exactly one fold away: the
	// fold keys records by the session of the first record only.
	childOnly := ConversationOf(records[1:4]).Value
	if len(childOnly) != 3 {
		t.Fatalf("Conversation of the child session records = %+v, want its user, assistant and tool messages", childOnly)
	}
}

// TestConversationDoesNotMergeAcrossAgentPath pins issue #7's conversation
// message-view criterion for the AgentPath half of the merge key: two agents
// inside one session never share a message even with the same MessageID.
func TestConversationDoesNotMergeAcrossAgentPath(t *testing.T) {
	records := []oarlock.Record{
		mkFrame(1, evTextDelta("root says", "m-1")),
		mkFrameAt(2, testRootSessionID, []string{"sub"}, evTextDelta("sub says", "m-1")),
	}

	got := ConversationOf(records).Value
	if len(got) != 2 {
		t.Fatalf("Conversation = %+v, want one message per agent path", got)
	}
	if got[0].Role != RoleAssistant || got[0].Text != "root says" || len(got[0].AgentPath) != 0 {
		t.Fatalf("message 0 = %+v, want the root agent's text", got[0])
	}
	if !slices.Equal(got[1].AgentPath, []string{"sub"}) || got[1].Text != "sub says" {
		t.Fatalf("message 1 = %+v (path %v), want the sub agent's text", got[1], got[1].AgentPath)
	}
}

// TestConversationIgnoresNonMessageEvents pins issue #7's Conversation
// criterion from the other side: every other event/kind is not a conversation
// message, so lifecycle and accounting frames never show up as transcript
// entries.
func TestConversationIgnoresNonMessageEvents(t *testing.T) {
	records := []oarlock.Record{
		mkPrompt(1, "req-1", "input-1", "hi"),
		mkRespond(2, "req-1", mkAcceptedBody()),
		mkFrame(3, evTurnActive()),
		mkUsageFrame(4, oarlock.UsageReport{Tokens: mkTokens(10, 2)}),
		mkFrame(5, oarlock.Event{Kind: oarlock.EventModel, Model: "some-model"}),
		mkFrame(6, oarlock.Event{Kind: oarlock.EventCompactionStarted, Trigger: "auto"}),
		mkFrame(7, oarlock.Event{Kind: oarlock.EventInputQueued, InputID: "input-9"}),
		mkFrame(8, evTurnEnded(oarlock.OutcomeCompleted)),
	}

	if got := ConversationOf(records).Value; len(got) != 0 {
		t.Fatalf("Conversation = %+v, want no messages for a stream with no conversation events", got)
	}
	if got := ConversationOf(records).Seq; got != 8 {
		t.Fatalf("Conversation Seq = %d, want the last consumed record 8", got)
	}
}
