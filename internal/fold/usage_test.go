package fold

import (
	"slices"
	"testing"

	"github.com/zzjcool/oarlock"
)

// mkUsageFrame builds a root-agent usage frame in the root session.
func mkUsageFrame(seq uint64, report oarlock.UsageReport) oarlock.Record {
	return mkUsageFrameAt(seq, testRootSessionID, nil, report)
}

// mkUsageFrameAt builds a usage frame for an explicit session and agent path.
// A nil agentPath is the root agent.
func mkUsageFrameAt(seq uint64, sessionID string, agentPath []string, report oarlock.UsageReport) oarlock.Record {
	return mkFrameAt(seq, sessionID, agentPath, oarlock.Event{Kind: oarlock.EventUsage, Usage: &report})
}

// derefInt64 treats a nil optional count as zero: the usage invariant is
// component-wise arithmetic where "not reported" counts as zero.
func derefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// componentsOf flattens a token total into its five frozen components in the
// order input, output, reasoning, cacheRead, cacheWrite.
func componentsOf(totals *oarlock.TokenTotals) [5]int64 {
	if totals == nil {
		return [5]int64{}
	}
	return [5]int64{
		totals.Input,
		totals.Output,
		derefInt64(totals.Reasoning),
		derefInt64(totals.CacheRead),
		derefInt64(totals.CacheWrite),
	}
}

// addComponents adds two component vectors, absent parts counting as zero.
func addComponents(a, b [5]int64) [5]int64 {
	var out [5]int64
	for i := range out {
		out[i] = a[i] + b[i]
	}
	return out
}

// sumAgents is the component-wise sum of every reported agent figure.
func sumAgents(entries []AgentUsage) [5]int64 {
	var out [5]int64
	for _, entry := range entries {
		out = addComponents(out, componentsOf(&entry.Tokens))
	}
	return out
}

// agentEntry finds the ByAgent entry for one agent path. An empty path is the
// root agent; nil and [] are the same path.
func agentEntry(entries []AgentUsage, path ...string) (oarlock.TokenTotals, bool) {
	for _, entry := range entries {
		if slices.Equal(entry.AgentPath, path) {
			return entry.Tokens, true
		}
	}
	return oarlock.TokenTotals{}, false
}

// TestEmptyStreamUsageIsNil pins issue #7's empty-stream acceptance criterion
// for the usage ledger: nothing reported is nil, never a guessed zero.
func TestEmptyStreamUsageIsNil(t *testing.T) {
	for _, records := range [][]oarlock.Record{nil, {}} {
		got := UsageOf(records)
		if got.Value.Total != nil {
			t.Fatalf("Usage.Total on an empty stream = %+v, want nil", got.Value.Total)
		}
		if got.Value.ByAgent != nil {
			t.Fatalf("Usage.ByAgent on an empty stream = %+v, want nil", got.Value.ByAgent)
		}
		if got.Value.Unattributed != nil {
			t.Fatalf("Usage.Unattributed on an empty stream = %+v, want nil", got.Value.Unattributed)
		}
		if got.Value.WithChildren != nil {
			t.Fatalf("Usage.WithChildren on an empty stream = %+v, want nil", got.Value.WithChildren)
		}
		if got.Seq != 0 {
			t.Fatalf("Usage Seq on an empty stream = %d, want 0", got.Seq)
		}
	}

	// A stream that never reports usage is the same nil: a turn that says
	// nothing about tokens must not invent a zero total.
	noUsage := []oarlock.Record{
		mkPrompt(1, "req-1", "input-1", "hi"),
		mkRespond(2, "req-1", mkAcceptedBody()),
		mkFrame(3, evTurnActive()),
		mkFrame(4, evTurnEnded(oarlock.OutcomeCompleted)),
	}
	if got := UsageOf(noUsage).Value; got.Total != nil || got.ByAgent != nil || got.Unattributed != nil || got.WithChildren != nil {
		t.Fatalf("Usage without any report = %+v, want all nil", got)
	}
}

// TestOverlappingViewsAreNotSummed pins issue #7's acceptance criterion "three
// overlapping usage views must never be added; take the authoritative one".
// One report carrying both an agent figure (Tokens) and a session total
// (Total) is one authoritative pair: the total is reproduced as reported,
// never as total+tokens.
func TestOverlappingViewsAreNotSummed(t *testing.T) {
	records := []oarlock.Record{
		mkUsageFrame(1, oarlock.UsageReport{
			Tokens: mkTokens(100, 50),
			Total:  mkTokens(400, 200),
		}),
	}

	got := UsageOf(records).Value
	if got.Total == nil {
		t.Fatal("Usage.Total = nil, want the reported total")
	}
	if got.Total.Input != 400 || got.Total.Output != 200 {
		t.Fatalf("Usage.Total = %+v, want the reported 400/200 (never total+tokens = 500/250)", got.Total)
	}

	// The agent figure is the report's Tokens, and the gap to the reported
	// total is unattributed, not a second copy of the agent figure.
	root, ok := agentEntry(got.ByAgent)
	if !ok {
		t.Fatalf("Usage.ByAgent = %+v, want the root agent's reported figure", got.ByAgent)
	}
	if root.Input != 100 || root.Output != 50 {
		t.Fatalf("root agent figure = %+v, want the report's Tokens 100/50", root)
	}
	if got.Unattributed == nil {
		t.Fatal("Usage.Unattributed = nil, want reported-attributed = 300/150")
	}
	if got.Unattributed.Input != 300 || got.Unattributed.Output != 150 {
		t.Fatalf("Usage.Unattributed = %+v, want 300/150", got.Unattributed)
	}
	if sum, total := addComponents(sumAgents(got.ByAgent), componentsOf(got.Unattributed)), componentsOf(got.Total); sum != total {
		t.Fatalf("sum(ByAgent)+Unattributed = %v, Total = %v: the views were summed instead of picked", sum, total)
	}
}

// TestRunningTotalsReplaceInsteadOfAccumulate pins issue #7's grok-style
// ledger criterion: a runtime that reports growing running totals per agent
// must be read as "latest wins", never summed across frames.
func TestRunningTotalsReplaceInsteadOfAccumulate(t *testing.T) {
	records := []oarlock.Record{
		mkUsageFrame(1, oarlock.UsageReport{Tokens: mkTokens(10, 5)}),
		mkUsageFrame(2, oarlock.UsageReport{Tokens: mkTokens(30, 20)}),
		mkUsageFrame(3, oarlock.UsageReport{Tokens: mkTokens(50, 40)}),
	}

	got := UsageOf(records).Value
	if got.Total == nil {
		t.Fatal("Usage.Total = nil, want the latest agent figure")
	}
	if got.Total.Input != 50 || got.Total.Output != 40 {
		t.Fatalf("Usage.Total = %+v, want the latest running total 50/40 (summation would be 90/65)", got.Total)
	}
	// One agent, nothing unattributed: ByAgent stays nil by contract.
	if got.ByAgent != nil {
		t.Fatalf("Usage.ByAgent = %+v, want nil for a single root agent with nothing unattributed", got.ByAgent)
	}
	if got.Unattributed != nil {
		t.Fatalf("Usage.Unattributed = %+v, want nil (reported == attributed)", got.Unattributed)
	}
}

// TestByAgentPlusUnattributedEqualsTotal pins issue #7's sub-agent attribution
// criterion: ByAgent plus Unattributed equals Total component-wise, with a
// reported total larger than the agents' sum landing in Unattributed.
func TestByAgentPlusUnattributedEqualsTotal(t *testing.T) {
	records := []oarlock.Record{
		mkUsageFrame(1, oarlock.UsageReport{Tokens: mkTokens(100, 50)}),
		mkUsageFrameAt(2, testRootSessionID, []string{"sub"}, oarlock.UsageReport{Tokens: mkTokens(60, 30)}),
		mkUsageFrame(3, oarlock.UsageReport{Total: mkTokens(400, 200)}),
	}

	got := UsageOf(records).Value
	if got.Total == nil || got.Total.Input != 400 || got.Total.Output != 200 {
		t.Fatalf("Usage.Total = %+v, want the reported total 400/200", got.Total)
	}
	if len(got.ByAgent) != 2 {
		t.Fatalf("Usage.ByAgent = %+v, want one entry per reporting agent (root + sub)", got.ByAgent)
	}
	root, ok := agentEntry(got.ByAgent)
	if !ok || root.Input != 100 || root.Output != 50 {
		t.Fatalf("root agent figure = %+v (found %v), want 100/50", root, ok)
	}
	sub, ok := agentEntry(got.ByAgent, "sub")
	if !ok || sub.Input != 60 || sub.Output != 30 {
		t.Fatalf("sub agent figure = %+v (found %v), want 60/30", sub, ok)
	}
	// reported 400/200 minus attributed 100+60/50+30.
	if got.Unattributed == nil {
		t.Fatal("Usage.Unattributed = nil, want reported-attributed = 240/120")
	}
	if got.Unattributed.Input != 240 || got.Unattributed.Output != 120 {
		t.Fatalf("Usage.Unattributed = %+v, want 240/120", got.Unattributed)
	}

	sum := addComponents(sumAgents(got.ByAgent), componentsOf(got.Unattributed))
	total := componentsOf(got.Total)
	if sum != total {
		t.Fatalf("sum(ByAgent)+Unattributed = %v, Total = %v, want component-wise equality", sum, total)
	}
}

// TestAttributedCarriesOptionalComponentsOnce pins issue #7's ledger criterion
// (Total = ByAgent + Unattributed) for the optional components: reasoning and
// cache counts appear once any report carried one, and never twice.
func TestAttributedCarriesOptionalComponentsOnce(t *testing.T) {
	withReasoning := mkTokens(100, 50)
	withReasoning.Reasoning = mkPtr(int64(20))
	withCache := mkTokens(10, 5)
	withCache.CacheRead = mkPtr(int64(7))

	records := []oarlock.Record{
		mkUsageFrame(1, oarlock.UsageReport{Tokens: withReasoning}),
		mkUsageFrameAt(2, testRootSessionID, []string{"sub"}, oarlock.UsageReport{Tokens: withCache}),
	}

	got := UsageOf(records).Value
	if got.Total == nil {
		t.Fatal("Usage.Total = nil, want the attributed sum")
	}
	if got.Total.Reasoning == nil || *got.Total.Reasoning != 20 {
		t.Fatalf("Total.Reasoning = %v, want 20 reported once", got.Total.Reasoning)
	}
	if got.Total.CacheRead == nil || *got.Total.CacheRead != 7 {
		t.Fatalf("Total.CacheRead = %v, want 7 reported once", got.Total.CacheRead)
	}
	if got.Total.CacheWrite != nil {
		t.Fatalf("Total.CacheWrite = %v, want nil (never reported)", got.Total.CacheWrite)
	}
	if sum := sumAgents(got.ByAgent); sum != componentsOf(got.Total) {
		t.Fatalf("sum(ByAgent) = %v, Total = %v, want component-wise equality", sum, componentsOf(got.Total))
	}
}

// TestWithChildrenAddsChildSessionOnce pins issue #7's child-session criterion:
// a linked child session's own total is added to WithChildren exactly once,
// even when the identical link is reported again.
func TestWithChildrenAddsChildSessionOnce(t *testing.T) {
	records := []oarlock.Record{
		mkFrame(1, oarlock.Event{Kind: oarlock.EventSessionLink, Parent: testRootSessionID, Child: testChildSessionID, Via: "task"}),
		// The very same edge reported a second time: lineage is a set.
		mkFrame(2, oarlock.Event{Kind: oarlock.EventSessionLink, Parent: testRootSessionID, Child: testChildSessionID, Via: "task"}),
		mkUsageFrame(3, oarlock.UsageReport{Total: mkTokens(400, 200)}),
		mkUsageFrameAt(4, testChildSessionID, nil, oarlock.UsageReport{Total: mkTokens(30, 10)}),
	}

	got := UsageOf(records).Value
	if got.Total == nil || got.Total.Input != 400 || got.Total.Output != 200 {
		t.Fatalf("Usage.Total = %+v, want the root session's own 400/200", got.Total)
	}
	if got.WithChildren == nil {
		t.Fatal("Usage.WithChildren = nil, want Total + the child session's total")
	}
	if got.WithChildren.Input != 430 || got.WithChildren.Output != 210 {
		t.Fatalf("Usage.WithChildren = %+v, want 430/210 (the identical link counted twice would be 460/220)", got.WithChildren)
	}
}

// TestWithChildrenIsTransitive pins issue #7's criterion that a child session
// has WithChildren for nested lineage: every session reachable from the root
// over session_linked edges, grandchild included, counts once.
func TestWithChildrenIsTransitive(t *testing.T) {
	records := []oarlock.Record{
		mkFrame(1, oarlock.Event{Kind: oarlock.EventSessionLink, Parent: testRootSessionID, Child: testChildSessionID, Via: "task"}),
		mkFrame(2, oarlock.Event{Kind: oarlock.EventSessionLink, Parent: testChildSessionID, Child: "grandchild-session", Via: "task"}),
		mkUsageFrame(3, oarlock.UsageReport{Total: mkTokens(400, 200)}),
		mkUsageFrameAt(4, testChildSessionID, nil, oarlock.UsageReport{Total: mkTokens(30, 10)}),
		mkUsageFrameAt(5, "grandchild-session", nil, oarlock.UsageReport{Total: mkTokens(5, 1)}),
	}

	got := UsageOf(records).Value
	if got.WithChildren == nil {
		t.Fatal("Usage.WithChildren = nil, want Total + child + grandchild")
	}
	if got.WithChildren.Input != 435 || got.WithChildren.Output != 211 {
		t.Fatalf("Usage.WithChildren = %+v, want 435/211 (400+30+5 / 200+10+1)", got.WithChildren)
	}
}

// TestChildSessionUsageDoesNotOverwriteRoot pins issue #7's sub-agent
// attribution criterion (ByAgent + Unattributed = Total, child sessions have
// WithChildren) at its root scope: a child session's figures reach
// WithChildren and never the root Total, the root ByAgent or the root
// Unattributed.
func TestChildSessionUsageDoesNotOverwriteRoot(t *testing.T) {
	records := []oarlock.Record{
		mkFrame(1, oarlock.Event{Kind: oarlock.EventSessionLink, Parent: testRootSessionID, Child: testChildSessionID, Via: "task"}),
		mkUsageFrame(2, oarlock.UsageReport{Tokens: mkTokens(100, 50), Total: mkTokens(400, 200)}),
		mkUsageFrameAt(3, testChildSessionID, nil, oarlock.UsageReport{Tokens: mkTokens(9000, 9000), Total: mkTokens(9999, 9999)}),
		mkUsageFrameAt(4, testChildSessionID, []string{"sub"}, oarlock.UsageReport{Tokens: mkTokens(7777, 7777)}),
	}

	got := UsageOf(records).Value
	if got.Total == nil || got.Total.Input != 400 || got.Total.Output != 200 {
		t.Fatalf("Usage.Total = %+v, want the root session's 400/200", got.Total)
	}
	if len(got.ByAgent) != 1 {
		t.Fatalf("Usage.ByAgent = %+v, want only the root agent (child-session agents are not root agents)", got.ByAgent)
	}
	root, ok := agentEntry(got.ByAgent)
	if !ok || root.Input != 100 || root.Output != 50 {
		t.Fatalf("root agent figure = %+v (found %v), want the root's own 100/50", root, ok)
	}
	if got.Unattributed == nil || got.Unattributed.Input != 300 || got.Unattributed.Output != 150 {
		t.Fatalf("Usage.Unattributed = %+v, want the root's 300/150", got.Unattributed)
	}
	if got.WithChildren == nil || got.WithChildren.Input != 10399 || got.WithChildren.Output != 10199 {
		t.Fatalf("Usage.WithChildren = %+v, want 10399/10199 (root 400/200 + child 9999/9999)", got.WithChildren)
	}
}
