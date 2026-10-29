package codex

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
)

// TestLiveCodex talks to a real `codex app-server`. It is opt-in
// (OARLOCK_LIVE_CODEX=1) and needs a signed-in codex on PATH inside the version
// window; everywhere else it skips. It has NOT been run: the machine this
// adapter was written on has no codex binary, so the live behaviour is
// unverified (see the issue report).
func TestLiveCodex(t *testing.T) {
	if os.Getenv("OARLOCK_LIVE_CODEX") != "1" {
		t.Skip("set OARLOCK_LIVE_CODEX=1 to run against a real codex")
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("no codex binary on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sess, err := Open(ctx, oarlock.Installation{Available: true, Path: path}, oarlock.Options{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Dispose(context.Background())
	out, err := sess.Prompt(ctx, "Reply with the single word: pong", oarlock.PromptOptions{})
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	waitFor(t, "turn_ended", func() bool { return hasEvent(sess.Records(oarlock.Cursor{}), oarlock.EventTurnEnded) })
}
