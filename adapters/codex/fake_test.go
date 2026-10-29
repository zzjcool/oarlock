package codex

// The fake app-server is this very test binary re-executed in a helper mode:
// no codex, no sh, no Node, no network, and the same binary on every platform.
// It speaks the JSON-RPC dialect of docs/runtimes/codex.md closely enough for
// the adapter: the scenario (env) and the prompt text pick what a turn does.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
)

const (
	modeEnv     = "OARLOCK_CODEX_TEST_MODE"
	scenarioEnv = "OARLOCK_CODEX_TEST_SCENARIO"
	versionEnv  = "OARLOCK_CODEX_TEST_VERSION"
	uaEnv       = "OARLOCK_CODEX_TEST_UA"
	logEnv      = "OARLOCK_CODEX_TEST_LOG"
	fixtureEnv  = "OARLOCK_CODEX_TEST_FIXTURE"

	rootThread = "thread-root"
)

// TestHelperProcess is not a test: it is the child body of the re-executed test
// binary, and always leaves through os.Exit so the framework never prints its
// banner into the pipe the parent is framing.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(modeEnv) == "" {
		return
	}
	os.Exit(helperFake())
}

func logLine(s string) {
	p := os.Getenv(logEnv)
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, s)
}

var outMu sync.Mutex

func emit(v any) {
	b, _ := json.Marshal(v)
	outMu.Lock()
	defer outMu.Unlock()
	os.Stdout.Write(append(b, '\n'))
}

func notify(method string, params map[string]any) {
	emit(map[string]any{"method": method, "params": params})
}

type fakeState struct {
	scenario string
	turns    int
	active   string // the turn codex runs
	pending  string // the prompt directive waiting for a client reply (ask)
}

func helperFake() int {
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	for _, a := range args {
		if a == "--version" {
			logLine("PROBE")
			switch v := os.Getenv(versionEnv); v {
			case "hang":
				time.Sleep(time.Hour)
			case "fail":
				fmt.Fprintln(os.Stderr, "codex: broken install")
				return 3
			case "empty":
			case "":
				fmt.Println("codex-cli 0.160.1")
			default:
				fmt.Println(v)
			}
			return 0
		}
	}
	logLine("START " + strings.Join(args, " "))
	st := &fakeState{scenario: os.Getenv(scenarioEnv)}
	r := bufio.NewReaderSize(os.Stdin, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			return 0
		}
		line = []byte(strings.TrimSpace(string(line)))
		logLine(string(line))
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
			Result any             `json:"result"`
		}
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		if m.Method == "" && len(m.ID) > 0 {
			st.clientReply(string(m.ID))
			continue
		}
		if len(m.ID) == 0 {
			continue
		}
		st.handle(m.ID, m.Method, m.Params)
	}
}

func reply(id json.RawMessage, result any) { emit(map[string]any{"id": id, "result": result}) }
func replyErr(id json.RawMessage, code int, msg string) {
	emit(map[string]any{"id": id, "error": map[string]any{"code": code, "message": msg}})
}

func firstText(params map[string]any) string {
	in, _ := params["input"].([]any)
	if len(in) == 0 {
		return ""
	}
	first, _ := in[0].(map[string]any)
	s, _ := first["text"].(string)
	return s
}

func (st *fakeState) handle(id json.RawMessage, method string, params map[string]any) {
	switch method {
	case "initialize":
		ua := os.Getenv(uaEnv)
		if ua == "" {
			ua = "codex-cli/0.160.1"
		}
		reply(id, map[string]any{"userAgent": ua})
	case "thread/start", "thread/resume":
		thread := map[string]any{"id": rootThread}
		switch st.scenario {
		case "no-thread":
			thread = map[string]any{}
		case "norollout":
			replyErr(id, -32600, "no rollout found for thread id "+fmt.Sprint(params["threadId"]))
			return
		case "archived":
			replyErr(id, -32600, "session x is archived. Run `codex unarchive x` to unarchive it first.")
			return
		}
		if method == "thread/resume" {
			thread["id"] = params["threadId"]
		}
		res := map[string]any{"thread": thread, "model": "gpt-5.5"}
		if st.scenario == "model-kept" {
			res["model"] = "gpt-other"
		}
		if r, ok := params["runtimeWorkspaceRoots"]; ok && st.scenario != "roots-unconfirmed" {
			res["runtimeWorkspaceRoots"] = r
		}
		reply(id, res)
		if st.scenario == "exit-after-open" {
			os.Exit(7)
		}
	case "thread/name/set":
		reply(id, map[string]any{})
	case "turn/start":
		st.turnStart(id, params)
	case "turn/steer":
		active := st.active
		if st.scenario == "steer-mismatch" {
			active = "turn-other"
		}
		if params["expectedTurnId"] != active || active == "" {
			replyErr(id, -32600, fmt.Sprintf("expected turn id %v does not match the active turn %q", params["expectedTurnId"], active))
			return
		}
		reply(id, map[string]any{"turnId": active})
		if cid, _ := params["clientUserMessageId"].(string); strings.HasPrefix(cid, "echo-") {
			notify("item/started", map[string]any{"threadId": rootThread, "turnId": active, "item": map[string]any{
				"type": "userMessage", "id": "um-steer", "clientId": cid,
				"content": []any{map[string]any{"type": "text", "text": firstText(params)}}}})
		}
	case "turn/interrupt":
		switch st.scenario {
		case "interrupt-refuse":
			replyErr(id, -32600, "turn already finished")
			return
		case "interrupt-ack-only":
			reply(id, map[string]any{})
			return
		case "interrupt-silent":
			return
		}
		reply(id, map[string]any{})
		turn := st.active
		st.active = ""
		notify("turn/completed", map[string]any{"threadId": rootThread, "turn": map[string]any{"id": turn, "status": "interrupted"}})
	case "thread/queue/add":
		reply(id, map[string]any{"submissionId": "sub-9"})
		if st.active == "" {
			st.turns++
			turn := fmt.Sprintf("turn-q%d", st.turns)
			notify("turn/started", map[string]any{"threadId": rootThread, "turn": map[string]any{"id": turn}})
			notify("item/agentMessage/delta", map[string]any{"threadId": rootThread, "turnId": turn, "itemId": "m1", "delta": "queued-ok"})
			notify("turn/completed", map[string]any{"threadId": rootThread, "turn": map[string]any{"id": turn, "status": "completed"}})
		}
	default:
		replyErr(id, -32601, "method not found: "+method)
	}
}

func (st *fakeState) turnStart(id json.RawMessage, params map[string]any) {
	text := firstText(params)
	st.turns++
	turn := fmt.Sprintf("turn-%d", st.turns)
	if text == "replay" {
		st.replay(id)
		return
	}
	reply(id, map[string]any{"turn": map[string]any{"id": turn}})
	st.active = turn
	root := func(extra map[string]any) map[string]any {
		extra["threadId"] = rootThread
		return extra
	}
	started := func() {
		notify("turn/started", map[string]any{"threadId": rootThread, "turn": map[string]any{"id": turn}})
	}
	completed := func(status string, errObj any) {
		st.active = ""
		t := map[string]any{"id": turn, "status": status}
		if errObj != nil {
			t["error"] = errObj
		}
		notify("turn/completed", map[string]any{"threadId": rootThread, "turn": t})
	}
	switch text {
	case "hold":
		started()
	case "crash":
		started()
		os.Exit(9)
	case "ask", "ask-cancel":
		started()
		req := "srv-7"
		if text == "ask-cancel" {
			req = "srv-8"
		}
		emit(map[string]any{"id": req, "method": "item/commandExecution/requestApproval", "params": root(map[string]any{
			"turnId": turn, "itemId": "exec-1", "command": "rm -rf build"})})
		if text == "ask-cancel" {
			notify("serverRequest/resolved", root(map[string]any{"requestId": req}))
			completed("completed", nil)
			return
		}
		st.pending = req
	case "child":
		started()
		notify("item/started", root(map[string]any{"turnId": turn, "item": map[string]any{
			"type": "subAgentActivity", "id": "call_1", "kind": "started", "agentThreadId": "thread-child", "agentPath": "/root/helper"}}))
		notify("item/completed", root(map[string]any{"turnId": turn, "item": map[string]any{
			"type": "subAgentActivity", "id": "call_1", "kind": "started", "agentThreadId": "thread-child", "agentPath": "/root/helper"}}))
		child := map[string]any{"threadId": "thread-child"}
		notify("turn/started", map[string]any{"threadId": "thread-child", "turn": map[string]any{"id": "turn-child"}})
		notify("item/agentMessage/delta", map[string]any{"threadId": "thread-child", "turnId": "turn-child", "itemId": "cm", "delta": "child says hi"})
		notify("thread/tokenUsage/updated", usage("thread-child", "turn-child", 30, 3))
		emit(map[string]any{"id": "srv-child", "method": "item/commandExecution/requestApproval", "params": map[string]any{
			"threadId": "thread-child", "turnId": "turn-child", "itemId": "cx"}})
		notify("turn/completed", map[string]any{"threadId": "thread-child", "turn": map[string]any{"id": "turn-child", "status": "completed"}})
		_ = child
		notify("thread/tokenUsage/updated", usage(rootThread, turn, 100, 10))
		notify("item/agentMessage/delta", root(map[string]any{"turnId": turn, "itemId": "m1", "delta": "root done"}))
		completed("completed", nil)
	case "compact":
		started()
		item := map[string]any{"id": "cc_1", "type": "contextCompaction"}
		notify("item/started", root(map[string]any{"turnId": turn, "item": item}))
		notify("item/completed", root(map[string]any{"turnId": turn, "item": item}))
		notify("thread/compacted", root(map[string]any{"turnId": turn}))
		completed("completed", nil)
	case "compact-legacy":
		started()
		notify("item/started", root(map[string]any{"turnId": turn, "item": map[string]any{"id": "cc_2", "type": "contextCompaction"}}))
		notify("thread/compacted", root(map[string]any{"turnId": turn}))
		completed("completed", nil)
	case "fail":
		started()
		notify("error", root(map[string]any{"error": map[string]any{"message": "boom", "additionalDetails": "quota"}}))
		completed("failed", map[string]any{"message": "boom", "codexErrorInfo": "usageLimitExceeded"})
	case "unknown":
		started()
		notify("mystery/event", root(map[string]any{"turnId": turn}))
		completed("completed", nil)
	default:
		started()
		cid, _ := params["clientUserMessageId"].(string)
		notify("item/started", root(map[string]any{"turnId": turn, "item": map[string]any{
			"type": "userMessage", "id": "um-1", "clientId": cid,
			"content": []any{map[string]any{"type": "text", "text": text}}}}))
		notify("item/agentMessage/delta", root(map[string]any{"turnId": turn, "itemId": "m1", "delta": "ok"}))
		notify("thread/tokenUsage/updated", usage(rootThread, turn, 120, 30))
		completed("completed", nil)
	}
}

func usage(thread, turn string, in, out int) map[string]any {
	b := func(i, o int) map[string]any {
		return map[string]any{"totalTokens": i + o, "inputTokens": i, "cachedInputTokens": 0, "outputTokens": o, "reasoningOutputTokens": 0}
	}
	return map[string]any{"threadId": thread, "turnId": turn, "tokenUsage": map[string]any{
		"total": b(in, out), "last": b(in, out), "modelContextWindow": 1000}}
}

// clientReply handles the client's answer to a server request.
func (st *fakeState) clientReply(id string) {
	if st.pending == "" || strings.Trim(id, `"`) != st.pending {
		return
	}
	st.pending = ""
	turn := st.active
	notify("serverRequest/resolved", map[string]any{"threadId": rootThread, "requestId": strings.Trim(id, `"`)})
	notify("item/completed", map[string]any{"threadId": rootThread, "turnId": turn, "item": map[string]any{
		"type": "commandExecution", "id": "exec-1", "command": "rm -rf build", "status": "completed", "exitCode": 0, "aggregatedOutput": "done"}})
	st.active = ""
	notify("turn/completed", map[string]any{"threadId": rootThread, "turn": map[string]any{"id": turn, "status": "completed"}})
}

// replay answers turn/start from a recorded OAR raw fixture: every line is a
// flattened notification (params beside method), re-wrapped here as JSON-RPC.
func (st *fakeState) replay(id json.RawMessage) {
	f, err := os.Open(os.Getenv(fixtureEnv))
	if err != nil {
		replyErr(id, -32603, err.Error())
		return
	}
	defer f.Close()
	var notes []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	turn := ""
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		method, _ := m["method"].(string)
		if method == "thread/start" || method == "thread/resume" {
			continue
		}
		if tid, _ := asObj(m["turn"]).str("id"); turn == "" && method == "turn/started" {
			turn = tid
		}
		delete(m, "method")
		notes = append(notes, map[string]any{"method": method, "params": m})
	}
	reply(id, map[string]any{"turn": map[string]any{"id": turn}})
	for _, n := range notes {
		emit(n)
	}
}

// ---- harness -------------------------------------------------------------------

type fakeEnv struct {
	t   *testing.T
	log string
}

func watchdog(t *testing.T, d time.Duration) func() {
	timer := time.AfterFunc(d, func() {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		fmt.Fprintf(os.Stderr, "WATCHDOG: %s still running after %v\n%s\n", t.Name(), d, buf)
		os.Exit(3)
	})
	return func() { timer.Stop() }
}

// newFake prepares the environment of a fake app-server scenario. Tests using
// it must not run in parallel (it sets process environment).
func newFake(t *testing.T, scenario string, kv ...string) *fakeEnv {
	t.Helper()
	stop := watchdog(t, 60*time.Second)
	t.Cleanup(stop)
	e := &fakeEnv{t: t, log: t.TempDir() + "/wire.log"}
	t.Setenv(modeEnv, "fake")
	t.Setenv(scenarioEnv, scenario)
	t.Setenv(logEnv, e.log)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	for i := 0; i+1 < len(kv); i += 2 {
		t.Setenv(kv[i], kv[i+1])
	}
	return e
}

func (e *fakeEnv) config(cfg Config) Config {
	exe, err := os.Executable()
	if err != nil {
		e.t.Fatal(err)
	}
	cfg.Command = exe
	cfg.Prefix = []string{"-test.run=TestHelperProcess$", "--"}
	if cfg.VersionTimeout == 0 {
		cfg.VersionTimeout = 10 * time.Second
	}
	return cfg
}

func (e *fakeEnv) open(opt oarlock.Options, cfg Config) (*Session, error) {
	e.t.Helper()
	if opt.Cwd == "" {
		opt.Cwd = e.t.TempDir()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return OpenWith(ctx, oarlock.Installation{Available: true}, opt, e.config(cfg))
}

func (e *fakeEnv) mustOpen(opt oarlock.Options, cfg Config) *Session {
	e.t.Helper()
	s, err := e.open(opt, cfg)
	if err != nil {
		e.t.Fatalf("open: %v", err)
	}
	e.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = s.Dispose(ctx)
	})
	return s
}

// wire returns what the fake app-server saw, one entry per line.
func (e *fakeEnv) wire() []string {
	b, err := os.ReadFile(e.log)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// methods are the JSON-RPC methods the client sent, in order; probes and starts
// are reported as "PROBE" / "START".
func (e *fakeEnv) methods() []string {
	var out []string
	for _, l := range e.wire() {
		if l == "PROBE" || strings.HasPrefix(l, "START") {
			out = append(out, strings.Fields(l)[0])
			continue
		}
		var m struct {
			Method string `json:"method"`
			ID     any    `json:"id"`
		}
		if json.Unmarshal([]byte(l), &m) == nil {
			if m.Method != "" {
				out = append(out, m.Method)
			} else if m.ID != nil {
				out = append(out, "reply:"+fmt.Sprint(m.ID))
			}
		}
	}
	return out
}

// sent returns the params of the n-th (0-based) client request of a method.
func (e *fakeEnv) sent(method string, n int) map[string]any {
	for _, l := range e.wire() {
		var m struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal([]byte(l), &m) == nil && m.Method == method {
			if n == 0 {
				return m.Params
			}
			n--
		}
	}
	return nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitIdle(t *testing.T, s *Session) {
	t.Helper()
	waitFor(t, "an idle status after a turn_ended", func() bool {
		return hasEvent(s.Records(oarlock.Cursor{}), oarlock.EventTurnEnded) && s.status().Kind == "idle"
	})
}

func hasEvent(recs []oarlock.Record, kind oarlock.EventKind) bool {
	for _, r := range recs {
		for _, e := range r.Events {
			if e.Kind == kind {
				return true
			}
		}
	}
	return false
}

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

// skeleton renders a stream as one line per record.
func skeleton(recs []oarlock.Record) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		where := ""
		if r.SessionID != rootThread && r.SessionID != "" {
			where = " @" + r.SessionID
		}
		switch r.Kind {
		case oarlock.KindRequest:
			if r.Request.Action == oarlock.ActionNative {
				out = append(out, fmt.Sprintf("%s native:%s%s", r.Direction, r.Request.NativeType, where))
			} else {
				out = append(out, fmt.Sprintf("%s %s", r.Direction, r.Request.Action))
			}
		case oarlock.KindResponse:
			s := "response " + string(r.Response.Kind)
			if r.Response.Code != "" {
				s += ":" + string(r.Response.Code)
			}
			out = append(out, s)
		default:
			var kinds []string
			for _, e := range r.Events {
				kinds = append(kinds, string(e.Kind))
			}
			s := "frame"
			if m, _ := parseObjMap(r.Native)["method"].(string); m != "" {
				s += " " + m
			}
			s += where
			if r.SpanID != "" {
				s += " span=" + r.SpanID
			}
			if len(kinds) > 0 {
				s += " → " + strings.Join(kinds, ",")
			}
			out = append(out, s)
		}
	}
	return out
}

func parseObjMap(raw []byte) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}
