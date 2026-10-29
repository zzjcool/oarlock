package cursor

// live_test.go — the one claim only a real cursor-agent can prove: that the
// argv, the stdin EOF handshake and the stream really work against the
// installed CLI. Opt-in (OAR_CURSOR_LIVE=1, a logged-in cursor-agent);
// otherwise skipped as NOT VERIFIED. Everything else in this package runs
// against real processes but a fake agent (helper_test.go) and against the
// recorded real stream (testdata/cursor).

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/observe"
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

// TestLivePromptAndWait is the cross-runtime claim of issue #41: a host that
// only imports this adapter can go through the public surface — Require, a
// fresh session, PromptAndWait for the runtime's own turn end, the observe
// status fold, then Dispose. TestLiveTurn above pins the adapter's own stream;
// this one pins the surface a host actually uses on top of it.
//
// Opt-in (OAR_CURSOR_LIVE=1, a logged-in cursor-agent) for the same reason as
// every live test here: it spawns the installed CLI, spends a real account's
// quota and takes about a minute, so CI and `go test -race ./...` must never
// need it. A missing login is a NOT VERIFIED skip, never a failure.
func TestLivePromptAndWait(t *testing.T) {
	if os.Getenv("OAR_CURSOR_LIVE") != "1" {
		t.Skip("NOT VERIFIED: set OAR_CURSOR_LIVE=1 (needs a logged-in cursor-agent) to run PromptAndWait against the real CLI")
	}
	path, err := exec.LookPath("cursor-agent")
	if err != nil {
		t.Skipf("NOT VERIFIED: no cursor-agent binary on PATH: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	rt, err := oarlock.Require("cursor")
	if err != nil {
		t.Fatalf("Require(cursor): %v", err)
	}
	sess, err := rt.Session(ctx, oarlock.Installation{Available: true, Path: path}, oarlock.Options{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	// The explicit double Dispose below is the assertion; this is the safety net
	// that leaves no cursor-agent process behind when an assertion fails first.
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dcancel()
		_ = sess.Dispose(dctx)
	}()

	run, err := oarlock.PromptAndWait(ctx, sess, "Reply with the single word: ok", oarlock.WithTimeout(2*time.Minute))
	records := sess.Records(oarlock.Cursor{})
	for _, r := range records {
		t.Logf("seq=%d kind=%s session=%q agentPath=%v events=%d", r.Seq, r.Kind, r.SessionID, r.AgentPath, len(r.Events))
	}
	if err != nil {
		t.Fatalf("PromptAndWait: %v; %s", err, liveEvidence(records))
	}
	t.Logf("run: outcome=%s seq=%d request=%q text=%q turn=%+v", run.Outcome, run.Seq, run.RequestID, run.Text, run.Turn)

	if run.Outcome != oarlock.RunCompleted {
		// A credential the CLI could not use is NOT VERIFIED, not a failure; a
		// stream that simply never ended the turn is.
		skipIfNotLoggedIn(t, liveFailureText(run, records))
		t.Fatalf("outcome = %s, want completed; run = %+v; %s", run.Outcome, run, liveEvidence(records))
	}
	if run.Text == "" || strings.TrimSpace(run.Text) == "" {
		t.Fatalf("run.Text = %q, want the runtime's own words; run = %+v; %s", run.Text, run, liveEvidence(records))
	}
	if run.Turn == nil || run.Turn.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("run.Turn = %+v, want one completed turn; %s", run.Turn, liveEvidence(records))
	}
	if got := observe.StatusOf(records); got.Value.Kind != observe.StatusIdle {
		t.Fatalf("status after the run = %+v (seq %d), want idle; %s", got.Value, got.Seq, liveEvidence(records))
	}
	if run.Seq == 0 {
		t.Fatal("run.Seq = 0, want the seq of the last record the run consumed")
	}
	var prev uint64
	for i, r := range records {
		if r.Seq <= prev {
			t.Fatalf("records[%d].Seq = %d, want strictly greater than %d", i, r.Seq, prev)
		}
		prev = r.Seq
	}

	// Idempotent dispose: the second call answers from the first one's result,
	// under its own bounded context.
	for i := 1; i <= 2; i++ {
		dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := sess.Dispose(dctx)
		dcancel()
		if err != nil {
			t.Fatalf("Dispose #%d: %v", i, err)
		}
	}
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

// liveFailureText is every piece of prose the live run left behind, for
// skipIfNotLoggedIn to judge: a rejection reason, the runtime's own turn
// outcome, and the stderr tail of an exit that had no result frame.
func liveFailureText(run oarlock.RunResult, rs []oarlock.Record) string {
	parts := []string{run.Reason}
	if run.Turn != nil {
		parts = append(parts, string(run.Turn.Failure), run.Turn.Reason)
	}
	if tail := exitedTail(rs); tail != "" {
		parts = append(parts, tail)
	}
	return strings.Join(parts, " ")
}

// liveEvidence is the record trail a failing live run is reported with: the
// seq range, the session ids the stream was actually stamped with, and every
// turn outcome plus any exit, so a failure says what the runtime said instead
// of only that an expectation was missed.
func liveEvidence(rs []oarlock.Record) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d records", len(rs))
	if len(rs) > 0 {
		fmt.Fprintf(&b, " (seq %d..%d)", rs[0].Seq, rs[len(rs)-1].Seq)
	}
	var sessions []string
	seen := map[string]bool{}
	for _, r := range rs {
		if !seen[r.SessionID] {
			seen[r.SessionID] = true
			sessions = append(sessions, fmt.Sprintf("%q", r.SessionID))
		}
	}
	fmt.Fprintf(&b, "; sessions %s; %d turn_ended", strings.Join(sessions, ", "), eventCount(rs, oarlock.EventTurnEnded))
	for _, o := range outcomes(rs) {
		fmt.Fprintf(&b, " [%s failure=%q reason=%q]", o.Kind, o.Failure, strings.TrimSpace(o.Reason))
	}
	if tail := strings.TrimSpace(exitedTail(rs)); tail != "" {
		fmt.Fprintf(&b, "; exited: %q", tail)
	}
	return b.String()
}
