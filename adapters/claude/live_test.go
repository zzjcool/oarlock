package claude

import (
	"os"
	"os/exec"
	"testing"

	"github.com/zzjcool/oarlock"
)

// TestLiveSteerEcho is the one claim only a real claude can prove: that the
// echo the steer protocol waits for really arrives with --replay-user-messages.
// Opt-in (OAR_CLAUDE_LIVE=1, a logged-in claude); otherwise skipped as NOT
// VERIFIED. The recorded fixture (testdata/steer-lifecycle.jsonl) is real claude
// output and TestReplaySteerEchoAndPartialStreaming asserts the echo in it, but
// that is a recording, not a live run.
func TestLiveSteerEcho(t *testing.T) {
	if os.Getenv("OAR_CLAUDE_LIVE") != "1" {
		t.Skip("NOT VERIFIED: set OAR_CLAUDE_LIVE=1 (needs a logged-in claude) to prove the steer echo live")
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("NOT VERIFIED: no claude binary on PATH: %v", err)
	}
	sess, err := Open(ctxT(t), oarlock.Installation{Available: true, Path: path}, oarlock.Options{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Dispose(ctxT(t))
	if out, _ := sess.Prompt(ctxT(t), "Run `sleep 8` with the Bash tool, then say done.", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt: %+v", out)
	}
	out, _ := sess.(oarlock.Steerer).Steer(ctxT(t), "Also say steer-ack.", oarlock.InputOptions{InputID: newUUID()})
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("live steer = %+v (echo never arrived)", out)
	}
}
