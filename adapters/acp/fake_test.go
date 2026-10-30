package acp

// A fake ACP agent that is the test binary itself: TestMain turns the process
// into the agent when ACP_FAKE_AGENT is set. It speaks raw JSON-RPC over
// stdio so a scenario can emit any vendor frame; no network, no real binary.
// Scenarios are chosen by the prompt text. Frame shapes for grok / opencode v2
// follow the OAR recordings (/tmp/oar-research fake-acp-grok.mjs,
// opencode-acp-v2-child.json), ids shortened.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("ACP_FAKE_AGENT") != "" {
		fakeAgent()
		return
	}
	os.Exit(m.Run())
}

type fake struct {
	mu      sync.Mutex
	out     *bufio.Writer
	nextID  int
	pending map[string]chan map[string]any
	cancel  chan struct{}
	steered chan struct{}
	flavor  string
	prompts int
}

func (f *fake) send(v map[string]any) {
	v["jsonrpc"] = "2.0"
	b, _ := json.Marshal(v)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.out.Write(append(b, '\n'))
	f.out.Flush()
}

func (f *fake) notify(method string, params any) {
	f.send(map[string]any{"method": method, "params": params})
}

func (f *fake) update(session string, u map[string]any) {
	f.notify("session/update", map[string]any{"sessionId": session, "update": u})
}

// call sends an agent->client request and waits for the reply.
func (f *fake) call(method string, params any) map[string]any {
	f.mu.Lock()
	f.nextID++
	id := fmt.Sprintf("a%d", f.nextID)
	ch := make(chan map[string]any, 1)
	f.pending[id] = ch
	f.mu.Unlock()
	f.send(map[string]any{"id": id, "method": method, "params": params})
	select {
	case r := <-ch:
		return r
	case <-time.After(20 * time.Second):
		return map[string]any{"error": "timeout"}
	}
}

func fakeAgent() {
	f := &fake{out: bufio.NewWriter(os.Stdout), pending: map[string]chan map[string]any{}, cancel: make(chan struct{}, 8), steered: make(chan struct{}, 8), flavor: os.Getenv("ACP_FAKE_AGENT")}
	if p := os.Getenv("ACP_FAKE_ARGS_FILE"); p != "" {
		os.WriteFile(p, []byte(strings.Join(os.Args[1:], "\n")), 0o600)
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		method, _ := m["method"].(string)
		id, hasID := m["id"]
		switch {
		case method == "" && hasID:
			f.mu.Lock()
			ch := f.pending[fmt.Sprint(id)]
			delete(f.pending, fmt.Sprint(id))
			f.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case method == "session/cancel":
			f.cancel <- struct{}{}
		case hasID:
			go f.handle(id, method, m["params"])
		}
	}
	os.Exit(0)
}

func (f *fake) reply(id any, result any) { f.send(map[string]any{"id": id, "result": result}) }
func (f *fake) fail(id any, code int, msg string, data any) {
	f.send(map[string]any{"id": id, "error": map[string]any{"code": code, "message": msg, "data": data}})
}

func (f *fake) handle(id any, method string, params any) {
	p, _ := params.(map[string]any)
	switch method {
	case "initialize":
		if path := os.Getenv("ACP_FAKE_MODELS_SEEN"); path != "" {
			// The listing handshake is asserted by tests/models_test.go; only a
			// models scenario asks for the record.
			b, _ := json.Marshal(p)
			os.WriteFile(path, b, 0o600)
		}
		caps := map[string]any{"loadSession": true, "promptCapabilities": map[string]any{"image": f.flavor != "grok"},
			"sessionCapabilities": map[string]any{"close": map[string]any{}, "list": map[string]any{}}}
		if os.Getenv("ACP_FAKE_NO_LOAD") != "" {
			caps["loadSession"] = false
		}
		res := map[string]any{"protocolVersion": 1, "agentCapabilities": caps,
			"authMethods": []any{map[string]any{"id": "cached_token", "name": "t"}}, "_meta": map[string]any{"defaultAuthMethodId": "cached_token"}}
		f.reply(id, res)
	case "authenticate":
		f.reply(id, map[string]any{})
	case "session/new":
		f.newSession(id)
	case "session/close":
		f.reply(id, map[string]any{})
	case "_x.ai/models/list":
		f.modelsList(id)
	case "session/load", "session/resume":
		sid, _ := p["sessionId"].(string)
		if sid == "missing" {
			f.fail(id, -32603, "Internal error", map[string]any{"code": "FS_NOT_FOUND"})
			return
		}
		// History replays as ordinary updates before the answer.
		f.update(sid, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "history"}})
		f.reply(id, map[string]any{})
	case "session/list":
		// Only when a test asks: the resume-absence walk must also be exercised
		// where the list itself fails (the default -32601 below).
		if os.Getenv("ACP_FAKE_LIST_OK") == "" {
			f.fail(id, -32601, "Method not found", nil)
			return
		}
		f.reply(id, map[string]any{"sessions": []any{}})
	case "session/set_mode", "session/set_config_option":
		f.reply(id, map[string]any{})
	case "session/prompt":
		f.prompt(id, p)
	default:
		f.fail(id, -32601, "Method not found", nil)
	}
}

// ---- models listing (issue #98) -------------------------------------------
//
// ACP_FAKE_MODELS switches the two answer surfaces a listing reads: grok's
// `_x.ai/models/list` extension and the ACP-standard `session/new`
// configOptions. Unset means the scenarios above, unchanged, so every existing
// ACP test keeps the answers it was written against.

// modelsMode is the ACP_FAKE_MODELS scenario:
//
//	ok | plain | empty | no-model-option | missing-id | no-models-method |
//	unauth | unauth-text | handler-error | server-request | hold | exit
func modelsMode() string { return os.Getenv("ACP_FAKE_MODELS") }

// newSession answers session/new. Unset keeps the answer the pre-models tests
// were written against: a session, no configOptions.
func (f *fake) newSession(id any) {
	result := map[string]any{"sessionId": "fake-session", "modes": map[string]any{"availableModes": []any{map[string]any{"id": "yolo"}}, "currentModeId": "default"}}
	switch modelsMode() {
	case "":
	case "ok":
		result["configOptions"] = modelsConfigOptions()
	case "plain":
		result["configOptions"] = modelsConfigOptionsPlain()
	case "empty":
		result["configOptions"] = modelsConfigOptionsEmpty()
	case "no-model-option":
		result["configOptions"] = modelsConfigOptionsNoModel()
	case "missing-id":
		result["configOptions"] = modelsConfigOptionsBlankIDs()
	case "server-request":
		if !f.awaitNoAnswer(id) {
			return
		}
		result["configOptions"] = modelsConfigOptions()
	case "no-models-method":
		f.fail(id, -32601, "Method not found", nil)
		return
	case "unauth":
		f.fail(id, -32000, "Authentication required", nil)
		return
	case "unauth-text":
		f.fail(id, -32603, "Not logged in. Please run `login`.", nil)
		return
	case "hold":
		return // never answer: only the client's own deadline ends this
	case "exit":
		fmt.Fprintln(os.Stderr, "fake agent: exiting before answering session/new")
		os.Exit(3)
	default:
		f.fail(id, -32603, "unknown ACP_FAKE_MODELS mode "+modelsMode(), nil)
		return
	}
	f.reply(id, result)
}

// modelsList answers grok's own listing extension. Its payload is
// double-enveloped: the JSON-RPC result carries `result` (or the handler's own
// `error`), the way the OAR recording of grok 1.0.25 shows it.
func (f *fake) modelsList(id any) {
	switch modelsMode() {
	case "", "plain", "no-models-method":
		// A build without the extension: the ACP-standard path takes over.
		f.fail(id, -32601, "Method not found", nil)
	case "ok", "server-request":
		f.emitListingTraffic()
		f.reply(id, map[string]any{"result": map[string]any{"availableModels": modelsGrokCatalogue(), "currentModelId": "grok-4"}})
	case "empty":
		f.reply(id, map[string]any{"result": map[string]any{"availableModels": []any{}}})
	case "no-model-option":
		// No array at all is not an answer: an absent catalogue must be an
		// error, never an empty list.
		f.reply(id, map[string]any{"result": map[string]any{"currentModelId": "grok-4"}})
	case "missing-id":
		f.reply(id, map[string]any{"result": map[string]any{"availableModels": []any{
			map[string]any{"modelId": "   "},
			map[string]any{"modelId": "grok-4", "displayName": "Grok 4"},
			map[string]any{"id": ""},
		}}})
	case "handler-error":
		// The handler's own failure envelope: the JSON-RPC result carries `error`
		// instead of the catalogue.
		f.reply(id, map[string]any{"error": map[string]any{"message": "models handler exploded"}})
	case "unauth":
		f.fail(id, -32000, "Authentication required", nil)
	case "unauth-text":
		f.fail(id, -32603, "Not logged in. Please run `login`.", nil)
	case "hold":
		return // never answer: only the client's own deadline ends this
	case "exit":
		fmt.Fprintln(os.Stderr, "fake agent: exiting before answering _x.ai/models/list")
		os.Exit(3)
	default:
		f.fail(id, -32603, "unknown ACP_FAKE_MODELS mode "+modelsMode(), nil)
	}
}

// awaitNoAnswer sends a server->client request mid-listing and reports whether
// the client stayed silent. The listing is failed loudly when it answered: a
// catalogue read must never drive a permission or terminal flow.
func (f *fake) awaitNoAnswer(id any) bool {
	reply := make(chan map[string]any, 1)
	f.mu.Lock()
	f.pending["srv-list"] = reply
	f.mu.Unlock()
	f.send(map[string]any{"id": "srv-list", "method": "session/request_permission", "params": map[string]any{
		"sessionId": "fake-session", "toolCall": map[string]any{"toolCallId": "t1"}, "options": []any{}}})
	select {
	case <-reply:
		f.fail(id, -32000, "the client answered a server request during a listing", nil)
		return false
	case <-time.After(250 * time.Millisecond):
		return true
	}
}

// emitListingTraffic is the traffic a real agent sends around a listing: a
// notification and a server->client request the client must skip.
func (f *fake) emitListingTraffic() {
	f.notify("session/update", map[string]any{"sessionId": "fake-session", "update": map[string]any{"sessionUpdate": "available_commands_update"}})
	f.send(map[string]any{"id": "srv-list", "method": "session/request_permission", "params": map[string]any{"sessionId": "fake-session", "options": []any{}}})
}

// modelsConfigOptions is the `ok` catalogue: one grouped options list (opencode
// groups), one leaf with no name (the name falls back to the value) and a
// session-scoped effort menu that only the selected model may claim.
func modelsConfigOptions() []any {
	return []any{
		map[string]any{"id": "model", "name": "Model", "category": "model", "currentValue": "opus", "options": []any{
			map[string]any{"group": "Claude", "name": "Claude models", "options": []any{
				map[string]any{"value": "opus", "name": "Opus"},
				map[string]any{"value": "sonnet", "name": "Sonnet"},
			}},
			map[string]any{"value": "haiku"},
		}},
		map[string]any{"id": "thought_level", "name": "Effort", "category": "thought_level", "currentValue": "low", "options": []any{
			map[string]any{"value": "off", "name": "Off"},
			map[string]any{"value": "low", "name": "Low"},
			map[string]any{"value": "high", "name": "High"},
		}},
	}
}

// modelsConfigOptionsPlain is the second model-option spelling OAR tolerated:
// no `category`, and only `id == "model"`.
func modelsConfigOptionsPlain() []any {
	return []any{map[string]any{"id": "model", "currentValue": "m2", "options": []any{
		map[string]any{"value": "m1", "name": "Model One"},
		map[string]any{"value": "m2", "name": "Model Two"},
	}}}
}

// modelsConfigOptionsEmpty is a build that answered "zero models".
func modelsConfigOptionsEmpty() []any {
	return []any{map[string]any{"id": "model", "category": "model", "currentValue": "", "options": []any{}}}
}

// modelsConfigOptionsNoModel is a build whose session/new carries configOptions
// but no model option at all.
func modelsConfigOptionsNoModel() []any {
	return []any{map[string]any{"id": "mode", "category": "mode", "currentValue": "default", "options": []any{
		map[string]any{"value": "default", "name": "Default"},
	}}}
}

// modelsConfigOptionsBlankIDs carries entries with no usable id next to real
// ones: a listing must skip them, never emit "".
func modelsConfigOptionsBlankIDs() []any {
	return []any{map[string]any{"id": "model", "category": "model", "currentValue": "sonnet", "options": []any{
		map[string]any{"value": "opus", "name": "Opus"},
		map[string]any{"value": "   "},
		map[string]any{"name": "No value"},
		map[string]any{"value": ""},
		map[string]any{"value": "sonnet", "name": "Sonnet"},
	}}}
}

// modelsGrokCatalogue is grok's own catalogue: `modelId` plus per-model
// `_meta` reasoning efforts, one hidden entry, one the account may not select,
// and one entry whose effort menu is the top-level spelling.
func modelsGrokCatalogue() []any {
	return []any{
		map[string]any{"modelId": "grok-4", "displayName": "Grok 4", "_meta": map[string]any{"supportsReasoningEffort": true,
			"reasoningEfforts": []any{map[string]any{"id": "low"}, map[string]any{"id": "high"}}}},
		map[string]any{"model_id": "grok-3", "name": "Grok 3", "_meta": map[string]any{"supportsReasoningEffort": false,
			"reasoningEfforts": []any{map[string]any{"id": "low"}}}, "reasoningEfforts": []any{map[string]any{"value": "medium"}}},
		map[string]any{"modelId": "grok-hidden", "displayName": "Grok Hidden", "hidden": true},
		map[string]any{"modelId": "grok-fixed", "displayName": "Grok Fixed", "userSelectable": false},
		map[string]any{"id": "grok-mini", "displayName": " ", "reasoningEfforts": []any{map[string]any{"value": "medium"}}},
	}
}

func promptText(p map[string]any) string {
	blocks, _ := p["prompt"].([]any)
	var parts []string
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok {
			if t, ok := m["text"].(string); ok {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, " ")
}

func chunk(text string) map[string]any {
	return map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}}
}

func (f *fake) prompt(id any, p map[string]any) {
	sid, _ := p["sessionId"].(string)
	text := promptText(p)
	meta, _ := p["_meta"].(map[string]any)
	f.mu.Lock()
	f.prompts++
	f.mu.Unlock()
	switch {
	case meta["sendNow"] == true || meta["priority"] == "next":
		// A steering prompt: tell the running prompt, answer ourselves.
		f.steered <- struct{}{}
		f.update(sid, chunk("steered:"+text))
		f.reply(id, map[string]any{"stopReason": "end_turn", "_meta": map[string]any{"totalTokens": 17405,
			"usage": map[string]any{"inputTokens": 34420, "outputTokens": 279, "cachedReadTokens": 19968}}})
	case text == "usage":
		f.update(sid, chunk("billed"))
		f.reply(id, map[string]any{"stopReason": "end_turn", "_meta": map[string]any{"totalTokens": 1001, "contextWindow": 200000,
			"usage": map[string]any{"inputTokens": 100, "outputTokens": 7, "cachedReadTokens": 64, "cacheCreationTokens": 0}}})
	case text == "perm" || text == "perm-reject-only":
		opts := []any{
			map[string]any{"optionId": "no", "name": "No", "kind": "reject_once"},
			map[string]any{"optionId": "once", "name": "Once", "kind": "allow_once"},
			map[string]any{"optionId": "always", "name": "Always", "kind": "allow_always"},
		}
		if text == "perm-reject-only" {
			opts = opts[:1]
		}
		f.update(sid, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t1", "title": "Bash", "kind": "execute", "status": "pending", "rawInput": map[string]any{"command": "ls"}})
		r := f.call("session/request_permission", map[string]any{"sessionId": sid, "toolCall": map[string]any{"toolCallId": "t1"}, "options": opts})
		out, _ := json.Marshal(r["result"])
		f.update(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t1", "status": "completed", "rawOutput": map[string]any{"exit_code": 0, "output": "ok"}})
		f.update(sid, chunk("perm:"+string(out)))
		f.reply(id, map[string]any{"stopReason": "end_turn"})
	case text == "term":
		cmd, args := "sh", []any{"-c", "echo term-ok; exit 3"}
		c := f.call("terminal/create", map[string]any{"sessionId": sid, "command": cmd, "args": args})
		tid, _ := c["result"].(map[string]any)["terminalId"].(string)
		f.update(sid, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t2", "title": "Bash", "status": "in_progress", "content": []any{map[string]any{"type": "terminal", "terminalId": tid}}})
		w := f.call("terminal/wait_for_exit", map[string]any{"sessionId": sid, "terminalId": tid})
		o := f.call("terminal/output", map[string]any{"sessionId": sid, "terminalId": tid})
		f.call("terminal/release", map[string]any{"sessionId": sid, "terminalId": tid})
		b1, _ := json.Marshal(w["result"])
		b2, _ := json.Marshal(o["result"])
		f.update(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t2", "status": "completed"})
		f.update(sid, chunk("term:"+string(b1)+string(b2)))
		f.reply(id, map[string]any{"stopReason": "end_turn"})
	case text == "slow":
		f.update(sid, chunk("working"))
		select {
		case <-f.cancel:
			f.reply(id, map[string]any{"stopReason": "cancelled"})
		case <-time.After(20 * time.Second):
			f.reply(id, map[string]any{"stopReason": "end_turn"})
		}
	case text == "stubborn":
		f.update(sid, chunk("deaf"))
		time.Sleep(time.Minute)
	case text == "steerable":
		f.update(sid, chunk("working"))
		select {
		case <-f.steered:
			// grok send-now: the running prompt is answered cancelled.
			f.reply(id, map[string]any{"stopReason": "cancelled", "_meta": map[string]any{"cancelTrigger": "send_now", "totalTokens": 16998,
				"usage": map[string]any{"inputTokens": 16776, "outputTokens": 222, "cachedReadTokens": 640}}})
		case <-time.After(20 * time.Second):
			f.reply(id, map[string]any{"stopReason": "end_turn"})
		}
	case text == "crash":
		f.update(sid, chunk("dying"))
		os.Exit(7)
	case text == "fail-auth":
		f.fail(id, -32000, "Authentication required", nil)
	case strings.HasPrefix(text, "child-"):
		f.child(id, sid, text)
	default:
		f.update(sid, chunk("echo:"+text))
		f.reply(id, map[string]any{"stopReason": "end_turn"})
	}
}

func (f *fake) child(id any, sid, text string) {
	switch text {
	case "child-grok":
		const child = "fake-child-grok"
		lineage := map[string]any{"parent_session_id": sid, "child_session_id": child, "subagent_type": "general-purpose"}
		vendor := func(session string, u map[string]any) {
			f.notify("_x.ai/session_notification", map[string]any{"sessionId": session, "update": u})
		}
		spawn := map[string]any{"sessionUpdate": "subagent_spawned", "subagent_id": child}
		for k, v := range lineage {
			spawn[k] = v
		}
		vendor(sid, spawn)
		f.update(child, chunk("child-says-hi"))
		vendor(child, map[string]any{"sessionUpdate": "turn_completed", "prompt_id": "child-prompt", "stop_reason": "end_turn",
			"usage": map[string]any{"inputTokens": 80, "outputTokens": 8}})
		vendor(sid, map[string]any{"sessionUpdate": "subagent_finished", "subagent_id": child, "child_session_id": child, "status": "completed"})
		f.update(sid, chunk("parent-continues"))
		// The parent's ledger already sums the child's calls.
		f.reply(id, map[string]any{"stopReason": "end_turn", "_meta": map[string]any{"totalTokens": 300,
			"usage": map[string]any{"inputTokens": 200, "outputTokens": 20}}})
	case "child-wake":
		// grok injects a prompt outside session/prompt; its terminal ledger is independent.
		w := map[string]any{"sessionUpdate": "turn_completed", "prompt_id": "subagent-completed-1", "usage": map[string]any{"inputTokens": 5, "outputTokens": 1}}
		for i := 0; i < 2; i++ { // duplicate delivery: billed once
			f.notify("_x.ai/session_notification", map[string]any{"sessionId": sid, "update": w, "_meta": map[string]any{"eventId": "ev-1"}})
		}
		f.reply(id, map[string]any{"stopReason": "end_turn", "_meta": map[string]any{"usage": map[string]any{"inputTokens": 10, "outputTokens": 2}}})
	case "child-oc2":
		meta := map[string]any{"opencode/child-session": map[string]any{"id": "child-1", "parentID": sid}}
		f.update(sid, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "call-1", "title": "subagent", "status": "pending"})
		f.update(sid, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "child-1:call-2", "title": "shell", "status": "pending", "_meta": meta})
		f.update(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "child-1:call-2", "status": "completed", "_meta": meta})
		f.update(sid, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "bad", "title": "x", "status": "pending",
			"_meta": map[string]any{"opencode/child-session": map[string]any{"id": sid, "parentID": sid}}})
		f.update(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "call-1", "status": "completed"})
		f.reply(id, map[string]any{"stopReason": "end_turn"})
	case "child-qwen":
		f.update(sid, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "task-1", "title": "Task", "status": "in_progress"})
		f.update(sid, map[string]any{"sessionUpdate": "agent_message_chunk", "parent_tool_use_id": "task-1", "content": map[string]any{"type": "text", "text": "from-child"}})
		f.update(sid, chunk("from-root"))
		f.reply(id, map[string]any{"stopReason": "end_turn"})
	case "child-flat":
		f.update(sid, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "task-1", "title": "start_subagent", "status": "completed"})
		f.update(sid, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "task-1:inner", "title": "run_command", "status": "in_progress"})
		f.update(sid, chunk("child text, flattened"))
		f.reply(id, map[string]any{"stopReason": "end_turn"})
	}
}
