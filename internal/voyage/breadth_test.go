package voyage_test

// Issue #76's committed evidence: the breadth #71 did not record.
//
//   - testdata/voyage/pi-1.4.0-multi-tool-turn.jsonl: a real Pi 1.4.0 turn with
//     two bash calls in one turn (echo oarlock-tool-one, echo oarlock-tool-two),
//     recorded by TestLiveMultiToolTurnRecordsAndReplays;
//   - testdata/voyage/pi-1.4.0-live-abort-turn.jsonl: a real Pi 1.4.0 turn
//     aborted while its bash `sleep 30` was running, recorded by
//     TestLiveAbortTurnRecordsAndReplays. The name carries "live" because
//     pi-1.4.0-abort-turn.jsonl is the stand-in fixture built from
//     testdata/pi, not a recording.
//
// These tests are the offline half: they replay the committed bytes with no
// runtime, no network and no process, pin the numbers the live sessions
// reported, and assert the call order, both exit codes and the text order. The
// sub-agent and retry claims are pinned here too, against the bytes: the
// recording has neither fact, and its own system prompt says why.

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
	"github.com/zzjcool/oarlock/internal/voyage"
)

const (
	multiFixture = "../../testdata/voyage/" + lbMultiFixture + ".jsonl"
	abortFixture = "../../testdata/voyage/" + lbAbortFixture + ".jsonl"

	// The call ids and inputs are the live session's own; they are evidence,
	// not something to normalise away.
	multiFirstCall   = "call_00_bxNqTunkkPDRW6iNT9J79628|fc_18e3036244e05ebf000093fe"
	multiSecondCall  = "call_00_ET_01adJxVl6mEBBXb4nrG65041|fc_18e30362ed4b135400009402"
	multiFirstInput  = `{"command":"echo oarlock-tool-one"}`
	multiSecondInput = `{"command":"echo oarlock-tool-two"}`
	abortCallID      = "call_00_XyaFVDdNMWvqjapHuMWF1964|fc_18e30365aac667fe00009407"
	abortCallInput   = `{"command":"sleep 30","timeout":60}`
	abortCallText    = "Command aborted"
)

// breadthPinned pins the fold answers of one committed live fixture: exited
// after dispose, the turn outcome, the token totals and the context figure the
// live session reported.
func breadthPinned(t *testing.T, a ltAnswers, outcome oarlock.OutcomeKind, tokens *oarlock.TokenTotals, ctx int64, pct float64) {
	t.Helper()
	st := a.Status.Value
	if st.Kind != fold.StatusExited {
		t.Fatalf("status = %s, want exited (the recording ends with the runtime exit)", st.Kind)
	}
	if st.LastTurnOutcome == nil || st.LastTurnOutcome.Kind != outcome {
		t.Fatalf("last turn outcome = %+v, want %s", st.LastTurnOutcome, outcome)
	}
	if !reflect.DeepEqual(a.Usage.Value.Total, tokens) {
		t.Fatalf("usage total = %s, want %s", goldenJSON(t, a.Usage.Value.Total), goldenJSON(t, tokens))
	}
	c := a.Context.Value
	if c == nil || c.Usage.Tokens == nil || c.Usage.ContextWindow == nil || c.Usage.Percent == nil {
		t.Fatalf("context usage missing: %s", goldenJSON(t, c))
	}
	if *c.Usage.Tokens != ctx || *c.Usage.ContextWindow != 1000000 || math.Abs(*c.Usage.Percent-pct) > 1e-9 {
		t.Fatalf("context = %s, want tokens %d of 1000000 = %v%%", goldenJSON(t, c), ctx, pct)
	}
}

func TestCommittedMultiToolTurnPin(t *testing.T) {
	recs := ltReplayNoProcess(t, multiFixture)
	log, err := voyage.Read(multiFixture)
	if err != nil {
		t.Fatal(err)
	}
	if !log.Complete() || log.End.Reason != "disposed" {
		t.Fatalf("log complete=%v end=%+v, want a complete log ended by dispose", log.Complete(), log.End)
	}
	if log.Header.Runtime != "pi" || log.Header.Cwd != "." || log.Header.SessionID != "pi-live-"+lbMultiFixture {
		t.Fatalf("header = %+v", log.Header)
	}
	if !strings.Contains(log.Header.Recorder, "#76") {
		t.Fatalf("recorder = %q, want it to name issue #76", log.Header.Recorder)
	}

	breadthPinned(t, ltFold(recs), oarlock.OutcomeCompleted,
		&oarlock.TokenTotals{Input: 32668, Output: 172, Reasoning: turnI64(72), CacheRead: turnI64(24192), CacheWrite: turnI64(0)},
		10990, 1.099)

	// Two bash calls in one turn: one turn opened, one ended, both calls inside.
	// A second turn_ended here would mean the second call started a new turn.
	calls := lbToolCalls(t, recs)
	if len(calls) != 2 {
		t.Fatalf("%d tool calls, want 2", len(calls))
	}
	if calls[0].Started.CallID != multiFirstCall || calls[1].Started.CallID != multiSecondCall {
		t.Fatalf("call ids = %q, %q", calls[0].Started.CallID, calls[1].Started.CallID)
	}
	if calls[0].Started.Input != multiFirstInput || calls[1].Started.Input != multiSecondInput {
		t.Fatalf("call inputs = %q, %q", calls[0].Started.Input, calls[1].Started.Input)
	}
	for _, c := range calls {
		if code, reported := c.Ended.ReportedExitCode(); !reported || code != 0 {
			t.Fatalf("call %q exit = %d (reported %v), want a reported 0", c.Started.CallID, code, reported)
		}
	}
	if got := lbToolText(calls[0].Ended); got != lbFirstText+"\n" {
		t.Fatalf("first call text = %q", got)
	}
	if got := lbToolText(calls[1].Ended); got != lbSecondText+"\n" {
		t.Fatalf("second call text = %q", got)
	}

	// The shared shape assertions: both calls in order, one after the other,
	// each exit 0, the answer after both, the same order in the conversation.
	lbMultiTurn(t, recs)

	// Written once, read back: the incremental fold and the batch fold agree on
	// the committed bytes.
	goldenEqual(t, "fold.Of vs Feed", goldenFoldAll(recs), goldenFeed(recs))

	// A tool turn, not a compaction.
	for _, ev := range ltEvents(recs) {
		if ev.Kind == oarlock.EventCompactionStarted || ev.Kind == oarlock.EventCompactionEnded {
			t.Fatalf("compaction event %s at seq %d in the multi-tool fixture", ev.Kind, ev.Seq)
		}
	}

	// The answer is the two outputs, one per line, in the command's order.
	var answer strings.Builder
	for _, ev := range ltEvents(recs) {
		if ev.Kind == oarlock.EventTextDelta {
			answer.WriteString(ev.Text)
		}
	}
	if got, want := answer.String(), lbFirstText+"\n"+lbSecondText; got != want {
		t.Fatalf("answer text = %q, want %q", got, want)
	}
}

func TestCommittedAbortTurnPin(t *testing.T) {
	recs := ltReplayNoProcess(t, abortFixture)
	log, err := voyage.Read(abortFixture)
	if err != nil {
		t.Fatal(err)
	}
	if !log.Complete() || log.End.Reason != "disposed" {
		t.Fatalf("log complete=%v end=%+v, want a complete log ended by dispose", log.Complete(), log.End)
	}
	if log.Header.SessionID != "pi-live-"+lbAbortFixture {
		t.Fatalf("header session = %q", log.Header.SessionID)
	}
	if !strings.Contains(log.Header.Recorder, "#76") {
		t.Fatalf("recorder = %q, want it to name issue #76", log.Header.Recorder)
	}

	breadthPinned(t, ltFold(recs), oarlock.OutcomeAborted,
		&oarlock.TokenTotals{Input: 10746, Output: 84, Reasoning: turnI64(28), CacheRead: turnI64(2688), CacheWrite: turnI64(0)},
		10834, 1.0834)

	// One turn opened, one ended, and it ended aborted. The abort request is in
	// the log, before the end: the stop is the host's, and the outcome is the
	// runtime's own report.
	var turnEnd, toolStart, toolEnd *ltEvent
	active, ended := 0, 0
	for _, ev := range ltEvents(recs) {
		e := ev
		switch ev.Kind {
		case oarlock.EventTurnActive:
			active++
		case oarlock.EventTurnEnded:
			ended++
			turnEnd = &e
		case oarlock.EventToolCallStarted:
			toolStart = &e
		case oarlock.EventToolCallEnded:
			toolEnd = &e
		}
	}
	if active != 1 || ended != 1 {
		t.Fatalf("turn events = %d active, %d ended, want exactly one of each", active, ended)
	}
	if turnEnd.Outcome == nil || turnEnd.Outcome.Kind != oarlock.OutcomeAborted {
		t.Fatalf("turn_ended outcome = %+v, want aborted", turnEnd.Outcome)
	}
	if turnEnd.Outcome.Failure != "" || turnEnd.Outcome.Reason != "" {
		t.Fatalf("aborted outcome carries words of its own: %+v", turnEnd.Outcome)
	}
	var abortSeq uint64
	for _, r := range recs {
		if r.Kind == oarlock.KindRequest && r.Request != nil && r.Request.Action == oarlock.ActionAbort {
			abortSeq = r.Seq
		}
	}
	if abortSeq == 0 {
		t.Fatal("no abort request record: the fixture does not show who stopped the turn")
	}
	if abortSeq >= turnEnd.Seq {
		t.Fatalf("abort request @%d is not before turn_ended @%d", abortSeq, turnEnd.Seq)
	}

	// The tool call the abort interrupted: started, then ended as failed with
	// the runtime's "Command aborted" text and no exit code at all — Pi
	// reported a signal, not a number, and the fixture keeps that.
	if toolStart == nil || toolEnd == nil {
		t.Fatalf("tool evidence missing: started=%v ended=%v", toolStart, toolEnd)
	}
	if toolStart.CallID != abortCallID || toolStart.Input != abortCallInput || toolStart.Tool != "bash" {
		t.Fatalf("interrupted call = %+v", toolStart)
	}
	if toolStart.Seq > toolEnd.Seq || toolEnd.Seq > turnEnd.Seq {
		t.Fatalf("order = started@%d ended@%d turn_ended@%d", toolStart.Seq, toolEnd.Seq, turnEnd.Seq)
	}
	if toolEnd.Result != oarlock.ToolFailed {
		t.Fatalf("interrupted call result = %q, want %q", toolEnd.Result, oarlock.ToolFailed)
	}
	if _, reported := toolEnd.ReportedExitCode(); reported {
		t.Fatalf("interrupted call reported an exit code: %+v", toolEnd.ExitCode)
	}
	if got := lbToolText(toolEnd); got != abortCallText {
		t.Fatalf("interrupted call text = %q, want %q", got, abortCallText)
	}

	// The conversation keeps the same shape: user, the interrupted call, and no
	// assistant answer — the model never got to speak.
	var tools int
	var answer strings.Builder
	for _, msg := range fold.Of(recs).Conversation().Value {
		switch msg.Role {
		case fold.RoleTool:
			tools++
			if msg.CallID != abortCallID || msg.ExitCodeReported || msg.Result != oarlock.ToolFailed {
				t.Fatalf("tool message = %+v", msg)
			}
		case fold.RoleAssistant:
			answer.WriteString(msg.Text)
		}
	}
	if tools != 1 {
		t.Fatalf("%d tool messages, want 1", tools)
	}
	// This session planned the call in reasoning and answered nothing before it
	// started, so any assistant text at all would be an answer the abort never
	// allowed.
	if got := answer.String(); got != "" {
		t.Fatalf("assistant text = %q, want none: the abort ended the turn while the call ran", got)
	}
	if fold.Of(recs).Status().Value.Kind == fold.StatusRunning {
		t.Fatal("status is still running after the abort")
	}
	goldenEqual(t, "fold.Of vs Feed", goldenFoldAll(recs), goldenFeed(recs))
}

// TestCommittedLiveFixturesHaveNoTaskOrRetryFacts pins the two UNVERIFIED
// claims to committed bytes instead of prose. Pi 1.4.0 exposed eight tools and
// none of them delegates to a sub-agent; the pi adapter has no task_* case at
// all (adapters/pi/project.go), and its only retry frames are
// `auto_retry_start` / `summarization_retry_scheduled`, which need a failing
// provider to fire. So a real turn has neither fact to record, and this test
// fails if one ever appears in a live fixture without the offline half being
// written for it.
func TestCommittedLiveFixturesHaveNoTaskOrRetryFacts(t *testing.T) {
	paths := map[string]string{
		ltToolFixture:  "../../testdata/voyage/" + ltToolFixture + ".jsonl",
		lbMultiFixture: multiFixture,
		lbAbortFixture: abortFixture,
	}
	wantTools := []string{"read", "bash", "edit", "write", "codemode", "sop_search", "sop_save", "tool_search"}
	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			recs := ltReplayNoProcess(t, path)
			for _, ev := range ltEvents(recs) {
				switch ev.Kind {
				case oarlock.EventTaskStarted, oarlock.EventTaskUpdated, oarlock.EventTaskEnded:
					t.Fatalf("task event %s at seq %d: record the task stream before claiming one", ev.Kind, ev.Seq)
				case oarlock.EventRetry:
					t.Fatalf("retry event at seq %d: record the retry before claiming one", ev.Seq)
				}
			}
			// The projection is deliberately silent for some retry frames
			// (auto_retry_end, summarization_retry_*), so the claim is checked
			// on the wire type as well, not only on the mapped events.
			for _, r := range recs {
				if r.Kind != oarlock.KindFrame {
					continue
				}
				var head struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(r.Native, &head); err == nil && strings.Contains(strings.ToLower(head.Type), "retry") {
					t.Fatalf("native retry frame %q at seq %d: record it before claiming there is none", head.Type, r.Seq)
				}
			}
			got := lbSlowTools(recs)
			for _, tool := range got {
				if lbDelegatingTool(tool) {
					t.Fatalf("tool surface offers %q: a sub-agent task is now triggerable and must be recorded, not left UNVERIFIED", tool)
				}
			}
			if !reflect.DeepEqual(got, wantTools) {
				t.Fatalf("recorded tool surface = %v, want %v (a change here is what would make a sub-agent measurable)", got, wantTools)
			}
		})
	}
}

// TestBreadthFixturesAreIndexed makes sure the two new fixtures reach the
// committed index, the credential scan and the size cap through the same code
// path as every other fixture.
func TestBreadthFixturesAreIndexed(t *testing.T) {
	if err := voyage.VerifyIndex("../../testdata/voyage"); err != nil {
		t.Fatalf("index: %v", err)
	}
	idx, err := voyage.BuildIndex("../../testdata/voyage")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]voyage.IndexEntry{}
	for _, e := range idx.Fixtures {
		found[e.Name] = e
	}
	for _, name := range []string{lbMultiFixture, lbAbortFixture} {
		e, ok := found[name]
		if !ok {
			t.Fatalf("%s is not in the committed index", name)
		}
		if !e.Complete || e.EndReason != "disposed" || e.Runtime != "pi" {
			t.Fatalf("%s index entry = %+v", name, e)
		}
		if e.Bytes > voyage.MaxFixtureBytes {
			t.Fatalf("%s is %d bytes, over the %d byte cap", name, e.Bytes, voyage.MaxFixtureBytes)
		}
		if e.SessionID != "pi-live-"+name {
			t.Fatalf("%s session = %q", name, e.SessionID)
		}
	}
	if err := voyage.CheckFixture(multiFixture); err != nil {
		t.Fatal(err)
	}
	if err := voyage.CheckFixture(abortFixture); err != nil {
		t.Fatal(err)
	}
}
