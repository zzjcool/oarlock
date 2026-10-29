package acp

// The projection: a pure reading of one wire message into events. It holds
// only the memory a fold needs (which tool calls already ended, the grok
// ledger already billed); a replay of the same lines gives the same events.
// Every message is retained verbatim in Native by the caller; this file only
// decides what oarlock READS out of it. Unknown kinds yield no event and are
// counted, never coerced.

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/zzjcool/oarlock"
)

const maxText = 10_000

var knownUpdates = map[string]bool{
	"agent_message_chunk": true, "agent_thought_chunk": true, "user_message_chunk": true,
	"tool_call": true, "tool_call_update": true, "usage_update": true, "plan": true,
	"available_commands_update": true, "current_mode_update": true, "config_option_update": true,
	"session_info_update": true,
}

type toolState struct {
	ended bool
	input string
	has   bool
}

type projector struct {
	prof Profile
	root string

	mu      sync.Mutex
	tools   map[string]*toolState
	unknown map[string]int
	billed  oarlock.TokenTotals
	seen    map[string]bool
}

func newProjector(prof Profile, root string) *projector {
	return &projector{prof: prof, root: root, tools: map[string]*toolState{}, unknown: map[string]int{}, seen: map[string]bool{}}
}

func (p *projector) setRoot(id string) {
	p.mu.Lock()
	p.root = id
	p.mu.Unlock()
}

func (p *projector) tally() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.unknown))
	for k, v := range p.unknown {
		out[k] = v
	}
	return out
}

func decodeMap(raw json.RawMessage) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

// notification reads one agent notification. It returns the attribution the
// record carries and the events in frame order.
func (p *projector) notification(w wireMsg) (sessionID string, path []string, events []oarlock.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	params := decodeMap(w.Params)
	sessionID = asString(params["sessionId"])
	info := &FrameInfo{bill: p.bill, Method: w.Method, Params: params, Update: asMap(params["update"]), Root: p.root, SessionID: sessionID}
	if w.Method == "session/update" && info.Update != nil {
		kind := asString(info.Update["sessionUpdate"])
		if !knownUpdates[kind] {
			p.unknown["session/update/"+kind]++
		}
		events = p.update(sessionID, info.Update)
	}
	if p.prof.Annotate != nil {
		p.prof.Annotate(info)
	}
	events = append(events, info.Events...)
	return info.SessionID, info.AgentPath, events
}

// response reads a handshake or prompt answer. prompt is true when w answers
// a session/prompt; final says this answer is the runtime's end of the turn
// (not a steered-over or non-ending one).
func (p *projector) response(w wireMsg, method string, final, bills bool) []oarlock.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := decodeMap(w.Result)
	var events []oarlock.Event
	switch method {
	case "session/prompt":
		if len(w.Error) > 0 && string(w.Error) != "null" {
			if final {
				events = append(events, oarlock.Event{Kind: oarlock.EventTurnEnded, Outcome: errorOutcome(w.Error)})
			}
			return events
		}
		if bills && (p.prof.PromptContext != nil || p.prof.PromptTokens != nil) {
			// An answer bills only when its usage is that prompt's own spend.
			// The answer that ends the turn always bills; a superseded turn
			// answer (grok sendNow answers the running prompt cancelled) is
			// still that prompt's spend and bills too. A steer's answer is not:
			// opencode answers it at idle with the same per-turn usage as the
			// turn's own answer, so billing it would count one turn twice. A
			// superseded answer also states its own context reading, which is
			// the vendor's number for that prompt and not a second ledger.
			usage := oarlock.UsageReport{}
			if p.prof.PromptContext != nil {
				usage.Context = p.prof.PromptContext(result)
			}
			if p.prof.PromptTokens != nil {
				if t := p.prof.PromptTokens(result); t != nil {
					p.billed = addTotals(p.billed, *t)
					cum := p.billed
					usage.Tokens = &cum
				}
			}
			if usage.Context != nil || usage.Tokens != nil {
				events = append(events, oarlock.Event{Kind: oarlock.EventUsage, Usage: &usage})
			}
		}
		if final {
			events = append(events, oarlock.Event{Kind: oarlock.EventTurnEnded, Outcome: stopOutcome(result)})
		}
	case "initialize":
	default:
		events = append(events, reported(result)...)
	}
	return events
}

// bill adds an independent vendor ledger once per native key. It is called
// from Annotate, which already runs under the projector's lock.
func (p *projector) bill(key string, t oarlock.TokenTotals) []oarlock.Event {
	if key == "" || p.seen[key] {
		return nil
	}
	p.seen[key] = true
	p.billed = addTotals(p.billed, t)
	cum := p.billed
	return []oarlock.Event{{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{Tokens: &cum}}}
}

func stopOutcome(result map[string]any) *oarlock.TurnOutcome {
	if asString(result["stopReason"]) == "cancelled" {
		return &oarlock.TurnOutcome{Kind: oarlock.OutcomeAborted}
	}
	return &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted}
}

// errorOutcome classifies by the JSON-RPC code only; prose is never the key.
func errorOutcome(raw json.RawMessage) *oarlock.TurnOutcome {
	e := decodeMap(raw)
	out := &oarlock.TurnOutcome{Kind: oarlock.OutcomeFailed, Failure: oarlock.FailureUnknown, Reason: asString(e["message"])}
	if code, ok := asInt64(e["code"]); ok && code == -32000 {
		out.Failure = oarlock.FailureAuth
	}
	return out
}

// reported reads the model and effort a frame names as in effect.
func reported(frame map[string]any) []oarlock.Event {
	var events []oarlock.Event
	if frame == nil {
		return nil
	}
	options, _ := frame["configOptions"].([]any)
	for _, o := range options {
		opt := asMap(o)
		cur := asString(opt["currentValue"])
		switch {
		case asString(opt["id"]) == "model" && cur != "":
			events = append(events, oarlock.Event{Kind: oarlock.EventModel, Model: cur})
		case asString(opt["category"]) == "thought_level" && cur != "":
			events = append(events, oarlock.Event{Kind: oarlock.EventEffort, Effort: cur})
		}
	}
	if m := asString(asMap(frame["models"])["currentModelId"]); m != "" && len(events) == 0 {
		events = append(events, oarlock.Event{Kind: oarlock.EventModel, Model: m})
	}
	if m := asString(asMap(frame["_meta"])["model"]); m != "" && len(events) == 0 {
		events = append(events, oarlock.Event{Kind: oarlock.EventModel, Model: m})
	}
	return events
}

func (p *projector) update(sessionID string, u map[string]any) []oarlock.Event {
	var events []oarlock.Event
	switch asString(u["sessionUpdate"]) {
	case "usage_update":
		used, ok1 := asInt64(u["used"])
		size, ok2 := asInt64(u["size"])
		if ok1 || ok2 {
			c := &oarlock.ContextUsage{}
			if ok1 {
				c.Tokens = &used
			}
			if ok2 {
				c.ContextWindow = &size
			}
			if ok1 && ok2 && size != 0 {
				c.Percent = ptr(float64(int(float64(used)/float64(size)*100 + 0.5)))
			}
			events = append(events, oarlock.Event{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{Context: c}})
		}
	case "agent_message_chunk":
		if t, ok := textOf(u["content"]); ok && t != "" {
			events = append(events, oarlock.Event{Kind: oarlock.EventTextDelta, Text: t, MessageID: asString(u["messageId"])})
		}
	case "agent_thought_chunk":
		if t, ok := textOf(u["content"]); ok {
			events = append(events, oarlock.Event{Kind: oarlock.EventReasoning, Text: t})
		} else {
			events = append(events, oarlock.Event{Kind: oarlock.EventReasoning, Redacted: true})
		}
	case "tool_call", "tool_call_update":
		events = append(events, p.tool(sessionID, u)...)
	}
	events = append(events, reported(u)...)
	return events
}

func textOf(v any) (string, bool) {
	switch c := v.(type) {
	case map[string]any:
		if s, ok := c["text"].(string); ok {
			return s, true
		}
		if asString(c["type"]) == "content" {
			return textOf(c["content"])
		}
	case []any:
		var parts []string
		for _, i := range c {
			if s, ok := textOf(i); ok {
				parts = append(parts, s)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n"), true
		}
	}
	return "", false
}

func cut(s string) string {
	if len(s) > maxText {
		return s[:maxText] + "…"
	}
	return s
}

func detail(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return cut(x), true
	}
	if t, ok := textOf(v); ok {
		return cut(t), true
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", false
	}
	return cut(string(b)), true
}

func toolName(u map[string]any) string {
	for _, k := range []string{"name", "toolName"} {
		if s := asString(u[k]); s != "" {
			return s
		}
	}
	// ACP's kind is a category, never a tool name; the opening frame's title is
	// the closest thing the protocol has. A later update retitles with progress.
	if asString(u["sessionUpdate"]) == "tool_call" && asString(u["title"]) != "" {
		return asString(u["title"])
	}
	if k := asString(u["kind"]); k != "" {
		return k
	}
	return "tool"
}

func (p *projector) tool(sessionID string, u map[string]any) []oarlock.Event {
	id := asString(u["toolCallId"])
	if id == "" {
		return nil
	}
	key := sessionID + "\x00" + id
	st, seen := p.tools[key]
	var events []oarlock.Event
	input, hasInput := detail(u["rawInput"])
	if !seen {
		st = &toolState{input: input, has: hasInput}
		p.tools[key] = st
		ev := oarlock.Event{Kind: oarlock.EventToolCallStarted, CallID: id, Tool: toolName(u)}
		if hasInput {
			ev.Input = input
		}
		events = append(events, ev)
	} else if !st.ended && hasInput && (!st.has || input != st.input) {
		st.input, st.has = input, true
		events = append(events, oarlock.Event{Kind: oarlock.EventToolCallInput, CallID: id, Input: input})
	}
	status := asString(u["status"])
	terminal := status == "completed" || status == "failed" || status == "cancelled"
	if seen && !st.ended && !terminal {
		if out, ok := detail(u["rawOutput"]); ok {
			events = append(events, oarlock.Event{Kind: oarlock.EventToolCallProgress, CallID: id, Output: out})
		}
	}
	if !st.ended && terminal {
		st.ended = true
		ev := oarlock.Event{Kind: oarlock.EventToolCallEnded, CallID: id, Content: toolParts(u)}
		switch status {
		case "completed":
			ev.Result = oarlock.ToolOK
		case "failed":
			ev.Result = oarlock.ToolFailed
		}
		if raw := asMap(u["rawOutput"]); raw != nil {
			for _, k := range []string{"exit_code", "exitCode"} {
				if v, present := raw[k]; present {
					ev.ExitCodeReported = true
					if n, ok := asInt64(v); ok {
						c := int(n)
						ev.ExitCode = &c
					}
					break
				}
			}
		}
		events = append(events, ev)
	}
	return events
}

func toolParts(u map[string]any) []oarlock.ToolOutputPart {
	var parts []oarlock.ToolOutputPart
	if items, ok := u["content"].([]any); ok {
		for _, it := range items {
			m := asMap(it)
			inner := it
			if asString(m["type"]) == "content" {
				inner = m["content"]
			}
			im := asMap(inner)
			switch {
			case asString(im["type"]) == "text":
				parts = append(parts, oarlock.ToolOutputPart{Type: "text", Text: cut(asString(im["text"]))})
			case asString(im["type"]) == "image":
				parts = append(parts, oarlock.ToolOutputPart{Type: "image", MediaType: asString(im["mimeType"]), Data: asString(im["data"])})
			default:
				b, _ := json.Marshal(inner)
				parts = append(parts, oarlock.ToolOutputPart{Type: "other", Value: b})
			}
		}
	}
	if len(parts) == 0 && u["rawOutput"] != nil {
		if s, ok := u["rawOutput"].(string); ok {
			parts = append(parts, oarlock.ToolOutputPart{Type: "text", Text: cut(s)})
		} else {
			b, _ := json.Marshal(u["rawOutput"])
			parts = append(parts, oarlock.ToolOutputPart{Type: "other", Value: b})
		}
	}
	return parts
}

func addTotals(a, b oarlock.TokenTotals) oarlock.TokenTotals {
	out := oarlock.TokenTotals{Input: a.Input + b.Input, Output: a.Output + b.Output}
	add := func(x, y *int64) *int64 {
		if x == nil && y == nil {
			return nil
		}
		var s int64
		if x != nil {
			s += *x
		}
		if y != nil {
			s += *y
		}
		return &s
	}
	out.Reasoning = add(a.Reasoning, b.Reasoning)
	out.CacheRead = add(a.CacheRead, b.CacheRead)
	out.CacheWrite = add(a.CacheWrite, b.CacheWrite)
	return out
}
