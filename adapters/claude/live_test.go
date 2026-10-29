package claude

// The live claude tests: the claims only a real binary can prove. Opt-in
// (OAR_CLAUDE_LIVE=1 plus a claude on PATH and credentials in the environment);
// without them they skip as NOT VERIFIED, and nothing else in this file turns a
// failure into a skip.
//
//   - TestLiveSteerEcho: a mid-turn user message inside a tool-backed turn (a
//     request boundary) is echoed back, and the adapter accepts it only on that
//     echo. Live probe evidence 2026-10-29 (claude 2.1.312, third-party
//     Anthropic-compatible endpoint): a steer written after system/task_started
//     during `Bash sleep 8` is echoed ~8.5 s later; a steer inside a one-message
//     text answer has no boundary, is queued natively and applied as a
//     follow-up turn after the result, so it can only be rejected.
//   - TestLivePromptAndWait: the public oarlock.PromptAndWait reads a completed
//     turn off the same stream.
//
// Both open their own session, bound every wait, and print redacted diagnostics
// on failure (user frames with isReplay/uuid, command_lifecycle states, result
// subtypes) — never the environment or a credential.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

const (
	// liveTurnBound bounds one whole live test: open, turn, steer/echo.
	liveTurnBound = 150 * time.Second
	// liveOpenBound bounds the opening handshake (a hang there is a failure).
	liveOpenBound = 60 * time.Second
	// liveBoundaryWait is how long a prompt gets to reach a tool/request
	// boundary before the steer leg gives up.
	liveBoundaryWait = 60 * time.Second

	// liveSteerPrompt asks for a tool round on purpose: the request boundary is
	// what makes a mid-turn user message land inside the running turn.
	liveSteerPrompt = "Use the Bash tool to run exactly: sleep 4. Then reply with the single word: done."
	// liveSteerInput is the steer text; its echo is the acceptance evidence.
	liveSteerInput = "STEER-LIVE: also say banana."
	// livePongPrompt is the PromptAndWait leg: one short answer, no tools.
	livePongPrompt = "Reply with the single word: pong"
)

// TestLiveSteerEcho proves the steer echo on a real claude: the steer is
// written inside a tool-backed turn, so claude applies it to the active request
// and replays the user frame with the same uuid. Acceptance without that echo is
// never reported.
func TestLiveSteerEcho(t *testing.T) {
	sess := liveOpen(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTurnBound)
	defer cancel()

	if out, err := sess.Prompt(ctx, liveSteerPrompt, oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt rejected: %+v (err %v)\n%s", out, err, liveDiagnostics(sess, "", liveSteerPrompt))
	}
	// The request boundary: claude is inside a Bash call, so the mid-turn user
	// message goes to the active request instead of being queued for a
	// follow-up turn (which would only echo after the result).
	waitLiveBoundary(t, sess, liveBoundaryWait)

	steerID := newUUID()
	out, err := sess.Steer(ctx, liveSteerInput, oarlock.InputOptions{InputID: steerID})
	if err != nil {
		t.Fatalf("Steer: %v\n%s", err, liveDiagnostics(sess, steerID, liveSteerPrompt))
	}
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("live steer rejected: %+v (reason %q)\n%s", out, out.Reason, liveDiagnostics(sess, steerID, liveSteerPrompt))
	}
	// Acceptance is not the proof: the replayed user frame with the steer uuid is.
	var echo *oarlock.Event
	for _, r := range sess.Records(oarlock.Cursor{}) {
		for _, e := range r.Events {
			if e.Kind == oarlock.EventUserMessage && e.InputID == steerID {
				ev := e
				echo = &ev
			}
		}
	}
	if echo == nil {
		t.Fatalf("steer accepted without an echoed user_message\n%s", liveDiagnostics(sess, steerID, liveSteerPrompt))
	}
	if echo.Evidence != oarlock.EvidenceAcknowledged || !strings.Contains(echo.Text, "STEER-LIVE") {
		t.Fatalf("echo event = %+v\n%s", echo, liveDiagnostics(sess, steerID, liveSteerPrompt))
	}
	logSteerTimeline(t, sess, steerID)
}

// TestLivePromptAndWait proves the public convenience on a real claude: one
// prompt, the runtime's own completed turn, and the streamed text.
func TestLivePromptAndWait(t *testing.T) {
	sess := liveOpen(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTurnBound)
	defer cancel()

	run, err := oarlock.PromptAndWait(ctx, sess, livePongPrompt, oarlock.WithTimeout(90*time.Second))
	if err != nil {
		t.Fatalf("PromptAndWait: %v\n%s", err, liveDiagnostics(sess, "", livePongPrompt))
	}
	if run.Outcome != oarlock.RunCompleted {
		t.Fatalf("run outcome = %q (turn %+v, reason %q)\n%s", run.Outcome, run.Turn, run.Reason, liveDiagnostics(sess, "", livePongPrompt))
	}
	if run.Turn == nil || run.Turn.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("turn = %+v, want a completed turn\n%s", run.Turn, liveDiagnostics(sess, "", livePongPrompt))
	}
	if !strings.Contains(strings.ToLower(run.Text), "pong") {
		t.Fatalf("text = %q, want it to contain pong\n%s", run.Text, liveDiagnostics(sess, "", livePongPrompt))
	}
	t.Logf("PromptAndWait completed the turn; prompt answered at seq %d; %d records", run.Seq, len(sess.Records(oarlock.Cursor{})))
}

// liveOpen starts one real claude session in a scratch cwd and disposes it with
// a bounded context. The only skips are "not opted in" and "no binary".
func liveOpen(t *testing.T) *Session {
	t.Helper()
	if os.Getenv("OAR_CLAUDE_LIVE") != "1" {
		t.Skip("NOT VERIFIED: set OAR_CLAUDE_LIVE=1 (needs a claude binary and credentials) to run against live claude")
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("NOT VERIFIED: no claude binary on PATH: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveOpenBound)
	defer cancel()
	sess, err := Open(ctx, oarlock.Installation{Available: true, Path: path}, oarlock.Options{
		Cwd: t.TempDir(),
		Env: map[string]*string{"IS_SANDBOX": one()},
	})
	if err != nil {
		t.Fatalf("Open live claude: %v", err)
	}
	s, ok := sess.(*Session)
	if !ok {
		t.Fatalf("Open returned %T, not *claude.Session", sess)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.Dispose(ctx)
	})
	return s
}

// waitLiveBoundary waits until the turn is inside a tool call: a started task or
// a tool call is the model-request boundary a mid-turn steer needs. A turn with
// none (a single streaming text answer) cannot prove the echo, so it fails and
// says which frames did arrive.
func waitLiveBoundary(t *testing.T, sess oarlock.Session, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		var tasks, tools int
		for _, r := range sess.Records(oarlock.Cursor{}) {
			m := sj.ParseRec(r.Native)
			if sj.Str(m["type"]) == "system" && sj.Str(m["subtype"]) == "task_started" {
				tasks++
			}
			for _, e := range r.Events {
				switch e.Kind {
				case oarlock.EventTaskStarted:
					tasks++
				case oarlock.EventToolCallStarted:
					tools++
				}
			}
		}
		if tasks > 0 || tools > 0 {
			t.Logf("request boundary reached: %d task_started, %d tool_call_started frames", tasks, tools)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no tool task start within %v: the prompt produced no request boundary, so a mid-turn steer would only be queued\n%s",
				d, liveDiagnostics(sess, "", liveSteerPrompt))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// liveDiagnostics is the redacted evidence a failing live test reports: the
// prompt, each user frame's isReplay/uuid, each command_lifecycle command_uuid
// and state, each result subtype/is_error and each system frame's subtype. It
// prints frame metadata only — no environment, no credential, no frame payload.
func liveDiagnostics(sess oarlock.Session, steerID, prompt string) string {
	var b strings.Builder
	b.WriteString("--- live diagnostics (redacted) ---\n")
	if prompt != "" {
		fmt.Fprintf(&b, "prompt: %q\n", prompt)
	}
	if steerID != "" {
		b.WriteString("steer uuid: " + steerID + "\n")
	}
	rs := sess.Records(oarlock.Cursor{})
	fmt.Fprintf(&b, "records: %d\n", len(rs))
	for _, r := range rs {
		m := sj.ParseRec(r.Native)
		switch sj.Str(m["type"]) {
		case "user":
			isReplay, _ := m["isReplay"].(bool)
			fmt.Fprintf(&b, "  seq %d user isReplay=%v uuid=%s parent_tool_use_id=%v\n",
				r.Seq, isReplay, sj.Str(m["uuid"]), frameValue(m["parent_tool_use_id"]))
		case "command_lifecycle":
			fmt.Fprintf(&b, "  seq %d command_lifecycle command_uuid=%s state=%s\n",
				r.Seq, sj.Str(m["command_uuid"]), sj.Str(m["state"]))
		case "result":
			isErr, _ := m["is_error"].(bool)
			fmt.Fprintf(&b, "  seq %d result subtype=%s is_error=%v terminal_reason=%q\n",
				r.Seq, sj.Str(m["subtype"]), isErr, sj.Str(m["terminal_reason"]))
		case "system":
			fmt.Fprintf(&b, "  seq %d system subtype=%s task_id=%q\n",
				r.Seq, sj.Str(m["subtype"]), sj.Str(m["task_id"]))
		}
	}
	b.WriteString("--- end diagnostics ---")
	return b.String()
}

// frameValue renders a frame field for a diagnostic line without assuming it is
// a string (parent_tool_use_id is null on root frames).
func frameValue(v any) string {
	if v == nil {
		return "null"
	}
	if s, ok := v.(string); ok && s == "" {
		return `""`
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	s := string(b)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// logSteerTimeline logs, with record sequence numbers and wall-clock offsets,
// what claude said about one steer uuid: its command_lifecycle states and the
// replayed user frame. Values only; no payloads.
func logSteerTimeline(t *testing.T, sess oarlock.Session, steerID string) {
	t.Helper()
	rs := sess.Records(oarlock.Cursor{})
	if len(rs) == 0 {
		return
	}
	base := rs[0].ReceivedAt
	for _, r := range rs {
		m := sj.ParseRec(r.Native)
		at := "+" + r.ReceivedAt.Sub(base).Round(10*time.Millisecond).String()
		switch {
		case sj.Str(m["type"]) == "command_lifecycle" && sj.Str(m["command_uuid"]) == steerID:
			t.Logf("seq %d %s command_lifecycle state=%s for steer %s", r.Seq, at, sj.Str(m["state"]), steerID)
		case sj.Str(m["type"]) == "user":
			for _, e := range r.Events {
				if e.Kind == oarlock.EventUserMessage && e.InputID == steerID {
					t.Logf("seq %d %s user echo for steer %s (evidence %s)", r.Seq, at, steerID, e.Evidence)
				}
			}
		}
	}
}

// one returns a pointer to "1" for Options.Env.
func one() *string { v := "1"; return &v }
