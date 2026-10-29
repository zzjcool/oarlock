package claude

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

func argsOf(t *testing.T, file string) []string {
	t.Helper()
	var b []byte
	var err error
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if b, err = os.ReadFile(file); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(b), "\x00")
}

func indexOf(args []string, a string) int {
	for i, x := range args {
		if x == a {
			return i
		}
	}
	return -1
}

// Acceptance 6: --resume <id>, and the new stream starts at Seq 1.
func TestResumePassesResumeFlagAndSeqStartsAtOne(t *testing.T) {
	s, argsFile, err := helperOpen(t, "chat", oarlock.Options{Resume: "sess-old"}, SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	args := argsOf(t, argsFile)
	if i := indexOf(args, "--resume"); i < 0 || args[i+1] != "sess-old" {
		t.Fatalf("argv = %v, want --resume sess-old", args)
	}
	if indexOf(args, "--session-id") >= 0 {
		t.Fatalf("argv = %v: a resume must not also set --session-id", args)
	}
	if indexOf(args, "--name") >= 0 {
		t.Fatal("--name on a resume would replace the user's name")
	}
	if out, _ := s.Prompt(ctxT(t), "say hi", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt: %+v", out)
	}
	rs := waitRecords(t, s, "turn", turnsEnded(1))
	if rs[0].Seq != 1 {
		t.Fatalf("first Seq = %d, want 1 (a resume is a new stream instance)", rs[0].Seq)
	}
	for i, r := range rs {
		if r.Seq != uint64(i+1) {
			t.Fatalf("Seq not dense: record %d has Seq %d", i, r.Seq)
		}
		if r.SessionID != "sess-old" {
			t.Fatalf("session id = %q, want the native id kept", r.SessionID)
		}
	}
	// The private readbacks (initialize, get_usage) never reach the stream.
	for _, r := range rs {
		if r.Kind == oarlock.KindFrame && strings.Contains(string(r.Native), `"control_response"`) {
			t.Fatalf("a private readback was recorded: %s", r.Native)
		}
	}
}

// A resumed process continues the previous total: the share counts from the
// get_usage baseline, so the earlier process's tokens are not claimed.
func TestResumeUsageCountsFromTheBaseline(t *testing.T) {
	s, _, err := helperOpen(t, "chat", oarlock.Options{Resume: "sess-old"}, SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s.Prompt(ctxT(t), "say hi", oarlock.PromptOptions{})
	rs := waitRecords(t, s, "turn", turnsEnded(1))
	u := fold.UsageOf(rs).Value
	// The fake continues 100/50/+10 cache read; baseline is exactly that, so
	// this turn's share is 3+10 input and 7 output.
	if u.Total == nil || u.Total.Input != 13 || u.Total.Output != 7 {
		t.Fatalf("total = %+v, want this session's share only", u.Total)
	}
}

func TestResumeWithoutABaselineReportsNoTotal(t *testing.T) {
	s, _, err := helperOpen(t, "chat", oarlock.Options{Resume: "sess-old"}, SessionOptions{}, "nobaseline")
	if err != nil {
		t.Fatalf("an unreadable baseline must not fail the open: %v", err)
	}
	s.Prompt(ctxT(t), "say hi", oarlock.PromptOptions{})
	rs := waitRecords(t, s, "turn", turnsEnded(1))
	if u := fold.UsageOf(rs).Value; u.Total != nil {
		t.Fatalf("total = %+v: don't know, don't report", u.Total)
	}
}

func TestResumeMissingConversationIsSessionNotFound(t *testing.T) {
	_, _, err := helperOpen(t, "missing-resume", oarlock.Options{Resume: "sess-gone"}, SessionOptions{})
	var nf *oarlock.SessionNotFound
	if !errors.As(err, &nf) || nf.SessionID != "sess-gone" {
		t.Fatalf("err = %v, want *SessionNotFound", err)
	}
	if nf.Cause == nil || !strings.Contains(nf.Cause.Error(), "No conversation found") {
		t.Fatalf("cause = %v", nf.Cause)
	}
}

func TestResumeRefusesPromptsBeforeAnythingStarts(t *testing.T) {
	_, argsFile, err := helperOpen(t, "chat", oarlock.Options{Resume: "x", SystemPrompt: "be terse"}, SessionOptions{})
	var u *oarlock.UnsupportedOption
	if !errors.As(err, &u) || u.Option != oarlock.OptionSystemPrompt {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(argsFile); statErr == nil {
		t.Fatal("a process started for a refused open")
	}
}
