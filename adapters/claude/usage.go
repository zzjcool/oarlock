package claude

// claude's token readings (OAR runtimes/claude/token-usage.ts, context-usage.ts).
//
// Two overlapping native views, one authority each:
//
//   - result.usage is the MAIN AGENT LOOP ONLY (claude's schema: "excludes Task
//     subagent, sidechain, and auxiliary model calls"). It is per turn, so the
//     root agent's running figure is the sum of the results seen. It is the
//     per-agent Tokens of the usage event.
//   - result.modelUsage is every model call of the session (sub-agents,
//     sidechains, compaction included), cumulative across turns: each result
//     carries the running total, so the latest is read, never summed. It is the
//     session Total of the usage event.
//
// A sub-agent therefore gets no per-agent entry: only the first assistant frame
// of a sub-agent carries parent_tool_use_id and its message.usage is the
// stream-start value, not final. Its spend stays in Total and the fold reports
// it as Unattributed (Total minus the attributed root).
//
// A new session's running total starts at zero. A resumed one continues the
// previous process's, so before the first turn the adapter asks claude
// get_usage and counts from that baseline; without a baseline the share is
// unknown and nothing is reported ("don't know, don't report").

import (
	"github.com/zzjcool/oarlock"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

func ptr(v int64) *int64 { return &v }

func addPart(a, b *int64) *int64 {
	if a == nil && b == nil {
		return nil
	}
	var s int64
	if a != nil {
		s += *a
	}
	if b != nil {
		s += *b
	}
	return &s
}

func addTokens(a, b oarlock.TokenTotals) oarlock.TokenTotals {
	return oarlock.TokenTotals{
		Input:      a.Input + b.Input,
		Output:     a.Output + b.Output,
		Reasoning:  addPart(a.Reasoning, b.Reasoning),
		CacheRead:  addPart(a.CacheRead, b.CacheRead),
		CacheWrite: addPart(a.CacheWrite, b.CacheWrite),
	}
}

func subTokens(total, base oarlock.TokenTotals) oarlock.TokenTotals {
	out := oarlock.TokenTotals{Input: total.Input - base.Input, Output: total.Output - base.Output}
	sub := func(t, b *int64) *int64 {
		if t == nil {
			return nil
		}
		v := *t
		if b != nil {
			v -= *b
		}
		return &v
	}
	out.Reasoning = sub(total.Reasoning, base.Reasoning)
	out.CacheRead = sub(total.CacheRead, base.CacheRead)
	out.CacheWrite = sub(total.CacheWrite, base.CacheWrite)
	return out
}

// cacheParts reads the cache parts a native usage record reports; a field the
// record lacks stays absent, never a guessed 0.
func cacheParts(u sj.Rec, read, write string) (r, w *int64) {
	if v, ok := sj.AsNum(u[read]); ok {
		r = ptr(int64(v))
	}
	if v, ok := sj.AsNum(u[write]); ok {
		w = ptr(int64(v))
	}
	return
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// mainLoop is one result's main-loop tokens (result.usage), nil when the frame
// carries no usage. input_tokens excludes cache reads and writes, so input
// counts them back in; each also stands as its own part.
func mainLoop(result sj.Rec) *oarlock.TokenTotals {
	u := sj.AsRec(result["usage"])
	if u == nil {
		return nil
	}
	r, w := cacheParts(u, "cache_read_input_tokens", "cache_creation_input_tokens")
	in, _ := sj.AsNum(u["input_tokens"])
	out, _ := sj.AsNum(u["output_tokens"])
	return &oarlock.TokenTotals{Input: int64(in) + deref(r) + deref(w), Output: int64(out), CacheRead: r, CacheWrite: w}
}

// modelTotals is a modelUsage (or get_usage model_usage) map read per model;
// ok is false when it is not one or an entry lacks its counts.
func modelTotals(v any) (map[string]oarlock.TokenTotals, bool) {
	m := sj.AsRec(v)
	if m == nil {
		return nil, false
	}
	out := make(map[string]oarlock.TokenTotals, len(m))
	for model, raw := range m {
		e := sj.AsRec(raw)
		in, ok1 := sj.AsNum(e["inputTokens"])
		o, ok2 := sj.AsNum(e["outputTokens"])
		if e == nil || !ok1 || !ok2 {
			return nil, false
		}
		r, w := cacheParts(e, "cacheReadInputTokens", "cacheCreationInputTokens")
		out[model] = oarlock.TokenTotals{Input: int64(in) + deref(r) + deref(w), Output: int64(o), CacheRead: r, CacheWrite: w}
	}
	return out, true
}

type baselineKind int

const (
	baselineKnown baselineKind = iota
	baselineAwaiting
	baselineUnknown
)

// modelUsage is claude's running total, by model, against this session's share.
type modelUsage struct {
	kind     baselineKind
	baseline map[string]oarlock.TokenTotals
	last     map[string]oarlock.TokenTotals // the running total last reported: a lower one is a reset
	carried  oarlock.TokenTotals            // the share counted before claude last reset (a /clear)
}

func freshModelUsage() modelUsage { return modelUsage{kind: baselineKnown} }

func resumedModelUsage() modelUsage { return modelUsage{kind: baselineAwaiting} }

// withBaseline sets where a resumed session's share starts. models == nil is an
// unknown baseline.
func (u *modelUsage) withBaseline(models map[string]oarlock.TokenTotals, known bool) {
	if !known {
		u.kind = baselineUnknown
		return
	}
	u.kind, u.baseline, u.last = baselineKnown, models, models
}

// usageBaseline reads a get_usage control_response's session.model_usage.
func usageBaseline(answer sj.Rec) (map[string]oarlock.TokenTotals, bool) {
	resp := sj.AsRec(answer["response"])
	if sj.Str(resp["subtype"]) != "success" {
		return nil, false
	}
	session := sj.AsRec(sj.AsRec(resp["response"])["session"])
	if session == nil {
		return nil, false
	}
	return modelTotals(session["model_usage"])
}

func shareOf(models, baseline map[string]oarlock.TokenTotals) oarlock.TokenTotals {
	var share oarlock.TokenTotals
	for model, t := range models {
		if b, ok := baseline[model]; ok {
			t = subTokens(t, b)
		}
		share = addTokens(share, t)
	}
	return share
}

func wentBack(models, last map[string]oarlock.TokenTotals) bool {
	for model, before := range last {
		now, ok := models[model]
		if !ok || now.Input < before.Input || now.Output < before.Output {
			return true
		}
	}
	return false
}

// fold reads one result's modelUsage: this session's total so far, nil when it
// is unknown (a resume without a baseline) or the frame says nothing (no
// modelUsage, or the zeroed one of a crash or startup-error result, which must
// not move the total back). A total below the last one is claude resetting its
// running total (a mid-session /clear): the share counted so far is kept and
// counting restarts from zero.
func (u *modelUsage) fold(result sj.Rec) *oarlock.TokenTotals {
	models, ok := modelTotals(result["modelUsage"])
	if u.kind != baselineKnown || !ok {
		return nil
	}
	reported := shareOf(models, nil)
	if reported.Input == 0 && reported.Output == 0 {
		return nil
	}
	base := u.baseline
	if wentBack(models, u.last) {
		u.carried = addTokens(u.carried, shareOf(u.last, u.baseline))
		base = nil
	}
	u.baseline, u.last = base, models
	total := addTokens(u.carried, shareOf(models, base))
	return &total
}

// ---- context -----------------------------------------------------------------

// What the context holds is what the root agent's LATEST model call sent: its
// assistant frame's usage.{input,cache_read,cache_creation}_tokens (output is
// left out: the frame's output_tokens is the stream's start). Not the result's
// usage: claude sums that over every main-loop call of the turn, so a turn of N
// tool round trips would read about N times the context.
type contextKind int

const (
	contextUnchanged contextKind = iota // no root call and no compaction since the last result
	contextSent                         // the latest root call sent tokens
	contextCompacted                    // compacted after the latest call: unknown until the next one
)

type contextReading struct {
	kind   contextKind
	tokens int64
	model  string
}

const syntheticModel = "<synthetic>"

func (c *contextReading) sent(assistant sj.Rec) {
	body := sj.AsRec(assistant["message"])
	usage := sj.AsRec(body["usage"])
	model := sj.Str(body["model"])
	if usage == nil || model == syntheticModel {
		return
	}
	var tokens int64
	for _, k := range []string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		if v, ok := sj.AsNum(usage[k]); ok {
			tokens += int64(v)
		}
	}
	if model == "" {
		model = c.model
	}
	*c = contextReading{kind: contextSent, tokens: tokens, model: model}
}

func (c *contextReading) compacted() { *c = contextReading{kind: contextCompacted, model: c.model} }

// windowOf is result.modelUsage[model].contextWindow for the latest call's
// model, else the first entry's (Go map order is random, so the smallest key
// keeps it deterministic).
func windowOf(result sj.Rec, model string) (int64, bool) {
	mu := sj.AsRec(result["modelUsage"])
	if mu == nil {
		return 0, false
	}
	entry := sj.AsRec(mu[model])
	if entry == nil {
		first := ""
		for k := range mu {
			if first == "" || k < first {
				first = k
			}
		}
		entry = sj.AsRec(mu[first])
	}
	w, ok := sj.AsNum(entry["contextWindow"])
	return int64(w), ok
}

// atResult reads the turn's context at its result, nil when nothing changed it,
// and starts the reading over for the next turn.
func (c *contextReading) atResult(result sj.Rec) *oarlock.ContextUsage {
	prev := *c
	*c = contextReading{kind: contextUnchanged, model: prev.model}
	if prev.kind == contextUnchanged {
		return nil
	}
	out := &oarlock.ContextUsage{}
	window, hasWindow := windowOf(result, prev.model)
	if hasWindow {
		out.ContextWindow = ptr(window)
	}
	if prev.kind == contextSent {
		out.Tokens = ptr(prev.tokens)
		if hasWindow && window != 0 {
			pct := float64(int64(float64(prev.tokens)/float64(window)*100 + 0.5))
			out.Percent = &pct
		}
	}
	return out
}
