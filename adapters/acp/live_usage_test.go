package acp

// live_usage_test.go — the one usage claim only a real opencode can prove: two
// ordinary prompts bill two per-turn `usage` objects and the folded total is
// their sum, not a session cumulative re-read. Opt-in through liveAgentOrSkip
// (OAR_ACP_LIVE=1 plus an OpenAI-compatible endpoint); otherwise it skips.
// The key never enters a record, a fixture or a log.

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

// liveUsageAnswerTotals applies the same mapping as opencodePromptTokens to
// one answer's raw `usage` object, computed here from the wire numbers so the
// live test checks the production mapping against what opencode sent rather
// than only against itself.
func liveUsageAnswerTotals(usage map[string]any) *oarlock.TokenTotals {
	in, ok1 := asInt64(usage["inputTokens"])
	out, ok2 := asInt64(usage["outputTokens"])
	if !ok1 || !ok2 {
		return nil
	}
	t := &oarlock.TokenTotals{Input: in, Output: out}
	if v, ok := asInt64(usage["cachedReadTokens"]); ok {
		t.CacheRead = &v
		t.Input += v
	}
	if v, ok := asInt64(usage["cachedWriteTokens"]); ok {
		t.CacheWrite = &v
		t.Input += v
	}
	if v, ok := asInt64(usage["thoughtTokens"]); ok {
		t.Reasoning = &v
		t.Output += v
	}
	return t
}

// TestLiveOpenCodeUsage: two prompts, then the ledger. opencode reports usage
// per turn (never cumulative), so the answer of each turn is billed once and
// the fold's total is the sum of the two.
func TestLiveOpenCodeUsage(t *testing.T) {
	a := liveAgentOrSkip(t)
	ctx := liveCtx(t)
	s := openLive(t, a, oarlock.Options{})

	for _, reply := range []string{"one", "two"} {
		out, err := s.Prompt(ctx, "Reply with exactly: "+reply, oarlock.PromptOptions{})
		if err != nil || out.Kind != oarlock.ControlAccepted {
			t.Fatalf("prompt %q = %+v, %v", reply, out, err)
		}
		waitLiveStatus(t, s, fold.StatusIdle, 2*time.Minute)
	}
	recs := records(s)
	assertDense(t, recs)

	// Every answer that carries a usage object, mapped by the same rules.
	var answers []*oarlock.TokenTotals
	for _, r := range recs {
		if r.Kind != oarlock.KindFrame {
			continue
		}
		var frame struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(r.Native, &frame); err != nil {
			continue // a notification is a frame too; only answers carry result
		}
		usage := asMap(frame.Result["usage"])
		if usage == nil {
			continue
		}
		answer := liveUsageAnswerTotals(usage)
		if answer == nil {
			t.Fatalf("an answer carries a usage object oarlock cannot bill: %s", r.Native)
		}
		if got := opencodePromptTokens(frame.Result); !reflect.DeepEqual(got, answer) {
			t.Fatalf("opencodePromptTokens = %+v, the wire numbers map to %+v: %s", got, answer, r.Native)
		}
		answers = append(answers, answer)
	}
	if len(answers) != 2 {
		t.Fatalf("answers carrying usage = %d, want one per turn (2)\n%s", len(answers), dump(recs))
	}
	want := addTotals(*answers[0], *answers[1])

	total := fold.UsageOf(recs).Value.Total
	if total == nil {
		t.Fatalf("the two live answers folded to no total\n%s", dump(recs))
	}
	if !reflect.DeepEqual(*total, want) {
		t.Fatalf("folded total = %+v, want the sum of the two answers %+v\n%s", *total, want, dump(recs))
	}

	// One billed usage event per turn (usage_update frames are context only),
	// the last one carrying the running cumulative total.
	var billed []*oarlock.TokenTotals
	for _, ev := range events(recs, oarlock.EventUsage) {
		if ev.Usage != nil && ev.Usage.Tokens != nil {
			billed = append(billed, ev.Usage.Tokens)
		}
	}
	if len(billed) != 2 {
		t.Fatalf("usage events carrying tokens = %d, want one per turn (2)\n%s", len(billed), dump(recs))
	}
	last := billed[len(billed)-1]
	if last.Input != want.Input || last.Output != want.Output {
		t.Fatalf("last usage event = %+v, want the cumulative %+v", last, want)
	}
	t.Logf("usage: turn1=%+v turn2=%+v cumulative=%+v", *answers[0], *answers[1], want)
}
