package claude

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
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
	// No lifecycle frame: claude never said it received the steer, so the reason
	// must not claim it did.
	if strings.Contains(out.Reason, "do not resend") {
		t.Fatalf("reason %q claims ownership without a command_lifecycle receipt", out.Reason)
	}
	for _, r := range s.Records(oarlock.Cursor{}) {
		if r.Kind == oarlock.KindResponse && r.RequestID == out.RequestID && r.Response.Kind == oarlock.ResponseAccepted {
			t.Fatal("an accepted response was recorded without an echo")
		}
	}
}

// The live deferred case (contract evidence, claude 2.1.312): a steer written
// inside a turn with no request boundary is queued by claude and the turn ends
// before the echo. The rejection must not hand the input back — claude owns it
// as a follow-up turn — and it still must not be accepted.
func TestSteerDeferredLifecycleIsRejectedAsRuntimeOwned(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-deferred", EchoTimeout: 5 * time.Second}, "lifecycle", "defersteer")
	if out, _ := s.Prompt(ctxT(t), "run a long turn", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt: %+v", out)
	}
	waitRecords(t, s, "turn running", func(rs []oarlock.Record) bool { return fold.StatusOf(rs).Value.Kind == fold.StatusRunning })
	out, _ := s.Steer(ctxT(t), "STEER-LIVE: also say banana.", oarlock.InputOptions{InputID: "steer-deferred"})
	if out.Kind != oarlock.ControlRejected {
		t.Fatalf("Steer with a queued lifecycle = %+v, want rejected: only an echo is acceptance", out)
	}
	if out.Code != oarlock.NoActiveTurn {
		t.Fatalf("code = %s, want no_active_turn (the turn ended before the echo)", out.Code)
	}
	if !strings.Contains(out.Reason, "turn ended before claude echoed the steer") {
		t.Fatalf("reason %q lost its stable prefix", out.Reason)
	}
	if !strings.Contains(out.Reason, "command_lifecycle queued") || !strings.Contains(out.Reason, "do not resend") {
		t.Fatalf("reason %q must say claude queued the steer and the caller must not resend it", out.Reason)
	}
	if strings.Contains(out.Reason, "still yours") {
		t.Fatalf("reason %q hands back an input claude already queued", out.Reason)
	}
	// The follow-up never echoed: no user_message event and no accepted response.
	for _, r := range s.Records(oarlock.Cursor{}) {
		if r.Kind == oarlock.KindResponse && r.RequestID == out.RequestID && r.Response.Kind == oarlock.ResponseAccepted {
			t.Fatal("an accepted response was recorded without an echo")
		}
		for _, e := range r.Events {
			if e.Kind == oarlock.EventUserMessage && e.InputID == "steer-deferred" {
				t.Fatalf("an echo event was recorded for a steer claude never echoed: %+v", e)
			}
		}
	}
	// The lifecycle receipt is there, and it says queued rather than cancelled.
	var states []string
	for _, r := range s.Records(oarlock.Cursor{}) {
		m := sj.ParseRec(r.Native)
		if sj.Str(m["type"]) == "command_lifecycle" && sj.Str(m["command_uuid"]) == "steer-deferred" {
			states = append(states, sj.Str(m["state"]))
		}
	}
	if len(states) != 1 || states[0] != "queued" {
		t.Fatalf("lifecycle states for the steer = %v, want [queued]", states)
	}
}

// The EchoTimeout path is honest too: with a lifecycle receipt but no echo the
// reason keeps its stable prefix and does not hand back an input claude holds.
func TestSteerEchoTimeoutWithLifecycleIsRuntimeOwned(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-lifecycle-timeout", EchoTimeout: 300 * time.Millisecond}, "lifecycle", "noecho")
	if out, _ := s.Prompt(ctxT(t), "run a long turn", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt: %+v", out)
	}
	waitRecords(t, s, "turn running", func(rs []oarlock.Record) bool { return fold.StatusOf(rs).Value.Kind == fold.StatusRunning })
	out, _ := s.Steer(ctxT(t), "never echoed", oarlock.InputOptions{InputID: "steer-timeout"})
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.ErrorCode {
		t.Fatalf("Steer = %+v, want rejected error", out)
	}
	if !strings.Contains(out.Reason, "claude did not echo the steer within") {
		t.Fatalf("reason %q lost its stable prefix", out.Reason)
	}
	if !strings.Contains(out.Reason, "do not resend") || strings.Contains(out.Reason, "still yours") {
		t.Fatalf("reason %q must say claude owns the queued input", out.Reason)
	}
	for _, r := range s.Records(oarlock.Cursor{}) {
		if r.Kind == oarlock.KindResponse && r.RequestID == out.RequestID && r.Response.Kind == oarlock.ResponseAccepted {
			t.Fatal("an accepted response was recorded without an echo")
		}
	}
}

// The lifecycle frames must not change acceptance: the echoed user frame with
// the steer uuid is still the only evidence that lands the input.
func TestSteerAcceptedWithLifecycleFramesOnTheEcho(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-lifecycle-echo"}, "lifecycle")
	if out, _ := s.Prompt(ctxT(t), "run a long turn", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt: %+v", out)
	}
	waitRecords(t, s, "turn running", func(rs []oarlock.Record) bool { return fold.StatusOf(rs).Value.Kind == fold.StatusRunning })
	out, err := s.Steer(ctxT(t), "also say hi", oarlock.InputOptions{InputID: "steer-lifecycle"})
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Steer = %+v, %v", out, err)
	}
	var echoSeq uint64
	for _, r := range s.Records(oarlock.Cursor{}) {
		for _, e := range r.Events {
			if e.Kind == oarlock.EventUserMessage && e.InputID == "steer-lifecycle" {
				echoSeq = r.Seq
			}
		}
	}
	if echoSeq == 0 || echoSeq >= out.Response.Seq {
		t.Fatalf("the echo frame (seq %d) must precede the accepted response (seq %d)", echoSeq, out.Response.Seq)
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
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX signal death; the process-tree kill is covered by internal/exec")
	}
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

// ---- withdraw ---------------------------------------------------------------

// helperStdin opens the fake claude with its stdin log on: every line the
// adapter writes is appended to the returned path. That log is the only evidence
// of the exact wire text — a record keeps the request, not the bytes.
func helperStdin(t *testing.T, mode string, so SessionOptions, flags ...string) (*Session, string) {
	t.Helper()
	path := t.TempDir() + "/stdin.jsonl"
	sess, _, err := helperOpenEnv(t, mode, oarlock.Options{}, so, []string{stdinFileEnv + "=" + path}, flags...)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return sess, path
}

// stdinRawLines returns the complete lines the fake claude has read so far. A
// torn tail is dropped: the fake appends while the test reads, so the last
// element after the final LF is either "" or a line still being written.
func stdinRawLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read stdin log %s: %v", path, err)
	}
	lines := strings.Split(string(b), "\n")
	return lines[:len(lines)-1]
}

func stdinFrames(lines []string) []sj.Rec {
	var out []sj.Rec
	for _, l := range lines {
		if m := sj.ParseRec([]byte(l)); m != nil {
			out = append(out, m)
		}
	}
	return out
}

// waitStdin polls the fake's stdin log until cond holds for the lines read so
// far, and returns the lines verbatim.
func waitStdin(t *testing.T, path, what string, cond func([]sj.Rec) bool) []string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		lines := stdinRawLines(t, path)
		if cond(stdinFrames(lines)) {
			return lines
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s on stdin; %d lines: %v", what, len(lines), lines)
		}
		time.Sleep(3 * time.Millisecond)
	}
}

// waitRootInit waits until claude's root init frame is in the stream (the frame
// the cancel_queued negotiation and SupportsCancelQueued both read) and the turn
// it announced is running.
func waitRootInit(t *testing.T, s *Session) {
	t.Helper()
	waitRecords(t, s, "the root init frame", func(rs []oarlock.Record) bool {
		for _, r := range rs {
			m := sj.ParseRec(r.Native)
			if sj.Str(m["type"]) == "system" && sj.Str(m["subtype"]) == "init" {
				return true
			}
		}
		return false
	})
	waitRecords(t, s, "turn running", func(rs []oarlock.Record) bool { return fold.StatusOf(rs).Value.Kind == fold.StatusRunning })
}

func interruptOn(frames []sj.Rec) bool {
	for _, m := range frames {
		if sj.Str(m["type"]) == "control_request" && sj.Str(sj.AsRec(m["request"])["subtype"]) == "interrupt" {
			return true
		}
	}
	return false
}

// interruptLine is the raw interrupt control_request line the adapter wrote and
// its request object.
func interruptLine(t *testing.T, lines []string) (string, sj.Rec) {
	t.Helper()
	for _, l := range lines {
		m := sj.ParseRec([]byte(l))
		if sj.Str(m["type"]) == "control_request" && sj.Str(sj.AsRec(m["request"])["subtype"]) == "interrupt" {
			return l, sj.AsRec(m["request"])
		}
	}
	t.Fatalf("no interrupt control_request on stdin: %v", lines)
	return "", nil
}

// Withdraw takes held input back before the drain sends it: nothing of it is
// ever written to claude, and the request/response pair is an ordinary record
// pair carrying ActionWithdraw.
func TestWithdrawTakesHeldInputBackBeforeTheDrain(t *testing.T) {
	s, stdinPath := helperStdin(t, "chat", SessionOptions{SessionID: "sess-withdraw", AbortGrace: 5 * time.Second})
	if out, _ := s.Prompt(ctxT(t), "run a long turn", oarlock.PromptOptions{InputOptions: oarlock.InputOptions{InputID: "in-1"}}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt: %+v", out)
	}
	waitRecords(t, s, "turn running", func(rs []oarlock.Record) bool { return fold.StatusOf(rs).Value.Kind == fold.StatusRunning })
	if q, _ := s.Queue(ctxT(t), "WITHDRAWN-INPUT: never send me", oarlock.InputOptions{InputID: "q-1"}); q.Kind != oarlock.ControlAccepted {
		t.Fatalf("Queue = %+v", q)
	}
	out, err := s.Withdraw(ctxT(t), "q-1")
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Withdraw = %+v, %v", out, err)
	}
	var withdrew, response *oarlock.Record
	for _, r := range s.Records(oarlock.Cursor{}) {
		r := r
		if r.Kind == oarlock.KindRequest && r.ID == out.RequestID {
			withdrew = &r
		}
		if r.Kind == oarlock.KindResponse && r.RequestID == out.RequestID {
			response = &r
		}
	}
	if withdrew == nil || withdrew.Request == nil || withdrew.Request.Action != oarlock.ActionWithdraw || withdrew.Request.InputID != "q-1" {
		t.Fatalf("withdraw request record = %+v, want ActionWithdraw carrying q-1", withdrew)
	}
	if response == nil || response.Response == nil || response.Response.Kind != oarlock.ResponseAccepted {
		t.Fatalf("withdraw response record = %+v, want accepted", response)
	}

	// End the turn, then give the drain that follows its chance: it must find
	// nothing, so neither a prompt request carrying the withdrawn input nor the
	// bytes themselves may appear anywhere.
	s.Abort(ctxT(t))
	waitRecords(t, s, "turn end", turnsEnded(1))
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		for _, r := range s.Records(oarlock.Cursor{}) {
			if r.Kind != oarlock.KindRequest || r.Request == nil || r.Request.Action != oarlock.ActionPrompt {
				continue
			}
			if strings.Contains(r.Request.Input, "WITHDRAWN-INPUT") {
				t.Fatalf("the drain sent the withdrawn input: request %+v", r)
			}
		}
		if strings.Contains(strings.Join(stdinRawLines(t, stdinPath), "\n"), "WITHDRAWN-INPUT") {
			t.Fatal("the withdrawn input reached claude's stdin")
		}
	}
}

func TestWithdrawUnknownIDIsNotQueued(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-withdraw-unknown"})
	out, err := s.Withdraw(ctxT(t), "never-queued")
	if err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.NotQueued {
		t.Fatalf("Withdraw of an unknown id = %+v, want rejected not_queued", out)
	}
	if !strings.Contains(out.Reason, "no held input") {
		t.Fatalf("reason %q must say no held input is waiting", out.Reason)
	}
}

func TestWithdrawEmptyIDIsUnsupported(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-withdraw-empty"})
	out, err := s.Withdraw(ctxT(t), "")
	if err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.Unsupported {
		t.Fatalf(`Withdraw("") = %+v, want rejected unsupported: an empty id names no held input`, out)
	}
	if !strings.Contains(out.Reason, "inputId") {
		t.Fatalf("reason %q must ask for the inputId the queue request carried", out.Reason)
	}
}

// After a dispose the kernel's reachability gate runs before the decision: the
// session itself is over, and the queue went with the process, so its code wins
// over not_queued.
func TestWithdrawAfterDisposeIsDisposedFirst(t *testing.T) {
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-withdraw-disposed"})
	if out, _ := s.Prompt(ctxT(t), "run a long turn", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt: %+v", out)
	}
	waitRecords(t, s, "turn running", func(rs []oarlock.Record) bool { return fold.StatusOf(rs).Value.Kind == fold.StatusRunning })
	if q, _ := s.Queue(ctxT(t), "held when the session ends", oarlock.InputOptions{InputID: "q-gone"}); q.Kind != oarlock.ControlAccepted {
		t.Fatalf("Queue = %+v", q)
	}
	if err := s.Dispose(ctxT(t)); err != nil {
		t.Fatalf("Dispose = %v", err)
	}
	out, err := s.Withdraw(ctxT(t), "q-gone")
	if err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if out.Kind != oarlock.ControlRejected {
		t.Fatalf("Withdraw after dispose = %+v, want rejected", out)
	}
	if out.Code != oarlock.Disposed && out.Code != oarlock.RuntimeExited {
		t.Fatalf("code = %s (%s), want disposed or runtime_exited: the kernel's refusal must win over not_queued", out.Code, out.Reason)
	}
}

// The interface is the capability: this session holds queued input itself, so it
// is an oarlock.Withdrawer.
func TestSessionIsAWithdrawer(t *testing.T) {
	var _ oarlock.Withdrawer = (*Session)(nil)
	s, _ := helperSession(t, "chat", SessionOptions{SessionID: "sess-withdraw-iface"})
	if _, ok := any(s).(oarlock.Withdrawer); !ok {
		t.Fatal("*claude.Session must implement oarlock.Withdrawer: the adapter holds the queue")
	}
}

// ---- cancel_queued ----------------------------------------------------------

// The root init's interrupt_cancel_queued_v1 is what lets an abort carry
// cancel_queued. With a prompt of ours written and no start receipt, the option
// rides on the same request object.
func TestAbortCarriesCancelQueuedWhenTheInitCapabilityIsNegotiated(t *testing.T) {
	s, stdinPath := helperStdin(t, "chat", SessionOptions{SessionID: "sess-cq", AbortGrace: 5 * time.Second})
	if out, _ := s.Prompt(ctxT(t), "run a long turn", oarlock.PromptOptions{InputOptions: oarlock.InputOptions{InputID: "cq-1"}}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt: %+v", out)
	}
	waitRootInit(t, s)
	if !s.SupportsCancelQueued() {
		t.Fatal("the root init declared interrupt_cancel_queued_v1: SupportsCancelQueued must be true")
	}
	if out, _ := s.Abort(ctxT(t)); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Abort = %+v", out)
	}
	lines := waitStdin(t, stdinPath, "the interrupt request", interruptOn)
	raw, req := interruptLine(t, lines)
	if v, _ := req["cancel_queued"].(bool); !v {
		t.Fatalf("interrupt line %s has no cancel_queued: the prompt uuid never started", raw)
	}
	if !strings.Contains(raw, `"cancel_queued":true`) {
		t.Fatalf("interrupt line %s must carry cancel_queued true", raw)
	}
}

// Without the declaration the option would be a guess, so the interrupt is
// exactly the bare request it always was.
func TestAbortWithoutTheInitCapabilityStaysAPlainInterrupt(t *testing.T) {
	s, stdinPath := helperStdin(t, "chat", SessionOptions{SessionID: "sess-cq-no", AbortGrace: 5 * time.Second}, "noinitcaps")
	if out, _ := s.Prompt(ctxT(t), "run a long turn", oarlock.PromptOptions{InputOptions: oarlock.InputOptions{InputID: "cq-2"}}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt: %+v", out)
	}
	waitRootInit(t, s)
	if s.SupportsCancelQueued() {
		t.Fatal("the init omits interrupt_cancel_queued_v1: SupportsCancelQueued must stay false")
	}
	if out, _ := s.Abort(ctxT(t)); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Abort = %+v", out)
	}
	lines := waitStdin(t, stdinPath, "the interrupt request", interruptOn)
	raw, req := interruptLine(t, lines)
	if got, _ := json.Marshal(req); string(got) != `{"subtype":"interrupt"}` {
		t.Fatalf("interrupt line %s = %s, want exactly {\"subtype\":\"interrupt\"}: the capability was never declared", raw, got)
	}
}

// The capability can be negotiated and the option still withheld: claude's own
// command_lifecycle "started" ends the unstarted window, so the input is no
// longer queued and cancel_queued would name nothing.
func TestAbortAfterStartedLifecycleIsAPlainInterrupt(t *testing.T) {
	s, stdinPath := helperStdin(t, "chat", SessionOptions{SessionID: "sess-cq-started", AbortGrace: 5 * time.Second}, "lifecycle")
	if out, _ := s.Prompt(ctxT(t), "run a long turn", oarlock.PromptOptions{InputOptions: oarlock.InputOptions{InputID: "cq-3"}}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt: %+v", out)
	}
	waitRecords(t, s, "the started receipt", func(rs []oarlock.Record) bool {
		for _, r := range rs {
			m := sj.ParseRec(r.Native)
			if sj.Str(m["type"]) == "command_lifecycle" && sj.Str(m["command_uuid"]) == "cq-3" && sj.Str(m["state"]) == "started" {
				return true
			}
		}
		return false
	})
	waitRootInit(t, s)
	if !s.SupportsCancelQueued() {
		t.Fatal("the default fake init declares the capability; the bare interrupt below is the tracking, not the negotiation")
	}
	if out, _ := s.Abort(ctxT(t)); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Abort = %+v", out)
	}
	lines := waitStdin(t, stdinPath, "the interrupt request", interruptOn)
	_, req := interruptLine(t, lines)
	if got, _ := json.Marshal(req); string(got) != `{"subtype":"interrupt"}` {
		t.Fatalf("interrupt request = %s, want exactly {\"subtype\":\"interrupt\"}: the prompt already started", got)
	}
}

// ---- images -----------------------------------------------------------------

// Images are native: the prompt line carries claude's image block before the
// text block, and Capabilities says so.
func TestPromptImagesAreNativeContentBlocks(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nnot-a-real-png-but-the-bytes-are-the-evidence")
	imgPath := filepath.Join(t.TempDir(), "pixel.png")
	if err := os.WriteFile(imgPath, png, 0o600); err != nil {
		t.Fatal(err)
	}
	s, stdinPath := helperStdin(t, "chat", SessionOptions{SessionID: "sess-images"})
	if !s.Capabilities().Images {
		t.Fatal("Capabilities().Images must be true: this adapter writes native image blocks")
	}
	if out, err := s.Prompt(ctxT(t), "what is in this image?", oarlock.PromptOptions{
		InputOptions: oarlock.InputOptions{InputID: "img-1", Images: []oarlock.InputImage{{Path: imgPath}}},
	}); err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Prompt = %+v, %v", out, err)
	}
	frames := stdinFrames(waitStdin(t, stdinPath, "the image prompt", func(fs []sj.Rec) bool {
		for _, m := range fs {
			if sj.Str(m["type"]) == "user" && sj.Str(m["uuid"]) == "img-1" {
				return true
			}
		}
		return false
	}))
	var user sj.Rec
	for _, m := range frames {
		if sj.Str(m["type"]) == "user" && sj.Str(m["uuid"]) == "img-1" {
			user = m
		}
	}
	content := sj.AsList(sj.AsRec(user["message"])["content"])
	if len(content) != 2 {
		t.Fatalf("content = %v, want exactly one image block then the text block", content)
	}
	img := sj.AsRec(content[0])
	src := sj.AsRec(img["source"])
	if sj.Str(img["type"]) != "image" || sj.Str(src["type"]) != "base64" ||
		sj.Str(src["media_type"]) != "image/png" || sj.Str(src["data"]) != base64.StdEncoding.EncodeToString(png) {
		t.Fatalf("first content block = %v, want the base64 png image block", img)
	}
	if text := sj.AsRec(content[1]); sj.Str(text["type"]) != "text" || sj.Str(text["text"]) != "what is in this image?" {
		t.Fatalf("last content block = %v, want the text block", text)
	}
}
