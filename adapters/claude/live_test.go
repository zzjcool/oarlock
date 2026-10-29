package claude

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
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
	sess, err := Open(ctxT(t), oarlock.Installation{Available: true, Path: path}, oarlock.Options{Cwd: t.TempDir(), Env: map[string]*string{"IS_SANDBOX": one()}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Dispose(ctxT(t))
	if out, _ := sess.Prompt(ctxT(t), "Write a 400-word explanation of why the sky is blue. Start writing immediately; do not use tools.", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt: %+v", out)
	}
	// Pi answers a prompt before the turn starts; Claude may too. A steer
	// written before the runtime has an active turn is not echoed, so wait
	// until the stream says running and has produced model output.
	deadline := time.Now().Add(30 * time.Second)
	for fold.StatusOf(sess.Records(oarlock.Cursor{})).Value.Kind != fold.StatusRunning {
		if time.Now().After(deadline) {
			t.Fatal("the turn never reached running")
		}
		time.Sleep(10 * time.Millisecond)
	}
	out, _ := sess.(oarlock.Steerer).Steer(ctxT(t), "Also say steer-ack.", oarlock.InputOptions{InputID: newUUID()})
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("live steer = %+v (echo never arrived)", out)
	}
}

// one returns a pointer to "1" for Options.Env.
func one() *string { v := "1"; return &v }
