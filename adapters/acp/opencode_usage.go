package acp

import "github.com/zzjcool/oarlock"

// opencodePromptTokens maps one opencode session/prompt answer's top-level
// `usage` object onto oarlock's totals.
//
// opencode 1.19.5 reports usage per turn, not per session, and its
// inputTokens excludes the cache. Live (2026-10-29, third-party
// OpenAI-compatible endpoint, one session):
//
//	turn 1: {"inputTokens":6440,"outputTokens":2,"totalTokens":8106,"cachedReadTokens":1664}
//	turn 2: {"inputTokens":180,"outputTokens":2,"totalTokens":8118,"cachedReadTokens":7936}
//	image:  {"inputTokens":6462,"outputTokens":10,"thoughtTokens":20,"cachedReadTokens":1664,"totalTokens":8156}
//
// The image turn pins the vendor rule: 6462+10+20+1664 = 8156, so
// totalTokens = inputTokens + outputTokens + thoughtTokens + cachedReadTokens.
// Turn 2 pins the shape: the second turn's input is its own 180 (the 7936
// cache reads are separate), i.e. the object is per turn, never cumulative.
//
// oarlock's TokenTotals contract counts cache as part of input and reasoning
// as part of output, so the reported parts are folded in here once and the
// reported values are kept separately. totalTokens is per turn and would
// double count what Input/Output already carry, so it is never read.
func opencodePromptTokens(result map[string]any) *oarlock.TokenTotals {
	u := asMap(result["usage"])
	in, ok1 := asInt64(u["inputTokens"])
	out, ok2 := asInt64(u["outputTokens"])
	if !ok1 || !ok2 {
		return nil // both or nothing: a half ledger would invent a zero
	}
	t := &oarlock.TokenTotals{Input: in, Output: out}
	if v, ok := asInt64(u["cachedReadTokens"]); ok {
		t.CacheRead = &v
		t.Input += v
	}
	if v, ok := asInt64(u["cachedWriteTokens"]); ok {
		t.CacheWrite = &v
		t.Input += v
	}
	if v, ok := asInt64(u["thoughtTokens"]); ok {
		t.Reasoning = &v
		t.Output += v
	}
	return t
}
