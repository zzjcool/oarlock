package voyage_test

// The issue #88 live acceptance: make a real Pi 1.4.0 retry a model call, and
// record that retry into a voyage fixture.
//
// Pi only emits `auto_retry_start` when a provider call fails with a retryable
// error, so the experiment needs a failing provider that is otherwise real.
// The test therefore runs a local OpenAI-compatible fault proxy: the first
// lrFaults requests answer HTTP 500 with a retriable body, every later request
// is forwarded verbatim to the endpoint the machine already uses (same model,
// same account, only the base URL and the Authorization header differ). Pi is
// pointed at the proxy through a throwaway agent directory
// (PI_CODING_AGENT_DIR), so the user's global Pi configuration is read, never
// written: the isolated directory gets its own models.json (base URL = proxy),
// its own settings.json (default model + a 1 ms retry base delay, so the test
// does not sleep through Pi's 2 s backoff) and no extensions, skills or
// sessions.
//
// The upstream credential is read from the global auth.json at run time and is
// only ever held in memory (never written, never printed, never in the
// fixture). When the machine cannot provide the pieces — no pi, no live gate,
// no discoverable default provider, or a Pi that simply does not retry — the
// test skips with UNVERIFIED and the frames it did see instead of inventing a
// fixture.
//
// The recorded log stays in a temp dir; with OAR_PI_RECORD=1 the normalised
// log replaces testdata/voyage/pi-1.4.0-retry-turn.jsonl, which is the only way
// that file is produced. The offline half (TestRetryFixtureReplays, below)
// replays the committed bytes with no process and no network.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/adapters/pi"
	"github.com/zzjcool/oarlock/internal/fold"
	"github.com/zzjcool/oarlock/internal/voyage"
)

const (
	lrFixture  = "pi-1.4.0-retry-turn"
	lrRecorder = "oarlock/voyage-live (pi 1.4.0, issue #88)"

	// lrPrompt is deliberately trivial: the turn is about the retry, not about
	// an answer, and a one-word prompt keeps the failed attempts cheap.
	lrPrompt = "Reply with the single word: ok"

	// lrFaults is how many provider calls the proxy fails before it forwards.
	// With Pi's default maxRetries 3 that produces two auto_retry_start frames
	// (attempt 1 and 2) and a successful third call.
	lrFaults = 2
	// lrMaxAttempts is the budget the isolated settings.json gives Pi.
	lrMaxAttempts = 3

	// lrProvider is the isolated provider name. It is not "any-endpoint" so a
	// frame can never be mistaken for the user's own provider configuration.
	lrProvider = "oarlock-retry-proxy"
)

// lrUpstream is everything the proxy needs from the machine's Pi configuration:
// where to forward, how to authenticate, and which model to declare to Pi.
type lrUpstream struct {
	Provider      string
	Model         string
	API           string
	BaseURL       string
	Token         string // never printed
	ContextWindow int64
	MaxTokens     int64
}

// lrUpstreamConfig reads <agentDir>/{settings,models,models-store,auth}.json and
// returns the default provider's forwarding facts. It reads only: the isolated
// agent directory is written by lrIsolatedAgentDir, never this one.
func lrUpstreamConfig(env []string) (lrUpstream, error) {
	agent, err := pi.AgentDir(env)
	if err != nil {
		return lrUpstream{}, err
	}
	readJSON := func(name string, v any) error {
		data, err := os.ReadFile(filepath.Join(agent, name))
		if err != nil {
			return err
		}
		return json.Unmarshal(data, v)
	}

	var settings struct {
		DefaultProvider string `json:"defaultProvider"`
		DefaultModel    string `json:"defaultModel"`
	}
	if err := readJSON("settings.json", &settings); err != nil {
		return lrUpstream{}, fmt.Errorf("read %s/settings.json: %w", agent, err)
	}
	if settings.DefaultProvider == "" || settings.DefaultModel == "" {
		return lrUpstream{}, fmt.Errorf("%s/settings.json names no defaultProvider/defaultModel", agent)
	}

	// models.json holds hand-written providers, models-store.json the cached
	// catalog of a dynamically refreshed one; the same shape, so one decoder.
	type modelDef struct {
		ID            string `json:"id"`
		API           string `json:"api"`
		BaseURL       string `json:"baseUrl"`
		ContextWindow int64  `json:"contextWindow"`
		MaxTokens     int64  `json:"maxTokens"`
	}
	type providerDef struct {
		BaseURL string     `json:"baseUrl"`
		API     string     `json:"api"`
		Models  []modelDef `json:"models"`
	}
	// models.json wraps the providers in {"providers": ...}; models-store.json
	// is that map itself (provider id -> cached catalog).
	providers := map[string]providerDef{}
	var handWritten struct {
		Providers map[string]providerDef `json:"providers"`
	}
	if err := readJSON("models.json", &handWritten); err == nil {
		for id, p := range handWritten.Providers {
			providers[id] = p
		}
	}
	if _, ok := providers[settings.DefaultProvider]; !ok {
		var cached map[string]providerDef
		if err := readJSON("models-store.json", &cached); err == nil {
			for id, p := range cached {
				providers[id] = p
			}
		}
	}
	prov, ok := providers[settings.DefaultProvider]
	if !ok {
		return lrUpstream{}, fmt.Errorf("%s provider %q is in neither models.json nor models-store.json", agent, settings.DefaultProvider)
	}
	var model modelDef
	found := false
	for _, m := range prov.Models {
		if m.ID == settings.DefaultModel {
			model, found = m, true
			break
		}
	}
	if !found {
		return lrUpstream{}, fmt.Errorf("%s provider %q has no model %q", agent, settings.DefaultProvider, settings.DefaultModel)
	}
	up := lrUpstream{
		Provider:      settings.DefaultProvider,
		Model:         model.ID,
		API:           firstNonEmpty(model.API, prov.API),
		BaseURL:       firstNonEmpty(model.BaseURL, prov.BaseURL),
		ContextWindow: model.ContextWindow,
		MaxTokens:     model.MaxTokens,
	}
	// auth.json: type api_key carries "key", type oauth carries "access". Only
	// the credential is copied out; it is handed to the proxy header and never
	// serialised into the recording.
	var auth map[string]struct {
		Type    string `json:"type"`
		Key     string `json:"key"`
		Access  string `json:"access"`
		BaseURL string `json:"baseUrl"`
	}
	if err := readJSON("auth.json", &auth); err != nil {
		return lrUpstream{}, fmt.Errorf("read %s/auth.json: %w", agent, err)
	}
	entry, ok := auth[settings.DefaultProvider]
	if !ok {
		return lrUpstream{}, fmt.Errorf("%s/auth.json has no credential for %q", agent, settings.DefaultProvider)
	}
	up.Token = firstNonEmpty(entry.Access, entry.Key)
	if up.Token == "" {
		return lrUpstream{}, fmt.Errorf("%s/auth.json entry for %q carries no credential", agent, settings.DefaultProvider)
	}
	if up.BaseURL == "" {
		up.BaseURL = entry.BaseURL
	}
	if up.API == "" || up.BaseURL == "" {
		return lrUpstream{}, fmt.Errorf("%s provider %q declares no api/baseUrl", agent, settings.DefaultProvider)
	}
	return up, nil
}

// lrProxy is the fault-injecting OpenAI-compatible proxy. The first fails
// requests answer 500 with a body Pi classifies as retryable ("overloaded",
// no quota/billing wording); every later request is forwarded to the real
// endpoint with the real credential.
type lrProxy struct {
	url      string // the base URL Pi is configured with (…/<upstream path>)
	front    string // URL prefix the proxy serves
	origin   string // scheme://host of the real endpoint
	token    string
	fails    int
	client   *http.Client
	server   *http.Server
	listener net.Listener

	mu    sync.Mutex
	calls int
}

// lrStartProxy starts the proxy in-process and registers its shutdown.
func lrStartProxy(t *testing.T, up lrUpstream, fails int) *lrProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	prefix := "/v1"
	base, err := url.Parse(up.BaseURL)
	if err != nil || base.Host == "" {
		_ = ln.Close()
		t.Fatalf("proxy: upstream base URL %q: %v", up.BaseURL, err)
	}
	if base.Path != "" && base.Path != "/" {
		prefix = strings.TrimSuffix(base.Path, "/")
	}
	p := &lrProxy{
		front:    prefix,
		token:    up.Token,
		fails:    fails,
		client:   &http.Client{Timeout: 2 * time.Minute},
		listener: ln,
	}
	p.origin = base.Scheme + "://" + base.Host
	p.url = "http://" + ln.Addr().String() + prefix
	p.server = &http.Server{Handler: http.HandlerFunc(p.serve), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = p.server.Serve(ln) }()
	t.Cleanup(func() {
		_ = p.server.Close()
	})
	return p
}

// callCount is how many model calls Pi made through the proxy.
func (p *lrProxy) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *lrProxy) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()

	if n <= p.fails {
		body := []byte(`{"error":{"message":"upstream overloaded","type":"server_error"}}`)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(body)
		return
	}
	target := p.origin + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		http.Error(w, "proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	// Forward Pi's headers verbatim (its User-Agent is what the endpoint
	// accepts); only the credential is replaced, and only here.
	for name, values := range r.Header {
		if strings.EqualFold(name, "authorization") || strings.EqualFold(name, "host") || strings.EqualFold(name, "content-length") {
			continue
		}
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := p.client.Do(req)
	if err != nil {
		http.Error(w, "proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for name, values := range resp.Header {
		if strings.EqualFold(name, "content-length") || strings.EqualFold(name, "transfer-encoding") {
			continue
		}
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// lrIsolatedAgentDir writes the throwaway Pi configuration home: a provider
// pointing at the proxy, the default model, and a retry policy whose backoff is
// short enough for a test. Nothing under the real agent directory is touched.
func lrIsolatedAgentDir(t *testing.T, up lrUpstream, proxyURL string) string {
	t.Helper()
	dir := t.TempDir()
	api := up.API
	if api == "" {
		api = "openai-responses"
	}
	models := map[string]any{
		"$schema": "https://pi.dev/schemas/models.schema.json",
		"providers": map[string]any{
			lrProvider: map[string]any{
				// A literal key: the proxy overwrites the header, but Pi refuses
				// a provider that looks unauthenticated.
				"baseUrl": proxyURL,
				"api":     api,
				"apiKey":  "oarlock-retry-proxy-key",
				"models": []any{map[string]any{
					"id":            up.Model,
					"name":          "Oarlock Retry Proxy",
					"reasoning":     false,
					"input":         []any{"text"},
					"contextWindow": up.ContextWindow,
					"maxTokens":     up.MaxTokens,
				}},
			},
		},
	}
	settings := map[string]any{
		"defaultProvider":      lrProvider,
		"defaultModel":         up.Model,
		"defaultThinkingLevel": "off",
		"defaultProjectTrust":  "never",
		// 1 ms, 2 ms: what Pi's retry.backoff would spend in seconds.
		"retry": map[string]any{
			"enabled":         true,
			"maxRetries":      lrMaxAttempts,
			"baseDelayMs":     1,
			"maxAgentDelayMs": 5,
		},
	}
	for name, doc := range map[string]any{"models.json": models, "settings.json": settings} {
		data, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// lrRetries returns the retry events of a stream, in Seq order.
func lrRetries(recs []oarlock.Record) []ltEvent {
	var out []ltEvent
	for _, ev := range ltEvents(recs) {
		if ev.Kind == oarlock.EventRetry {
			out = append(out, ev)
		}
	}
	return out
}

// TestLiveRetryRecordsAndReplays drives a real Pi through the fault proxy,
// proves the retry events Pi emitted, normalises the stream and (with
// OAR_PI_RECORD=1) writes the committed fixture. Without the gate it still runs
// the recording and the replay comparison in a temp dir.
func TestLiveRetryRecordsAndReplays(t *testing.T) {
	if os.Getenv("OAR_PI_LIVE") != "1" {
		t.Skip("set OAR_PI_LIVE=1 to make a real Pi retry through a fault proxy")
	}
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skipf("pi is not on PATH: %v", err)
	}
	up, err := lrUpstreamConfig(os.Environ())
	if err != nil {
		t.Skipf("UNVERIFIED: no forwardable default provider on this machine: %v", err)
	}
	proxy := lrStartProxy(t, up, lrFaults)
	agentDir := lrIsolatedAgentDir(t, up, proxy.url)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cwd := t.TempDir()
	// PI_OFFLINE/PI_SKIP_VERSION_CHECK keep the run to the one model call under
	// test; PI_CODING_AGENT_DIR is the whole isolation.
	env := map[string]*string{
		"PI_CODING_AGENT_DIR":   &agentDir,
		"PI_OFFLINE":            ptr("1"),
		"PI_SKIP_VERSION_CHECK": ptr("1"),
	}
	sess, err := pi.Open(ctx, oarlock.Installation{}, oarlock.Options{Cwd: cwd, Env: env})
	if err != nil {
		t.Skipf("UNVERIFIED: pi could not be opened against the isolated agent dir: %v", err)
	}
	if out, err := sess.Prompt(ctx, lrPrompt, oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
		_ = sess.Dispose(ctx)
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	ltSettle(t, sess, time.Now().Add(3*time.Minute))
	if err := sess.Dispose(ctx); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	records := sess.Records(oarlock.Cursor{})
	live := ltFold(records)

	retries := lrRetries(records)
	if len(retries) == 0 {
		// Never invent the fact the issue asks for: report the frames instead.
		t.Skipf("UNVERIFIED: pi made %d call(s) through the fault proxy but emitted no retry event; frames since the prompt: %s",
			proxy.callCount(), ltFrameTypes(records, 0))
	}
	if got := proxy.callCount(); got < lrFaults+1 {
		t.Fatalf("proxy saw %d model calls, want at least %d (two failed + one forwarded)", got, lrFaults+1)
	}
	for i, ev := range retries {
		if ev.Attempt != i+1 {
			t.Fatalf("retry %d has attempt %d, want %d (attempts increment by one)", i+1, ev.Attempt, i+1)
		}
		if ev.MaxAttempts != lrMaxAttempts {
			t.Fatalf("retry %d has maxAttempts %d, want %d", i+1, ev.MaxAttempts, lrMaxAttempts)
		}
		if ev.DelayMS <= 0 {
			t.Fatalf("retry %d has delayMs %d, want a positive backoff", i+1, ev.DelayMS)
		}
		if ev.Reason == "" {
			t.Fatalf("retry %d has no reason; Pi's errorMessage is what the projection reads", i+1)
		}
	}
	// The successful turn is the one the fold reports, not a failed retry: Pi
	// ends each failed attempt with its own turn_end, and the last wins.
	if status := live.Status.Value; status.Kind != fold.StatusExited {
		t.Fatalf("live status after dispose = %s, want exited", status.Kind)
	}

	norm := ltNormaliseLive(t, records, "pi-live-"+lrFixture, cwd)
	// The listen port is the one nondeterministic byte the proxy adds; the
	// fixture records a fixed placeholder instead (like $HOME and ".").
	hostPort := strings.TrimPrefix(strings.TrimSuffix(proxy.url, proxy.front), "http://")
	norm = ltNormalise(t, norm, ltNormalisation{HostPort: hostPort, HostPortTo: "127.0.0.1:0"})
	path := ltWriteAs(t, lrFixture, norm, "disposed", lrRecorder)

	replayed := ltFold(ltReplayNoProcess(t, path))
	if !sameFold(live, replayed) {
		t.Fatalf("replay differs from live:\n live=%s\n replay=%s", ltJSON(t, live), ltJSON(t, replayed))
	}
	assertRetryStream(t, ltReplayNoProcess(t, path), lrMaxAttempts)
}

// ptr is the address of a string literal for oarlock.Options.Env.
func ptr(s string) *string { return &s }

func sameFold(a, b ltAnswers) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// TestRetryFixtureReplays is the offline half: no pi, no process, no network.
// It replays the committed fixture and asserts the retry facts, their order and
// that Status/Usage survive them.
func TestRetryFixtureReplays(t *testing.T) {
	path := filepath.Join(ltFixtureDir, lrFixture+".jsonl")
	log, err := voyage.Read(path)
	if err != nil {
		t.Fatalf("read committed retry fixture: %v", err)
	}
	if !log.Complete() {
		t.Fatal("the committed retry fixture has no end line")
	}
	if log.Header.SessionID != "pi-live-"+lrFixture || log.Header.Runtime != "pi" {
		t.Fatalf("fixture header = %+v", log.Header)
	}
	if log.End == nil || log.End.Reason != "disposed" {
		t.Fatalf("fixture end = %+v, want reason disposed", log.End)
	}
	recs := ltReplayNoProcess(t, path)
	assertRetryStream(t, recs, lrMaxAttempts)
}

// assertRetryStream pins the recorded retry semantics: two attempts that both
// increment, both carry Pi's budget and backoff and a retryable provider error,
// each one arrives while a turn is open and hands the run back to the model;
// the silent auto_retry_end frame stays silent; the fold's Status is idle at the
// end and Usage carries the successful answer's tokens.
func assertRetryStream(t *testing.T, recs []oarlock.Record, maxAttempts int) {
	t.Helper()
	retries := lrRetries(recs)
	if len(retries) != lrFaults {
		t.Fatalf("%d retry events, want %d (the proxy fails %d calls, then forwards)", len(retries), lrFaults, lrFaults)
	}
	for i, ev := range retries {
		if ev.Attempt != i+1 || ev.MaxAttempts != maxAttempts || ev.DelayMS <= 0 || ev.Reason == "" {
			t.Fatalf("retry %d = %+v, want attempt %d, maxAttempts %d, positive delayMs, non-empty reason", i+1, ev, i+1, maxAttempts)
		}
		if !strings.Contains(ev.Reason, "500") {
			t.Fatalf("retry %d reason = %q, want the provider's 500 in it", i+1, ev.Reason)
		}
		if i > 0 && retries[i-1].Seq >= ev.Seq {
			t.Fatalf("retry seq order = %d then %d, want increasing", retries[i-1].Seq, ev.Seq)
		}
		// A retry arrives while the run is open and puts it back into
		// waiting_model; it must not settle or restart the status.
		status := fold.StatusOf(recs[:indexOfSeq(recs, ev.Seq)+1]).Value
		if status.Kind != fold.StatusRunning || status.Phase.Kind != fold.PhaseWaitingModel {
			t.Fatalf("status at retry %d = %s/%s, want running/waiting_model", i+1, status.Kind, status.Phase.Kind)
		}
	}
	// Pi's own end-of-retry frame is recorded verbatim and deliberately not
	// projected: a native record with no events.
	sawEnd := false
	for _, r := range recs {
		if r.Kind != oarlock.KindFrame {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(r.Native, &head); err != nil || head.Type != "auto_retry_end" {
			continue
		}
		if len(r.Events) != 0 {
			t.Fatalf("auto_retry_end at seq %d produced events %+v, want none (deliberately silent)", r.Seq, r.Events)
		}
		sawEnd = true
	}
	if !sawEnd {
		t.Fatal("no auto_retry_end frame in the retry fixture: Pi's retry stream is incomplete")
	}
	// The failed attempts are native `turn_end` frames with stopReason
	// "error"; the projection deliberately reports no outcome for them (the
	// adapter's rule: the last assistant turn of a run wins), so the only
	// turn_ended the stream carries is the successful one.
	var outcomes []oarlock.OutcomeKind
	for _, ev := range ltEvents(recs) {
		if ev.Kind == oarlock.EventTurnEnded && ev.Outcome != nil {
			outcomes = append(outcomes, ev.Outcome.Kind)
		}
	}
	if len(outcomes) != 1 || outcomes[0] != oarlock.OutcomeCompleted {
		t.Fatalf("turn outcomes = %v, want exactly one completed (failed attempts are not the turn's outcome)", outcomes)
	}
	// Order on the wire: each auto_retry_start follows the native turn_end of
	// the attempt that failed, and the next attempt starts after it.
	var failedEnds, attemptStarts []uint64
	for _, r := range recs {
		if r.Kind != oarlock.KindFrame {
			continue
		}
		var frame struct {
			Type    string `json:"type"`
			Message *struct {
				Role       string `json:"role"`
				StopReason string `json:"stopReason"`
			} `json:"message"`
		}
		if err := json.Unmarshal(r.Native, &frame); err != nil {
			continue
		}
		switch {
		case frame.Type == "turn_end" && frame.Message != nil && frame.Message.StopReason == "error":
			failedEnds = append(failedEnds, r.Seq)
		case frame.Type == "agent_start":
			attemptStarts = append(attemptStarts, r.Seq)
		}
	}
	if len(failedEnds) != lrFaults {
		t.Fatalf("%d failed turn_end frames in %v, want %d", len(failedEnds), failedEnds, lrFaults)
	}
	for i, ev := range retries {
		if failedEnds[i] > ev.Seq {
			t.Fatalf("retry %d at seq %d precedes its failed turn_end at seq %d", i+1, ev.Seq, failedEnds[i])
		}
		next := uint64(0)
		for _, seq := range attemptStarts {
			if seq > ev.Seq {
				next = seq
				break
			}
		}
		if next == 0 {
			t.Fatalf("retry %d at seq %d is not followed by an agent_start: no attempt after the retry", i+1, ev.Seq)
		}
	}
	// Status settles when Pi settles the run: idle, no phase. (The last record
	// of a live fixture is the dispose, which moves status to exited.)
	var settled uint64
	for _, r := range recs {
		if r.Kind == oarlock.KindFrame && strings.Contains(string(r.Native), `"type":"agent_settled"`) {
			settled = r.Seq
		}
	}
	if settled == 0 {
		t.Fatal("no agent_settled frame in the retry fixture")
	}
	status := fold.StatusOf(recs[:indexOfSeq(recs, settled)+1]).Value
	if status.Kind != fold.StatusIdle || status.Phase.Kind != "" {
		t.Fatalf("status after the retried turn = %s/%s, want idle", status.Kind, status.Phase.Kind)
	}
	// Usage: the failed attempts report 0 tokens; the answer's usage is what
	// the fold must keep, exactly as the successful assistant message_end says.
	usage := fold.UsageOf(recs).Value
	if usage.Total == nil || usage.Total.Output == 0 || usage.Total.Input == 0 {
		t.Fatalf("folded usage = %+v, want the successful answer's tokens", usage)
	}
	var wantIn, wantOut int64
	for _, r := range recs {
		if r.Kind != oarlock.KindFrame {
			continue
		}
		var frame struct {
			Type    string `json:"type"`
			Message *struct {
				Role  string `json:"role"`
				Usage *struct {
					Input      int64 `json:"input"`
					Output     int64 `json:"output"`
					CacheRead  int64 `json:"cacheRead"`
					CacheWrite int64 `json:"cacheWrite"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(r.Native, &frame); err != nil || frame.Type != "message_end" || frame.Message == nil || frame.Message.Usage == nil {
			continue
		}
		// The projection counts cache reads/writes back into input; a failed
		// attempt reports zeros and adds nothing.
		if u := frame.Message.Usage; u.Output > 0 {
			wantIn += u.Input + u.CacheRead + u.CacheWrite
			wantOut += u.Output
		}
	}
	if wantOut > 0 && (usage.Total.Input != wantIn || usage.Total.Output != wantOut) {
		t.Fatalf("folded usage = %d in/%d out, want %d/%d from the successful answer", usage.Total.Input, usage.Total.Output, wantIn, wantOut)
	}
	if ctx := fold.ContextUsageOf(recs).Value; ctx == nil || ctx.Usage.Tokens == nil || *ctx.Usage.Tokens == 0 || ctx.Usage.ContextWindow == nil || *ctx.Usage.ContextWindow == 0 {
		t.Fatalf("folded context = %+v, want the successful answer's fullness", ctx)
	}
}

func indexOfSeq(recs []oarlock.Record, seq uint64) int {
	for i, r := range recs {
		if r.Seq == seq {
			return i
		}
	}
	return -1
}
