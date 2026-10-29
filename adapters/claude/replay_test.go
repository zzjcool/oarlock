package claude

// Offline replay of REAL claude stream-json recordings (testdata/, copied from
// OAR tests/replay/fixtures and redacted with internal/redact). Every assertion
// is on a specific frame, so a change in the projection shows up as a named
// frame, not a diff.

import (
	"bytes"
	"os"
	"testing"

	"github.com/zzjcool/oarlock"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

func loadFrames(t *testing.T, name string) [][]byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
}

func kinds(f sj.Frame) (out []oarlock.EventKind) {
	for _, e := range f.Events {
		out = append(out, e.Kind)
	}
	return
}

func wantKinds(t *testing.T, i int, f sj.Frame, want ...oarlock.EventKind) {
	t.Helper()
	got := kinds(f)
	if len(got) != len(want) {
		t.Fatalf("frame %d (%s): events %v, want %v", i, f.Type, got, want)
	}
	for j := range got {
		if got[j] != want[j] {
			t.Fatalf("frame %d (%s): events %v, want %v", i, f.Type, got, want)
		}
	}
}

func TestReplayToolRound(t *testing.T) {
	fr := Project(loadFrames(t, "tool-round"), false)
	if len(fr) != 15 {
		t.Fatalf("frames = %d", len(fr))
	}
	wantKinds(t, 0, fr[0], oarlock.EventTurnActive) // an init nobody prompted
	for _, i := range []int{1, 3, 4, 5, 9, 10, 11} {
		wantKinds(t, i, fr[i]) // thinking_tokens: kept in Native, no event
	}
	wantKinds(t, 2, fr[2]) // rate_limit_event
	wantKinds(t, 6, fr[6], oarlock.EventReasoning)
	if fr[6].Events[0].Text == "" || fr[6].Events[0].Redacted {
		t.Fatalf("reasoning = %+v", fr[6].Events[0])
	}
	wantKinds(t, 7, fr[7], oarlock.EventToolCallStarted)
	if e := fr[7].Events[0]; e.CallID != "toolu_01DXx134zr68zT8tCZpRDiTG" || e.Tool != "Bash" ||
		e.Input != `{"command":"echo oar-replay-marker","description":"Echo the marker string"}` {
		t.Fatalf("tool_use = %+v", e)
	}
	wantKinds(t, 8, fr[8], oarlock.EventToolCallEnded)
	e := fr[8].Events[0]
	if e.CallID != "toolu_01DXx134zr68zT8tCZpRDiTG" || e.Result != oarlock.ToolOK ||
		len(e.Content) != 1 || e.Content[0].Type != "text" || e.Content[0].Text != "oar-replay-marker" {
		t.Fatalf("tool_result = %+v", e)
	}
	wantKinds(t, 13, fr[13], oarlock.EventTextDelta)
	if got := fr[13].Events[0].Text; got != "Done. I ran `echo oar-replay-marker` and it output the marker string." {
		t.Fatalf("text = %q", got)
	}
	wantKinds(t, 14, fr[14], oarlock.EventTurnEnded)
	if o := fr[14].Events[0].Outcome; o.Kind != oarlock.OutcomeCompleted || !fr[14].TurnEnded {
		t.Fatalf("result = %+v", o)
	}
}

func TestReplayMultiTurnTurnActiveOnlyForUnpromptedInit(t *testing.T) {
	fr := Project(loadFrames(t, "multi-turn"), false)
	// Two turns, two inits, two results. Nobody called Prompted, so each init is
	// a turn that nobody asked for.
	n := 0
	for i, f := range fr {
		for _, e := range f.Events {
			if e.Kind == oarlock.EventTurnEnded {
				n++
				if e.Outcome.Kind != oarlock.OutcomeCompleted {
					t.Fatalf("frame %d outcome %+v", i, e.Outcome)
				}
			}
		}
	}
	if n != 2 {
		t.Fatalf("turn_ended = %d, want 2", n)
	}
	wantKinds(t, 0, fr[0], oarlock.EventTurnActive)
	if fr[len(fr)-2].Events[0].Text != "banana" {
		t.Fatalf("second answer = %+v", fr[len(fr)-2].Events)
	}
}

// Acceptance 4 on a REAL recording: result.is_error=true with subtype "success".
func TestReplayErrorResultIsFailedDespiteSubtypeSuccess(t *testing.T) {
	fr := Project(loadFrames(t, "error"), false)
	if fr[2].Type != "result/success" {
		t.Fatalf("fixture frame 2 = %s", fr[2].Type)
	}
	wantKinds(t, 2, fr[2], oarlock.EventTurnEnded)
	o := fr[2].Events[0].Outcome
	if o.Kind != oarlock.OutcomeFailed {
		t.Fatalf("outcome = %+v, want failed", o)
	}
	// The recording carries no result text, status or category, so the class is
	// honestly unknown and the reason is the bare word: no guessing from prose.
	if o.Failure != oarlock.FailureUnknown || o.Reason != "error" {
		t.Fatalf("outcome = %+v", o)
	}
	wantKinds(t, 1, fr[1], oarlock.EventTextDelta)
}

// Acceptance 2: text, tools, terminal and usage on the real sub-agent +
// compaction recording, frame by frame.
func TestReplayUsageSubagentCompact(t *testing.T) {
	fr := Project(loadFrames(t, "usage-subagent"), false)
	if len(fr) != 32 {
		t.Fatalf("frames = %d", len(fr))
	}
	wantKinds(t, 0, fr[0], oarlock.EventTurnActive, oarlock.EventModel, oarlock.EventServiceTier)
	if fr[0].Events[1].Model != "claude-haiku-4-5-20251001" || fr[0].Events[2].ServiceTier != "default" {
		t.Fatalf("init = %+v", fr[0].Events)
	}
	// A redacted-signature thinking block with empty text: a reasoning event, not dropped.
	wantKinds(t, 7, fr[7], oarlock.EventReasoning)

	// The root issues the Agent tool call; the sub-agent's frames carry its path.
	wantKinds(t, 8, fr[8], oarlock.EventToolCallStarted)
	agent := fr[8].Events[0].CallID
	if fr[8].Events[0].Tool != "Agent" || len(fr[8].AgentPath) != 0 {
		t.Fatalf("root tool_use = %+v path %v", fr[8].Events[0], fr[8].AgentPath)
	}
	for _, i := range []int{10, 13, 14} {
		if len(fr[i].AgentPath) != 1 || fr[i].AgentPath[0] != agent {
			t.Fatalf("frame %d path = %v, want [%s]", i, fr[i].AgentPath, agent)
		}
	}
	wantKinds(t, 13, fr[13], oarlock.EventToolCallStarted)
	wantKinds(t, 14, fr[14], oarlock.EventToolCallEnded)

	// Tasks.
	wantKinds(t, 9, fr[9], oarlock.EventTaskStarted)
	if k := fr[9].Events[0].Task; k.TaskID != "a43327fbf12ef25c9" || k.TaskType != "agent" || k.NativeType != "local_agent" || k.ToolCallID != agent {
		t.Fatalf("task_started = %+v", k)
	}
	wantKinds(t, 12, fr[12], oarlock.EventTaskUpdated)
	wantKinds(t, 15, fr[15], oarlock.EventTaskUpdated)
	if fr[15].Events[0].Task.Status != oarlock.TaskCompleted {
		t.Fatalf("task_updated = %+v", fr[15].Events[0].Task)
	}
	wantKinds(t, 16, fr[16], oarlock.EventTaskEnded)
	if k := fr[16].Events[0].Task; k.Status != oarlock.TaskCompleted || k.Summary == "" {
		t.Fatalf("task_ended = %+v", k)
	}

	// Turn 1's result: main loop (result.usage) per agent, modelUsage the total.
	wantKinds(t, 20, fr[20], oarlock.EventServiceTier, oarlock.EventTurnEnded, oarlock.EventUsage)
	u := fr[20].Events[2].Usage
	// result.usage: 18 + 35315 cache read + 8543 cache write; output 834.
	if u.Tokens == nil || u.Tokens.Input != 18+35315+8543 || u.Tokens.Output != 834 ||
		*u.Tokens.CacheRead != 35315 || *u.Tokens.CacheWrite != 8543 {
		t.Fatalf("main-loop tokens = %+v", u.Tokens)
	}
	// modelUsage: 36 + 47627 + 21484; output 1011. Larger than the main loop:
	// the difference is the sub-agent's spend (Unattributed).
	if u.Total == nil || u.Total.Input != 36+47627+21484 || u.Total.Output != 1011 {
		t.Fatalf("total = %+v", u.Total)
	}
	if u.Total.Input <= u.Tokens.Input {
		t.Fatal("sub-agent spend must make the total exceed the main loop")
	}
	// Context: the root's LATEST call (frame 19): 8 + 21416 + 1026 of a 200000 window.
	if u.Context == nil || *u.Context.Tokens != 22450 || *u.Context.ContextWindow != 200000 || *u.Context.Percent != 11 {
		t.Fatalf("context = %+v", u.Context)
	}

	// Turn 2 sums the main loop and replaces the total (cumulative, never summed).
	wantKinds(t, 24, fr[24], oarlock.EventServiceTier, oarlock.EventTurnEnded, oarlock.EventUsage)
	u2 := fr[24].Events[2].Usage
	if u2.Tokens.Input != u.Tokens.Input+10+22442+111 || u2.Tokens.Output != 834+29 {
		t.Fatalf("turn 2 main loop = %+v", u2.Tokens)
	}
	if u2.Total.Input != 46+70069+21595 || u2.Total.Output != 1040 {
		t.Fatalf("turn 2 total = %+v", u2.Total)
	}

	// Compaction: boundary completed; the context is unknown until the next call.
	wantKinds(t, 28, fr[28], oarlock.EventCompactionEnded)
	if e := fr[28].Events[0]; e.CompactionResult != oarlock.CompactionCompleted || e.Trigger != "manual" {
		t.Fatalf("compaction = %+v", e)
	}
	wantKinds(t, 31, fr[31], oarlock.EventServiceTier, oarlock.EventTurnEnded, oarlock.EventUsage)
	u3 := fr[31].Events[2].Usage
	if u3.Context == nil || u3.Context.Tokens != nil || u3.Context.Percent != nil || *u3.Context.ContextWindow != 200000 {
		t.Fatalf("post-compaction context = %+v, want unknown tokens with the window", u3.Context)
	}
	// A compaction (an internal call) grows modelUsage and leaves result.usage alone.
	if u3.Total.Input != 1517+92511+21669 || u3.Total.Output != 2213 || u3.Tokens.Input != u2.Tokens.Input {
		t.Fatalf("turn 3 usage = %+v / %+v", u3.Tokens, u3.Total)
	}
}

// The steer recording: the replayed user message of the steer is the echo, with
// the uuid the host wrote and the text.
func TestReplaySteerEchoAndPartialStreaming(t *testing.T) {
	fr := Project(loadFrames(t, "steer-lifecycle"), false)
	var echo *oarlock.Event
	var started, deltas int
	var input string
	for _, f := range fr {
		for i, e := range f.Events {
			switch e.Kind {
			case oarlock.EventUserMessage:
				if e.InputID == "6f1c2d3e-00b2-4a5b-8c7d-0000000000b2" {
					echo = &f.Events[i]
				}
			case oarlock.EventToolCallStarted:
				started++
			case oarlock.EventToolCallInputDelta:
				deltas++
				input += e.Delta
			case oarlock.EventToolCallInput:
				// the completed block replaces the fragments
				if input != "" && e.Input == "" {
					t.Fatal("empty complete input")
				}
			}
		}
	}
	if echo == nil || echo.Text != "STEER-INPUT: also say steer-ack" || echo.Evidence != oarlock.EvidenceAcknowledged ||
		echo.NativeMessageID != echo.InputID {
		t.Fatalf("steer echo = %+v", echo)
	}
	if started != 1 {
		t.Fatalf("tool_call_started = %d: the completed assistant block must not announce the call twice", started)
	}
	if want := `{"command":"timeout 4 tail -f /dev/null; echo slow-done","description":"Wait four seconds"}`; input != want {
		t.Fatalf("fragments = %q", input)
	}
	// The streamed text is not repeated by the completed assistant frame.
	var text string
	for _, f := range fr {
		for _, e := range f.Events {
			if e.Kind == oarlock.EventTextDelta {
				text += e.Text
			}
		}
	}
	if text != "first-done" {
		t.Fatalf("assistant text = %q, want first-done exactly once", text)
	}
}

// The interrupt recording: the control_response is a response to OUR request,
// the aborted turn's result is is_error with error_during_execution, and a
// requested abort turns it into aborted.
func TestReplayInterruptResult(t *testing.T) {
	frames := loadFrames(t, "interrupt")
	var resp *sj.ControlResponse
	f := sj.NewFolder(newProfile(false))
	f.Prompted()
	f.AbortRequested()
	var outcome *oarlock.TurnOutcome
	for _, raw := range frames {
		fr := f.Fold(raw)
		if fr.Response != nil {
			resp = fr.Response
		}
		for _, e := range fr.Events {
			if e.Kind == oarlock.EventTurnEnded {
				outcome = e.Outcome
			}
		}
	}
	if resp == nil || resp.RequestID != "interrupt-1" || !resp.OK {
		t.Fatalf("control_response = %+v", resp)
	}
	if outcome == nil || outcome.Kind != oarlock.OutcomeAborted {
		t.Fatalf("outcome = %+v, want aborted", outcome)
	}
	// Without the abort intent the very same result is a failure.
	plain := Project(frames, false)
	for _, fr := range plain {
		for _, e := range fr.Events {
			if e.Kind == oarlock.EventTurnEnded && e.Outcome.Kind != oarlock.OutcomeFailed {
				t.Fatalf("an is_error result with no abort requested = %+v, want failed", e.Outcome)
			}
		}
	}
}

func TestReplayUnknownTopLevelTypesAreCounted(t *testing.T) {
	f := sj.NewFolder(newProfile(false))
	for _, raw := range loadFrames(t, "usage-subagent") {
		f.Fold(raw)
	}
	if u := f.Unknown(); len(u) != 0 {
		t.Fatalf("known recording tallied unknown types: %v", u)
	}
	f.Fold([]byte(`{"type":"brand_new_frame"}`))
	if f.Unknown()["brand_new_frame"] != 1 {
		t.Fatalf("unknown = %v", f.Unknown())
	}
}
