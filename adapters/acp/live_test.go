package acp

// live_test.go — the one claim only a real `opencode` can prove: that the ACP
// transport, the record projection and the turn lifecycle work against the
// installed agent on a real OpenAI-compatible endpoint, with no vendor account.
//
// Opt-in: OAR_ACP_LIVE=1 plus a third-party OpenAI-compatible endpoint
// (OAR_ACP_LIVE_BASE_URL, OAR_ACP_LIVE_API_KEY — the key travels only through
// the child's environment, never a file, a fixture or a record). Everything
// else in this package runs against real processes with a fake agent
// (fake_test.go) and against a recorded v2 stream (testdata/). Without the
// gate, or without the binary/endpoint, each test skips and says why.
//
// The provider lives in a temporary XDG_CONFIG_HOME so the user's own
// opencode configuration is neither read nor written. See
// docs/runtimes/opencode.md for the exact config and the run record.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

// liveKeyEnv is the variable name opencode reads the provider key from. The
// value is passed in oarlock.Options.Env (so it is also a redaction
// candidate); the config file only carries the {env:...} reference.
const liveKeyEnv = "OAR_LIVE_PROVIDER_KEY"

// liveAgent is one real opencode installation plus the temporary provider
// that points it at a third-party OpenAI-compatible endpoint.
type liveAgent struct {
	bin     string
	version string
	profile Profile
	// model is the native `provider/model` the session is asked for.
	model string
	env   map[string]*string
}

// liveAgentOrSkip resolves gate, binary and endpoint; every reason it cannot
// run is a skip that names the missing piece.
func liveAgentOrSkip(t *testing.T) liveAgent {
	t.Helper()
	if os.Getenv("OAR_ACP_LIVE") != "1" {
		t.Skip("NOT VERIFIED: set OAR_ACP_LIVE=1 (with OAR_ACP_LIVE_BASE_URL and OAR_ACP_LIVE_API_KEY) to run a real ACP session")
	}
	return liveAgentFor(t)
}

// liveAgentFor is liveAgentOrSkip without the gate, for the family smoke test
// that keeps its own opt-in (OARLOCK_LIVE_ACP=opencode).
func liveAgentFor(t *testing.T) liveAgent {
	t.Helper()
	base := os.Getenv("OAR_ACP_LIVE_BASE_URL")
	key := os.Getenv("OAR_ACP_LIVE_API_KEY")
	if base == "" || key == "" {
		t.Skip("NOT VERIFIED: set OAR_ACP_LIVE_BASE_URL (e.g. https://host/v1) and OAR_ACP_LIVE_API_KEY (read from an auth store into the environment; never written to the repo)")
	}
	bin := os.Getenv("OAR_ACP_LIVE_OPENCODE")
	if bin == "" {
		found, err := exec.LookPath("opencode")
		if err != nil {
			t.Skip("NOT VERIFIED: no opencode binary on PATH (install: npm i -g opencode-ai) and OAR_ACP_LIVE_OPENCODE is unset")
		}
		bin = found
	}
	version := opencodeVersion(t, bin)
	model := os.Getenv("OAR_ACP_LIVE_MODEL")
	if model == "" {
		model = "workbuddy/deepseek-v4.1-flash"
	}
	xdg := liveConfig(t, base, model)
	env := map[string]*string{
		// The provider key reaches the agent, and only the agent.
		liveKeyEnv: &key,
		// A temporary config dir: the user's own ~/.config/opencode is not read
		// (so their providers, permissions and default model cannot leak in)
		// and is not written.
		"XDG_CONFIG_HOME": &xdg,
		// An inherited config override would outrank the temporary file.
		"OPENCODE_CONFIG":         nil,
		"OPENCODE_CONFIG_CONTENT": nil,
		"OPENCODE_CONFIG_DIR":     nil,
	}
	t.Logf("live opencode %s at %s; endpoint %s; model %s", version, bin, base, model)
	return liveAgent{bin: bin, version: version, profile: OpenCode.Select(oarlock.Installation{Version: version}), model: "live/" + model, env: env}
}

// opencodeVersion reads `opencode --version`; an unreadable version is ""
// (Profile.Select then takes the conservative v1 line).
func opencodeVersion(t *testing.T, bin string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		t.Logf("opencode --version failed (%v); treating the line as unknown (v1)", err)
		return ""
	}
	// v1 prints `1.19.5`; v2 prints `opencode v2.0.26`.
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "opencode v"))
}

// liveConfig writes the temporary provider config. The model id is the key
// under `models` and the key is an {env:...} reference, so no credential is
// ever written to disk. `permission.bash: ask` is what makes the runtime issue
// session/request_permission instead of running the tool unasked.
func liveConfig(t *testing.T, base, model string) string {
	t.Helper()
	xdg := t.TempDir()
	dir := filepath.Join(xdg, "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"model":   "live/" + model,
		"provider": map[string]any{
			"live": map[string]any{
				"name": "oarlock live ACP",
				"npm":  "@ai-sdk/openai-compatible",
				"models": map[string]any{
					model: map[string]any{
						"name":       "live " + model,
						"tool_call":  true,
						"attachment": false,
						"limit":      map[string]any{"context": 128000, "output": 16000},
					},
				},
				"options": map[string]any{
					"baseURL": base,
					"apiKey":  "{env:" + liveKeyEnv + "}",
				},
			},
		},
		"permission": map[string]any{"bash": "ask"},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return xdg
}

// openLive starts one real session and guarantees the process is gone.
func openLive(t *testing.T, a liveAgent, opt oarlock.Options) *Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	inst := oarlock.Installation{Available: true, Path: a.bin, Version: a.version, Source: "path"}
	opt.Env = a.env
	if opt.Cwd == "" {
		opt.Cwd = t.TempDir()
	}
	if opt.Model == "" {
		opt.Model = a.model
	}
	sess, err := a.profile.Start(ctx, inst, opt)
	if err != nil {
		t.Fatalf("open the live opencode: %v", err)
	}
	// A profile with SteerMeta opens as steerSession, which embeds *Session.
	var s *Session
	switch v := sess.(type) {
	case *Session:
		s = v
	case steerSession:
		s = v.Session
	default:
		t.Fatalf("unexpected session type %T", sess)
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		_ = s.Dispose(dctx)
	})
	return s
}

// liveCtx bounds one live turn.
func liveCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func TestLiveOpenCodeTurn(t *testing.T) {
	a := liveAgentOrSkip(t)
	ctx := liveCtx(t)
	s := openLive(t, a, oarlock.Options{})

	if got := s.Capabilities().Attribution; got != a.profile.Attribution {
		t.Fatalf("attribution = %q, want the profile's %q", got, a.profile.Attribution)
	}

	// initialize: the agent's own capability answer is on the stream, and the
	// capability the session exposes is read from it.
	init := liveFrame(t, records(s), "agentCapabilities")
	var ir struct {
		Result struct {
			AgentCapabilities struct {
				PromptCapabilities struct {
					Image bool `json:"image"`
				} `json:"promptCapabilities"`
			} `json:"agentCapabilities"`
			AgentInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"agentInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(init.Native, &ir); err != nil {
		t.Fatalf("initialize frame: %v", err)
	}
	if ir.Result.AgentInfo.Name == "" || ir.Result.AgentInfo.Version == "" {
		t.Fatalf("initialize named no agent: %s", init.Native)
	}
	if s.Capabilities().Images != ir.Result.AgentCapabilities.PromptCapabilities.Image {
		t.Fatalf("images = %v, agent advertised %v", s.Capabilities().Images, ir.Result.AgentCapabilities.PromptCapabilities.Image)
	}
	t.Logf("agent %s %s; images=%v; attribution=%s", ir.Result.AgentInfo.Name, ir.Result.AgentInfo.Version, s.Capabilities().Images, s.Capabilities().Attribution)

	// session/new: the runtime named the session, and the model the host asked
	// for is the one it reports in effect.
	if id := s.ID(); id == "" {
		t.Fatal("session/new returned no id")
	}
	if f := liveFrame(t, records(s), s.ID()); f.Kind != oarlock.KindFrame {
		t.Fatalf("session id is not in a frame: %+v", f)
	}
	if got := liveModel(records(s)); got != a.model {
		t.Fatalf("model in effect = %q, want %q", got, a.model)
	}

	// A prompt the runtime answers itself: no turn exists before it.
	if hasEvent(records(s), oarlock.EventTurnEnded) {
		t.Fatal("a turn ended before any prompt")
	}
	out, err := s.Prompt(ctx, "Reply with exactly: pong", oarlock.PromptOptions{})
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	waitLiveStatus(t, s, fold.StatusIdle, 2*time.Minute)

	recs := records(s)
	assertDense(t, recs)
	if got := text(recs); !strings.Contains(strings.ToLower(got), "pong") {
		t.Fatalf("the turn produced no pong: %q", got)
	}
	if got := liveTurnEnds(recs); len(got) != 1 {
		t.Fatalf("turn_ended = %d, want exactly one:\n%s", len(got), dump(recs))
	} else if got[0].Outcome == nil || got[0].Outcome.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("turn outcome = %+v, want completed", got[0].Outcome)
	}
	// Not synthesized: the answer that ends the turn is the runtime's own
	// session/prompt response, and it says end_turn. Nothing else ends a turn.
	end := liveTurnEndRecord(t, recs)
	if end.Kind != oarlock.KindFrame || !strings.Contains(string(end.Native), `"stopReason"`) {
		t.Fatalf("the turn end is not the runtime's prompt answer: %+v\n%s", end, end.Native)
	}
	if st := fold.StatusOf(recs).Value.Kind; st != fold.StatusIdle {
		t.Fatalf("status = %v, want idle", st)
	}
	if strings.Contains(dump(recs), *a.env[liveKeyEnv]) {
		t.Fatal("the provider key leaked into the record stream")
	}
	t.Logf("%d records, %d text deltas, outcome completed, status idle", len(recs), len(events(recs, oarlock.EventTextDelta)))

	// Dispose is idempotent: one dispose request, one exited response, and
	// the stream stays dense.
	for i := 0; i < 2; i++ {
		if err := s.Dispose(ctx); err != nil {
			t.Fatalf("dispose #%d: %v", i+1, err)
		}
	}
	recs = records(s)
	assertDense(t, recs)
	if d, ex := liveCount(recs, oarlock.ActionDispose), liveExits(recs); d != 1 || ex != 1 {
		t.Fatalf("disposes=%d exits=%d\n%s", d, ex, dump(recs))
	}
	if st := fold.StatusOf(recs).Value.Kind; st != fold.StatusExited {
		t.Fatalf("status after dispose = %v, want exited", st)
	}
}

func TestLiveOpenCodeCancel(t *testing.T) {
	a := liveAgentOrSkip(t)
	ctx := liveCtx(t)
	s := openLive(t, a, oarlock.Options{})

	// Cancel while the runtime is streaming. The abort is sent a moment after
	// the prompt, because opencode answers `session/prompt` with
	// `Internal service failure` when session/cancel lands before the turn has
	// produced anything at all (observed live; a cancelled turn is otherwise a
	// normal `stopReason: cancelled` answer).
	prompts := []string{
		"Count slowly from 1 to 100, one number per line, with a short comment on every line. Do not stop early.",
		"Count slowly from 1 to 300, one number per line, with two comments on every line. Do not stop early.",
	}
	accepted := false
	var out oarlock.ControlOutcome
	var err error
	for attempt := 0; attempt < len(prompts) && !accepted; attempt++ {
		out, err = s.Prompt(ctx, prompts[attempt], oarlock.PromptOptions{})
		if err != nil || out.Kind != oarlock.ControlAccepted {
			t.Fatalf("prompt %d = %+v, %v", attempt+1, out, err)
		}
		waitLiveStatus(t, s, fold.StatusRunning, 2*time.Minute)
		time.Sleep(2 * time.Second)
		if fold.StatusOf(records(s)).Value.Kind != fold.StatusRunning {
			continue // the model answered first: ask again, longer
		}
		out, err = s.Abort(ctx)
		if err != nil {
			t.Fatalf("abort: %v", err)
		}
		if out.Kind == oarlock.ControlAccepted {
			accepted = true
			break
		}
		if out.Code != oarlock.NoActiveTurn {
			t.Fatalf("abort = %+v", out)
		}
	}
	if !accepted {
		t.Fatal("the runtime answered before session/cancel could be sent twice")
	}
	waitLiveStatus(t, s, fold.StatusIdle, 60*time.Second)

	recs := records(s)
	assertDense(t, recs)
	ends := liveTurnEnds(recs)
	if len(ends) != 1 {
		t.Fatalf("turn_ended = %d, want one:\n%s", len(ends), dump(recs))
	}
	if ends[0].Outcome == nil || ends[0].Outcome.Kind != oarlock.OutcomeAborted {
		t.Fatalf("turn outcome after cancel = %+v, want aborted\n%s", ends[0].Outcome, dump(recs))
	}
	if end := liveTurnEndRecord(t, recs); !strings.Contains(string(end.Native), `"cancelled"`) {
		t.Fatalf("the cancelled turn's end is not the runtime's own answer: %s", end.Native)
	}
	// The turn really ended: a second abort has nothing to cancel.
	if out, _ := s.Abort(ctx); out.Kind == oarlock.ControlAccepted {
		t.Fatalf("abort after the cancelled turn was accepted: %+v", out)
	}
	t.Logf("cancel: %d records, outcome aborted, status idle", len(recs))
}

func TestLiveOpenCodePermissionPath(t *testing.T) {
	a := liveAgentOrSkip(t)
	ctx := liveCtx(t)
	s := openLive(t, a, oarlock.Options{})

	out, err := s.Prompt(ctx, "Use the bash tool to run exactly `echo oarlock-live`, then reply with its output.", oarlock.PromptOptions{})
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	waitLiveStatus(t, s, fold.StatusIdle, 2*time.Minute)
	recs := records(s)
	var req *oarlock.Record
	for i := range recs {
		if r := &recs[i]; r.Kind == oarlock.KindRequest && r.Direction == oarlock.ToApp && r.Request != nil && r.Request.NativeType == "session/request_permission" {
			req = r
		}
	}
	if req == nil {
		t.Skip("NOT VERIFIED: this run's model answered without calling a tool, so opencode issued no session/request_permission; the permission path is exercised only when it does")
	}
	var answered *oarlock.Record
	for i := range recs {
		if r := &recs[i]; r.Kind == oarlock.KindResponse && r.RequestID == req.ID && r.Response != nil && r.Response.Kind == oarlock.ResponseAnswered {
			answered = r
		}
	}
	if answered == nil {
		t.Fatalf("the permission request was never answered:\n%s", dump(recs))
	}
	// The default rule prefers allow_always over allow_once.
	if !strings.Contains(string(answered.Response.Native), `"optionId":"always"`) {
		t.Fatalf("permission answer did not select allow_always: %s", answered.Response.Native)
	}
	if ends := liveTurnEnds(recs); len(ends) != 1 || ends[0].Outcome == nil || ends[0].Outcome.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("turn ends = %+v, want one completed", ends)
	}
	t.Logf("permission: answered %s for %q", answered.Response.Native, req.Request.NativeType)
}

// liveFrame returns the first agent frame whose bytes contain marker.
func liveFrame(t *testing.T, recs []oarlock.Record, marker string) oarlock.Record {
	t.Helper()
	for _, r := range recs {
		if r.Kind == oarlock.KindFrame && strings.Contains(string(r.Native), marker) {
			return r
		}
	}
	t.Fatalf("no frame contains %q\n%s", marker, dump(recs))
	return oarlock.Record{}
}

// liveTurnEnds are the turn_ended events, in stream order.
func liveTurnEnds(recs []oarlock.Record) []oarlock.Event { return events(recs, oarlock.EventTurnEnded) }

// liveTurnEndRecord is the record the (single) turn end hangs off.
func liveTurnEndRecord(t *testing.T, recs []oarlock.Record) oarlock.Record {
	t.Helper()
	for _, r := range recs {
		for _, e := range r.Events {
			if e.Kind == oarlock.EventTurnEnded {
				return r
			}
		}
	}
	t.Fatalf("no turn_ended in the stream\n%s", dump(recs))
	return oarlock.Record{}
}

// liveModel is the last model the stream reports in effect.
func liveModel(recs []oarlock.Record) string {
	out := ""
	for _, e := range events(recs, oarlock.EventModel) {
		if e.Model != "" {
			out = e.Model
		}
	}
	return out
}

func liveCount(recs []oarlock.Record, action oarlock.Action) int {
	n := 0
	for _, r := range recs {
		if r.Kind == oarlock.KindRequest && r.Request != nil && r.Request.Action == action {
			n++
		}
	}
	return n
}

func liveExits(recs []oarlock.Record) int {
	n := 0
	for _, r := range recs {
		if r.Kind == oarlock.KindResponse && r.Response != nil && r.Response.Kind == oarlock.ResponseExited {
			n++
		}
	}
	return n
}

// waitLiveStatus waits for the folded status, bounded by limit.
func waitLiveStatus(t *testing.T, s oarlock.Session, want fold.StatusKind, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if fold.StatusOf(records(s)).Value.Kind == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("status never reached %s\n%s", want, dump(records(s)))
}

// liveSmokeOpenCode is the family smoke test's one implemented runtime: the
// shared gate (OARLOCK_LIVE_ACP=opencode) gets a real initialize, a real
// session and one real turn. The depth is in TestLiveOpenCode*.
func liveSmokeOpenCode(t *testing.T) {
	t.Helper()
	if os.Getenv("OARLOCK_LIVE_ACP") != "opencode" {
		t.Skip("live opencode smoke is opt-in (OARLOCK_LIVE_ACP=opencode) and needs opencode plus an OpenAI-compatible endpoint: UNVERIFIED")
	}
	a := liveAgentFor(t)
	ctx := liveCtx(t)
	s := openLive(t, a, oarlock.Options{})
	if out, err := s.Prompt(ctx, "Reply with exactly: pong", oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	waitLiveStatus(t, s, fold.StatusIdle, 3*time.Minute)
	recs := records(s)
	assertDense(t, recs)
	if ends := liveTurnEnds(recs); len(ends) != 1 || ends[0].Outcome == nil || ends[0].Outcome.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("turn ends = %+v, want one completed", ends)
	}
	if text(recs) == "" {
		t.Fatal("the live turn produced no text")
	}
}
