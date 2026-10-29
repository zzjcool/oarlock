package acp

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

var envMu sync.Mutex

// open starts the profile against this test binary acting as the agent.
func open(t *testing.T, prof Profile, flavor string, opt oarlock.Options, env ...string) oarlock.Session {
	t.Helper()
	s, err := tryOpen(prof, flavor, opt, env...)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = s.Dispose(ctx)
	})
	return s
}

func tryOpen(prof Profile, flavor string, opt oarlock.Options, env ...string) (oarlock.Session, error) {
	exe, _ := os.Executable()
	// Harness variables ride the process environment, not Options.Env: every
	// Options.Env value is a redaction candidate, and "opencode" must stay readable.
	envMu.Lock()
	defer envMu.Unlock()
	set := map[string]string{"ACP_FAKE_AGENT": flavor}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		set[k] = v
	}
	for k, v := range set {
		old, had := os.LookupEnv(k)
		os.Setenv(k, v)
		defer func(k, old string, had bool) {
			if had {
				os.Setenv(k, old)
			} else {
				os.Unsetenv(k)
			}
		}(k, old, had)
	}
	if opt.Cwd == "" {
		opt.Cwd, _ = os.Getwd()
	}
	// The fake ignores the vendor arguments; the test binary needs none of them.
	p := prof
	p.Args = func(oarlock.Options) []string { return []string{"-test.run=^$"} }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return p.Start(ctx, oarlock.Installation{Path: exe}, opt)
}

func prompt(t *testing.T, s oarlock.Session, text string) oarlock.ControlOutcome {
	t.Helper()
	out, err := s.Prompt(context.Background(), text, oarlock.PromptOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func records(s oarlock.Session) []oarlock.Record { return s.Records(oarlock.Cursor{}) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s\n%s", what, dump(nil))
}

func waitIdle(t *testing.T, s oarlock.Session) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		recs := records(s)
		st := fold.StatusOf(recs).Value.Kind
		if st == fold.StatusIdle && hasEvent(recs, oarlock.EventTurnEnded) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("turn never ended\n%s", dump(records(s)))
}

func hasEvent(recs []oarlock.Record, kind oarlock.EventKind) bool { return len(events(recs, kind)) > 0 }

func events(recs []oarlock.Record, kind oarlock.EventKind) []oarlock.Event {
	var out []oarlock.Event
	for _, r := range recs {
		for _, e := range r.Events {
			if e.Kind == kind {
				out = append(out, e)
			}
		}
	}
	return out
}

func text(recs []oarlock.Record) string {
	var b strings.Builder
	for _, e := range events(recs, oarlock.EventTextDelta) {
		b.WriteString(e.Text)
	}
	return b.String()
}

func dump(recs []oarlock.Record) string {
	var b strings.Builder
	for _, r := range recs {
		j, _ := json.Marshal(r)
		b.Write(j)
		b.WriteByte('\n')
	}
	return b.String()
}

func assertDense(t *testing.T, recs []oarlock.Record) {
	t.Helper()
	for i, r := range recs {
		if r.Seq != uint64(i+1) {
			t.Fatalf("seq not dense at %d: %d\n%s", i, r.Seq, dump(recs))
		}
	}
}
