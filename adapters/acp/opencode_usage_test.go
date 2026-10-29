package acp

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/zzjcool/oarlock"
)

// The pure half of the opencode usage ledger: one answer's `usage` object onto
// oarlock totals, and the projector billing only the answer that ends a turn.
// Numbers come from the live reconnaissance in opencode_usage.go.
func TestOpenCodePromptTokens(t *testing.T) {
	cases := []struct {
		name  string
		usage map[string]any // nil = no usage object at all
		want  *oarlock.TokenTotals
	}{
		{
			// turn 1 live: 6440 + 1664 cache reads = 8104 input.
			name:  "first turn counts cache as input",
			usage: map[string]any{"inputTokens": 6440, "outputTokens": 2, "totalTokens": 8106, "cachedReadTokens": 1664},
			want:  &oarlock.TokenTotals{Input: 8104, Output: 2, CacheRead: ptr(int64(1664))},
		},
		{
			// turn 2 live: per turn, not cumulative — 180 + 7936 = 8116.
			name:  "second turn is its own per-turn usage",
			usage: map[string]any{"inputTokens": 180, "outputTokens": 2, "totalTokens": 8118, "cachedReadTokens": 7936},
			want:  &oarlock.TokenTotals{Input: 8116, Output: 2, CacheRead: ptr(int64(7936))},
		},
		{
			// image live: 6462+1664 = 8126 input, 10+20 = 30 output,
			// reasoning a part of output.
			name:  "thinking and cache are parts of output and input",
			usage: map[string]any{"inputTokens": 6462, "outputTokens": 10, "thoughtTokens": 20, "cachedReadTokens": 1664, "totalTokens": 8156},
			want:  &oarlock.TokenTotals{Input: 8126, Output: 30, Reasoning: ptr(int64(20)), CacheRead: ptr(int64(1664))},
		},
		{
			name:  "cache write is input and reported",
			usage: map[string]any{"inputTokens": 100, "outputTokens": 5, "cachedWriteTokens": 7},
			want:  &oarlock.TokenTotals{Input: 107, Output: 5, CacheWrite: ptr(int64(7))},
		},
		{
			// totalTokens is per turn; 6462+1664+10+20 = 8156 already, so any
			// second reading of it would double count.
			name:  "totalTokens is never read",
			usage: map[string]any{"inputTokens": 100, "outputTokens": 5, "totalTokens": 999999},
			want:  &oarlock.TokenTotals{Input: 100, Output: 5},
		},
		{
			name:  "missing input is no ledger",
			usage: map[string]any{"outputTokens": 2, "totalTokens": 8106},
			want:  nil,
		},
		{
			name:  "missing output is no ledger",
			usage: map[string]any{"inputTokens": 6440, "totalTokens": 8106},
			want:  nil,
		},
		{
			name:  "non-numeric counts are no ledger",
			usage: map[string]any{"inputTokens": "6440", "outputTokens": 2},
			want:  nil,
		},
		{
			// A reported zero stays zero: the runtime said it, it was not guessed.
			name:  "reported zero stays zero",
			usage: map[string]any{"inputTokens": 0, "outputTokens": 0, "cachedReadTokens": 0},
			want:  &oarlock.TokenTotals{Input: 0, Output: 0, CacheRead: ptr(int64(0))},
		},
		{
			name:  "no usage object",
			usage: nil,
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var result map[string]any
			if tc.usage != nil {
				// Through JSON, as the runtime's answer reaches the projector:
				// numbers decode as float64, never as Go ints.
				raw, err := json.Marshal(map[string]any{"stopReason": "end_turn", "usage": tc.usage})
				if err != nil {
					t.Fatal(err)
				}
				result = decodeMap(raw)
			}
			if got := opencodePromptTokens(result); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("opencodePromptTokens = %+v, want %+v", got, tc.want)
			}
		})
	}
	if got := opencodePromptTokens(nil); got != nil {
		t.Fatalf("a nil result billed %+v", got)
	}
}

// openUsageAnswer builds the response frame opencode sends for a session/prompt
// RPC (the live shape: stopReason + top-level usage + empty _meta).
func openUsageAnswer(t *testing.T, usage map[string]any) wireMsg {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"result":  map[string]any{"stopReason": "end_turn", "usage": usage, "_meta": map[string]any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, ok := parseWire(line)
	if !ok {
		t.Fatalf("unparseable answer: %s", line)
	}
	return w
}

// TestOpenCodeProjectorUsageAccumulates: two turn-ending answers bill two
// running totals; the second one is the first plus the second turn.
func TestOpenCodeProjectorUsageAccumulates(t *testing.T) {
	p := newProjector(Profile{ID: "opencode", PromptTokens: opencodePromptTokens}, "sid")

	first := map[string]any{"inputTokens": 6440, "outputTokens": 2, "totalTokens": 8106, "cachedReadTokens": 1664}
	events := p.response(openUsageAnswer(t, first), "session/prompt", true, true)
	if len(events) != 2 || events[0].Kind != oarlock.EventUsage || events[1].Kind != oarlock.EventTurnEnded {
		t.Fatalf("first answer events = %+v, want usage then turn_ended", events)
	}
	want := &oarlock.TokenTotals{Input: 8104, Output: 2, CacheRead: ptr(int64(1664))}
	if got := events[0].Usage.Tokens; !reflect.DeepEqual(got, want) {
		t.Fatalf("first tokens = %+v, want %+v", got, want)
	}

	second := map[string]any{"inputTokens": 180, "outputTokens": 2, "totalTokens": 8118, "cachedReadTokens": 7936}
	events = p.response(openUsageAnswer(t, second), "session/prompt", true, true)
	if len(events) != 2 || events[0].Kind != oarlock.EventUsage || events[1].Kind != oarlock.EventTurnEnded {
		t.Fatalf("second answer events = %+v, want usage then turn_ended", events)
	}
	want = &oarlock.TokenTotals{Input: 8104 + 8116, Output: 4, CacheRead: ptr(int64(1664 + 7936))}
	if got := events[0].Usage.Tokens; !reflect.DeepEqual(got, want) {
		t.Fatalf("second tokens = %+v, want the accumulated %+v", got, want)
	}
	if !reflect.DeepEqual(p.billed, *want) {
		t.Fatalf("billed = %+v, want %+v", p.billed, *want)
	}
}

// TestOpenCodeProjectorSteerAnswerBillsNothing: opencode answers a steer prompt
// with the same usage as the running turn's answer. That answer does not end
// the turn, so it is a frame and nothing else — billing it would double the
// ledger; usage_update handling (context) is untouched.
func TestOpenCodeProjectorSteerAnswerBillsNothing(t *testing.T) {
	p := newProjector(Profile{ID: "opencode", PromptTokens: opencodePromptTokens}, "sid")

	// The turn's own answer bills once.
	turnUsage := map[string]any{"inputTokens": 6440, "outputTokens": 2, "cachedReadTokens": 1664}
	events := p.response(openUsageAnswer(t, turnUsage), "session/prompt", true, true)
	if len(events) != 2 || events[0].Kind != oarlock.EventUsage {
		t.Fatalf("turn answer events = %+v, want one usage", events)
	}
	billed := p.billed

	// The steer prompt's answer repeats the same usage and is not final.
	events = p.response(openUsageAnswer(t, turnUsage), "session/prompt", false, false)
	if len(events) != 0 {
		t.Fatalf("steer answer events = %+v, want none", events)
	}
	if !reflect.DeepEqual(p.billed, billed) {
		t.Fatalf("steer answer billed %+v, want the unchanged %+v", p.billed, billed)
	}
}
