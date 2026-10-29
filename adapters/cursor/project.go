package cursor

// The Cursor projection: the vendor half of the shared stream-json fold
// (internal/streamjson, issue #22). cursor-agent speaks almost the shared
// dialect, so the three ways it differs are stated here as Profile extensions
// instead of forking a parser:
//
//   - top-level `thinking` (subtype delta/completed) and `tool_call` (subtype
//     started/completed) frames are the vendor's own vocabulary: declared in
//     Types() so the fold never tallies them as unknown, and read by
//     VendorFrame. This is the MUL-5231 lesson — cursor's reasoning and tool
//     calls are TOP-LEVEL frames, and a type nobody declared would be dropped
//     silently;
//   - assistant frames are cumulative snapshots WITHOUT message ids, so the
//     vendor projects content itself (content.go, ContentReader);
//   - result.usage is camelCase, read here as the turn's Tokens.
//
// Every frame the fold does not understand stays in Frame.Rec (the record's
// Native) and is tallied by Folder.Unknown(); nothing here invents an event for
// it, and no branch below reads vendor prose.

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/zzjcool/oarlock"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

// profile is cursor's half of the fold (sj.Profile + sj.VendorTypes +
// sj.FrameReader + sj.ContentReader). Hooks run under the Folder's lock; the
// mutex here is for readers that arrive from outside the pump (Session.Unknown
// may copy the subtype tally while a frame is being folded).
type profile struct {
	mu      sync.Mutex
	lanes   map[string]string // assistant text already emitted, per lane (content.go)
	unknown map[string]int    // "<type>/<subtype>" of a known vendor frame we do not read
}

// newProfile is the profile of one stream: cursor's fold state, empty.
func newProfile() *profile {
	return &profile{lanes: map[string]string{}, unknown: map[string]int{}}
}

var (
	_ sj.Profile       = (*profile)(nil)
	_ sj.VendorTypes   = (*profile)(nil)
	_ sj.ContentReader = (*profile)(nil)
	_ sj.FrameReader   = (*profile)(nil)
)

// Types is cursor's own top-level vocabulary (sj.VendorTypes): the fold treats
// these as known — they are never tallied as unknown-and-dropped — and hands
// each to VendorFrame. A type NOT listed here is still tallied by the folder
// and kept in Native, which is the signal the fixture test pins.
func (p *profile) Types() []string { return []string{"thinking", "tool_call"} }

// Assistant sees every assistant frame; cursor's frames state content only
// (projected in Content), no turn facts, so there is nothing to keep.
func (p *profile) Assistant(m sj.Rec, root bool) {}

// Init is a new turn: the split assistant stream starts with no text emitted.
// The fold itself reads the model out of system/init.
func (p *profile) Init(m sj.Rec, root bool) []oarlock.Event {
	if root {
		p.resetLanes()
	}
	return nil
}

// Compacted: cursor does not compact; the fold's compaction_end still stands.
func (p *profile) Compacted(root bool) {}

// System sees system frames other than init/compact_boundary. cursor states
// none we read, so they stay in Native.
func (p *profile) System(m sj.Rec, path []string) []oarlock.Event { return nil }

// Other sees every frame of a type the fold does not read itself (an unknown
// top-level type, rate_limit_event, ...). The folder already tallies the type
// and keeps the frame in Native; counting it again here would double-report.
func (p *profile) Other(m sj.Rec) {}

// ---- vendor frames: thinking and tool_call --------------------------------------

// VendorFrame is sj.FrameReader: cursor's own top-level frames. The fold calls
// it for every frame it does not handle itself, so unknown types reach this
// too — they are the folder's tally, not ours, and produce nothing.
func (p *profile) VendorFrame(m sj.Rec, path []string) []oarlock.Event {
	typ, sub := sj.Str(m["type"]), sj.Str(m["subtype"])
	switch typ {
	case "thinking":
		switch sub {
		case "delta":
			if text := sj.Str(m["text"]); text != "" {
				return []oarlock.Event{{Kind: oarlock.EventReasoning, Text: text}}
			}
			return nil
		case "completed":
			// Closes the reasoning block; the frame states no content of its own.
			return nil
		}
	case "tool_call":
		switch sub {
		case "started":
			call := readToolCall(m)
			return []oarlock.Event{{
				Kind:   oarlock.EventToolCallStarted,
				CallID: call.id,
				Tool:   call.tool,
				Input:  compactArgs(call.args),
			}}
		case "completed":
			return readToolCall(m).endedEvents()
		}
	default:
		return nil
	}
	// A thinking/tool_call subtype nobody declared: never absorb it silently
	// (MUL-5231). No event — the frame stays in Native — but the tally makes it
	// visible to the adapter and its tests.
	p.tallySubtype(typ, sub)
	return nil
}

func (p *profile) tallySubtype(typ, sub string) {
	key := typ
	if sub != "" {
		key += "/" + sub
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unknown[key]++
}

// unknownSubtypes is the tally of known-type frames whose subtype is not read
// (Session.Unknown merges it with the folder's top-level tally).
func (p *profile) unknownSubtypes() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.unknown))
	for k, v := range p.unknown {
		out[k] = v
	}
	return out
}

// toolCall is one `tool_call` frame projected onto what the record needs. The
// id and the tool may be absent when the payload cannot be decoded; the events
// are still emitted so started/completed stay paired (a call row survives).
type toolCall struct {
	id     string // call_id, first line
	tool   string // the `<name>ToolCall` key minus the suffix
	args   any    // that key's `args`, as stated
	result sj.Rec // that key's `result`, nil when absent
}

// toolCallSuffix is how cursor names the per-tool payload: the tool is the key
// of a nested object (`shellToolCall`), not a `name` field.
const toolCallSuffix = "ToolCall"

// readToolCall extracts identity, arguments and result from a tool_call frame:
//
//	{"type":"tool_call","subtype":"started","call_id":"call-…\nfc_…",
//	 "tool_call":{"shellToolCall":{"args":{"command":"echo hi"}},"toolCallId":"call-…"}}
func readToolCall(m sj.Rec) toolCall {
	envelope := sj.AsRec(m["tool_call"])
	call := toolCall{id: firstLine(sj.Str(m["call_id"]))}
	if call.id == "" {
		call.id = firstLine(sj.Str(envelope["toolCallId"]))
	}
	name, payload := toolPayload(envelope)
	call.tool = name
	if payload != nil {
		call.args = payload["args"]
		call.result = sj.AsRec(payload["result"])
	}
	return call
}

// toolPayload picks the `<name>ToolCall` key of a tool_call envelope. Observed
// envelopes carry exactly one; sorting keeps the choice deterministic if a
// future CLI emits more.
func toolPayload(envelope sj.Rec) (name string, payload sj.Rec) {
	key := ""
	for k := range envelope {
		if len(k) > len(toolCallSuffix) && strings.HasSuffix(k, toolCallSuffix) && (key == "" || k < key) {
			key = k
		}
	}
	if key == "" {
		return "", nil
	}
	return strings.TrimSuffix(key, toolCallSuffix), sj.AsRec(envelope[key])
}

// endedEvents is the completion of one tool call: the tool's own result, its
// output and — for a command — the exit code it reported.
func (call toolCall) endedEvents() []oarlock.Event {
	ev := oarlock.Event{Kind: oarlock.EventToolCallEnded, CallID: call.id, Tool: call.tool}
	if call.result != nil {
		if _, ok := call.result["success"]; ok {
			// Cursor names the successful payload `success`; any other key is
			// its failure shape (`failure`, `error`, ...). The presence of the
			// key is the runtime's own statement — never inferred from output.
			ev.Result = oarlock.ToolOK
			success := sj.AsRec(call.result["success"])
			ev.Content = outputParts(success)
			if code, ok := sj.AsNum(success["exitCode"]); ok {
				n := int(code)
				ev.ExitCode, ev.ExitCodeReported = &n, true
			}
		} else {
			ev.Result = oarlock.ToolFailed
		}
	}
	return []oarlock.Event{ev}
}

// outputParts is the tool's output in the runtime's order: stdout, then stderr
// when the tool wrote one. An empty or absent field states no output, so no
// empty part is invented for it.
func outputParts(r sj.Rec) []oarlock.ToolOutputPart {
	var parts []oarlock.ToolOutputPart
	if stdout := sj.Str(r["stdout"]); stdout != "" {
		parts = append(parts, oarlock.ToolOutputPart{Type: "text", Text: stdout})
	}
	if stderr := sj.Str(r["stderr"]); stderr != "" {
		parts = append(parts, oarlock.ToolOutputPart{Type: "text", Text: stderr})
	}
	return parts
}

// firstLine is the call id without the second id cursor packs after a newline
// (`call-…\nfc_…`): one call, one id.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// compactArgs is a decoded value as one line of JSON. json.Marshal sorts object
// keys, so the projection is deterministic even though the frame's own key
// order is lost in decoding.
func compactArgs(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// ---- result: outcome and usage --------------------------------------------------

// Result is the end of the turn: the outcome (aborted wins; then a failed turn;
// else completed) and, after turn_ended, the tokens the result states.
func (p *profile) Result(m sj.Rec, path []string, aborted bool) (oarlock.TurnOutcome, []oarlock.Event, []oarlock.Event) {
	p.resetLanes()
	outcome := p.outcome(m, aborted)
	var after []oarlock.Event
	if ev, ok := usageEvent(m); ok {
		after = []oarlock.Event{ev}
	}
	return outcome, nil, after
}

func (p *profile) outcome(m sj.Rec, aborted bool) oarlock.TurnOutcome {
	if aborted {
		// A requested abort ends the turn aborted whatever the result says.
		return oarlock.TurnOutcome{Kind: oarlock.OutcomeAborted}
	}
	isErr, _ := m["is_error"].(bool)
	sub := sj.Str(m["subtype"])
	if !isErr && sub != "error" {
		return oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted}
	}
	status := 0
	if v, ok := sj.AsNum(m["api_error_status"]); ok {
		status = int(v)
	}
	return oarlock.TurnOutcome{
		Kind:    oarlock.OutcomeFailed,
		Failure: classifyFailure(m, status),
		Reason:  failureReason(m, sub),
		Status:  status,
	}
}

// failureReason is the prose of a failed turn, kept in Reason only: the class is
// decided by the structured signals in classifyFailure, never by these words.
func failureReason(m sj.Rec, subtype string) string {
	for _, s := range []string{sj.Str(m["result"]), sj.Str(m["error"]), sj.Str(m["detail"])} {
		if s != "" {
			return s
		}
	}
	if subtype != "" {
		return subtype
	}
	return "error"
}

// failureStatus maps the HTTP status a failed request carried. The classes are
// the frozen cursor set (auth, quota, rate limit, model unavailable, input too
// large, overloaded); a status nobody named stays unknown.
var failureStatus = map[int]oarlock.FailureClass{
	401: oarlock.FailureAuth,
	403: oarlock.FailureAuth,
	402: oarlock.FailureQuota,
	404: oarlock.FailureModelUnavail,
	413: oarlock.FailureInputTooLarge,
	429: oarlock.FailureRateLimited,
	503: oarlock.FailureOverloaded,
	529: oarlock.FailureOverloaded,
}

// failureCodes are the structured error tokens a failed result may name (an
// error's own `type`/`code`, or a top-level `error_type`). Exact matches only:
// a token that is not in the table is unknown, not guessed.
var failureCodes = map[string]oarlock.FailureClass{
	"authentication_error":    oarlock.FailureAuth,
	"invalid_api_key":         oarlock.FailureAuth,
	"unauthorized":            oarlock.FailureAuth,
	"insufficient_quota":      oarlock.FailureQuota,
	"quota_exceeded":          oarlock.FailureQuota,
	"usage_limit_exceeded":    oarlock.FailureQuota,
	"rate_limit_error":        oarlock.FailureRateLimited,
	"rate_limit_exceeded":     oarlock.FailureRateLimited,
	"model_not_found":         oarlock.FailureModelUnavail,
	"model_unavailable":       oarlock.FailureModelUnavail,
	"context_length_exceeded": oarlock.FailureInputTooLarge,
	"input_too_large":         oarlock.FailureInputTooLarge,
	"request_too_large":       oarlock.FailureInputTooLarge,
	"overloaded_error":        oarlock.FailureOverloaded,
	"overloaded":              oarlock.FailureOverloaded,
	"server_is_overloaded":    oarlock.FailureOverloaded,
	"server_overloaded":       oarlock.FailureOverloaded,
}

// classifyFailure decides a failed turn from structure only. No recorded cursor
// error frame exists yet (testdata/cursor carries two success traces), so every
// shape nobody has seen stays unknown — the honest answer — and the vendor's
// prose lives in Reason.
func classifyFailure(m sj.Rec, status int) oarlock.FailureClass {
	if c, ok := failureStatus[status]; ok {
		return c
	}
	if c, ok := failureCodes[sj.Str(m["error_type"])]; ok {
		return c
	}
	if e := sj.AsRec(m["error"]); e != nil {
		if c, ok := failureCodes[sj.Str(e["type"])]; ok {
			return c
		}
		if c, ok := failureCodes[sj.Str(e["code"])]; ok {
			return c
		}
	}
	return oarlock.FailureUnknown
}

// usageEvent reads result.usage {inputTokens, outputTokens, cacheReadTokens,
// cacheWriteTokens} as this turn's Tokens. A count the frame does not state is
// not stated: the cache pointers stay nil when their key is absent, and a
// reported zero stays a pointer to zero. No usage key at all → no event.
func usageEvent(m sj.Rec) (oarlock.Event, bool) {
	usage := sj.AsRec(m["usage"])
	if usage == nil {
		return oarlock.Event{}, false
	}
	var tok oarlock.TokenTotals
	stated := false
	if v, ok := sj.AsNum(usage["inputTokens"]); ok {
		tok.Input, stated = int64(v), true
	}
	if v, ok := sj.AsNum(usage["outputTokens"]); ok {
		tok.Output, stated = int64(v), true
	}
	if v, ok := sj.AsNum(usage["cacheReadTokens"]); ok {
		n := int64(v)
		tok.CacheRead, stated = &n, true
	}
	if v, ok := sj.AsNum(usage["cacheWriteTokens"]); ok {
		n := int64(v)
		tok.CacheWrite, stated = &n, true
	}
	if !stated {
		return oarlock.Event{}, false
	}
	return oarlock.Event{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{Tokens: &tok}}, true
}

// Project is the pure replay entry point: it folds raw native frames, in order,
// through the same profile and Folder the live pump uses, so an offline replay
// and a live session cannot disagree (fixture_test.go pins both fixtures).
func Project(frames [][]byte) []sj.Frame {
	f := sj.NewFolder(newProfile())
	out := make([]sj.Frame, 0, len(frames))
	for _, raw := range frames {
		out = append(out, f.Fold(raw))
	}
	return out
}
