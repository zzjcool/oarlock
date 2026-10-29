package claude

import (
	"context"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
	"github.com/zzjcool/oarlock/internal/testing/scripted"
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func waitRecords(t *testing.T, s *Session, what string, cond func([]oarlock.Record) bool) []oarlock.Record {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		rs := s.Records(oarlock.Cursor{})
		if cond(rs) {
			return rs
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; %d records", what, len(rs))
		}
		time.Sleep(3 * time.Millisecond)
	}
}

func turnsEnded(n int) func([]oarlock.Record) bool {
	return func(rs []oarlock.Record) bool {
		c := 0
		for _, r := range rs {
			for _, e := range r.Events {
				if e.Kind == oarlock.EventTurnEnded {
					c++
				}
			}
		}
		return c >= n
	}
}

func outcomes(rs []oarlock.Record) (out []oarlock.TurnOutcome) {
	for _, r := range rs {
		for _, e := range r.Events {
			if e.Kind == oarlock.EventTurnEnded && e.Outcome != nil {
				out = append(out, *e.Outcome)
			}
		}
	}
	return
}

// Acceptance 1: the SAME behaviour suite Pi runs, zero kernel change. Against a
// fake claude here (always runs); the live binary leg is claude_live_test.go in
// internal/testing/scripted, skipped without claude.
func TestFakeClaudePassesSharedSuite(t *testing.T) {
	scripted.RunSuite(t, scripted.Target{
		Name: "claude(fake process)",
		Open: func(t *testing.T, _ ...scripted.Behaviour) oarlock.Session {
			s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-suite", AbortGrace: 5 * time.Second})
			return s
		},
		Prompt: func(b scripted.Behaviour) string {
			if b == scripted.Long {
				return "run a long turn"
			}
			return "run a quick turn"
		},
		Timeout: 30 * time.Second,
	})
}

func TestPromptTurnEndsCompletedWithUsage(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-1"})
	out, err := s.Prompt(ctxT(t), "say hi", oarlock.PromptOptions{InputOptions: oarlock.InputOptions{InputID: "in-1"}})
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Prompt = %+v, %v", out, err)
	}
	rs := waitRecords(t, s, "turn end", turnsEnded(1))
	if got := outcomes(rs); got[0].Kind != oarlock.OutcomeCompleted {
		t.Fatalf("outcome = %+v", got[0])
	}
	if st := fold.StatusOf(rs).Value; st.Kind != fold.StatusIdle {
		t.Fatalf("status = %s", st.Kind)
	}
	u := fold.UsageOf(rs).Value
	if u.Total == nil || u.Total.Input != 13 || u.Total.Output != 7 {
		t.Fatalf("usage total = %+v, want input 13 (3+10 cache read) output 7", u.Total)
	}
	var text string
	for _, m := range fold.ConversationOf(rs).Value {
		if m.Role == fold.RoleAssistant {
			text += m.Text
		}
	}
	if text != "working" {
		t.Fatalf("assistant text = %q", text)
	}
	if n := s.Unknown(); len(n) != 0 {
		t.Fatalf("unknown frames: %v", n)
	}
}

// Acceptance 3: steer is accepted only when claude's echo arrives.
func TestSteerAcceptedOnlyWithEcho(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-steer"})
	if _, ok := any(s).(oarlock.Steerer); !ok {
		t.Fatal("claude session must be an oarlock.Steerer")
	}
	if out, _ := s.Prompt(ctxT(t), "run a long turn", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt: %+v", out)
	}
	out, err := s.Steer(ctxT(t), "also say hi", oarlock.InputOptions{InputID: "steer-1"})
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Steer = %+v, %v", out, err)
	}
	rs := s.Records(oarlock.Cursor{})
	var echoSeq uint64
	for _, r := range rs {
		for _, e := range r.Events {
			if e.Kind == oarlock.EventUserMessage && e.InputID == "steer-1" {
				echoSeq = r.Seq
				if e.Evidence != oarlock.EvidenceAcknowledged || e.Text != "also say hi" {
					t.Fatalf("echo event = %+v", e)
				}
			}
		}
	}
	if echoSeq == 0 || echoSeq >= out.Response.Seq {
		t.Fatalf("the echo frame (seq %d) must precede the accepted response (seq %d)", echoSeq, out.Response.Seq)
	}
}

func TestSteerRejectedWithoutEchoKeepsTheInput(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-noecho", EchoTimeout: 300 * time.Millisecond}, "noecho")
	if out, _ := s.Prompt(ctxT(t), "run a long turn", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt: %+v", out)
	}
	waitRecords(t, s, "turn running", func(rs []oarlock.Record) bool { return fold.StatusOf(rs).Value.Kind == fold.StatusRunning })
	out, _ := s.Steer(ctxT(t), "never lands", oarlock.InputOptions{InputID: "steer-x"})
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.ErrorCode {
		t.Fatalf("Steer without an echo = %+v, want rejected (the caller keeps the input)", out)
	}
	if !strings.Contains(out.Reason, "still yours") {
		t.Fatalf("reason %q must say the input is still the caller's", out.Reason)
	}
	for _, r := range s.Records(oarlock.Cursor{}) {
		if r.Kind == oarlock.KindResponse && r.RequestID == out.RequestID && r.Response.Kind == oarlock.ResponseAccepted {
			t.Fatal("an accepted response was recorded without an echo")
		}
	}
}

func TestSteerIdleIsNoActiveTurn(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-idle-steer"})
	out, _ := s.Steer(ctxT(t), "x", oarlock.InputOptions{})
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.NoActiveTurn {
		t.Fatalf("Steer on idle = %+v", out)
	}
}

// Acceptance 5 (graceful half): an interrupt the runtime honours ends the turn
// aborted by claude's own result frame.
func TestAbortEndsTurnAborted(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-abort", AbortGrace: 5 * time.Second})
	s.Prompt(ctxT(t), "run a long turn", oarlock.PromptOptions{})
	out, _ := s.Abort(ctxT(t))
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Abort = %+v", out)
	}
	rs := waitRecords(t, s, "aborted turn", turnsEnded(1))
	got := outcomes(rs)
	if len(got) != 1 || got[0].Kind != oarlock.OutcomeAborted {
		t.Fatalf("outcomes = %+v, want [aborted]: is_error:true must not read as failed after a requested abort", got)
	}
}

func TestAbortRefusedIsRuntimeRefusedAndKillsNothing(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-refuse", AbortGrace: 300 * time.Millisecond}, "refuseabort")
	s.Prompt(ctxT(t), "run a long turn", oarlock.PromptOptions{})
	out, _ := s.Abort(ctxT(t))
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.RuntimeRefused {
		t.Fatalf("Abort = %+v, want rejected runtime_refused", out)
	}
	time.Sleep(700 * time.Millisecond) // past the grace: the refused attempt's timer must be gone
	if st := fold.StatusOf(s.Records(oarlock.Cursor{})).Value; st.Kind != fold.StatusRunning {
		t.Fatalf("status = %s: a refused abort must not kill the process", st.Kind)
	}
}

// Acceptance 5: an interrupt claude ignores: SIGTERM to the process group after
// the grace, SIGKILL after the kill grace; accepted ("we took the stop"), no
// turn_ended invented, the exit recorded.
func TestAbortTakeOverKillsTheProcessGroup(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-takeover", AbortGrace: 200 * time.Millisecond}, "ignoreabort")
	s.Prompt(ctxT(t), "run a long turn", oarlock.PromptOptions{})
	out, _ := s.Abort(ctxT(t))
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Abort = %+v, want accepted (taken over)", out)
	}
	rs := waitRecords(t, s, "exit", func(rs []oarlock.Record) bool {
		return fold.StatusOf(rs).Value.Kind == fold.StatusExited
	})
	if n := len(outcomes(rs)); n != 0 {
		t.Fatalf("the take-over invented %d turn_ended", n)
	}
	var exit *oarlock.ResponseBody
	for _, r := range rs {
		if r.Kind == oarlock.KindResponse && r.Response.Kind == oarlock.ResponseExited {
			exit = r.Response
		}
	}
	if exit == nil || !exit.ExitCodeReported {
		t.Fatalf("no exited response: %+v", exit)
	}
	if late, _ := s.Abort(ctxT(t)); late.Code != oarlock.RuntimeExited {
		t.Fatalf("abort after exit = %+v", late)
	}
}

func TestAbortTakeOverEndsWithSIGTERMToTheGroup(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-sig", AbortGrace: 100 * time.Millisecond}, "ignoreabort")
	s.Prompt(ctxT(t), "run a long turn", oarlock.PromptOptions{})
	s.Abort(ctxT(t))
	rs := waitRecords(t, s, "exit", func(rs []oarlock.Record) bool { return fold.StatusOf(rs).Value.Kind == fold.StatusExited })
	for _, r := range rs {
		if r.Kind == oarlock.KindResponse && r.Response.Kind == oarlock.ResponseExited {
			// The fake ignores stdin EOF and does not handle SIGTERM, so SIGTERM
			// (not SIGKILL, not a clean exit) is what ended it: no exit code.
			if r.Response.ExitCode != nil {
				t.Fatalf("exit code %d: expected death by signal", *r.Response.ExitCode)
			}
			if !strings.Contains(string(r.Response.Native), syscall.SIGTERM.String()) {
				t.Fatalf("native = %s, want signal %s", r.Response.Native, syscall.SIGTERM)
			}
			return
		}
	}
	t.Fatal("no exited response")
}

// Acceptance 4: is_error:true + subtype:success is failed.
func TestIsErrorWithSubtypeSuccessIsFailed(t *testing.T) {
	frames := [][]byte{
		[]byte(`{"type":"system","subtype":"init"}`),
		[]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"API Error: 400 max_tokens exceeds model limit"}]}}`),
		[]byte(`{"type":"result","subtype":"success","is_error":true,"result":"API Error: 400 max_tokens exceeds model limit","api_error_status":400}`),
	}
	got := Project(frames, false)
	end := got[2].Events[0]
	if end.Kind != oarlock.EventTurnEnded || end.Outcome.Kind != oarlock.OutcomeFailed {
		t.Fatalf("outcome = %+v, want failed: is_error outranks subtype", end.Outcome)
	}
	if end.Outcome.Failure != oarlock.FailureInvalidRequest || end.Outcome.Status != 400 {
		t.Fatalf("outcome = %+v", end.Outcome)
	}
}

func TestDisposeMidTurnAndLiveProcessGroupGone(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-dispose"})
	s.Prompt(ctxT(t), "run a long turn", oarlock.PromptOptions{})
	waitRecords(t, s, "frames", func(rs []oarlock.Record) bool { return len(rs) > 3 })
	if err := s.Dispose(ctxT(t)); err != nil {
		t.Fatalf("Dispose = %v", err)
	}
	rs := s.Records(oarlock.Cursor{})
	if n := len(outcomes(rs)); n != 0 {
		t.Fatalf("dispose invented %d turn_ended", n)
	}
	if st := fold.StatusOf(rs).Value; st.Kind != fold.StatusExited {
		t.Fatalf("status = %s", st.Kind)
	}
	if err := s.Dispose(ctxT(t)); err != nil {
		t.Fatalf("second Dispose = %v", err)
	}
}

func TestQueueRunsAfterTheTurn(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-queue", AbortGrace: 5 * time.Second})
	s.Prompt(ctxT(t), "run a long turn", oarlock.PromptOptions{})
	q, _ := s.Queue(ctxT(t), "quick follow-up", oarlock.InputOptions{InputID: "q1"})
	if q.Kind != oarlock.ControlAccepted {
		t.Fatalf("Queue = %+v", q)
	}
	s.Abort(ctxT(t))
	rs := waitRecords(t, s, "two turns", turnsEnded(2))
	got := outcomes(rs)
	if got[0].Kind != oarlock.OutcomeAborted || got[1].Kind != oarlock.OutcomeCompleted {
		t.Fatalf("outcomes = %+v", got)
	}
}

func TestToAppRequestIsRecordedNotAnswered(t *testing.T) {
	// A runtime control_request is a toApp request record; headless claude
	// skips permissions, so nothing answers it.
	f := Project([][]byte{[]byte(`{"type":"control_request","request_id":"perm-1","request":{"subtype":"can_use_tool"}}`)}, false)
	if f[0].Request == nil || f[0].Request.ID != "perm-1" || f[0].Request.Subtype != "can_use_tool" {
		t.Fatalf("request = %+v", f[0].Request)
	}
}

func TestOpenArgsAreTheDocumentedFlags(t *testing.T) {
	_, argsFile := helperSession(t, "chat", SessionOptions{SessionID: "x"})
	var b []byte
	var err error
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if b, err = os.ReadFile(argsFile); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(string(b), "\x00")
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{
		" -p ", " --input-format stream-json ", " --output-format stream-json ",
		" --verbose ", " --replay-user-messages ", " --include-partial-messages ", " --dangerously-skip-permissions ", " --session-id ",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv lacks %q: %v", want, args)
		}
	}
}
