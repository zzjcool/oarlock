package fold

import (
	"testing"

	"github.com/zzjcool/oarlock"
)

// mkContextUsage is a runtime-reported context fullness. Every number is
// reported here; a nil field would mean "not reported", never zero.
func mkContextUsage(tokens, window int64, percent float64) *oarlock.ContextUsage {
	return &oarlock.ContextUsage{
		Tokens:        mkPtr(tokens),
		ContextWindow: mkPtr(window),
		Percent:       mkPtr(percent),
	}
}

// TestEmptyStreamContextIsNil pins issue #7's empty-stream acceptance
// criterion for the context fold: nothing reported means nil (never a guessed
// zero) and Seq 0.
func TestEmptyStreamContextIsNil(t *testing.T) {
	for _, records := range [][]oarlock.Record{nil, {}} {
		got := ContextUsageOf(records)
		if got.Value != nil {
			t.Fatalf("ContextUsage on an empty stream = %+v, want nil", got.Value)
		}
		if got.Seq != 0 {
			t.Fatalf("ContextUsage Seq on an empty stream = %d, want 0", got.Seq)
		}
	}

	// A stream that never reports fullness is the same nil, not an empty
	// report: a usage frame without Context carries no fullness.
	noFullness := []oarlock.Record{
		mkFrame(1, oarlock.Event{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{Tokens: mkTokens(10, 2)}}),
		mkFrame(2, evTextDelta("hi", "m-1")),
	}
	if got := ContextUsageOf(noFullness); got.Value != nil {
		t.Fatalf("ContextUsage without any Context report = %+v, want nil", got.Value)
	}
}

// TestContextUsageIsLatestReportWithReportingSeq pins issue #7's
// "ContextUsage() = latest reported value + the Seq it was reported at": the
// answer carries the latest fullness and the record Seq that reported it, not
// the Seq of the query.
func TestContextUsageIsLatestReportWithReportingSeq(t *testing.T) {
	records := []oarlock.Record{
		mkFrame(1, oarlock.Event{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{Context: mkContextUsage(1000, 200000, 0.5)}}),
		mkFrame(2, evTextDelta("working", "m-1")),
		mkFrame(3, oarlock.Event{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{Context: mkContextUsage(2500, 200000, 1.25)}}),
		// A later usage frame without Context must not clear the report.
		mkFrame(4, oarlock.Event{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{Tokens: mkTokens(9, 2)}}),
	}

	got := ContextUsageOf(records)
	if got.Value == nil {
		t.Fatal("ContextUsage = nil, want the latest reported fullness")
	}
	if got.Value.Seq != 3 {
		t.Fatalf("ContextReport.Seq = %d, want 3 (the reporting record)", got.Value.Seq)
	}
	if got.Seq != 4 {
		t.Fatalf("QueryResult.Seq = %d, want 4 (the last consumed record)", got.Seq)
	}
	if got.Value.Usage.Tokens == nil || *got.Value.Usage.Tokens != 2500 {
		t.Fatalf("ContextUsage.Tokens = %v, want the latest report 2500", got.Value.Usage.Tokens)
	}
	if got.Value.Usage.ContextWindow == nil || *got.Value.Usage.ContextWindow != 200000 {
		t.Fatalf("ContextUsage.ContextWindow = %v, want 200000", got.Value.Usage.ContextWindow)
	}
	if got.Value.Usage.Percent == nil || *got.Value.Usage.Percent != 1.25 {
		t.Fatalf("ContextUsage.Percent = %v, want 1.25", got.Value.Usage.Percent)
	}
}

// TestContextUsageQuerySeqIsMonotonic pins issue #7's "every answer carries a
// Seq" alignment rule for this fold: QueryResult.Seq never decreases across
// feeds and always equals the last consumed record, independent of when the
// context was actually reported.
func TestContextUsageQuerySeqIsMonotonic(t *testing.T) {
	records := []oarlock.Record{
		mkFrame(1, oarlock.Event{Kind: oarlock.EventUserMessage, Text: "go", MessageID: "u-1"}),
		mkFrame(2, oarlock.Event{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{Context: mkContextUsage(1000, 200000, 0.5)}}),
		mkFrame(3, evTextDelta("working", "m-1")),
		mkFrame(4, oarlock.Event{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{Context: mkContextUsage(2000, 200000, 1.0)}}),
		mkFrame(5, evTurnEnded(oarlock.OutcomeCompleted)),
	}

	f := New()
	var last uint64
	for _, record := range records {
		f.Feed(record)
		got := f.ContextUsage()
		if got.Seq < last {
			t.Fatalf("QueryResult.Seq went backwards at record %d: %d after %d", record.Seq, got.Seq, last)
		}
		if got.Seq != record.Seq {
			t.Fatalf("QueryResult.Seq after feeding record %d = %d, want the last consumed record", record.Seq, got.Seq)
		}
		last = got.Seq
	}

	// Before the first report the answer is nil, but the query Seq already
	// tracks the stream.
	beforeReport := ContextUsageOf(records[:1])
	if beforeReport.Seq != 1 {
		t.Fatalf("QueryResult.Seq without a report = %d, want 1", beforeReport.Seq)
	}
	if beforeReport.Value != nil {
		t.Fatalf("ContextUsage without a report = %+v, want nil", beforeReport.Value)
	}
	if final := f.ContextUsage(); final.Value == nil || final.Value.Seq != 4 {
		t.Fatalf("final ContextReport = %+v, want the report from record 4", final.Value)
	}
}

// TestChildAgentContextIsIgnored pins issue #7's ContextUsage criterion (the
// latest reported value plus the Seq that reported it) together with the fixed
// root scope: only the root agent's report answers for the root, never a
// sub-agent's or a child session's.
func TestChildAgentContextIsIgnored(t *testing.T) {
	records := []oarlock.Record{
		mkFrame(1, oarlock.Event{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{Context: mkContextUsage(1000, 200000, 0.5)}}),
		// Child agent inside the root session.
		mkFrameAt(2, testRootSessionID, []string{"sub"}, oarlock.Event{
			Kind:  oarlock.EventUsage,
			Usage: &oarlock.UsageReport{Context: mkContextUsage(9000, 200000, 4.5)},
		}),
		// Derived child session.
		mkFrameAt(3, testChildSessionID, nil, oarlock.Event{
			Kind:  oarlock.EventUsage,
			Usage: &oarlock.UsageReport{Context: mkContextUsage(5555, 200000, 2.8)},
		}),
	}

	got := ContextUsageOf(records)
	if got.Value == nil {
		t.Fatal("ContextUsage = nil, want the root agent's report")
	}
	if got.Value.Seq != 1 {
		t.Fatalf("ContextReport.Seq = %d, want 1 (the root report)", got.Value.Seq)
	}
	if got.Value.Usage.Tokens == nil || *got.Value.Usage.Tokens != 1000 {
		t.Fatalf("ContextUsage.Tokens = %v, want the root agent's 1000", got.Value.Usage.Tokens)
	}

	// The same rule from the other side: a stream whose only fullness belongs
	// to children has no root report at all.
	childrenOnly := ContextUsageOf(records[1:])
	if childrenOnly.Value != nil {
		t.Fatalf("ContextUsage for child-only reports = %+v, want nil", childrenOnly.Value)
	}
}
