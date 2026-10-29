package acp

// live_v2_test.go — the v2 line's live claims: the same transport, turn
// machinery and record projection as live_test.go, but against a real
// opencode 2.x, plus the two things only v2 claims — a nested child session
// the vendor names itself, and a tier that bills no tokens because the profile
// has no usage hook.
//
// Opt-in: OAR_ACP_LIVE=1, OAR_ACP_LIVE_BASE_URL / OAR_ACP_LIVE_API_KEY, and
// OAR_ACP_LIVE_OPENCODE_V2 pointing at a 2.x binary. The v2 tests never fall
// back to PATH: a 1.x `opencode` there is not the line under test. Every
// reason a test cannot run is a skip that starts with NOT VERIFIED; a skip is
// never a pass, and the nested claim stays unverified when the model of the
// run invokes no subagent tool.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

// openLiveV2 is openLive for the v2 line: it keeps the value the profile's
// open returned as well as the *Session, so the concrete type can be asserted
// (v2 must be *Session, not the steer wrapper) and not only its fields.
func openLiveV2(t *testing.T, a liveAgent, opt oarlock.Options) (oarlock.Session, *Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	inst := oarlock.Installation{Available: true, Path: a.bin, Version: a.version, Source: "path"}
	opt.Env = a.env
	if opt.Cwd == "" {
		opt.Cwd = t.TempDir()
	}
	if opt.Model == "" {
		opt.Model = a.model
	}
	sess, err := a.profile.Start(ctx, inst, opt)
	if err != nil {
		t.Fatalf("open the live opencode v2: %v", err)
	}
	s, ok := sess.(*Session)
	if !ok {
		t.Fatalf("the v2 open returned %T, want *Session (v2 exposes no Steer)", sess)
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		_ = s.Dispose(dctx)
	})
	return sess, s
}

func TestLiveOpenCodeV2Turn(t *testing.T) {
	a := liveAgentV2OrSkip(t)
	ctx := liveCtx(t)
	sess, s := openLiveV2(t, a, oarlock.Options{})

	// The concrete type is the plain session: v2 has no steer, and Go cannot
	// express that negative assertion at compile time (the other half is
	// OpenCodeV2.SteerMeta == nil, so start never wraps it).
	if _, steers := sess.(oarlock.Steerer); steers {
		t.Fatalf("the v2 open returned %T, which is a Steerer; v2 has no steer channel", sess)
	}

	// The declared tier is the profile's, and it is nested.
	if got := s.Capabilities().Attribution; got != oarlock.AttributionNested || got != OpenCodeV2.Attribution {
		t.Fatalf("attribution = %q, want nested (%q)", got, OpenCodeV2.Attribution)
	}

	// initialize: the agent names itself and its own line.
	init := liveFrame(t, records(s), "agentCapabilities")
	var ir struct {
		Result struct {
			AgentInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"agentInfo"`
			AgentCapabilities struct {
				PromptCapabilities struct {
					Image bool `json:"image"`
				} `json:"promptCapabilities"`
			} `json:"agentCapabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(init.Native, &ir); err != nil {
		t.Fatalf("initialize frame: %v", err)
	}
	if ir.Result.AgentInfo.Name == "" || ir.Result.AgentInfo.Version == "" {
		t.Fatalf("initialize named no agent: %s", init.Native)
	}
	if !isV2Version(ir.Result.AgentInfo.Version) {
		t.Fatalf("the agent reports %q, not the 2.x line this test is about", ir.Result.AgentInfo.Version)
	}
	if s.Capabilities().Images != ir.Result.AgentCapabilities.PromptCapabilities.Image {
		t.Fatalf("images = %v, agent advertised %v", s.Capabilities().Images, ir.Result.AgentCapabilities.PromptCapabilities.Image)
	}
	t.Logf("agent %s %s; images=%v; attribution=%s", ir.Result.AgentInfo.Name, ir.Result.AgentInfo.Version, s.Capabilities().Images, s.Capabilities().Attribution)

	// session/new: the runtime named the session, and the model the host asked
	// for is the one it reports in effect.
	if id := s.ID(); id == "" {
		t.Fatal("session/new returned no id")
	}
	if f := liveFrame(t, records(s), s.ID()); f.Kind != oarlock.KindFrame {
		t.Fatalf("session id is not in a frame: %+v", f)
	}
	if got := liveModel(records(s)); got != a.model {
		t.Fatalf("model in effect = %q, want %q", got, a.model)
	}

	// A prompt the runtime answers itself: no turn exists before it.
	if hasEvent(records(s), oarlock.EventTurnEnded) {
		t.Fatal("a turn ended before any prompt")
	}
	out, err := s.Prompt(ctx, "Reply with exactly: pong", oarlock.PromptOptions{})
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	waitLiveStatus(t, s, fold.StatusIdle, 2*time.Minute)

	recs := records(s)
	assertDense(t, recs)
	if got := text(recs); !strings.Contains(strings.ToLower(got), "pong") {
		t.Fatalf("the turn produced no pong: %q", got)
	}
	if got := liveTurnEnds(recs); len(got) != 1 {
		t.Fatalf("turn_ended = %d, want exactly one:\n%s", len(got), dump(recs))
	} else if got[0].Outcome == nil || got[0].Outcome.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("turn outcome = %+v, want completed", got[0].Outcome)
	}
	// Not synthesized: the answer that ends the turn is the runtime's own
	// session/prompt response, and it says a stopReason.
	end := liveTurnEndRecord(t, recs)
	if end.Kind != oarlock.KindFrame || !strings.Contains(string(end.Native), `"stopReason"`) {
		t.Fatalf("the turn end is not the runtime's prompt answer: %+v\n%s", end, end.Native)
	}
	if st := fold.StatusOf(recs).Value.Kind; st != fold.StatusIdle {
		t.Fatalf("status = %v, want idle", st)
	}
	if key := a.env[liveKeyEnv]; key != nil && strings.Contains(dump(recs), *key) {
		t.Fatal("the provider key leaked into the record stream")
	}
	t.Logf("%d records, %d text deltas, outcome completed, status idle", len(recs), len(events(recs, oarlock.EventTextDelta)))

	// Dispose is idempotent: one dispose request, one exited response, and the
	// stream stays dense.
	for i := 0; i < 2; i++ {
		if err := s.Dispose(ctx); err != nil {
			t.Fatalf("dispose #%d: %v", i+1, err)
		}
	}
	recs = records(s)
	assertDense(t, recs)
	if d, ex := liveCount(recs, oarlock.ActionDispose), liveExits(recs); d != 1 || ex != 1 {
		t.Fatalf("disposes=%d exits=%d\n%s", d, ex, dump(recs))
	}
	if st := fold.StatusOf(recs).Value.Kind; st != fold.StatusExited {
		t.Fatalf("status after dispose = %v, want exited", st)
	}
}

// liveUsageKeys are the token fields opencode is known to put on a prompt
// answer, in a fixed order: a skip reason built from them is stable, and it
// names fields, never values.
var liveUsageKeys = []string{"inputTokens", "outputTokens", "totalTokens", "thoughtTokens", "cachedReadTokens", "cachedWriteTokens"}

// liveAnswerUsageFields lists the token fields the prompt answers carry. It
// reads the wire frame, not the projection: the question is what the runtime
// said, not what the profile billed.
func liveAnswerUsageFields(recs []oarlock.Record) []string {
	seen := map[string]bool{}
	for _, r := range recs {
		if r.Kind != oarlock.KindFrame {
			continue
		}
		var frame struct {
			Result map[string]any `json:"result"`
		}
		if json.Unmarshal(r.Native, &frame) != nil {
			continue // a notification is a frame too; only answers carry result
		}
		usage := asMap(frame.Result["usage"])
		for _, key := range liveUsageKeys {
			if _, ok := asInt64(usage[key]); ok {
				seen[key] = true
			}
		}
	}
	var out []string
	for _, key := range liveUsageKeys {
		if seen[key] {
			out = append(out, key)
		}
	}
	return out
}

// TestLiveOpenCodeV2NoUsage pins the accounting claim the v2 profile makes by
// having no PromptTokens hook: nothing the runtime sends is billed, so the
// records carry no token-bearing usage event and the folded total stays nil,
// never a guessed zero. If the runtime does report usage on the wire, that is
// said out loud (a skip naming the fields) instead of passing on a half-truth.
func TestLiveOpenCodeV2NoUsage(t *testing.T) {
	a := liveAgentV2OrSkip(t)
	ctx := liveCtx(t)
	s := openLive(t, a, oarlock.Options{})

	if out, err := s.Prompt(ctx, "Reply with exactly: pong", oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	waitLiveStatus(t, s, fold.StatusIdle, 2*time.Minute)
	recs := records(s)
	assertDense(t, recs)

	// The record-level claim: no token-bearing usage event, and a nil total.
	var billed int
	for _, ev := range events(recs, oarlock.EventUsage) {
		if ev.Usage != nil && ev.Usage.Tokens != nil {
			billed++
		}
	}
	if billed != 0 {
		t.Fatalf("%d usage events carry tokens, but OpenCodeV2 has no PromptTokens hook\n%s", billed, dump(recs))
	}
	if total := fold.UsageOf(recs).Value.Total; total != nil {
		t.Fatalf("fold.UsageOf(...).Total = %+v, want nil: nothing was billed\n%s", *total, dump(recs))
	}

	// The name of this test claims the runtime reports no usage. That is not
	// true of opencode 2.0.26's wire: its prompt answer carries a usage object.
	// The profile does not read it, so the record stream bills nothing (above);
	// the wire fact is UNVERIFIED-tier evidence for the profile, never a
	// fabricated nil.
	if fields := liveAnswerUsageFields(recs); len(fields) > 0 {
		t.Skipf("NOT VERIFIED: opencode v2 %s does report usage on the session/prompt answer (%s); OpenCodeV2 has no PromptTokens hook, so no token-bearing EventUsage is recorded and fold.UsageOf(...).Total is nil (asserted above) — the profile bills nothing, the runtime reports the turn", a.version, strings.Join(fields, ", "))
	}
	t.Logf("no usage: %d records, no token-bearing usage event, folded total nil", len(recs))
}

// TestLiveOpenCodeV2Nested is the nested claim against a real v2: when the
// model calls the subagent tool, the vendor's own
// `_meta["opencode/child-session"]` names the child, the stream states the
// edge from this root, and the child's frames are owned by the child's session
// id instead of being flattened onto the root. When the model calls no such
// tool the claim is UNVERIFIED, never faked.
func TestLiveOpenCodeV2Nested(t *testing.T) {
	a := liveAgentV2OrSkip(t)
	ctx := liveCtx(t)
	s := openLive(t, a, oarlock.Options{})
	root := s.ID()
	if root == "" {
		t.Fatal("session/new returned no id")
	}

	const ask = "Use your subagent tool (agent: general) to run `echo CHILD-OK-7731` in its shell, then report the output."
	if out, err := s.Prompt(ctx, ask, oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	waitLiveStatus(t, s, fold.StatusIdle, 3*time.Minute)
	recs := records(s)
	assertDense(t, recs)

	// The root's own tool calls: the call that opens the child is a tool call
	// record here, never root text, and it tells a run where the model tried to
	// delegate from a run where it never did.
	var rootCalls []string
	for _, r := range recs {
		if r.SessionID != root {
			continue
		}
		for _, e := range r.Events {
			if e.Kind == oarlock.EventToolCallStarted {
				rootCalls = append(rootCalls, e.Tool)
			}
		}
	}

	links := events(recs, oarlock.EventSessionLink)
	if len(links) == 0 {
		// A model that called the delegation tool and got no child frame back
		// would refute the nested claim; only "the tool was never called" is
		// the honest UNVERIFIED case.
		for _, tool := range rootCalls {
			if tool == "subagent" || tool == "task" {
				t.Fatalf("the model called %q but no opencode/child-session frame arrived: the nested claim is refuted by this run\n%s", tool, dump(recs))
			}
		}
		t.Skip("NOT VERIFIED: this run's model invoked no subagent tool, so opencode v2 emitted no child-session frame")
	}
	// Every stated edge runs from this root to one child that is not the root.
	child := ""
	for _, l := range links {
		if l.Parent != root {
			t.Fatalf("link parent = %q, want the root session %q: %+v\n%s", l.Parent, root, l, dump(recs))
		}
		if l.Child == "" || l.Child == root {
			t.Fatalf("link child = %q, want a child session that is not the root: %+v", l.Child, l)
		}
		if l.Via != "opencode/child-session" {
			t.Fatalf("link via = %q, want the vendor marker opencode/child-session", l.Via)
		}
		if child == "" {
			child = l.Child
		}
	}

	// The child's frames are attributed to the child, not flattened onto root.
	var owned int
	for _, r := range recs {
		if r.SessionID == child {
			owned++
		}
	}
	if owned == 0 {
		t.Fatalf("no record is attributed to the child session %q\n%s", child, dump(recs))
	}
	if len(rootCalls) == 0 {
		t.Fatalf("no root tool call record: the call that opened the child is not a tool call on the root\n%s", dump(recs))
	}

	// The child's frames never end the root's turn: the runtime's own answer
	// does, exactly once.
	if ends := liveTurnEnds(recs); len(ends) != 1 || ends[0].Outcome == nil || ends[0].Outcome.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("turn ends = %+v, want exactly one completed\n%s", ends, dump(recs))
	}
	t.Logf("nested: %d records, %d links, child %s owns %d records, root tool calls %v", len(recs), len(links), child, owned, rootCalls)
}
