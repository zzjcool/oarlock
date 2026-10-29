package codex

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
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

// ---- issue #63: the option surface, live ------------------------------------------------

// TestLiveCodexOptions is the live counterpart of the option surface issue #54
// could only assert against the fake app-server: userConfiguration=exclude, the
// effort of a resumed thread, and an MCP server (with a tool denial when the
// model really calls the tool).
//
// It shares TestLivePromptAndWait's gate and preconditions (OARLOCK_LIVE_CODEX=1,
// codex on PATH inside the version window, a CODEX_HOME whose config.toml names
// an OpenAI-Responses provider, that provider's token in the env_key variable
// the file names — OPENAI_API_KEY here) and skips as NOT VERIFIED everywhere
// else. The provider file is only ever copied; its content (a base URL, a model
// id, the *name* of the token variable) is never printed, and the token itself
// is never read by this test — `codex app-server` inherits the process
// environment.
//
// Each subtest runs on its own temporary CODEX_HOME, so a live run leaves the
// operator's codex state untouched.
func TestLiveCodexOptions(t *testing.T) {
	if os.Getenv("OARLOCK_LIVE_CODEX") != "1" {
		t.Skip("NOT VERIFIED: set OARLOCK_LIVE_CODEX=1 to run the codex option surface against a real codex")
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		t.Skipf("NOT VERIFIED: no codex binary on PATH: %v", err)
	}
	provider, ok := liveProviderConfig()
	if !ok {
		t.Skip("NOT VERIFIED: set CODEX_HOME to a directory whose config.toml names the model, model_provider and env_key")
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("NOT VERIFIED: set OPENAI_API_KEY to the token CODEX_HOME/config.toml names as env_key")
	}
	t.Run("userConfigurationExclude", func(t *testing.T) { liveExcludeTurn(t, path, provider) })
	t.Run("resumeEffort", func(t *testing.T) { liveResumeEffort(t, path, provider) })
	t.Run("mcpServers", func(t *testing.T) { liveMCP(t, path, provider) })
}

// liveProviderConfig is $CODEX_HOME/config.toml: the operator's provider block,
// kept in memory. It holds a base URL, a model id and the name of the token
// variable — never the token.
func liveProviderConfig() ([]byte, bool) {
	home := os.Getenv(codexHomeEnv)
	if home == "" {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return b, true
}

// liveHome is a fresh temporary CODEX_HOME holding the operator's provider
// config: the session writes its state there, not into the operator's home.
func liveHome(t *testing.T, provider []byte) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), provider, 0o600); err != nil {
		t.Fatalf("write the temporary codex home: %v", err)
	}
	t.Setenv(codexHomeEnv, home)
	return home
}

// liveTurn is one prompt through the public surface, bounded, asserting a
// completed run with text: what a host is promised about a turn.
func liveTurn(t *testing.T, ctx context.Context, sess oarlock.Session, input string) oarlock.RunResult {
	t.Helper()
	run, err := oarlock.PromptAndWait(ctx, sess, input, oarlock.WithTimeout(2*time.Minute))
	if err != nil {
		t.Fatalf("PromptAndWait(%q): %v", input, err)
	}
	if run.Outcome != oarlock.RunCompleted {
		t.Fatalf("run.Outcome = %q (reason %q), want %q", run.Outcome, run.Reason, oarlock.RunCompleted)
	}
	if strings.TrimSpace(run.Text) == "" {
		t.Fatalf("run.Text = %q; the answer never reached the stream", run.Text)
	}
	if run.Turn == nil || run.Turn.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("run.Turn = %+v, want the runtime's own completed turn", run.Turn)
	}
	return run
}

// liveDir is a directory's shape (file name → content, subdirectory → "/") so a
// test can prove an open wrote nothing there.
func liveDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out[e.Name()+"/"] = ""
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s/%s: %v", dir, e.Name(), err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

// liveNames is liveDir's keys, for an error message that quotes the shape of a
// directory and not its files (a config.toml here may name a private endpoint).
func liveNames(d map[string]string) []string {
	out := make([]string, 0, len(d))
	for k := range d {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// livePoisonConfig rewrites the provider config into one that cannot serve a
// turn (loopback port 9, a model the endpoint does not serve). It is what the
// user's own home holds in the exclude test: a session that read it could not
// complete a turn.
func livePoisonConfig(t *testing.T, provider []byte) []byte {
	t.Helper()
	lines := strings.Split(string(provider), "\n")
	var sawModel, sawBase bool
	for i, line := range lines {
		key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "model":
			lines[i] = `model = "oarlock-live-poison"`
			sawModel = true
		case "base_url":
			lines[i] = `base_url = "http://127.0.0.1:9/v1"`
			sawBase = true
		}
	}
	if !sawModel || !sawBase {
		t.Skipf("NOT VERIFIED: CODEX_HOME/config.toml names no top-level model (%v) or base_url (%v) to poison", sawModel, sawBase)
	}
	return []byte(strings.Join(lines, "\n"))
}

// liveExcludeTurn: userConfiguration=exclude runs one real turn on the isolated
// home. The user's home holds a config that cannot work, so a completed turn is
// proof the isolated home — not the user's configuration — served it; the user's
// home is unchanged afterwards.
func liveExcludeTurn(t *testing.T, path string, provider []byte) {
	home := t.TempDir() // the "user's" codex home: poison, and read-only to oarlock
	if err := os.WriteFile(filepath.Join(home, "config.toml"), livePoisonConfig(t, provider), 0o600); err != nil {
		t.Fatalf("write the poisoned user config: %v", err)
	}
	isolated := home + isolatedHomeSuffix
	t.Cleanup(func() { _ = os.RemoveAll(isolated) }) // the sibling of a temp dir is not cleaned by t.TempDir
	if err := os.MkdirAll(isolated, 0o700); err != nil {
		t.Fatalf("create the isolated home: %v", err)
	}
	if err := os.WriteFile(filepath.Join(isolated, "config.toml"), provider, 0o600); err != nil {
		t.Fatalf("seed the isolated home with the working provider config: %v", err)
	}
	t.Setenv(codexHomeEnv, home)
	before := liveDir(t, home)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sess, err := OpenWith(ctx, oarlock.Installation{Available: true, Path: path},
		oarlock.Options{Cwd: t.TempDir(), UserConfiguration: oarlock.UserConfigurationExclude}, Config{})
	if err != nil {
		t.Fatalf("open with userConfiguration=exclude: %v", err)
	}
	disposed := false
	defer func() {
		if !disposed {
			_ = sess.Dispose(context.Background())
		}
	}()

	run := liveTurn(t, ctx, sess, "Reply with the single word: pong")
	t.Logf("exclude turn completed at seq %d on thread %s: %d bytes of text", run.Seq, sess.ThreadID(), len(run.Text))

	// The user's own home is read, never written: the poison is still there, byte
	// for byte, and nothing else appeared.
	if after := liveDir(t, home); !reflect.DeepEqual(after, before) {
		t.Errorf("the user's own codex home changed: %v → %v", liveNames(before), liveNames(after))
	}
	// The isolated home is where the session ran: codex wrote its own state next
	// to the config.toml the test seeded (sessions/, logs, the thread database).
	entries, err := os.ReadDir(isolated)
	if err != nil {
		t.Fatalf("read the isolated home: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	t.Logf("isolated home %s holds %v", isolated, names)
	if len(names) < 2 {
		t.Errorf("the isolated home holds %v; a real turn leaves codex's own state there, so the session cannot have run on it", names)
	}
	// And the isolated home really is a resumable home of its own: the thread this
	// session opened exists there, so the same home reopens it (thread ids are
	// home-scoped). Not a second turn — one turn per subtest keeps the run short.
	if sess.ThreadID() == "" {
		t.Error("the exclude session opened no thread")
	}

	dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dcancel()
	disposed = true
	if err := sess.Dispose(dctx); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
}

// liveResumeEffort: a resumed thread gets the requested level through codex's
// own thread/settings/update and only counts when codex reports it back.
func liveResumeEffort(t *testing.T, path string, provider []byte) {
	liveHome(t, provider)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cwd := t.TempDir()
	inst := oarlock.Installation{Available: true, Path: path}

	first, err := OpenWith(ctx, inst, oarlock.Options{Cwd: cwd, Effort: "low"}, Config{})
	if err != nil {
		t.Fatalf("open a new thread at effort low: %v", err)
	}
	run := liveTurn(t, ctx, first, "Reply with the single word: pong")
	thread := first.ThreadID()
	t.Logf("thread %s ran at effort low (seq %d)", thread, run.Seq)
	dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = first.Dispose(dctx)
	dcancel()
	if err != nil {
		t.Fatalf("Dispose: %v", err)
	}

	second, err := OpenWith(ctx, inst, oarlock.Options{Cwd: cwd, Resume: thread, Effort: "high"}, Config{})
	if err != nil {
		t.Fatalf("resume %s at effort high: %v", thread, err)
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		_ = second.Dispose(dctx)
	}()
	if got := second.ThreadID(); got != thread {
		t.Fatalf("resumed thread = %q, want %q", got, thread)
	}

	recs := second.Records(oarlock.Cursor{})
	// What the resume reply named before any settings update: the level the
	// thread last ran with. Different from "high" is why thread/settings/update
	// had to run below (a fact for the docs, not an assertion).
	for _, r := range recs {
		for _, e := range r.Events {
			if e.Kind == oarlock.EventEffort {
				t.Logf("the resume reply itself named effort %q (requested %q)", e.Effort, "high")
			}
		}
	}
	frames := settingsFrames(recs)
	if len(frames) == 0 {
		t.Fatalf("no thread/settings/updated on the resumed stream; stream = %q", skeleton(recs))
	}
	last := frames[len(frames)-1]
	effort, _ := parseObj(last.Native).obj("params").obj("threadSettings").str("effort")
	if effort != "high" {
		t.Fatalf("thread/settings/updated reports effort %q, want %q (stream = %q)", effort, "high", skeleton(recs))
	}
	if got := observe.EffortOf(recs); got.Value != "high" {
		t.Fatalf("observe.EffortOf = {%q @%d}, want high (stream = %q)", got.Value, got.Seq, skeleton(recs))
	}
	t.Logf("resume set effort=high through codex's own settings channel; stream = %q", skeleton(recs))
}

// liveMCPStatus is codex's own view of a session's MCP servers
// (mcpServerStatus/list): the same inventory a host's tool set would be built
// from, so a server config the app-server only stored but never started fails
// here.
type liveMCPStatus struct {
	Data []struct {
		Name          string `json:"name"`
		RuntimeStatus string `json:"runtimeStatus"`
		ServerInfo    *struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		// tools is codex's own map (tool name → definition), mode/type
		// included; the test only reads the keys.
		Tools      map[string]json.RawMessage `json:"tools"`
		ToolsError json.RawMessage            `json:"toolsError"`
	} `json:"data"`
}

// liveMCPView polls mcpServerStatus/list until codex lists the named server,
// and returns codex's own view of it: the runtime status and the tool names the
// model can reach. A name absent from Tools is a tool codex never offers, which
// is what a native denial looks like from the runtime's side.
func liveMCPView(t *testing.T, ctx context.Context, sess *Session, name string) liveMCPStatus {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last liveMCPStatus
	for {
		reply, err := sess.conn.Call(ctx, "mcpServerStatus/list", map[string]any{"threadId": sess.ThreadID()}, nil)
		if err != nil {
			t.Fatalf("mcpServerStatus/list: %v", err)
		}
		last = liveMCPStatus{}
		if err := json.Unmarshal(reply.Result, &last); err != nil {
			t.Fatalf("mcpServerStatus/list reply is not the pinned shape: %v (%s)", err, reply.Result)
		}
		for _, s := range last.Data {
			if s.Name != name {
				continue
			}
			if s.RuntimeStatus == "failed" || s.RuntimeStatus == "cancelled" {
				t.Fatalf("codex accepted the config but the server %q is %q: %s", name, s.RuntimeStatus, string(s.ToolsError))
			}
			// The connection is started lazily: a first answer may be "starting".
			if s.RuntimeStatus != "" && s.RuntimeStatus != "starting" {
				t.Logf("codex reports MCP server %q: status %q, serverInfo %+v, tools %v",
					s.Name, s.RuntimeStatus, s.ServerInfo, liveKeys(s.Tools))
				return last
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("codex did not report MCP server %q within 30s: %+v", name, last.Data)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// liveTools is codex's tool inventory for one server, in order.
func liveTools(view liveMCPStatus, name string) []string {
	for _, s := range view.Data {
		if s.Name == name {
			return liveKeys(s.Tools)
		}
	}
	return nil
}

// liveApprovals answers the app-server's toApp requests the way a headless host
// would, and counts them. codex asks for an MCP tool call as
// `mcpServer/elicitation/request` with `_meta.codex_approval_kind:
// "mcp_tool_call"`, and only runs the tool when the answer is
// `{"action":"accept"}`. It runs until stop is closed.
type liveApprovals struct {
	mu   sync.Mutex
	saw  []string
	done chan struct{}
}

func (a *liveApprovals) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.saw)
}

func (a *liveApprovals) methods() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.saw...)
}

func liveAutoApprove(t *testing.T, ctx context.Context, sess *Session, a *liveApprovals) {
	t.Helper()
	for {
		select {
		case <-a.done:
			return
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
		for _, r := range sess.Records(oarlock.Cursor{}) {
			if r.Kind != oarlock.KindRequest || r.Direction != oarlock.ToApp {
				continue
			}
			a.mu.Lock()
			seen := false
			for _, id := range a.saw {
				if id == r.ID {
					seen = true
				}
			}
			if !seen {
				a.saw = append(a.saw, r.ID)
			}
			a.mu.Unlock()
			if seen {
				continue
			}
			// The runtime's own shape for that request: an MCP approval is an
			// elicitation ("accept" runs the tool), a command/file approval
			// carries a decision.
			var result any = map[string]any{"decision": "accept"}
			if r.Request.NativeType == "mcpServer/elicitation/request" {
				result = map[string]any{"action": "accept"}
			}
			t.Logf("answering %s %s", r.Request.NativeType, r.ID)
			if err := sess.Answer(ctx, r.ID, result); err != nil {
				t.Logf("Answer(%s): %v", r.ID, err)
			}
		}
	}
}

// liveTurnBounded sends one prompt and waits up to wait for the turn to end,
// answering approvals in the background. It reports whether the turn ended and
// the streamed text; a turn still running after wait is aborted (one live turn
// must never hang the suite), and the test decides whether that is a failure.
func liveTurnBounded(t *testing.T, ctx context.Context, sess *Session, input string, wait time.Duration) (string, bool) {
	t.Helper()
	approvals := &liveApprovals{done: make(chan struct{})}
	go liveAutoApprove(t, ctx, sess, approvals)
	defer close(approvals.done)
	out, err := sess.Prompt(ctx, input, oarlock.PromptOptions{})
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	deadline := time.Now().Add(wait)
	for !hasEvent(sess.Records(oarlock.Cursor{}), oarlock.EventTurnEnded) {
		if time.Now().After(deadline) {
			actx, acancel := context.WithTimeout(context.Background(), 30*time.Second)
			if _, aerr := sess.Abort(actx); aerr != nil {
				t.Logf("Abort after the bounded wait: %v", aerr)
			}
			acancel()
			t.Logf("the turn did not end within %v; stream = %q", wait, skeleton(sess.Records(oarlock.Cursor{})))
			return liveText(sess), false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return liveText(sess), true
}

// liveText is the text the root agent streamed so far.
func liveText(sess *Session) string {
	var b strings.Builder
	for _, r := range sess.Records(oarlock.Cursor{}) {
		for _, e := range r.Events {
			if e.Kind == oarlock.EventTextDelta {
				b.WriteString(e.Text)
			}
		}
	}
	return b.String()
}

// liveMCP: an MCP tool call that really runs, the same tool denied natively,
// and the server read back on a resumed thread.
//
// Issue #63 left this as NOT VERIFIED: under the adapter's default approval
// policy ("never") codex auto-denies an MCP tool call and never asks, so the
// tool never reaches the server and the model's reply proves nothing. Issue #70
// drives the approval channel instead: with Config{ApprovalPolicy:
// "on-request"} codex asks the host (mcpServer/elicitation/request), the test
// accepts through Session.Answer, and the call lands on the echo server —
// which is the difference a native denial has to be told apart from.
func liveMCP(t *testing.T, path string, provider []byte) {
	liveHome(t, provider)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	inst := oarlock.Installation{Available: true, Path: path}
	marker := filepath.Join(t.TempDir(), "mcp-echo.marker.txt")
	server := mcpEchoServer(t, marker)

	// 1. The config override is accepted: codex starts the server and reports
	// it with the tool it lists.
	cwd := t.TempDir()
	allowed, err := OpenWith(ctx, inst, oarlock.Options{
		Cwd: cwd, MCPServers: []oarlock.MCPServer{server},
	}, Config{ApprovalPolicy: "on-request"})
	if err != nil {
		t.Fatalf("open with an mcpServers entry: %v", err)
	}
	dispose := func(s *Session) {
		t.Helper()
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		if err := s.Dispose(dctx); err != nil {
			t.Fatalf("Dispose: %v", err)
		}
	}
	thread := allowed.ThreadID()
	view := liveMCPView(t, ctx, allowed, "echo")
	if got := liveTools(view, "echo"); !slices.Contains(got, "shout") {
		t.Fatalf("codex lists the server without its tool: tools = %v, want shout", got)
	}
	if got := liveMarker(t, marker); !strings.Contains(got, "initialize") {
		t.Fatalf("the MCP server process was never started by codex: marker = %q", got)
	}

	// 2. Positive control: the approval channel lets the call run, and the echo
	// server — not the model's prose — records `tools/call shout`.
	output, ended := liveMCPCall(t, ctx, allowed, marker)
	if !ended {
		t.Fatalf("the allowed turn never ended; see the stream above")
	}
	if calls := strings.Count(liveMarker(t, marker), "tools/call shout"); calls == 0 {
		t.Fatalf("codex never ran the MCP tool (text %q); without a real call a denial proves nothing", output)
	}
	t.Logf("the MCP tool ran: the server recorded tools/call shout; the model answered %q", output)
	dispose(allowed)

	// 3. Denied: the same tool, a session that disables it natively
	// (disabled_tools). codex's own inventory no longer carries it, no approval
	// is ever asked (there is nothing to approve), and the call never reaches
	// the server — which is exactly how this differs from an approval refusal,
	// where the request is recorded and the runtime answers "rejected".
	callsBefore := strings.Count(liveMarker(t, marker), "tools/call shout")
	denied, err := OpenWith(ctx, inst, oarlock.Options{
		Cwd: t.TempDir(), MCPServers: []oarlock.MCPServer{server},
		DisallowedTools: []string{"mcp__echo__shout"},
	}, Config{ApprovalPolicy: "on-request"})
	if err != nil {
		t.Fatalf("open with disallowedTools: %v", err)
	}
	viewDenied := liveMCPView(t, ctx, denied, "echo")
	if got := liveTools(viewDenied, "echo"); len(got) != 0 {
		t.Fatalf("disallowedTools did not remove the tool from codex's inventory: tools = %v, want none", got)
	}
	// A turn that asks for the denied tool: whether the model gives up quietly
	// or keeps going is not the point, so it is bounded and aborted if needed.
	text, _ := liveTurnBounded(t, ctx, denied, liveMCPPrompt, 2*time.Minute)
	if calls := strings.Count(liveMarker(t, marker), "tools/call shout"); calls != callsBefore {
		t.Fatalf("the denied tool reached the MCP server anyway: %d tools/call shout before, %d after", callsBefore, calls)
	}
	approvals := 0
	for _, r := range denied.Records(oarlock.Cursor{}) {
		if r.Kind == oarlock.KindRequest && r.Direction == oarlock.ToApp {
			approvals++
		}
	}
	if approvals != 0 {
		t.Fatalf("the denied session asked for %d approval(s); a native denial never reaches the approval channel (stream = %q)",
			approvals, skeleton(denied.Records(oarlock.Cursor{})))
	}
	t.Logf("disallowedTools removed mcp__echo__shout natively: codex inventory empty, no approval asked, no tools/call on the server (model answered %q)", text)
	dispose(denied)

	// 4. Resume readback: the resumed thread still knows the server. The
	// evidence is codex's own mcpServerStatus/list for that thread — config/read
	// is NOT thread-scoped (it reports the user's config.toml, whose
	// mcp_servers table is empty here), so it cannot answer this question.
	resumed, err := OpenWith(ctx, inst, oarlock.Options{
		Cwd: cwd, Resume: thread, MCPServers: []oarlock.MCPServer{server},
	}, Config{ApprovalPolicy: "on-request"})
	if err != nil {
		t.Fatalf("resume %s: %v", thread, err)
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		_ = resumed.Dispose(dctx)
	}()
	if got := resumed.ThreadID(); got != thread {
		t.Fatalf("resumed thread = %q, want %q", got, thread)
	}
	viewResumed := liveMCPView(t, ctx, resumed, "echo")
	if got := liveTools(viewResumed, "echo"); !slices.Contains(got, "shout") {
		t.Fatalf("the resumed thread lost its MCP server: tools = %v, want shout", got)
	}
	if reply, err := resumed.conn.Call(ctx, "config/read", map[string]any{"cwd": cwd, "includeLayers": false}, nil); err != nil {
		t.Logf("config/read on the resumed session: %v (the resume readback above is codex's own status)", err)
	} else {
		var read struct {
			Config struct {
				MCPServers map[string]json.RawMessage `json:"mcp_servers"`
			} `json:"config"`
		}
		if uerr := json.Unmarshal(reply.Result, &read); uerr != nil {
			t.Fatalf("config/read reply is not the pinned shape: %v", uerr)
		}
		t.Logf("config/read reports %d mcp_servers from the config layers (a thread override is not one of them)", len(read.Config.MCPServers))
	}
	t.Logf("the resumed thread %s still reports MCP server echo with tool shout", thread)
}

// liveMCPPrompt asks for the echo tool on a fixed word, so the server's own
// marker — not the model's wording — decides whether the call happened.
const liveMCPPrompt = `Call the tool mcp__echo__shout with {"text":"banana"} and reply with exactly the tool's output, nothing else.`

// liveMCPCall runs the prompt on a session and reports the model's text and
// whether the turn ended. A second prompt is not tried: the marker, not the
// text, is what the caller asserts on.
func liveMCPCall(t *testing.T, ctx context.Context, sess *Session, marker string) (string, bool) {
	t.Helper()
	before := strings.Count(liveMarker(t, marker), "tools/call shout")
	text, ended := liveTurnBounded(t, ctx, sess, liveMCPPrompt, 2*time.Minute)
	if !ended {
		return text, false
	}
	if strings.Count(liveMarker(t, marker), "tools/call shout") == before {
		t.Logf("the model did not reach the tool in this turn: %q", text)
	}
	return text, true
}

// liveKeys is a map's keys in order (a tool inventory, for a log line).
func liveKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// liveMarker is the marker file the MCP echo server appends to ("" when absent).
func liveMarker(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}
