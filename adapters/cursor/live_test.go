package cursor

// live_test.go — the one claim only a real cursor-agent can prove: that the
// argv, the stdin EOF handshake and the stream really work against the
// installed CLI. Opt-in (OAR_CURSOR_LIVE=1, a logged-in cursor-agent);
// otherwise skipped as NOT VERIFIED. Everything else in this package runs
// against real processes but a fake agent (helper_test.go) and against the
// recorded real stream (testdata/cursor).

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
)

func TestLiveTurn(t *testing.T) {
	if os.Getenv("OAR_CURSOR_LIVE") != "1" {
		t.Skip("NOT VERIFIED: set OAR_CURSOR_LIVE=1 (needs a logged-in cursor-agent) to run a real turn")
	}
	path, err := exec.LookPath("cursor-agent")
	if err != nil {
		t.Skipf("NOT VERIFIED: no cursor-agent binary on PATH: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sess, err := Open(ctx, oarlock.Installation{Available: true, Path: path}, oarlock.Options{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s := sess.(*Session)
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		_ = s.Dispose(dctx)
	}()

	out, err := s.Prompt(ctx, "Reply with the single word: ok", oarlock.PromptOptions{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if out.Kind == oarlock.ControlRejected {
		skipIfNotLoggedIn(t, out.Reason)
		t.Fatalf("Prompt = %+v", out)
	}

	rs := waitLiveRecordsFor(t, s, ctx, 0)
	if len(rs) == 0 {
		t.Skip("NOT VERIFIED: cursor-agent exited without a single frame (not logged in?)")
	}
	for _, r := range rs {
		t.Logf("seq=%d kind=%s session=%s native=%.120s events=%d", r.Seq, r.Kind, r.SessionID, r.Native, len(r.Events))
	}
	for _, e := range outcomes(rs) {
		t.Logf("outcome=%s failure=%s reason=%.120s", e.Kind, e.Failure, e.Reason)
		if e.Kind == oarlock.OutcomeFailed {
			skipIfNotLoggedIn(t, e.Reason+string(e.Failure))
			t.Fatalf("the live turn failed: %+v", e)
		}
	}

	if n := eventCount(rs, oarlock.EventTurnEnded); n != 1 {
		if tail := exitedTail(rs); tail != "" {
			skipIfNotLoggedIn(t, tail)
			t.Fatalf("the live turn never ended (%d turn_ended); exited: %s", n, tail)
		}
		t.Fatalf("the live turn never ended (%d turn_ended events)", n)
	}
	if got := liveOutcomes(rs); len(got) != 1 || got[0] != oarlock.OutcomeCompleted {
		t.Fatalf("live outcome = %+v, want one completed", got)
	}
	chat := s.ChatID()
	if chat == "" {
		t.Fatal("the live run reported no chat id in system/init")
	}
	t.Logf("chat id: %s", chat)
	t.Logf("%d records from the live cursor-agent", len(rs))

	// A second turn is a second real process on the same chat: this is the one
	// claim only the installed CLI can prove (--resume <chat id>).
	before := len(rs)
	if out, err := s.Prompt(ctx, "Reply with the single word: done", oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("second Prompt = %+v, %v (the chat %s must be resumable)", out, err, chat)
	}
	rs = waitLiveRecordsFor(t, s, ctx, before)
	if got := liveOutcomes(rs[before:]); len(got) != 1 || got[0] != oarlock.OutcomeCompleted {
		t.Fatalf("second turn outcomes = %+v, want one completed", got)
	}
	if got := s.ChatID(); got != chat {
		t.Fatalf("the resumed turn reported chat %q, want %q", got, chat)
	}
	t.Logf("second turn completed on the same chat; %d records total", len(s.Records(oarlock.Cursor{})))
}

// waitLiveRecords waits for the live turn to end (a turn_ended event or an
// exited response), bounded by ctx.
func waitLiveRecords(t *testing.T, s *Session, ctx context.Context) []oarlock.Record {
	t.Helper()
	return waitLiveRecordsFor(t, s, ctx, 0)
}

// waitLiveRecordsFor is waitLiveRecords for the turn that started after the
// first `records` records.
func waitLiveRecordsFor(t *testing.T, s *Session, ctx context.Context, records int) []oarlock.Record {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		rs := s.Records(oarlock.Cursor{})
		if eventCount(rs[records:], oarlock.EventTurnEnded) > 0 || len(responses(rs[records:], oarlock.ResponseExited)) > 0 {
			return rs
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return rs
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func exitedTail(rs []oarlock.Record) string {
	if exits := responses(rs, oarlock.ResponseExited); len(exits) > 0 {
		return exits[len(exits)-1].Response.Reason
	}
	return ""
}

func liveOutcomes(rs []oarlock.Record) (out []oarlock.OutcomeKind) {
	for _, o := range outcomes(rs) {
		out = append(out, o.Kind)
	}
	return out
}

func skipIfNotLoggedIn(t *testing.T, text string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, marker := range []string{"login", "log in", "logged out", "unauthorized", "not authenticated", "api key", "credential"} {
		if strings.Contains(lower, marker) {
			t.Skipf("NOT VERIFIED: cursor-agent is not logged in (%q); run `cursor-agent login`", strings.TrimSpace(text))
		}
	}
}
