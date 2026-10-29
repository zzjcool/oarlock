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

// TestLiveExplicitResume is the resume claim of issue #64: a chat opened by
// its id in a NEW session (Options.Resume, the CLI's `--resume <chat id>`)
// really continues the earlier conversation, and the new session is a new
// stream instance — Seq starts at 1 and nothing is replayed. TestLiveTurn above
// proves only a second turn of the same session; this one disposes the first
// session, reopens the chat from its id and asks a question that can only be
// answered from the earlier turn's context.
//
// Opt-in (OAR_CURSOR_LIVE=1, a logged-in cursor-agent) for the same reason as
// every live test here. Only a credential the CLI could not use, or the CLI
// saying its own words that it cannot resume, is NOT VERIFIED: a resumed chat
// that does not remember the earlier turn is the failure this test exists to
// catch, never a skip.
func TestLiveExplicitResume(t *testing.T) {
	if os.Getenv("OAR_CURSOR_LIVE") != "1" {
		t.Skip("NOT VERIFIED: set OAR_CURSOR_LIVE=1 (needs a logged-in cursor-agent) to run an explicit --resume")
	}
	path, err := exec.LookPath("cursor-agent")
	if err != nil {
		t.Skipf("NOT VERIFIED: no cursor-agent binary on PATH: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// The code is the only thing that can come back from the first chat's
	// context. One workspace for both sessions: resume must not depend on a
	// reconstructed directory.
	const code = "FLAMINGO-42"
	dir := t.TempDir()
	inst := oarlock.Installation{Available: true, Path: path}

	// ---- session 1: plant the code in a fresh chat --------------------------
	first, err := Open(ctx, inst, oarlock.Options{Cwd: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s1 := first.(*Session)
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		_ = s1.Dispose(dctx)
	}()

	out, err := s1.Prompt(ctx, "Remember this code: "+code+". Reply with the single word: stored", oarlock.PromptOptions{})
	if err != nil {
		t.Fatalf("first Prompt: %v", err)
	}
	if out.Kind != oarlock.ControlAccepted {
		skipIfNotLoggedIn(t, out.Reason)
		t.Fatalf("first Prompt = %+v", out)
	}
	rs1 := waitLiveRecords(t, s1, ctx)
	logLiveRecords(t, rs1)
	requireLiveCompleted(t, "session 1", rs1)
	chat := s1.ChatID()
	if chat == "" {
		t.Fatal("the first run reported no chat id in system/init")
	}
	t.Logf("session 1 chat id: %s", chat)

	// ---- dispose it: one dispose pair, then an idempotent nil ----------------
	dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = s1.Dispose(dctx)
	dcancel()
	if err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	dctx, dcancel = context.WithTimeout(context.Background(), 30*time.Second)
	err = s1.Dispose(dctx)
	dcancel()
	if err != nil {
		t.Fatalf("second Dispose: %v, want the first call's nil (idempotent)", err)
	}
	rs1 = s1.Records(oarlock.Cursor{})
	disposeReq, disposeRes := disposePair(t, rs1)
	if n := countAction(rs1, oarlock.ActionDispose); n != 1 {
		t.Fatalf("the disposed session holds %d dispose requests, want one: %s", n, liveEvidence(rs1))
	}
	if n := len(responses(rs1, oarlock.ResponseExited)); n != 0 {
		t.Fatalf("the cleanly disposed session recorded %d exited responses, want none: %s", n, liveEvidence(rs1))
	}
	t.Logf("dispose pair: request seq=%d id=%s -> response seq=%d kind=%s",
		disposeReq.Seq, disposeReq.ID, disposeRes.Seq, disposeRes.Response.Kind)

	// ---- session 2: reopen the chat by id -----------------------------------
	resumeOpt := oarlock.Options{Cwd: dir, Resume: chat}
	resumed, err := Open(ctx, inst, resumeOpt)
	if err != nil {
		t.Fatalf("Open(resume %s): %v", chat, err)
	}
	s2 := resumed.(*Session)
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		_ = s2.Dispose(dctx)
	}()

	// Before any prompt: the new kernel is empty (nothing replayed) and the
	// session already knows the chat it opens on.
	if rs := s2.Records(oarlock.Cursor{}); len(rs) != 0 {
		t.Fatalf("the resumed session opened with %d records, want an empty stream: %s", len(rs), liveEvidence(rs))
	}
	if got := s2.ChatID(); got != chat {
		t.Fatalf("the resumed session's ChatID = %q, want the chat id %q from Options.Resume", got, chat)
	}

	// ARGV EVIDENCE (a): the very function open uses to build the child argv.
	launch := LaunchArgs(resumeOpt, chat)
	if !hasArgPair(launch, "--resume", chat) {
		t.Fatalf("LaunchArgs(%+v, %q) = %q, want --resume immediately followed by %s", resumeOpt, chat, launch, chat)
	}
	t.Logf("LaunchArgs pair: %q %q; full argv: %q", "--resume", chat, launch)
	t.Logf("resumed session opened on chat %s with an empty stream", chat)

	const question = "What code did I ask you to remember? Reply with the code only."
	out, err = s2.Prompt(ctx, question, oarlock.PromptOptions{})
	if err != nil {
		t.Fatalf("resumed Prompt: %v", err)
	}
	if out.Kind != oarlock.ControlAccepted {
		skipIfNotLoggedIn(t, out.Reason)
		skipIfResumeUnsupported(t, out.Reason)
		t.Fatalf("resumed Prompt = %+v", out)
	}

	// ARGV EVIDENCE (b): the real child's argv, read from /proc while the turn
	// runs. os.ReadFile compiles on every GOOS; a host without /proc (or a
	// process that exited first) skips only this sub-assertion.
	if childArgv, argvErr := liveChildArgv(ctx, s2); argvErr != nil {
		t.Logf("SKIP /proc argv sub-assertion (LaunchArgs above still pins the argv open uses): %v", argvErr)
	} else {
		t.Logf("real child argv (/proc): %q", childArgv)
		if !hasArgPair(childArgv, "--resume", chat) {
			t.Fatalf("the real child argv %q has no --resume immediately followed by %s", childArgv, chat)
		}
	}

	rs2 := waitLiveRecordsFor(t, s2, ctx, 0)
	logLiveRecords(t, rs2)
	// Only the runtime's own words can excuse a resume this host cannot do.
	skipIfResumeUnsupported(t, liveTurnText(rs2))
	requireLiveCompleted(t, "resumed turn", rs2)

	answer := liveText(rs2)
	t.Logf("resumed turn question: %q", question)
	t.Logf("resumed turn assistant text: %q", answer)
	if !strings.Contains(strings.ToUpper(answer), code) {
		t.Fatalf("the resumed chat did not remember %s (assistant text %q): the prior context did not survive --resume; %s",
			code, answer, liveEvidence(rs2))
	}

	// A new stream instance: Seq starts at 1 and only grows, and nothing from
	// session 1 was replayed into it.
	if len(rs2) == 0 {
		t.Fatal("the resumed session recorded nothing")
	}
	if got := rs2[0].Seq; got != 1 {
		t.Fatalf("the resumed session's first record has Seq %d, want 1: %s", got, liveEvidence(rs2))
	}
	var prev uint64
	for i, r := range rs2 {
		if r.Seq <= prev {
			t.Fatalf("the resumed session's records[%d].Seq = %d, want strictly greater than %d", i, r.Seq, prev)
		}
		prev = r.Seq
		if r.SessionID != chat {
			t.Fatalf("the resumed session's records[%d].SessionID = %q, want %q", i, r.SessionID, chat)
		}
	}
	if !rs2[0].ReceivedAt.After(rs1[len(rs1)-1].ReceivedAt) {
		t.Fatalf("the resumed stream's first record (%v) is not newer than session 1's last (%v): a session 1 record was replayed",
			rs2[0].ReceivedAt, rs1[len(rs1)-1].ReceivedAt)
	}
	if n := countAction(rs2, oarlock.ActionPrompt); n != 1 {
		t.Fatalf("the resumed session holds %d prompt requests, want one (session 1's is not replayed)", n)
	}
	t.Logf("resumed stream: %d records, seq %d..%d, all on chat %s", len(rs2), rs2[0].Seq, rs2[len(rs2)-1].Seq, chat)
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

// logLiveRecords is the evidence trail every live failure is reported with:
// one line per record, the runtime's own frames included.
func logLiveRecords(t *testing.T, rs []oarlock.Record) {
	t.Helper()
	for _, r := range rs {
		t.Logf("seq=%d kind=%s session=%q native=%.160s events=%d", r.Seq, r.Kind, r.SessionID, r.Native, len(r.Events))
	}
	for _, o := range outcomes(rs) {
		t.Logf("outcome=%s failure=%s reason=%.160s", o.Kind, o.Failure, o.Reason)
	}
}

// requireLiveCompleted asserts the turn in rs ended once and completed. A
// credential the CLI could not use is NOT VERIFIED, not a failure; anything
// else the runtime said is.
func requireLiveCompleted(t *testing.T, label string, rs []oarlock.Record) {
	t.Helper()
	if n := eventCount(rs, oarlock.EventTurnEnded); n != 1 {
		skipIfNotLoggedIn(t, liveTurnText(rs))
		t.Fatalf("%s: %d turn_ended events, want one; %s", label, n, liveEvidence(rs))
	}
	if got := liveOutcomes(rs); len(got) != 1 || got[0] != oarlock.OutcomeCompleted {
		skipIfNotLoggedIn(t, liveTurnText(rs))
		t.Fatalf("%s: outcomes = %+v, want one completed; %s", label, got, liveEvidence(rs))
	}
}

// liveText is the assistant text the stream states, in order: the per-frame
// delta (project.go's content lane) concatenated back into one answer.
func liveText(rs []oarlock.Record) string {
	var b strings.Builder
	for _, r := range rs {
		for _, e := range r.Events {
			if e.Kind == oarlock.EventTextDelta {
				b.WriteString(e.Text)
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// liveTurnText is every piece of prose the live run left behind about how its
// turn went: the outcome reasons and the stderr tail of an exit that had no
// result frame. Assistant text is deliberately not here.
func liveTurnText(rs []oarlock.Record) string {
	var parts []string
	for _, o := range outcomes(rs) {
		parts = append(parts, string(o.Failure), o.Reason)
	}
	if tail := exitedTail(rs); tail != "" {
		parts = append(parts, tail)
	}
	return strings.Join(parts, " ")
}

// skipIfResumeUnsupported keeps the one honest NOT VERIFIED case of the resume
// test: the runtime itself says this chat cannot be continued. It never matches
// on the model's answer, only on outcome/exit prose, so a chat that resumes but
// does not remember the code still fails.
func skipIfResumeUnsupported(t *testing.T, text string) {
	t.Helper()
	lower := strings.ToLower(text)
	markers := []string{
		"chat not found", "session not found", "no such session", "no such chat",
		"invalid chat", "invalid session", "invalid resume",
		"cannot resume", "can't resume", "could not resume", "unable to resume", "failed to resume",
	}
	// A flag-level complaint (unknown flag, unsupported option) only excuses
	// the resume when the runtime names the flag it could not take.
	if strings.Contains(lower, "resume") {
		markers = append(markers, "unknown option", "unknown flag", "unrecognized option", "unsupported", "not supported")
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			t.Skipf("NOT VERIFIED: cursor-agent cannot resume chat (%q)", strings.TrimSpace(text))
		}
	}
}

// disposePair returns the one dispose request and the response that answers it.
func disposePair(t *testing.T, rs []oarlock.Record) (oarlock.Record, oarlock.Record) {
	t.Helper()
	var req, res *oarlock.Record
	for i := range rs {
		r := &rs[i]
		if r.Kind == oarlock.KindRequest && r.Request != nil && r.Request.Action == oarlock.ActionDispose {
			if req != nil {
				t.Fatalf("a second dispose request was recorded (seq %d and %d); a dispose is answered once", req.Seq, r.Seq)
			}
			req = r
		}
	}
	if req == nil {
		t.Fatalf("no dispose request was recorded: %s", liveEvidence(rs))
	}
	for i := range rs {
		r := &rs[i]
		if r.Kind == oarlock.KindResponse && r.RequestID == req.ID {
			if r.Response == nil || r.Response.Kind != oarlock.ResponseAccepted {
				t.Fatalf("the dispose response = %+v, want accepted", r.Response)
			}
			res = r
		}
	}
	if res == nil {
		t.Fatalf("the dispose request %s was never answered: %s", req.ID, liveEvidence(rs))
	}
	return *req, *res
}

// hasArgPair reports whether argv holds a immediately followed by b.
func hasArgPair(argv []string, a, b string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == a && argv[i+1] == b {
			return true
		}
	}
	return false
}

// liveChildArgv reads the real argv of the running turn's child from
// /proc/<pid>/cmdline (NUL-separated) while the turn is in flight. The pid is
// the session's own current *sj.Process, read under the session mutex. It waits
// for the child to reach its own exec (the argv carries --output-format
// stream-json) so a fork that has not exec'd yet is not mistaken for the child.
// os.ReadFile is used so this compiles on every GOOS: a host without /proc just
// reports the reason and the caller skips only the /proc sub-assertion.
func liveChildArgv(ctx context.Context, s *Session) ([]string, error) {
	var lastErr error
	for {
		s.mu.Lock()
		turn := s.cur
		idle := turn == nil && !s.starting
		pid := 0
		if turn != nil && turn.p != nil {
			pid = turn.p.Pid()
		}
		s.mu.Unlock()

		if pid > 0 {
			raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
			switch {
			case err != nil:
				lastErr = fmt.Errorf("/proc/%d/cmdline: %w", pid, err)
			case len(raw) == 0:
				lastErr = fmt.Errorf("/proc/%d/cmdline is empty (the process is gone)", pid)
			default:
				parts := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
				if hasArgPair(parts, "--output-format", "stream-json") {
					return parts, nil
				}
				lastErr = fmt.Errorf("/proc/%d/cmdline = %q, not the executed child yet", pid, parts)
			}
		} else if idle {
			if lastErr == nil {
				lastErr = fmt.Errorf("the turn's process exited before /proc/<pid>/cmdline could be read")
			}
			return nil, lastErr
		}

		select {
		case <-ctx.Done():
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			return nil, lastErr
		case <-time.After(2 * time.Millisecond):
		}
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
