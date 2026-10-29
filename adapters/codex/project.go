package codex

// The Codex projection (issue #20): one pure fold from a native app-server
// notification to the records it states.
//
//	foldNotification(state, method, params) -> (state', []frameCmd)
//
// Rules (docs/spec/record-stream.md, ported from OAR runtimes/codex/projection.ts):
//
//   - One notification in, one frame out: Native is the line verbatim, Events is
//     what oarlock read out of it, SpanID is codex's own turn id. Nothing is
//     gated on turn state and nothing is dropped.
//   - A notification of another thread is a record of a derived child session:
//     SessionID is that thread id. Nested attribution means the child is the
//     root agent of its own session, so its AgentPath stays empty; lineage lives
//     in the session graph (session_linked), reported once per edge by the
//     runtime's own collaboration items.
//   - No I/O, no clock, no goroutine. The state is a value; maps are copied on
//     write.
//   - A number the runtime did not report is nil, never 0.

import (
	"encoding/json"
	"math"
	"strings"

	"github.com/zzjcool/oarlock"
)

type obj map[string]any

func parseObj(raw []byte) obj {
	if len(raw) == 0 {
		return nil
	}
	var o obj
	if json.Unmarshal(raw, &o) != nil {
		return nil
	}
	return o
}

func (o obj) str(k string) (string, bool) {
	s, ok := o[k].(string)
	return s, ok
}

func (o obj) num(k string) (float64, bool) {
	f, ok := o[k].(float64)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func (o obj) obj(k string) obj {
	m, _ := o[k].(map[string]any)
	return obj(m)
}

func (o obj) arr(k string) []any {
	a, _ := o[k].([]any)
	return a
}

func asObj(v any) obj {
	m, _ := v.(map[string]any)
	return obj(m)
}

// ---- state --------------------------------------------------------------

// tokenBaseline is where the root thread's token count starts. Codex's total
// spans the thread's life; after a resume it re-reports that total once, before
// this session's first turn, and the session counts from there.
type tokenBaseline struct {
	kind   baselineKind
	tokens oarlock.TokenTotals
}

type baselineKind int

const (
	baselineKnown    baselineKind = iota // totals count from tokens
	baselineAwaiting                     // a resume still waiting for the re-report
	baselineUnknown                      // a resumed thread whose first turn started without a re-report
)

type turnInputs struct {
	steers map[string]bool
	echoed map[string]bool
}

type subagent struct {
	parent string
	path   string
}

// codexState is the fold's state. Never mutate a value you were given.
type codexState struct {
	root       string
	inputs     map[string]turnInputs
	lastError  string
	compacting bool
	subagents  map[string]subagent
	baseline   tokenBaseline
	unknown    map[string]int
}

func initialState(root string, resumed bool) codexState {
	b := tokenBaseline{kind: baselineKnown}
	if resumed {
		b = tokenBaseline{kind: baselineAwaiting}
	}
	return codexState{
		root:      root,
		inputs:    map[string]turnInputs{},
		subagents: map[string]subagent{},
		unknown:   map[string]int{},
		baseline:  b,
	}
}

// frameCmd is the one frame a notification becomes.
type frameCmd struct {
	SessionID string // "" = the root session
	SpanID    string
	Events    []oarlock.Event
	// Links are the edges a collaboration item names, every time it names them.
	// The session adds a session_linked event only for an edge the graph does
	// not hold yet (lineage is reported once per edge).
	Links []edge
}

// knownMethods are the notifications the evidence records. Others are kept
// verbatim and counted (never coerced).
var knownMethods = map[string]bool{
	"turn/started": true, "turn/completed": true,
	"item/started": true, "item/completed": true,
	"item/agentMessage/delta": true, "item/commandExecution/outputDelta": true,
	"item/reasoning/summaryTextDelta": true, "item/reasoning/textDelta": true, "item/reasoning/summaryPartAdded": true,
	"item/fileChange/outputDelta": true, "item/mcpToolCall/progress": true, "item/plan/delta": true,
	"thread/started": true, "thread/status/changed": true, "thread/tokenUsage/updated": true,
	"thread/compacted": true, "thread/settings/updated": true, "thread/name/updated": true,
	"serverRequest/resolved": true, "rawResponseItem/completed": true,
	"error": true, "remoteControl/status/changed": true, "turn/diff/updated": true, "turn/plan/updated": true,
	"account/rateLimits/updated": true, "mcpServer/startupStatus/updated": true,
	"thread/start": true, "thread/resume": true,
}

var toolItemTypes = map[string]bool{"commandExecution": true, "fileChange": true, "mcpToolCall": true, "webSearch": true, "sleep": true}
var collabItemTypes = map[string]bool{"collabAgentToolCall": true, "collabToolCall": true, "subAgentActivity": true}

const compactionItemType = "contextCompaction"
const rootPath = "/root"

func isCompactionItem(p obj) bool {
	t, _ := p.obj("item").str("type")
	return t == compactionItemType
}

// foldNotification folds one notification. params is the notification's params
// member (an object); a missing or malformed one reads as empty.
func foldNotification(prev codexState, method string, params obj) (codexState, frameCmd) {
	if params == nil {
		params = obj{}
	}
	thread, ok := params.str("threadId")
	if !ok {
		thread = prev.root
	}
	isRoot := thread == prev.root
	st := prev

	if !knownMethods[method] {
		st.unknown = copyCounts(prev.unknown)
		st.unknown[method]++
	}

	// Before the views: a re-reported total is read against itself (zero).
	if isRoot {
		st.baseline = nextBaseline(prev.baseline, method, params)
	}
	var events []oarlock.Event
	if isRoot {
		var in []oarlock.Event
		st.inputs, in = foldInputs(prev.inputs, method, params)
		events = append(events, in...)
	}
	events = append(events, st.views(thread, method, params)...)

	cmd := frameCmd{SpanID: spanID(params), Events: events}
	if !isRoot {
		cmd.SessionID = thread
	}

	if method == "item/started" || method == "item/completed" {
		cmd.Links = st.collabEdges(thread, params.obj("item"))
	}

	switch {
	case method == "error":
		e := params.obj("error")
		msg, _ := e.str("message")
		det, _ := e.str("additionalDetails")
		var parts []string
		for _, p := range []string{msg, det} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		if len(parts) > 0 {
			st.lastError = strings.Join(parts, ": ")
		}
	case method == "turn/completed" && isRoot:
		st.lastError = ""
	case isRoot && isCompactionItem(params) && (method == "item/started" || method == "item/completed"):
		st.compacting = method == "item/started"
	case method == "thread/compacted" && isRoot:
		st.compacting = false
	}
	if sub := withStartedChild(st.subagents, thread, params.obj("item")); sub != nil {
		st.subagents = sub
	}
	return st, cmd
}

func copyCounts(m map[string]int) map[string]int {
	out := make(map[string]int, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func spanID(p obj) string {
	if s, ok := p.str("turnId"); ok {
		return s
	}
	s, _ := p.obj("turn").str("id")
	return s
}

// ---- views --------------------------------------------------------------

func (st codexState) views(reporter, method string, p obj) []oarlock.Event {
	switch method {
	case "turn/started":
		return []oarlock.Event{{Kind: oarlock.EventTurnActive}}
	case "item/agentMessage/delta":
		d, ok := p.str("delta")
		if !ok {
			return nil
		}
		id, _ := p.str("itemId")
		return []oarlock.Event{{Kind: oarlock.EventTextDelta, Text: d, MessageID: id}}
	case "item/commandExecution/outputDelta":
		id, ok := p.str("itemId")
		if !ok {
			return nil
		}
		d, _ := p.str("delta")
		return []oarlock.Event{{Kind: oarlock.EventToolCallProgress, CallID: id, OutputDelta: d}}
	case "serverRequest/resolved":
		switch id := p["requestId"].(type) {
		case string:
			return []oarlock.Event{{Kind: oarlock.EventAppRequestCancelled, RequestID: id}}
		case float64:
			return []oarlock.Event{{Kind: oarlock.EventAppRequestCancelled, RequestID: formatNumber(id)}}
		}
		return nil
	case "thread/compacted":
		if st.compacting {
			return []oarlock.Event{{Kind: oarlock.EventCompactionEnded, CompactionResult: oarlock.CompactionCompleted}}
		}
		return nil
	case "rawResponseItem/completed":
		if ev, ok := reasoningEvent(p.obj("item")); ok {
			return []oarlock.Event{ev}
		}
		return nil
	case "item/started":
		item := p.obj("item")
		if t, _ := item.str("type"); t == "userMessage" {
			if content, ok := item["content"].([]any); ok {
				var b strings.Builder
				for _, part := range content {
					po := asObj(part)
					if pt, _ := po.str("type"); pt == "text" {
						s, _ := po.str("text")
						b.WriteString(s)
					}
				}
				ev := oarlock.Event{Kind: oarlock.EventUserMessage, Text: b.String(), Evidence: oarlock.EvidenceTurnItem}
				ev.InputID, _ = item.str("clientId")
				ev.NativeMessageID, _ = item.str("id")
				ev.TurnID, _ = p.str("turnId")
				return []oarlock.Event{ev}
			}
		}
		if isCompactionItem(p) {
			return []oarlock.Event{{Kind: oarlock.EventCompactionStarted}}
		}
		return toolViews(method, item)
	case "item/completed":
		if isCompactionItem(p) {
			return []oarlock.Event{{Kind: oarlock.EventCompactionEnded, CompactionResult: oarlock.CompactionCompleted}}
		}
		item := p.obj("item")
		if t, _ := item.str("type"); t == "subAgentActivity" {
			if st.aboutOwnChild(reporter, item) {
				return taskViews(item)
			}
			return nil
		}
		return toolViews(method, item)
	case "turn/completed":
		return []oarlock.Event{{Kind: oarlock.EventTurnEnded, Outcome: st.outcomeFromTurn(p.obj("turn"))}}
	case "thread/tokenUsage/updated":
		var base *oarlock.TokenTotals
		if reporter == st.root {
			if st.baseline.kind == baselineKnown {
				b := st.baseline.tokens
				base = &b
			}
		} else {
			base = &oarlock.TokenTotals{}
		}
		return usageViews(p, base)
	case "thread/settings/updated":
		return settingsViews(p)
	}
	return nil
}

func formatNumber(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func reasoningEvent(item obj) (oarlock.Event, bool) {
	if t, _ := item.str("type"); t != "reasoning" {
		return oarlock.Event{}, false
	}
	var parts []string
	for _, key := range []string{"summary", "content"} {
		for _, part := range item.arr(key) {
			if s, _ := asObj(part).str("text"); s != "" {
				parts = append(parts, s)
			}
		}
	}
	if len(parts) > 0 {
		return oarlock.Event{Kind: oarlock.EventReasoning, Text: strings.Join(parts, "\n")}, true
	}
	if _, ok := item.str("encrypted_content"); ok {
		return oarlock.Event{Kind: oarlock.EventReasoning, Redacted: true}, true
	}
	return oarlock.Event{Kind: oarlock.EventReasoning}, true
}

func (st codexState) outcomeFromTurn(turn obj) *oarlock.TurnOutcome {
	status, _ := turn.str("status")
	switch status {
	case "interrupted":
		return &oarlock.TurnOutcome{Kind: oarlock.OutcomeAborted}
	case "completed":
		return &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted}
	}
	word := status
	if word == "" {
		word = "unknown"
	}
	reason := word
	if st.lastError != "" {
		reason = word + ": " + st.lastError
	}
	return codexFailure(reason, turn["error"])
}

func toolViews(method string, item obj) []oarlock.Event {
	itemType, _ := item.str("type")
	if !toolItemTypes[itemType] {
		return nil
	}
	id, ok := item.str("id")
	if !ok {
		id = "unknown"
	}
	if method == "item/started" {
		ev := oarlock.Event{Kind: oarlock.EventToolCallStarted, CallID: id, Tool: itemType}
		ev.Input, _ = itemInput(item)
		return []oarlock.Event{ev}
	}
	ev := oarlock.Event{Kind: oarlock.EventToolCallEnded, CallID: id, Content: toolContent(item)}
	switch s, _ := item.str("status"); s {
	case "completed":
		ev.Result = oarlock.ToolOK
	case "failed":
		ev.Result = oarlock.ToolFailed
	}
	if itemType == "commandExecution" {
		if v, has := item["exitCode"]; has {
			ev.ExitCodeReported = true
			if f, isNum := v.(float64); isNum {
				c := int(f)
				ev.ExitCode = &c
			}
		}
	}
	return []oarlock.Event{ev}
}

func itemInput(item obj) (string, bool) {
	switch t, _ := item.str("type"); t {
	case "commandExecution":
		return item.str("command")
	case "fileChange":
		if c, ok := item["changes"].([]any); ok {
			b, _ := json.Marshal(c)
			return string(b), true
		}
	case "mcpToolCall":
		if v, has := item["arguments"]; has {
			b, _ := json.Marshal(v)
			return string(b), true
		}
	case "webSearch":
		return item.str("query")
	case "sleep":
		if d, ok := item.num("durationMs"); ok {
			b, _ := json.Marshal(map[string]float64{"durationMs": d})
			return string(b), true
		}
	}
	return "", false
}

func textPart(s string) []oarlock.ToolOutputPart {
	return []oarlock.ToolOutputPart{{Type: "text", Text: s}}
}

func toolContent(item obj) []oarlock.ToolOutputPart {
	status, _ := item.str("status")
	switch t, _ := item.str("type"); t {
	case "commandExecution":
		if out, _ := item.str("aggregatedOutput"); out != "" {
			return textPart(out)
		}
		if status != "" {
			return textPart(status)
		}
	case "fileChange":
		if status != "" {
			return textPart(status)
		}
	case "mcpToolCall":
		if msg, ok := item.obj("error").str("message"); ok {
			return textPart("error: " + msg)
		}
		res := item.obj("result")
		if res == nil {
			return nil
		}
		if blocks, ok := res["content"].([]any); ok {
			if len(blocks) == 0 {
				return nil
			}
			parts := make([]oarlock.ToolOutputPart, 0, len(blocks))
			for _, b := range blocks {
				parts = append(parts, outputPart(b))
			}
			return parts
		}
		raw, _ := json.Marshal(res)
		return []oarlock.ToolOutputPart{{Type: "other", Value: raw}}
	case "webSearch":
		if r, ok := item["results"].([]any); ok {
			raw, _ := json.Marshal(r)
			return []oarlock.ToolOutputPart{{Type: "other", Value: raw}}
		}
	}
	return nil
}

func outputPart(block any) oarlock.ToolOutputPart {
	o := asObj(block)
	switch t, _ := o.str("type"); t {
	case "text":
		if s, ok := o.str("text"); ok {
			return oarlock.ToolOutputPart{Type: "text", Text: s}
		}
	case "image":
		data, d := o.str("data")
		mt, m := o.str("mimeType")
		if d && m {
			return oarlock.ToolOutputPart{Type: "image", MediaType: mt, Data: data}
		}
	}
	raw, _ := json.Marshal(block)
	return oarlock.ToolOutputPart{Type: "other", Value: raw}
}

func settingsViews(p obj) []oarlock.Event {
	s := p.obj("threadSettings")
	events := tierEvents(s)
	if m, ok := s.str("model"); ok && m != "" {
		events = append(events, oarlock.Event{Kind: oarlock.EventModel, Model: m})
	}
	if e, ok := s.str("effort"); ok && e != "" {
		events = append(events, oarlock.Event{Kind: oarlock.EventEffort, Effort: e})
	}
	return events
}

// serviceTier: null explicitly means no tier ("default"); an absent field is no evidence.
func serviceTier(o obj) (string, bool) {
	v, has := o["serviceTier"]
	if !has {
		return "", false
	}
	if v == nil {
		return "default", true
	}
	s, ok := v.(string)
	return s, ok && s != ""
}

func tierEvents(o obj) []oarlock.Event {
	if t, ok := serviceTier(o); ok {
		return []oarlock.Event{{Kind: oarlock.EventServiceTier, ServiceTier: t}}
	}
	return nil
}

// ---- steer inputs ---------------------------------------------------------

func cloneInputs(in map[string]turnInputs) map[string]turnInputs {
	out := make(map[string]turnInputs, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneSet(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in)+1)
	for k := range in {
		out[k] = true
	}
	return out
}

// acceptSteer is called at the accepted turn/steer reply (on the pump, before
// later notifications): codex clears pending steering on abort, so the steers
// that never echoed are the ones an interrupt drops.
func acceptSteer(in map[string]turnInputs, turnID, inputID string) map[string]turnInputs {
	t := in[turnID]
	if t.echoed[inputID] {
		return in
	}
	out := cloneInputs(in)
	out[turnID] = turnInputs{steers: func() map[string]bool { s := cloneSet(t.steers); s[inputID] = true; return s }(), echoed: t.echoed}
	return out
}

func foldInputs(in map[string]turnInputs, method string, p obj) (map[string]turnInputs, []oarlock.Event) {
	turnID, ok := p.str("turnId")
	if !ok {
		turnID, ok = p.obj("turn").str("id")
	}
	if !ok {
		return in, nil
	}
	item := p.obj("item")
	if t, _ := item.str("type"); method == "item/started" && t == "userMessage" {
		if cid, has := item.str("clientId"); has {
			t0 := in[turnID]
			steers := cloneSet(t0.steers)
			delete(steers, cid)
			echoed := cloneSet(t0.echoed)
			echoed[cid] = true
			out := cloneInputs(in)
			out[turnID] = turnInputs{steers: steers, echoed: echoed}
			return out, nil
		}
		return in, nil
	}
	if method == "turn/completed" {
		var events []oarlock.Event
		if status, _ := p.obj("turn").str("status"); status == "interrupted" {
			ids := make([]string, 0, len(in[turnID].steers))
			for id := range in[turnID].steers {
				ids = append(ids, id)
			}
			sortStrings(ids)
			for _, id := range ids {
				events = append(events, oarlock.Event{Kind: oarlock.EventInputDropped, InputID: id, Dropped: oarlock.DroppedTurnInterrupted})
			}
		}
		out := cloneInputs(in)
		delete(out, turnID)
		return out, events
	}
	return in, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ---- sub-agents -----------------------------------------------------------

func taskViews(item obj) []oarlock.Event {
	id, _ := item.str("agentThreadId")
	if id == "" {
		return nil
	}
	task := &oarlock.TaskEvent{TaskID: id}
	switch kind, _ := item.str("kind"); kind {
	case "started":
		task.TaskType, task.NativeType = "agent", "subAgent"
		task.ChildSessionID = id
		task.Background = true
		task.Description, _ = item.str("agentPath")
		task.ToolCallID, _ = item.str("id")
		return []oarlock.Event{{Kind: oarlock.EventTaskStarted, Task: task}}
	case "interacted":
		task.Status = oarlock.TaskRunning
		return []oarlock.Event{{Kind: oarlock.EventTaskUpdated, Task: task}}
	case "completed":
		task.Status = oarlock.TaskCompleted
		return []oarlock.Event{{Kind: oarlock.EventTaskEnded, Task: task}}
	case "interrupted":
		task.Status = oarlock.TaskStopped
		return []oarlock.Event{{Kind: oarlock.EventTaskEnded, Task: task}}
	}
	return nil
}

// withStartedChild returns the subagent map plus the child a `started` item on
// reporter's thread names, or nil when nothing changes.
func withStartedChild(threads map[string]subagent, reporter string, item obj) map[string]subagent {
	if t, _ := item.str("type"); t != "subAgentActivity" {
		return nil
	}
	if k, _ := item.str("kind"); k != "started" {
		return nil
	}
	child, _ := item.str("agentThreadId")
	if child == "" {
		return nil
	}
	if _, known := threads[child]; known {
		return nil
	}
	entry := subagent{parent: reporter}
	if p, ok := item.str("agentPath"); ok {
		entry.path = p
	}
	out := make(map[string]subagent, len(threads)+1)
	for k, v := range threads {
		out[k] = v
	}
	out[child] = entry
	return out
}

func (st codexState) threadAt(path string) (string, bool) {
	if path == rootPath {
		return st.root, true
	}
	for id, t := range st.subagents {
		if t.path == path {
			return id, true
		}
	}
	return "", false
}

// childByPath judges by the parent path whether path names a child of reporter:
// decided when the parent path's thread is known, or when the reporter's own
// path is (then the parent is someone else); otherwise undecided.
func (st codexState) childByPath(reporter, path string) (bool, bool) {
	parent := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		parent = path[:i]
	}
	if id, ok := st.threadAt(parent); ok {
		return id == reporter, true
	}
	if reporter == st.root || st.subagents[reporter].path != "" {
		return false, true
	}
	return false, false
}

// aboutOwnChild: a thread reports subAgentActivity on its own thread for its
// own children and for the messages it sends to peers. Only an item about the
// reporting thread's own child is a task change or an edge.
func (st codexState) aboutOwnChild(reporter string, item obj) bool {
	kind, _ := item.str("kind")
	child, hasChild := item.str("agentThreadId")
	if kind == "started" || !hasChild {
		return kind == "started"
	}
	started, known := st.subagents[child]
	if child == st.root || known {
		return known && started.parent == reporter
	}
	if path, ok := item.str("agentPath"); ok {
		if own, decided := st.childByPath(reporter, path); decided {
			return own
		}
	}
	return true
}

type edge struct{ Parent, Child string }

// collabEdges are the edges a collaboration item establishes: the sender thread
// (the reporting thread unless the item names one) spawned or addressed the
// named threads.
func (st codexState) collabEdges(reporter string, item obj) []edge {
	t, _ := item.str("type")
	if !collabItemTypes[t] {
		return nil
	}
	if t == "subAgentActivity" && !st.aboutOwnChild(reporter, item) {
		return nil
	}
	parent, ok := item.str("senderThreadId")
	if !ok {
		parent = reporter
	}
	var candidates []any
	candidates = append(candidates, item.arr("receiverThreadIds")...)
	candidates = append(candidates, item["agentThreadId"])
	var out []edge
	seen := map[string]bool{}
	for _, c := range candidates {
		id, ok := c.(string)
		if !ok || id == "" || id == parent || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, edge{parent, id})
	}
	return out
}

// ---- token usage ------------------------------------------------------------

func reportedTotal(p obj) (oarlock.TokenTotals, bool) {
	total := p.obj("tokenUsage").obj("total")
	in, okIn := total.num("inputTokens")
	out, okOut := total.num("outputTokens")
	if total == nil || !okIn || !okOut {
		return oarlock.TokenTotals{}, false
	}
	t := oarlock.TokenTotals{Input: int64(in), Output: int64(out)}
	if v, ok := total.num("cachedInputTokens"); ok {
		x := int64(v)
		t.CacheRead = &x
	}
	if v, ok := total.num("cacheWriteInputTokens"); ok {
		x := int64(v)
		t.CacheWrite = &x
	}
	if v, ok := total.num("reasoningOutputTokens"); ok {
		x := int64(v)
		t.Reasoning = &x
	}
	return t, true
}

func nextBaseline(b tokenBaseline, method string, p obj) tokenBaseline {
	if b.kind != baselineAwaiting {
		return b
	}
	if method == "turn/started" {
		return tokenBaseline{kind: baselineUnknown}
	}
	if method == "thread/tokenUsage/updated" {
		if t, ok := reportedTotal(p); ok {
			return tokenBaseline{kind: baselineKnown, tokens: t}
		}
	}
	return b
}

func subPart(total, base *int64) *int64 {
	if total == nil {
		return nil
	}
	v := *total
	if base != nil {
		v -= *base
	}
	return &v
}

func subtractTokens(total, base oarlock.TokenTotals) oarlock.TokenTotals {
	return oarlock.TokenTotals{
		Input: total.Input - base.Input, Output: total.Output - base.Output,
		Reasoning: subPart(total.Reasoning, base.Reasoning),
		CacheRead: subPart(total.CacheRead, base.CacheRead), CacheWrite: subPart(total.CacheWrite, base.CacheWrite),
	}
}

// usageViews reads thread/tokenUsage/updated. total is codex's own running sum
// over the thread's life: less baseline it is this session's tokens (nil
// baseline: unknown, so no tokens). last/modelContextWindow are the context
// reading, whatever the baseline: last.totalTokens is codex's own occupancy
// figure. Without last the occupancy is unknown and the cumulative input stands
// in as tokens with no window and no percent.
func usageViews(p obj, base *oarlock.TokenTotals) []oarlock.Event {
	tu := p.obj("tokenUsage")
	total := tu.obj("total")
	if total == nil {
		return nil
	}
	rep := &oarlock.UsageReport{}
	if t, ok := reportedTotal(p); ok && base != nil {
		d := subtractTokens(t, *base)
		rep.Tokens = &d
	}
	last := tu.obj("last")
	if last == nil {
		in, ok := total.num("inputTokens")
		ctx := &oarlock.ContextUsage{}
		if ok {
			v := int64(in)
			ctx.Tokens = &v
		}
		rep.Context = ctx
		return []oarlock.Event{{Kind: oarlock.EventUsage, Usage: rep}}
	}
	ctx := &oarlock.ContextUsage{}
	ct, okT := last.num("totalTokens")
	if okT {
		v := int64(ct)
		ctx.Tokens = &v
	}
	if w, ok := tu.num("modelContextWindow"); ok {
		v := int64(w)
		ctx.ContextWindow = &v
		if okT && w > 0 {
			pc := math.Round(ct / w * 100)
			ctx.Percent = &pc
		}
	}
	rep.Context = ctx
	return []oarlock.Event{{Kind: oarlock.EventUsage, Usage: rep}}
}

// ---- failure ----------------------------------------------------------------

var namedFailures = map[string]oarlock.FailureClass{
	"modelNotFound":         oarlock.FailureModelUnavail,
	"usageLimitExceeded":    oarlock.FailureQuota,
	"rateLimitExceeded":     oarlock.FailureRateLimited,
	"serverOverloaded":      oarlock.FailureOverloaded,
	"internalServerError":   oarlock.FailureProvider,
	"contextWindowExceeded": oarlock.FailureInputTooLarge,
}

var transportVariants = []string{"httpConnectionFailed", "responseStreamConnectionFailed", "responseStreamDisconnected", "responseTooManyFailedAttempts"}

func failureFromStatus(s int) oarlock.FailureClass {
	switch {
	case s == 400:
		return oarlock.FailureInvalidRequest
	case s == 401:
		return oarlock.FailureAuth
	case s == 402:
		return oarlock.FailureBilling
	case s == 404:
		return oarlock.FailureModelUnavail
	case s == 429:
		return oarlock.FailureRateLimited
	case s == 503 || s == 529:
		return oarlock.FailureOverloaded
	case s >= 500 && s < 600:
		return oarlock.FailureProvider
	}
	return oarlock.FailureUnknown
}

var errorCodes = map[string]oarlock.FailureClass{
	"invalid_api_key": oarlock.FailureAuth, "model_not_found": oarlock.FailureModelUnavail,
	"rate_limit_exceeded": oarlock.FailureRateLimited, "insufficient_quota": oarlock.FailureQuota,
	"organization_usage_limit_exceeded": oarlock.FailureQuota, "usage_limit_reached": oarlock.FailureQuota,
	"usage_not_included": oarlock.FailureQuota, "credit_balance_exhausted": oarlock.FailureBilling,
	"context_length_exceeded": oarlock.FailureInputTooLarge, "server_is_overloaded": oarlock.FailureOverloaded,
	"authentication_error": oarlock.FailureAuth, "not_found_error": oarlock.FailureModelUnavail,
	"rate_limit_error": oarlock.FailureRateLimited, "billing_error": oarlock.FailureBilling,
	"overloaded_error": oarlock.FailureOverloaded, "api_error": oarlock.FailureProvider,
	"server_error": oarlock.FailureProvider, "invalid_request_error": oarlock.FailureInvalidRequest,
}

func failureFromErrorBody(text string) (oarlock.FailureClass, bool) {
	e := parseObj([]byte(text)).obj("error")
	if e == nil {
		return "", false
	}
	if code, ok := e.str("code"); ok {
		if c, ok := errorCodes[code]; ok {
			return c, true
		}
	}
	if t, ok := e.str("type"); ok {
		if c, ok := errorCodes[t]; ok {
			return c, true
		}
	}
	return "", false
}

// codexFailure classifies a failed turn from its own turn.error.codexErrorInfo:
// a named value, or a transport variant carrying the HTTP status. Values no run
// produced stay unknown; prose is never the branch word.
func codexFailure(reason string, turnError any) *oarlock.TurnOutcome {
	e := asObj(turnError)
	msg, _ := e.str("message")
	out := &oarlock.TurnOutcome{Kind: oarlock.OutcomeFailed, Reason: reason, Failure: oarlock.FailureUnknown}
	switch info := e["codexErrorInfo"].(type) {
	case string:
		if c, ok := namedFailures[info]; ok {
			out.Failure = c
		} else if info == "other" {
			if c, ok := failureFromErrorBody(msg); ok {
				out.Failure = c
			}
		}
	case map[string]any:
		for _, v := range transportVariants {
			if status, ok := asObj(info[v]).num("httpStatusCode"); ok {
				out.Status = int(status)
				out.Failure = failureFromStatus(int(status))
				break
			}
		}
	}
	return out
}
