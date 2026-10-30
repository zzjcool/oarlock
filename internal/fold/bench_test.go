package fold

import (
	"strconv"
	"testing"

	"github.com/zzjcool/oarlock"
)

// This file holds the issue #84 benchmarks: what a host pays to fold a whole
// stream once, to feed one more record into a live fold, and to answer the four
// queries for one UI refresh. Records come from the builders fold_test.go
// shares with the tests (mkPrompt, mkFrame, mkFrameAt, mkTokens, mkContextUsage,
// evTurnActive, evTurnEnded, …), so a benchmark measures the fold and not a
// second, private idea of what a record is. Nothing here reads a clock, a
// network or a random source: the same input folds the same stream on every run,
// which is what makes two runs comparable across commits.
//
// The sinks below keep the measured calls reachable: a result the loop only
// assigns to a local it overwrites can be discarded wholesale, and a benchmark
// of nothing is worse than no benchmark at all.

var (
	benchFoldSink    *Fold
	benchStatusSink  Status
	benchUsageSink   Usage
	benchContextSink *ContextReport
	benchMessageSink []Message
)

// benchStreamSize is the stream length the issue asks to characterise. 10k
// records is a long agent session: large enough that message growth and the
// per-message copies dominate per-call overhead, small enough to fold once per
// iteration.
const benchStreamSize = 10_000

// benchOfStream builds one deterministic n-record stream that feeds all four
// folds. Per turn it emits: a toRuntime prompt and its accepted response, a
// turn_active, a reasoning delta, two text deltas that merge into one message,
// a usage report carrying both the agent's running total and the context
// fullness (so usage and context both move), the full lifecycle of one tool call
// with a streamed argument prefix, the session_linked edge of a derived child
// session and that child reporting its own total, and the runtime's turn_ended.
//
// Seq is dense from 1 and the stream is cut at exactly n records, so the last
// turn may be left open — that is what a live stream looks like anyway. Every
// value is derived from the turn counter, never from the machine.
func benchOfStream(n int) []oarlock.Record {
	records := make([]oarlock.Record, 0, n)
	var seq uint64
	next := func() uint64 {
		seq++
		return seq
	}
	for turn := 0; len(records) < n; turn++ {
		tag := strconv.Itoa(turn)
		requestID := "req-" + tag
		messageID := "msg-a-" + tag
		callID := "call-" + tag

		records = append(records, mkPrompt(next(), requestID, "input-"+tag, "prompt "+tag))
		records = append(records, mkRespond(next(), requestID, mkAcceptedBody()))
		records = append(records, mkFrame(next(), evTurnActive()))
		records = append(records, mkFrame(next(), oarlock.Event{
			Kind:      oarlock.EventReasoning,
			Text:      "planning " + tag,
			MessageID: "msg-r-" + tag,
		}))
		records = append(records, mkFrame(next(), evTextDelta("working on "+tag, messageID)))
		records = append(records, mkFrame(next(), evTextDelta(" and done", messageID)))
		records = append(records, mkFrame(next(), oarlock.Event{
			Kind: oarlock.EventUsage,
			Usage: &oarlock.UsageReport{
				Tokens:  mkTokens(1200, 80),
				Context: mkContextUsage(9000, 200_000, 4.5),
			},
		}))
		records = append(records, mkFrame(next(), oarlock.Event{
			Kind:   oarlock.EventToolCallStarted,
			CallID: callID,
			Tool:   "bash",
		}))
		records = append(records, mkFrame(next(), oarlock.Event{
			Kind:   oarlock.EventToolCallInputDelta,
			CallID: callID,
			Delta:  `{"command":"ls`,
		}))
		records = append(records, mkFrame(next(), oarlock.Event{
			Kind:   oarlock.EventToolCallInput,
			CallID: callID,
			Input:  `{"command":"ls -la"}`,
		}))
		records = append(records, mkFrame(next(), oarlock.Event{
			Kind:        oarlock.EventToolCallProgress,
			CallID:      callID,
			OutputDelta: "total 0\n",
		}))
		records = append(records, mkFrame(next(), oarlock.Event{
			Kind:             oarlock.EventToolCallEnded,
			CallID:           callID,
			Result:           oarlock.ToolOK,
			Content:          []oarlock.ToolOutputPart{{Type: "text", Text: "total 0\n"}},
			ExitCode:         mkPtr(0),
			ExitCodeReported: true,
		}))
		records = append(records, mkFrame(next(), oarlock.Event{
			Kind:   oarlock.EventSessionLink,
			Parent: testRootSessionID,
			Child:  testChildSessionID,
			Via:    "task",
		}))
		records = append(records, mkFrameAt(next(), testChildSessionID, nil, oarlock.Event{
			Kind:  oarlock.EventUsage,
			Usage: &oarlock.UsageReport{Total: mkTokens(700, 60)},
		}))
		records = append(records, mkFrame(next(), evTurnEnded(oarlock.OutcomeCompleted)))
	}
	return records[:n]
}

// BenchmarkOf_10kRecords measures folding a whole 10k-record stream in one call:
// the cost a host pays to rebuild every answer from a retained log — resume,
// reconnect, or the first paint of a session nothing has folded yet.
//
// The stream is built once, before the timer, and every iteration folds the same
// records with Of. A fold that silently degenerated to a no-op (no message, no
// usage) would still be measured, and it would be the wrong thing to measure, so
// a probe outside the timed region pins that the stream really reaches all the
// folds.
func BenchmarkOf_10kRecords(b *testing.B) {
	records := benchOfStream(benchStreamSize)

	probe := Of(records)
	if got := len(probe.Conversation().Value); got == 0 {
		b.Fatalf("stream of %d records folded to no message", len(records))
	}
	if probe.Usage().Value.Total == nil {
		b.Fatalf("stream of %d records folded to no usage total", len(records))
	}
	if probe.ContextUsage().Value == nil {
		b.Fatalf("stream of %d records folded to no context report", len(records))
	}
	benchFoldSink = probe

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchFoldSink = Of(records)
	}
}

// BenchmarkFeed_SteadyState measures the per-record incremental cost a live pump
// pays: one New() fold, one recycled frame per iteration with a fresh monotonic
// Seq, so the number is what one more delivered record adds to a fold that is
// already warm.
//
// The frame is deliberately shaped so the fold's state stays bounded — every
// part of it replaces what is already there instead of appending:
//
//   - turn_active keeps the running span (a running status keeps its start, its
//     request and its input; only the phase is reset);
//   - the usage report overwrites the one root agent's running total and the one
//     context report, and a usage report is a running total, never one to add to
//     the previous;
//   - tool_call_input rewrites the single message that (agent path, call id)
//     already owns; it does not append to the message's text the way a
//     text_delta would, and the call id never changes, so no message is added.
//
// So memory is O(1) in the iteration count: the benchmark measures the steady
// per-record cost of a stream that keeps arriving, which is exactly what a long
// session looks like once the transcript has stopped growing between deltas.
func BenchmarkFeed_SteadyState(b *testing.B) {
	record := oarlock.Record{
		Kind:      oarlock.KindFrame,
		SessionID: testRootSessionID,
		Events: []oarlock.Event{
			evTurnActive(),
			{
				Kind: oarlock.EventUsage,
				Usage: &oarlock.UsageReport{
					Tokens:  mkTokens(1200, 80),
					Context: mkContextUsage(9000, 200_000, 4.5),
				},
			},
			{
				Kind:   oarlock.EventToolCallInput,
				CallID: "call-steady",
				Input:  `{"command":"ls -la"}`,
			},
		},
	}
	f := New()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		record.Seq = uint64(i + 1)
		f.Feed(record)
	}
	b.StopTimer()

	benchFoldSink = f
	if b.N == 0 {
		return
	}
	// The boundedness the doc comment promises, asserted rather than assumed:
	// repeated feeds of the same call id fold into the same one message, and the
	// usage report did land. A check that fails here means the benchmark stopped
	// measuring steady state and started measuring unbounded growth.
	if got := len(f.Conversation().Value); got != 1 {
		b.Fatalf("%d feeds produced %d messages, want 1", b.N, got)
	}
	if f.Usage().Value.Total == nil {
		b.Fatalf("%d feeds produced no usage total", b.N)
	}
}

// BenchmarkQueries_10k measures one UI refresh against a fold that already
// exists: Status, Usage, ContextUsage and Conversation, all four per iteration.
//
// This is what a host pays per read, not per record. Every answer is a fresh
// copy — Status clones its turn outcome, Usage clones each token total it hands
// out (plus the by-agent and with-children breakdowns), ContextUsage clones its
// numbers, and Conversation clones every message together with its agent path,
// tool output parts and exit code — which is why the query cost on a long
// transcript is worth pinning: a host that re-reads the fold instead of caching
// its own snapshot pays all of it on every frame, and a caller can never reach
// back into the fold through an answer.
func BenchmarkQueries_10k(b *testing.B) {
	records := benchOfStream(benchStreamSize)

	f := Of(records)
	if got := len(f.Conversation().Value); got == 0 {
		b.Fatalf("fold built from %d records has no message", len(records))
	}
	// The most expensive answer: the stream links a derived child session that
	// reports its own total, so Usage() also walks the session graph and sums
	// WithChildren. Without this the benchmark would silently measure the cheap
	// case only.
	if f.Usage().Value.WithChildren == nil {
		b.Fatalf("fold built from %d records has no child session total", len(records))
	}
	benchFoldSink = f

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchStatusSink = f.Status().Value
		benchUsageSink = f.Usage().Value
		benchContextSink = f.ContextUsage().Value
		benchMessageSink = f.Conversation().Value
	}
}
