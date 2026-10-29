package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/observe"
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

// TestLivePromptAndWait drives the public surface a host actually uses —
// oarlock.PromptAndWait plus observe.StatusOf over the record stream — against
// a real `codex app-server` (issue #41).
//
// It is opt-in because it spends a real turn on a real account: OARLOCK_LIVE_CODEX=1
// is the same gate TestLiveCodex uses, and it still needs a codex on PATH inside
// the version window the open gate enforces ([0.149.0, 0.166.0)), a CODEX_HOME
// whose config.toml names the model, the model_provider and the env_key holding
// the token, and that token in OPENAI_API_KEY. Everywhere else it skips as NOT
// VERIFIED rather than failing, so `go test -race ./...` and CI need no
// credentials.
//
// No Options.Env is passed on purpose: `codex app-server` inherits this
// process's environment, so CODEX_HOME and OPENAI_API_KEY reach it untouched
// and the token is never copied into a fixture, a log line or this file.
func TestLivePromptAndWait(t *testing.T) {
	if os.Getenv("OARLOCK_LIVE_CODEX") != "1" {
		t.Skip("NOT VERIFIED: set OARLOCK_LIVE_CODEX=1 to run PromptAndWait against a real codex")
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		t.Skipf("NOT VERIFIED: no codex binary on PATH: %v", err)
	}
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		t.Skip("NOT VERIFIED: set CODEX_HOME to a directory whose config.toml names the model, model_provider and env_key")
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); err != nil {
		t.Skipf("NOT VERIFIED: CODEX_HOME/config.toml is not readable: %v", err)
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("NOT VERIFIED: set OPENAI_API_KEY to the token CODEX_HOME/config.toml names as env_key")
	}

	rt, err := oarlock.Require("codex")
	if err != nil {
		t.Fatalf("oarlock.Require(\"codex\"): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sess, err := rt.Session(ctx, oarlock.Installation{Available: true, Path: path}, oarlock.Options{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	// Cleanup only: the two asserted Dispose calls are below. A third call is
	// idempotent, so this borrows the first call's result when the test bails
	// out early instead of leaking the app-server process.
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		_ = sess.Dispose(dctx)
	}()

	run, err := oarlock.PromptAndWait(ctx, sess, "Reply with the single word: ok", oarlock.WithTimeout(2*time.Minute))
	if err != nil {
		t.Fatalf("PromptAndWait: %v", err)
	}
	if run.Outcome != oarlock.RunCompleted {
		t.Fatalf("run.Outcome = %q (reason %q), want %q", run.Outcome, run.Reason, oarlock.RunCompleted)
	}
	if strings.TrimSpace(run.Text) == "" {
		t.Fatalf("run.Text = %q, want the root agent's streamed text", run.Text)
	}
	if run.Turn == nil || run.Turn.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("run.Turn = %+v, want the runtime's own completed turn", run.Turn)
	}
	if run.Seq == 0 {
		t.Fatalf("run.Seq = 0, want the stream position the wait reached")
	}

	records := sess.Records(oarlock.Cursor{})
	if len(records) == 0 {
		t.Fatal("sess.Records(Cursor{}) is empty after a completed run")
	}
	var prev uint64
	for i, rec := range records {
		if rec.Seq == 0 || (i > 0 && rec.Seq <= prev) {
			t.Fatalf("records[%d].Seq = %d after %d: the kernel seq must be dense and strictly increasing", i, rec.Seq, prev)
		}
		prev = rec.Seq
	}
	if got, want := observe.StatusOf(records).Value.Kind, observe.StatusIdle; got != want {
		t.Fatalf("observe.StatusOf(sess.Records(Cursor{})).Value.Kind = %q, want %q", got, want)
	}
	t.Logf("run completed at seq %d: %d records, %d bytes of text, status idle", run.Seq, len(records), len(run.Text))

	// Dispose is idempotent: the first call ends the process tree, the second
	// returns that same result, both under a bounded context and both nil.
	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
	firstErr := sess.Dispose(dctx)
	dcancel()
	if firstErr != nil {
		t.Fatalf("first Dispose: %v", firstErr)
	}
	dctx, dcancel = context.WithTimeout(context.Background(), 10*time.Second)
	secondErr := sess.Dispose(dctx)
	dcancel()
	if secondErr != nil {
		t.Fatalf("second Dispose after the first completed: %v", secondErr)
	}
}
