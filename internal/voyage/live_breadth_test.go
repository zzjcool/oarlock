package voyage_test

// The issue #76 live acceptance: the breadth #71 left out. #71 recorded one
// bash call and its answer; this file records two tool calls in one turn, and
// an abort in the middle of a long turn, and probes the two shapes that need a
// native event Pi may not have at all (a sub-agent task and a provider retry).
//
// Every test here is opt-in (OAR_PI_LIVE=1) because it needs the real pi, its
// login and its session directory. The live fold is compared to the replayed
// fold before the file is written; with OAR_PI_RECORD=1 the normalised log
// replaces the committed fixture under testdata/voyage, which is the only way
// those files are produced. Without the gates the tests skip and the offline
// half in breadth_test.go replays the committed bytes.
//
// What cannot be triggered is reported, not invented: the sub-agent and retry
// probe runs a real turn, reads the real event stream, and skips with
// UNVERIFIED and the frames it saw when the runtime produces neither fact.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

const (
	lbMultiFixture = "pi-1.4.0-multi-tool-turn"
	lbAbortFixture = "pi-1.4.0-live-abort-turn"
	lbProbeFixture = "pi-1.4.0-task-probe" // recorded only when Pi has the tool
	lbRecorder     = "oarlock/voyage-live (pi 1.4.0, issue #76)"

	// lbFirstText and lbSecondText are the two commands' outputs, in the order
	// the prompt asks for them.
	lbFirstText  = "oarlock-tool-one"
	lbSecondText = "oarlock-tool-two"

	// lbMultiPrompt asks for two bash calls one at a time, then the outputs.
	lbMultiPrompt = "Run two bash commands, one tool call at a time, waiting for each result before starting the next: " +
		"first `echo " + lbFirstText + "`, then `echo " + lbSecondText + "`. " +
		"Then reply with exactly those two outputs, one per line, and nothing else."

	// lbAbortPrompt opens a turn that stays running long enough to abort: a
	// 30 second sleep the model has to wait for.
	lbAbortPrompt = "Run this exact bash command and wait for it to finish, then report what it printed. Command: sleep 30"
	lbAbortText   = "sleep 30"
)

// lbCall is one tool call as the stream reported it: the start and the end
// event, matched by call id.
type lbCall struct {
	Started *ltEvent
	Ended   *ltEvent
}

// lbToolCalls matches every tool_call_started with the tool_call_ended that
// carries the same call id, in first-start order, and fails on a call that
// started twice, ended twice or ended without having started.
func lbToolCalls(t *testing.T, recs []oarlock.Record) []lbCall {
	t.Helper()
	var calls []lbCall
	byID := map[string]*lbCall{}
	for _, ev := range ltEvents(recs) {
		e := ev
		switch ev.Kind {
		case oarlock.EventToolCallStarted:
			if ev.CallID == "" {
				t.Fatalf("tool call started without a call id at seq %d", ev.Seq)
			}
			if _, dup := byID[ev.CallID]; dup {
				t.Fatalf("tool call %q started twice (again at seq %d)", ev.CallID, ev.Seq)
			}
			calls = append(calls, lbCall{Started: &e})
			byID[ev.CallID] = &calls[len(calls)-1]
		case oarlock.EventToolCallEnded:
			c, ok := byID[ev.CallID]
			if !ok {
				t.Fatalf("tool call %q ended at seq %d without a start", ev.CallID, ev.Seq)
			}
			if c.Ended != nil {
				t.Fatalf("tool call %q ended twice", ev.CallID)
			}
			c.Ended = &e
		}
	}
	for i := range calls {
		if calls[i].Ended == nil {
			t.Fatalf("tool call %q started and never ended", calls[i].Started.CallID)
		}
		if calls[i].Started.Seq > calls[i].Ended.Seq {
			t.Fatalf("tool call %q ended (@%d) before it started (@%d)", calls[i].Started.CallID, calls[i].Ended.Seq, calls[i].Started.Seq)
		}
		if i > 0 && calls[i-1].Ended.Seq > calls[i].Started.Seq {
			t.Fatalf("tool calls overlapped: %q ended @%d after %q started @%d",
				calls[i-1].Started.CallID, calls[i-1].Ended.Seq, calls[i].Started.CallID, calls[i].Started.Seq)
		}
	}
	return calls
}

// lbToolText is the text one ended tool call reported.
func lbToolText(ev *ltEvent) string {
	var out strings.Builder
	for _, part := range ev.Content {
		out.WriteString(part.Text)
	}
	return out.String()
}

// lbMultiTurn pins the two-call evidence of one stream: two bash calls, one
// after the other, each with a reported exit code 0, the two outputs in the
// command's order, the answer after both calls, and the same order in the
// folded conversation.
func lbMultiTurn(t *testing.T, recs []oarlock.Record) {
	t.Helper()
	calls := lbToolCalls(t, recs)
	if len(calls) != 2 {
		t.Fatalf("%d tool calls, want 2", len(calls))
	}
	for i, want := range []string{lbFirstText, lbSecondText} {
		c := calls[i]
		if c.Started.Tool != "bash" {
			t.Fatalf("tool call %d = %q, want bash", i, c.Started.Tool)
		}
		if code, reported := c.Ended.ReportedExitCode(); !reported || code != 0 {
			t.Fatalf("tool call %d exit code = %d (reported %v), want a reported 0", i, code, reported)
		}
		if c.Ended.Result != oarlock.ToolOK {
			t.Fatalf("tool call %d result = %q, want %q", i, c.Ended.Result, oarlock.ToolOK)
		}
		if got := lbToolText(c.Ended); !strings.Contains(got, want) {
			t.Fatalf("tool call %d output = %q, want it to contain %q", i, got, want)
		}
	}

	// The answer comes after both calls, names the first output before the
	// second, and no full turn_ended got in between (both calls are one turn).
	var answer strings.Builder
	seenEnds := 0
	for _, ev := range ltEvents(recs) {
		switch ev.Kind {
		case oarlock.EventToolCallEnded:
			seenEnds++
		case oarlock.EventTextDelta:
			if seenEnds == 2 {
				answer.WriteString(ev.Text)
			}
		case oarlock.EventTurnEnded:
			if seenEnds < 2 {
				t.Fatalf("turn ended at seq %d after %d of 2 tool calls: the calls are not one turn", ev.Seq, seenEnds)
			}
		}
	}
	text := answer.String()
	first, second := strings.Index(text, lbFirstText), strings.Index(text, lbSecondText)
	if first < 0 || second < 0 || first > second {
		t.Fatalf("answer text = %q, want %q before %q", text, lbFirstText, lbSecondText)
	}

	conv := fold.Of(recs).Conversation().Value
	var user *fold.Message
	var tools []*fold.Message
	var reply strings.Builder
	for i := range conv {
		m := &conv[i]
		switch m.Role {
		case fold.RoleUser:
			if user == nil {
				user = m
			}
		case fold.RoleTool:
			tools = append(tools, m)
		case fold.RoleAssistant:
			reply.WriteString(m.Text)
		}
	}
	if user == nil || len(tools) != 2 {
		t.Fatalf("conversation = user %v, %d tool messages, want 1 user and 2 tool messages", user, len(tools))
	}
	for i, c := range calls {
		m := tools[i]
		if m.CallID != c.Started.CallID || m.Tool != "bash" {
			t.Fatalf("tool message %d = %+v, want call %q on bash", i, m, c.Started.CallID)
		}
		if m.Result != oarlock.ToolOK || !m.ExitCodeReported || m.ExitCode == nil || *m.ExitCode != 0 {
			t.Fatalf("tool message %d result/exit = %q/%v/%v", i, m.Result, m.ExitCode, m.ExitCodeReported)
		}
		want := []string{lbFirstText, lbSecondText}[i]
		if !strings.Contains(m.Output, want) {
			t.Fatalf("tool message %d output = %q, want it to contain %q", i, m.Output, want)
		}
		if user.Seq > m.Seq {
			t.Fatalf("tool message %d (@%d) precedes the user message (@%d)", i, m.Seq, user.Seq)
		}
		if i > 0 && tools[i-1].Seq > m.Seq {
			t.Fatalf("tool messages out of order: %d then %d", tools[i-1].Seq, m.Seq)
		}
	}
	got := reply.String()
	if f, s := strings.Index(got, lbFirstText), strings.Index(got, lbSecondText); f < 0 || s < 0 || f > s {
		t.Fatalf("conversation answer = %q, want %q before %q", got, lbFirstText, lbSecondText)
	}
}

// lbAbortTurn pins the abort evidence: exactly one turn was opened and exactly
// one ended, and it ended aborted — the runtime's own report, never a
// synthetic completion.
func lbAbortTurn(t *testing.T, recs []oarlock.Record) {
	t.Helper()
	active, ended := 0, 0
	var outcome *oarlock.TurnOutcome
	for _, ev := range ltEvents(recs) {
		switch ev.Kind {
		case oarlock.EventTurnActive:
			active++
		case oarlock.EventTurnEnded:
			ended++
			outcome = ev.Outcome
		}
	}
	if active == 0 || ended == 0 {
		t.Fatalf("turn events = %d active, %d ended, want one of each", active, ended)
	}
	if active != ended {
		t.Fatalf("%d turns started, %d ended: a turn was left open or synthesized", active, ended)
	}
	if outcome == nil || outcome.Kind != oarlock.OutcomeAborted {
		t.Fatalf("turn outcome = %+v, want aborted", outcome)
	}
	if fold.Of(recs).Status().Value.Kind == fold.StatusRunning {
		t.Fatal("status is still running after the abort turn ended")
	}
}

// TestLiveMultiToolTurnRecordsAndReplays records a real Pi turn that runs two
// bash calls, then proves the replayed log reproduces the live evidence.
func TestLiveMultiToolTurnRecordsAndReplays(t *testing.T) {
	if os.Getenv("OAR_PI_LIVE") != "1" {
		t.Skip("set OAR_PI_LIVE=1 to record a real Pi multi-tool turn")
	}
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skipf("pi is not on PATH: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	sess, cwd := ltSession(t, ctx)
	if out, err := sess.Prompt(ctx, lbMultiPrompt, oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	ltSettle(t, sess, time.Now().Add(4*time.Minute))
	if err := sess.Dispose(ctx); err != nil {
		t.Fatal(err)
	}
	records := sess.Records(oarlock.Cursor{})
	if len(records) == 0 {
		t.Fatal("the session retained no record")
	}

	lbMultiTurn(t, records)
	live := ltFold(records)
	if live.Status.Value.Kind != fold.StatusExited {
		t.Fatalf("live status = %s, want exited after dispose", live.Status.Value.Kind)
	}
	norm := ltNormaliseLive(t, records, "pi-live-"+lbMultiFixture, cwd)
	ltEqual(t, "live vs normalised", live, ltFold(norm))

	path := ltWriteAs(t, lbMultiFixture, norm, "disposed", lbRecorder)
	replayed := ltReplayNoProcess(t, path)
	ltEqual(t, "live vs replayed", live, ltFold(replayed))
	lbMultiTurn(t, replayed)
	t.Logf("recorded %d records (%d bytes) to %s", len(replayed), fileSize(t, path), path)
}

// TestLiveAbortTurnRecordsAndReplays opens a long turn, aborts it in the
// middle, and records what the runtime reported: turn_ended aborted.
func TestLiveAbortTurnRecordsAndReplays(t *testing.T) {
	if os.Getenv("OAR_PI_LIVE") != "1" {
		t.Skip("set OAR_PI_LIVE=1 to abort a real Pi turn")
	}
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skipf("pi is not on PATH: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	sess, cwd := ltSession(t, ctx)
	if out, err := sess.Prompt(ctx, lbAbortPrompt, oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	// Abort needs a running turn; waiting for the bash call as well keeps the
	// abort in the middle of real work instead of racing agent_start.
	lbWaitRunning(t, sess, time.Now().Add(2*time.Minute))
	lbWaitTool(t, sess, time.Now().Add(30*time.Second))
	out, err := sess.Abort(ctx)
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("abort = %+v, %v; want accepted", out, err)
	}
	ltSettle(t, sess, time.Now().Add(2*time.Minute))
	if code := fold.StatusOf(sess.Records(oarlock.Cursor{})).Value.LastTurnOutcome; code == nil || code.Kind != oarlock.OutcomeAborted {
		t.Fatalf("live turn outcome before dispose = %+v, want aborted", code)
	}
	if err := sess.Dispose(ctx); err != nil {
		t.Fatal(err)
	}
	records := sess.Records(oarlock.Cursor{})
	lbAbortTurn(t, records)
	live := ltFold(records)
	norm := ltNormaliseLive(t, records, "pi-live-"+lbAbortFixture, cwd)
	ltEqual(t, "live vs normalised abort", live, ltFold(norm))

	path := ltWriteAs(t, lbAbortFixture, norm, "disposed", lbRecorder)
	replayed := ltReplayNoProcess(t, path)
	ltEqual(t, "live vs replayed abort", live, ltFold(replayed))
	lbAbortTurn(t, replayed)
	t.Logf("recorded an aborted turn: %d records (%d bytes), %s", len(replayed), fileSize(t, path), path)
}

// TestLiveSubagentAndRetryProbe asks a real Pi for a sub-agent task and reads
// the stream: when Pi 1.4.0 has no such tool and sends no task or retry frame,
// the two shapes stay UNVERIFIED with the frames it did send, and nothing is
// recorded.
func TestLiveSubagentAndRetryProbe(t *testing.T) {
	if os.Getenv("OAR_PI_LIVE") != "1" {
		t.Skip("set OAR_PI_LIVE=1 to probe a real Pi for task and retry frames")
	}
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skipf("pi is not on PATH: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	sess, cwd := ltSession(t, ctx)
	prompt := "If you have a tool named subagent, task or agent to delegate a sub-agent task, call it once with the prompt " +
		"\"reply with the single word ok\". Otherwise reply with exactly NO_SUBAGENT_TOOL and nothing else."
	if out, err := sess.Prompt(ctx, prompt, oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	ltSettle(t, sess, time.Now().Add(3*time.Minute))
	if err := sess.Dispose(ctx); err != nil {
		t.Fatal(err)
	}
	records := sess.Records(oarlock.Cursor{})
	tasks, retries := 0, 0
	for _, ev := range ltEvents(records) {
		switch ev.Kind {
		case oarlock.EventTaskStarted, oarlock.EventTaskUpdated, oarlock.EventTaskEnded:
			tasks++
		case oarlock.EventRetry:
			retries++
		}
	}
	if tasks > 0 {
		// A real task stream is evidence: record it instead of reporting.
		live := ltFold(records)
		norm := ltNormaliseLive(t, records, "pi-live-"+lbProbeFixture, cwd)
		ltEqual(t, "live vs normalised task", live, ltFold(norm))
		path := ltWriteAs(t, lbProbeFixture, norm, "disposed", lbRecorder)
		ltEqual(t, "live vs replayed task", live, ltFold(ltReplayNoProcess(t, path)))
		t.Logf("recorded %d task events and %d retry events to %s", tasks, retries, path)
		return
	}
	t.Logf("live tool surface: %s", strings.Join(lbSlowTools(records), ", "))
	t.Skipf("UNVERIFIED: Pi 1.4.0 sent no task frame and no retry frame in a real turn (%d task events, %d retry events); "+
		"the system prompt offered these tools: %s — nothing safe to record",
		tasks, retries, strings.Join(lbSlowTools(records), ", "))
}

// lbWaitRunning blocks until the runtime reports a running turn.
func lbWaitRunning(t *testing.T, sess oarlock.Session, deadline time.Time) {
	t.Helper()
	ltWait(t, deadline, "a running turn", func() bool {
		return fold.StatusOf(sess.Records(oarlock.Cursor{})).Value.Kind == fold.StatusRunning
	})
}

// lbWaitTool blocks until a tool call has started in the running turn, which
// is the point the abort is aimed at.
func lbWaitTool(t *testing.T, sess oarlock.Session, deadline time.Time) {
	t.Helper()
	ltWait(t, deadline, "a tool call in the running turn", func() bool {
		for _, ev := range ltEvents(sess.Records(oarlock.Cursor{})) {
			if ev.Kind == oarlock.EventToolCallStarted {
				return true
			}
		}
		return false
	})
}

// lbSlowTools lists the tools the runtime's own system prompt offered, read
// out of the recorded frames: the evidence for "Pi has no sub-agent tool here".
func lbSlowTools(recs []oarlock.Record) []string {
	for _, rec := range recs {
		if rec.Kind != oarlock.KindFrame {
			continue
		}
		var frame struct {
			Type    string `json:"type"`
			Message struct {
				Role     string `json:"role"`
				Sections struct {
					Tools string `json:"tools"`
				} `json:"sections"`
			} `json:"message"`
		}
		if err := json.Unmarshal(rec.Native, &frame); err != nil || frame.Type != "message_start" || frame.Message.Role != "system" {
			continue
		}
		var names []string
		for _, line := range strings.Split(frame.Message.Sections.Tools, "\n") {
			line = strings.TrimSpace(line)
			name, _, ok := strings.Cut(strings.TrimPrefix(line, "- "), ":")
			if strings.HasPrefix(line, "- ") && ok && name != "" {
				names = append(names, name)
			}
		}
		return names
	}
	return nil
}
