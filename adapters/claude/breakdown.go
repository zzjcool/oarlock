package claude

// ContextBreakdownReader: Claude's own itemization of its context window
// (control request get_context_usage, detail "full"). It is Claude-only — Pi has
// no such surface — so it is its own interface, never a Capabilities flag.
//
// The answer is read now and never recorded: it carries local paths (memory
// files) and the request id is private (privatePrefix), so the control_response
// is consumed by the pump. Concurrent reads share the one in flight. Each read
// costs claude a batch of count_tokens calls, so a host reads on demand.
//
// oarlock.ContextBreakdown carries categories (name, tokens) in claude's order
// and words; ParseContextBreakdown also returns the totals and the per-entry
// itemization the shared type has no field for.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zzjcool/oarlock"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

// BreakdownTimeout is how long claude gets to answer (it answered in 0.5 s,
// mid-turn too, on 2.1.292).
const BreakdownTimeout = 10 * time.Second

// BreakdownKind is claude's category kind in oarlock's words.
type BreakdownKind string

const (
	KindUsed     BreakdownKind = "used"
	KindDeferred BreakdownKind = "deferred"
	KindReserved BreakdownKind = "reserved" // claude: buffer
	KindFree     BreakdownKind = "free"
	KindUnknown  BreakdownKind = "unknown"
)

var breakdownKinds = map[string]BreakdownKind{"used": KindUsed, "deferred": KindDeferred, "buffer": KindReserved, "free": KindFree}

// BreakdownItem is one entry itemizing a category.
type BreakdownItem struct {
	Name   string
	Tokens int64
}

// BreakdownCategory is one native category with its itemization.
type BreakdownCategory struct {
	Name   string
	Tokens int64
	Kind   BreakdownKind
	Items  []BreakdownItem
}

// Breakdown is the full reading: totals, window and categories.
type Breakdown struct {
	Tokens        int64
	ContextWindow int64
	Categories    []BreakdownCategory
}

const (
	mcpLoaded   = "MCP tools"
	mcpDeferred = "MCP tools (deferred)"
)

func item(name, tokens any) []BreakdownItem {
	n, ok1 := sj.AsStr(name)
	t, ok2 := sj.AsNum(tokens)
	if !ok1 || !ok2 {
		return nil
	}
	return []BreakdownItem{{Name: n, Tokens: int64(t)}}
}

// mcpItems puts claude's MCP tools under the category its answer has. isLoaded
// does not follow the category on 2.1.292 (with tool search off every tool says
// isLoaded:false under the one MCP category), so it splits them only when both
// categories are present.
func mcpItems(payload sj.Rec, names map[string]bool) map[string][]BreakdownItem {
	tools := sj.AsRecList(payload["mcpTools"])
	listed := func(keep func(sj.Rec) bool) []BreakdownItem {
		var out []BreakdownItem
		for _, t := range tools {
			if keep(t) {
				out = append(out, item(t["name"], t["tokens"])...)
			}
		}
		return out
	}
	loaded := func(t sj.Rec) bool { l, _ := t["isLoaded"].(bool); return l }
	if names[mcpLoaded] && names[mcpDeferred] {
		return map[string][]BreakdownItem{
			mcpLoaded:   listed(loaded),
			mcpDeferred: listed(func(t sj.Rec) bool { return !loaded(t) }),
		}
	}
	only := mcpDeferred
	if names[mcpLoaded] {
		only = mcpLoaded
	}
	return map[string][]BreakdownItem{only: listed(func(sj.Rec) bool { return true })}
}

// ParseContextBreakdown reads claude's get_context_usage (detail "full") answer:
// its categories in its order and words, with the lists that itemize them. An
// answer without totalTokens or maxTokens is an error, not an empty breakdown.
func ParseContextBreakdown(payload sj.Rec) (*Breakdown, error) {
	tokens, ok1 := sj.AsNum(payload["totalTokens"])
	window, ok2 := sj.AsNum(payload["maxTokens"])
	if !ok1 || !ok2 {
		return nil, errors.New("claude's get_context_usage answer has no totalTokens or maxTokens")
	}
	native := sj.AsRecList(payload["categories"])
	names := map[string]bool{}
	for _, c := range native {
		if n, ok := sj.AsStr(c["name"]); ok {
			names[n] = true
		}
	}
	items := map[string][]BreakdownItem{"Memory files": nil, "Skills": nil, "Custom agents": nil}
	for _, f := range sj.AsRecList(payload["memoryFiles"]) {
		items["Memory files"] = append(items["Memory files"], item(f["path"], f["tokens"])...)
	}
	for _, s := range sj.AsRecList(sj.AsRec(payload["skills"])["skillFrontmatter"]) {
		items["Skills"] = append(items["Skills"], item(s["name"], s["tokens"])...)
	}
	for _, a := range sj.AsRecList(payload["agents"]) {
		items["Custom agents"] = append(items["Custom agents"], item(a["agentType"], a["tokens"])...)
	}
	for k, v := range mcpItems(payload, names) {
		items[k] = v
	}
	out := &Breakdown{Tokens: int64(tokens), ContextWindow: int64(window)}
	for _, c := range native {
		name, okN := sj.AsStr(c["name"])
		t, okT := sj.AsNum(c["tokens"])
		if !okN || !okT {
			continue
		}
		kind := KindUnknown
		if k, ok := sj.AsStr(c["kind"]); ok {
			if mapped, known := breakdownKinds[k]; known {
				kind = mapped
			}
		}
		out.Categories = append(out.Categories, BreakdownCategory{Name: name, Tokens: int64(t), Kind: kind, Items: items[name]})
	}
	return out, nil
}

// breakdownCall is one read in flight; concurrent callers wait on it.
type breakdownCall struct {
	done chan struct{}
	out  *Breakdown
	err  error
}

// ContextBreakdown asks claude now and returns its categories (name, tokens) in
// its order and words. It records nothing. A session that is disposing, exited
// or whose stdin was closed by the abort take-over answers (nil, nil): there is
// nobody to ask. claude's refusal is an error carrying its words.
func (s *Session) ContextBreakdown(ctx context.Context) (*oarlock.ContextBreakdown, error) {
	full, err := s.ReadContextBreakdown(ctx)
	if err != nil || full == nil {
		return nil, err
	}
	out := &oarlock.ContextBreakdown{}
	for _, c := range full.Categories {
		out.Categories = append(out.Categories, oarlock.ContextCategory{Name: c.Name, Tokens: c.Tokens})
	}
	return out, nil
}

// ReadContextBreakdown is ContextBreakdown with the totals, kinds and items the
// shared oarlock type has no field for. (nil, nil) means nobody to ask.
func (s *Session) ReadContextBreakdown(ctx context.Context) (*Breakdown, error) {
	s.mu.Lock()
	if s.goneForRead {
		s.mu.Unlock()
		return nil, nil
	}
	if call := s.breakdown; call != nil {
		s.mu.Unlock()
		return s.awaitBreakdown(ctx, call)
	}
	call := &breakdownCall{done: make(chan struct{})}
	s.breakdown = call
	s.mu.Unlock()

	// The read runs detached from the first caller's ctx: the others share it.
	go func() {
		defer func() {
			s.mu.Lock()
			s.breakdown = nil
			s.mu.Unlock()
			close(call.done)
		}()
		id := s.nextPrivateID("context")
		rec, err := s.request(context.Background(), id, map[string]any{"subtype": "get_context_usage", "detail": "full"}, BreakdownTimeout)
		if err != nil {
			if errors.Is(err, sj.ErrExited) {
				return // nobody to ask: (nil, nil)
			}
			call.err = err
			return
		}
		resp := sj.AsRec(rec["response"])
		if sj.Str(resp["subtype"]) != "success" {
			msg := sj.Str(resp["error"])
			if msg == "" {
				msg = "error"
			}
			call.err = fmt.Errorf("claude did not answer get_context_usage: %s", msg)
			return
		}
		call.out, call.err = ParseContextBreakdown(sj.AsRec(resp["response"]))
	}()
	return s.awaitBreakdown(ctx, call)
}

func (s *Session) awaitBreakdown(ctx context.Context, call *breakdownCall) (*Breakdown, error) {
	select {
	case <-call.done:
		return call.out, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
