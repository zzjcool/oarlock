package scripted

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/adapters/claude"
)

const (
	claudeLiveQuick   = "Reply with the single word: ok"
	claudeLiveLong    = "Count from 1 to 400, one number per line, no other text."
	claudeLiveTimeout = 120 * time.Second
	claudeLiveGrace   = 5 * time.Second
)

// TestClaudePassesSuite runs the SAME shared behaviour suite on a live claude
// (issue #19 acceptance 1, zero kernel change). It needs a logged-in claude and
// a reachable model, so it is opt-in. Without OAR_CLAUDE_LIVE=1, or without a
// claude binary on PATH, it is skipped and says NOT VERIFIED: a green go test
// ./... proves the suite on the scripted runtime and on the fake-process claude
// adapter (adapters/claude), never on a real claude.
func TestClaudePassesSuite(t *testing.T) {
	if os.Getenv("OAR_CLAUDE_LIVE") != "1" {
		t.Skip("NOT VERIFIED: set OAR_CLAUDE_LIVE=1 (needs a logged-in claude) to run the shared suite against real Claude")
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("NOT VERIFIED: no claude binary on PATH: %v", err)
	}
	RunSuite(t, Target{
		Name: "claude",
		Open: func(t *testing.T, _ ...Behaviour) oarlock.Session {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), claudeLiveTimeout)
			defer cancel()
			sess, err := claude.Open(ctx, oarlock.Installation{Available: true, Path: path}, oarlock.Options{Cwd: t.TempDir(), Env: map[string]*string{"IS_SANDBOX": one()}})
			if err != nil {
				t.Fatalf("claude.Open: %v", err)
			}
			return sess
		},
		Prompt: func(b Behaviour) string {
			if b == Long {
				return claudeLiveLong
			}
			return claudeLiveQuick
		},
		Timeout: claudeLiveTimeout,
	})
	_ = claudeLiveGrace
}

// one returns a pointer to "1" for Options.Env.
func one() *string { v := "1"; return &v }
