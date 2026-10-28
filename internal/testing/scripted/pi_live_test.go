package scripted

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/adapters/pi"
)

// piLiveArgs are the arguments of TestSessionLiveSmoke
// (adapters/pi/session_core_test.go): an offline RPC pi with nothing that could
// answer a prompt except the model.
var piLiveArgs = []string{
	"--mode", "rpc", "--offline", "--no-session", "--no-tools", "--no-mcp", "--no-skills",
	"--no-prompt-templates", "--no-context-files",
	"--model", "any-endpoint/workbuddy/deepseek-v4.1-flash",
}

const (
	piLiveQuick = "Reply with the single word: ok"
	piLiveLong  = "Count from 1 to 400, one number per line, no other text."

	// piLiveTimeout bounds every wait and control of the suite: a real model
	// answers in seconds, but a cold start or a slow endpoint must not flake it.
	piLiveTimeout = 120 * time.Second
	// piLiveGrace is how long a live turn may outlive an accepted abort before
	// the adapter takes the process over. The suite expects Pi to stop by
	// itself well inside it.
	piLiveGrace = 3 * time.Second
)

// TestPiPassesSuite is acceptance 1 for the real runtime: the shared behaviour
// suite against adapters/pi and a live pi 1.4.0. It needs a logged-in pi and a
// model, so it is opt-in; without OAR_PI_LIVE=1 it is skipped and says so, and
// a green go test ./... therefore proves the suite on the scripted runtime
// alone.
func TestPiPassesSuite(t *testing.T) {
	if os.Getenv("OAR_PI_LIVE") != "1" {
		t.Skip("NOT VERIFIED: set OAR_PI_LIVE=1 (needs a logged-in pi 1.4.0) to run the shared suite against real Pi")
	}

	// One launch before the suite, so a machine without pi skips as a whole
	// instead of passing nine skipped sub-tests under an "ok" parent.
	probe, err := pi.Launch(context.Background(), pi.Options{Args: piLiveArgs})
	if err != nil {
		t.Skipf("NOT VERIFIED: pi is not launchable here: %v", err)
	}
	_ = probe.Close()

	RunSuite(t, Target{
		Name: "pi",
		Open: piLiveOpen,
		Prompt: func(b Behaviour) string {
			if b == Long {
				return piLiveLong
			}
			return piLiveQuick
		},
		Timeout: piLiveTimeout,
	})
}

// piLiveOpen launches a fresh pi and wraps it in a session. The plan is not
// needed: the prompt text selects the behaviour of a turn.
func piLiveOpen(t *testing.T, _ ...Behaviour) oarlock.Session {
	t.Helper()

	// Launch's ctx owns the process tree, so it must outlive this call: it is
	// the sub-test's, and it is cancelled last (cleanups run in reverse), after
	// RunSuite's Dispose has ended the process gracefully.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	p, err := pi.Launch(ctx, pi.Options{Args: piLiveArgs})
	if err != nil {
		// The probe in TestPiPassesSuite launched pi, so this is a failure.
		t.Fatalf("launching pi: %v", err)
	}
	// If Open fails below RunSuite never sees a session to dispose; Close is
	// idempotent, so it is also safe after the session's own Dispose.
	t.Cleanup(func() { _ = p.Close() })

	callCtx, callCancel := context.WithTimeout(ctx, piLiveTimeout)
	defer callCancel()
	var state struct {
		SessionID string `json:"sessionId"`
	}
	if err := p.Do(callCtx, pi.Command{Type: "get_state"}, &state); err != nil {
		t.Fatalf("get_state: %v", err)
	}
	sess, err := pi.NewSession(p, pi.SessionOptions{SessionID: state.SessionID, AbortGrace: piLiveGrace})
	if err != nil {
		t.Fatalf("pi.NewSession: %v", err)
	}
	return sess
}
