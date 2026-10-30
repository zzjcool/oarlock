package streamjson

// Benchmarks for the shared fold (issue #84): what one big frame costs to parse
// and project, what a realistic per-turn stream costs through a fresh folder
// (the per-process state a live pump pays), and the decode step on its own so a
// regression can be attributed to decoding rather than projection.
//
// Every frame is generated in code from fixed constants: no fixture files and no
// randomness, so two runs of a benchmark fold byte-identical input.

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// benchLargeTextBytes is the size of the single text block the large frame
// carries: 128 KiB, a full Read of a large file (issue #84's threshold).
const benchLargeTextBytes = 128 << 10

// benchText returns a deterministic ASCII text of exactly n bytes, cycling one
// fixed line: ASCII only, so the byte length is the character count and nothing
// in it needs JSON escaping beyond its newlines.
func benchText(n int) string {
	const line = "the fold reads one frame at a time and states only what that frame says\n"
	var b strings.Builder
	b.Grow(n + len(line))
	for b.Len() < n {
		b.WriteString(line)
	}
	return b.String()[:n]
}

// benchAssistantTextFrame wraps text in the smallest assistant frame the
// projector reads: one message with one text content block, the shape claude
// sends once for a completed block.
func benchAssistantTextFrame(text string) ([]byte, error) {
	quoted, err := json.Marshal(text)
	if err != nil {
		return nil, err
	}
	const head = `{"type":"assistant","message":{"id":"msg_bench_large","role":"assistant",` +
		`"usage":{"input_tokens":1200,"output_tokens":30000},"content":[{"type":"text","text":`
	const tail = `}]}}`
	frame := make([]byte, 0, len(head)+len(quoted)+len(tail))
	frame = append(frame, head...)
	frame = append(frame, quoted...)
	return append(frame, tail...), nil
}

// benchLargeFrame builds the one big assistant frame both large-frame
// benchmarks read, plus the text it carries, and fails the benchmark if
// generation did not reach the size the bench is supposed to measure.
func benchLargeFrame(tb testing.TB) (frame []byte, text string) {
	tb.Helper()
	text = benchText(benchLargeTextBytes)
	if len(text) < benchLargeTextBytes {
		tb.Fatalf("generated text = %d bytes, want >= %d", len(text), benchLargeTextBytes)
	}
	frame, err := benchAssistantTextFrame(text)
	if err != nil {
		tb.Fatalf("build large frame: %v", err)
	}
	return frame, text
}

// BenchmarkFold_LargeFrame measures the full fold of one assistant frame
// carrying a single >=128 KiB text block: decode the frame, then project its
// content into events. The folder is reused across iterations; a completed text
// block leaves no partial-message state behind, so folder state stays flat.
func BenchmarkFold_LargeFrame(b *testing.B) {
	frame, text := benchLargeFrame(b)
	// Outside the timer: the frame must reach the projector whole, or the bench
	// would be measuring a no-op.
	if ev := NewFolder(&nullProfile{}).Fold(frame).Events; len(ev) != 1 || len(ev[0].Text) != len(text) {
		got := 0
		if len(ev) > 0 {
			got = len(ev[0].Text)
		}
		b.Fatalf("projection = %d events (first %d bytes), want one text event of %d bytes",
			len(ev), got, len(text))
	}

	f := NewFolder(&nullProfile{})
	b.SetBytes(int64(len(frame)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.Fold(frame)
	}
}

// BenchmarkFold_MultiFrame measures folding one realistic ~1000-frame stream
// (init, a text block in deltas and then completed, a tool call and its result,
// a result frame with usage) through a fresh folder per iteration, so folder
// construction — the per-process state a live pump allocates — is included.
func BenchmarkFold_MultiFrame(b *testing.B) {
	frames := benchStream()
	framesPerOp := len(frames)
	// Outside the timer: the stream must reach the fold's own branches and be
	// entirely known vocabulary, or the bench would time frame dispatch alone.
	check := NewFolder(&nullProfile{})
	var stated, turns int
	for _, raw := range frames {
		fr := check.Fold(raw)
		if len(fr.Events) > 0 {
			stated++
		}
		if fr.TurnEnded {
			turns++
		}
	}
	if stated == 0 || turns == 0 {
		b.Fatalf("stream states nothing: %d frames with events, %d turns ended", stated, turns)
	}
	if unknown := check.Unknown(); len(unknown) != 0 {
		b.Fatalf("stream carries frames the fold does not know: %v", unknown)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f := NewFolder(&nullProfile{})
		for _, raw := range frames {
			f.Fold(raw)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(framesPerOp), "frames/op")
}

// BenchmarkParseRec measures the decode step alone — ParseRec of the same large
// frame the fold bench uses — so a regression can be attributed to decoding
// rather than to content projection.
func BenchmarkParseRec(b *testing.B) {
	frame, _ := benchLargeFrame(b)
	if rec := ParseRec(frame); Str(rec["type"]) != "assistant" {
		b.Fatalf("ParseRec = %v, want an assistant frame", rec)
	}

	b.SetBytes(int64(len(frame)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ParseRec(frame)
	}
}

// benchStream returns the deterministic stream BenchmarkFold_MultiFrame folds:
// 27 turns of what a real claude session sends — a system/init, a text block
// deltas then completed, a tool_use block deltas then completed, the user's
// tool_result, and a result frame with usage. 999 frames, built once.
func benchStream() [][]byte {
	const (
		turns     = 27
		deltasPer = 25
	)
	frames := make([][]byte, 0, turns*(deltasPer+12))
	for turn := 0; turn < turns; turn++ {
		msgID := "msg_bench_" + strconv.Itoa(turn)
		callID := "toolu_bench_" + strconv.Itoa(turn)
		var full strings.Builder
		frames = append(frames,
			[]byte(`{"type":"system","subtype":"init","session_id":"bench-session","model":"claude-bench-1",`+
				`"tools":["Read","Write"],"cwd":"/repo","permissionMode":"default","apiKeySource":"none"}`),
			[]byte(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"`+msgID+
				`","role":"assistant","content":[],"usage":{"input_tokens":1200,"output_tokens":0}}}}`),
		)
		for d := 0; d < deltasPer; d++ {
			delta := benchDelta(d)
			full.WriteString(delta)
			frames = append(frames, []byte(`{"type":"stream_event","event":{"type":"content_block_delta",`+
				`"index":0,"delta":{"type":"text_delta","text":"`+delta+`"}}}`))
		}
		frames = append(frames,
			// The text block ends, then arrives completed: the deltas already said
			// it, so the projector emits only what they did not carry.
			[]byte(`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`),
			[]byte(`{"type":"assistant","message":{"id":"`+msgID+`","role":"assistant",`+
				`"content":[{"type":"text","text":"`+full.String()+`"}]}}`),
			// A tool call: announced, streamed as partial JSON, then completed.
			[]byte(`{"type":"stream_event","event":{"type":"content_block_start","index":1,`+
				`"content_block":{"type":"tool_use","id":"`+callID+`","name":"Read"}}}`),
			[]byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":1,`+
				`"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"/repo/a.go\"}"}}}`),
			[]byte(`{"type":"stream_event","event":{"type":"content_block_stop","index":1}}`),
			[]byte(`{"type":"assistant","message":{"id":"`+msgID+`","role":"assistant","content":[`+
				`{"type":"tool_use","id":"`+callID+`","name":"Read","input":{"file_path":"/repo/a.go"}}]}}`),
			// The runtime answers the call.
			[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result",`+
				`"tool_use_id":"`+callID+`","content":[{"type":"text","text":"package bench\n"}]}]}}`),
			[]byte(`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"},`+
				`"usage":{"output_tokens":42}}}`),
			[]byte(`{"type":"stream_event","event":{"type":"message_stop"}}`),
			[]byte(`{"type":"result","subtype":"success","is_error":false,"duration_ms":1200,"num_turns":1,`+
				`"result":"done","session_id":"bench-session","usage":{"input_tokens":1200,"output_tokens":42,`+
				`"cache_read_input_tokens":1100,"cache_creation_input_tokens":100},"total_cost_usd":0.0123}`),
		)
	}
	return frames
}

// benchDelta is one text delta of the multi-frame stream: fixed words numbered
// by position, ASCII only, nothing that needs JSON escaping.
func benchDelta(i int) string {
	return "chunk " + strconv.Itoa(i) + " of the reply the fold accumulates "
}
