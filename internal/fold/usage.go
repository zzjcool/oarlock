package fold

import "github.com/zzjcool/oarlock"

// usageState is the token ledger folded from usage events: one entry per
// session id seen, each holding every agent path's latest running total in
// first-report order plus the session's own latest reported total.
type usageState struct {
	sessions map[string]*sessionUsage
	// edges are the runtime-reported session_linked parent → child links, in
	// first-report order and deduplicated: the same link is often repeated.
	edges []sessionEdge
}

// sessionEdge is one runtime-reported derived-session link.
type sessionEdge struct {
	parent string
	child  string
}

// sessionUsage is one session's figures. Each agent's total and the session
// total are running totals the adapter reported: a later report replaces the
// earlier one, it is never added to it.
type sessionUsage struct {
	agents []agentUsage
	index  map[string]int // pathKey → position in agents
	total  *oarlock.TokenTotals
}

// agentUsage is one agent path's latest reported running total.
type agentUsage struct {
	path   []string
	tokens oarlock.TokenTotals
}

// foldUsage applies one fed record's usage and session-graph events. Usage is
// attributed per (session id, agent path), so a child session's figures land
// in their own bucket and never overwrite the root's.
func (f *Fold) foldUsage(r oarlock.Record) {
	for _, ev := range r.Events {
		switch ev.Kind {
		case oarlock.EventUsage:
			if ev.Usage != nil {
				f.usage.report(r.SessionID, r.AgentPath, *ev.Usage)
			}
		case oarlock.EventSessionLink:
			f.usage.link(ev.Parent, ev.Child)
		}
	}
}

// Usage answers the root session's usage. The value owns every pointer and
// slice it hands out.
func (f *Fold) Usage() QueryResult[Usage] {
	return QueryResult[Usage]{Value: f.usage.answer(f.root), Seq: f.seq}
}

// report folds one usage report into its session's ledger. A report carrying
// both figures is one authoritative pair: Tokens is the agent's figure and
// Total the session's; the two are never summed together.
func (s *usageState) report(sessionID string, path []string, report oarlock.UsageReport) {
	if report.Tokens == nil && report.Total == nil {
		return
	}
	session := s.session(sessionID)
	if report.Tokens != nil {
		session.setAgent(path, cloneTokens(*report.Tokens))
	}
	if report.Total != nil {
		total := cloneTokens(*report.Total)
		session.total = &total
	}
}

// link records one parent → child session edge. Self-links and links without a
// child session id are not lineage and are dropped; a repeated link is already
// in the graph.
func (s *usageState) link(parent, child string) {
	if child == "" || child == parent {
		return
	}
	for _, edge := range s.edges {
		if edge.parent == parent && edge.child == child {
			return
		}
	}
	s.edges = append(s.edges, sessionEdge{parent: parent, child: child})
}

func (s *usageState) session(id string) *sessionUsage {
	if s.sessions == nil {
		s.sessions = make(map[string]*sessionUsage)
	}
	session := s.sessions[id]
	if session == nil {
		session = &sessionUsage{index: make(map[string]int)}
		s.sessions[id] = session
	}
	return session
}

// setAgent replaces one agent path's latest running total. A newly reporting
// path is appended, so ByAgent keeps first-report order.
func (s *sessionUsage) setAgent(path []string, tokens oarlock.TokenTotals) {
	key := pathKey(path)
	if at, ok := s.index[key]; ok {
		s.agents[at].tokens = tokens
		return
	}
	s.index[key] = len(s.agents)
	s.agents = append(s.agents, agentUsage{path: clonePath(path), tokens: tokens})
}

// answer builds the root session's usage, then adds each derived child
// session's own total once under WithChildren.
func (s *usageState) answer(root string) Usage {
	var agents []agentUsage
	var reported *oarlock.TokenTotals
	if session := s.sessions[root]; session != nil {
		agents = session.agents
		reported = session.total
	}
	usage := sessionUsageOf(agents, reported)
	if usage.Total == nil {
		return usage
	}
	var children []oarlock.TokenTotals
	for _, id := range s.derived(root) {
		session := s.sessions[id]
		if session == nil {
			continue
		}
		if total := sessionTotal(session); total != nil {
			children = append(children, *total)
		}
	}
	if len(children) == 0 {
		return usage
	}
	withChildren := cloneTokens(*usage.Total)
	for _, total := range children {
		withChildren = addTokens(withChildren, total)
	}
	usage.WithChildren = &withChildren
	return usage
}

// derived is every session reachable from root over session_linked edges,
// nested ones included, each once. The root is never its own descendant.
func (s *usageState) derived(root string) []string {
	found := make(map[string]bool)
	queue := []string{root}
	var order []string
	for at := 0; at < len(queue); at++ {
		parent := queue[at]
		for _, edge := range s.edges {
			if edge.parent != parent || edge.child == root || found[edge.child] {
				continue
			}
			found[edge.child] = true
			order = append(order, edge.child)
			queue = append(queue, edge.child)
		}
	}
	return order
}

// sessionTotal is one session's total folded by the same rules as the root's:
// the runtime's own reported total when it reported one, else the sum of its
// agents' figures. Nil when the session reported nothing at all.
func sessionTotal(session *sessionUsage) *oarlock.TokenTotals {
	if session.total != nil {
		return cloneTokensPtr(session.total)
	}
	if len(session.agents) == 0 {
		return nil
	}
	totals := make([]oarlock.TokenTotals, len(session.agents))
	for i, agent := range session.agents {
		totals[i] = agent.tokens
	}
	sum := sumTokens(totals)
	return &sum
}

// sessionUsageOf is one session's usage from its agents' latest running totals
// and, where the runtime reported one, its own session total: then the total
// is the runtime's and what no agent accounts for is Unattributed, the
// difference, never split by estimate. Elsewhere the agents' figures sum to
// the total.
func sessionUsageOf(agents []agentUsage, reported *oarlock.TokenTotals) Usage {
	if len(agents) == 0 && reported == nil {
		// Nothing reported (or a runtime whose interface never carries token
		// totals): nil, never a guessed zero.
		return Usage{}
	}
	totals := make([]oarlock.TokenTotals, len(agents))
	for i, agent := range agents {
		totals[i] = agent.tokens
	}
	attributed := sumTokens(totals)
	total := attributed
	if reported != nil {
		total = cloneTokens(*reported)
	}
	var unattributed *oarlock.TokenTotals
	if reported != nil {
		remainder := subTokens(total, attributed)
		if hasTokens(remainder) {
			unattributed = &remainder
		}
	}
	// ByAgent earns its place only when the breakdown says something the total
	// does not: several agents, a sub-agent path, or an unattributed remainder.
	onlyRoot := len(agents) <= 1 && (len(agents) == 0 || len(agents[0].path) == 0)
	var byAgent []AgentUsage
	if !onlyRoot || unattributed != nil {
		byAgent = make([]AgentUsage, 0, len(agents))
		for _, agent := range agents {
			byAgent = append(byAgent, AgentUsage{AgentPath: clonePath(agent.path), Tokens: cloneTokens(agent.tokens)})
		}
	}
	return Usage{Total: &total, ByAgent: byAgent, Unattributed: unattributed}
}

// addTokens sums two running totals. Input and output always add; an optional
// part (reasoning, cache read, cache write) exists only once a report carried
// it, and stays absent while both sides lack it.
func addTokens(previous, report oarlock.TokenTotals) oarlock.TokenTotals {
	return oarlock.TokenTotals{
		Input:      previous.Input + report.Input,
		Output:     previous.Output + report.Output,
		Reasoning:  sumPart(previous.Reasoning, report.Reasoning),
		CacheRead:  sumPart(previous.CacheRead, report.CacheRead),
		CacheWrite: sumPart(previous.CacheWrite, report.CacheWrite),
	}
}

// subTokens is total minus baseline. A part total lacks stays absent: the
// difference cannot report a number the runtime never gave. A part the baseline
// lacks subtracts nothing.
func subTokens(total, baseline oarlock.TokenTotals) oarlock.TokenTotals {
	return oarlock.TokenTotals{
		Input:      total.Input - baseline.Input,
		Output:     total.Output - baseline.Output,
		Reasoning:  subPart(total.Reasoning, baseline.Reasoning),
		CacheRead:  subPart(total.CacheRead, baseline.CacheRead),
		CacheWrite: subPart(total.CacheWrite, baseline.CacheWrite),
	}
}

// sumTokens adds running totals component-wise, from the zero total.
func sumTokens(totals []oarlock.TokenTotals) oarlock.TokenTotals {
	var sum oarlock.TokenTotals
	for _, total := range totals {
		sum = addTokens(sum, total)
	}
	return sum
}

func sumPart(previous, report *int64) *int64 {
	if previous == nil && report == nil {
		return nil
	}
	var sum int64
	if previous != nil {
		sum += *previous
	}
	if report != nil {
		sum += *report
	}
	return &sum
}

func subPart(total, baseline *int64) *int64 {
	if total == nil {
		return nil
	}
	value := *total
	if baseline != nil {
		value -= *baseline
	}
	return &value
}

// hasTokens reports whether a difference carries tokens at all. The cache and
// reasoning parts are parts of input and output, so they cannot make an empty
// remainder non-empty.
func hasTokens(tokens oarlock.TokenTotals) bool {
	return tokens.Input > 0 || tokens.Output > 0
}
