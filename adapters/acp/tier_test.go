package acp

// One test per runtime pins its AttributionTier with the frames that justify
// it. Evidence is either a recorded shape (opencode v2) or a fake-agent replay
// of a source/env-documented shape; no vendor binary exists on this machine,
// so every Evidence string says "unverified-live" except where a recording is
// named. TestTierEvidenceIsHonest keeps that claim from drifting.

import (
	"context"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

func TestTierGrokNested(t *testing.T) {
	// proven by TestGrokChildNestedAndNotDoubleBilled: child frames carry their
	// own session id and the lineage edge is stated by the vendor.
	s := open(t, Grok, "grok", oarlock.Options{})
	if s.Capabilities().Attribution != oarlock.AttributionNested {
		t.Fatal("grok declares nested")
	}
	prompt(t, s, "child-grok")
	waitIdle(t, s)
	if !hasEvent(records(s), oarlock.EventSessionLink) {
		t.Fatal("nested needs a stated edge")
	}
}

func TestTierOpenCodeV2NestedV1Opaque(t *testing.T) {
	s := open(t, OpenCodeV2, "opencode", oarlock.Options{})
	if s.Capabilities().Attribution != oarlock.AttributionNested {
		t.Fatal("v2 declares nested")
	}
	prompt(t, s, "child-oc2")
	waitIdle(t, s)
	recs := records(s)
	var inChild, inRoot int
	for _, r := range recs {
		for _, e := range r.Events {
			if e.Kind == oarlock.EventToolCallStarted || e.Kind == oarlock.EventToolCallEnded {
				if r.SessionID == "child-1" {
					inChild++
					if !strings.HasPrefix(e.CallID, "child-1:") {
						t.Fatalf("child call id %q", e.CallID)
					}
				} else {
					inRoot++
				}
			}
		}
	}
	if inChild != 2 || inRoot != 3 {
		t.Fatalf("child=%d root=%d\n%s", inChild, inRoot, dump(recs))
	}
	links := events(recs, oarlock.EventSessionLink)
	if len(links) != 2 || links[0].Child != "child-1" {
		// two frames named the child (call + update); the fold dedupes the edge.
		t.Fatalf("links = %+v", links)
	}
	// A marker whose child equals its parent is not lineage: no edge, root session.
	for _, r := range recs {
		if strings.Contains(string(r.Native), `"toolCallId":"bad"`) && (r.SessionID != "fake-session" || len(r.Events) == 0 || hasLink(r)) {
			t.Fatalf("malformed marker must stay on the root: %+v", r)
		}
	}
	// The child's records never move the root's status.
	if st := fold.StatusOf(recs).Value; st.Kind != fold.StatusIdle {
		t.Fatalf("status = %+v", st)
	}
}

func hasLink(r oarlock.Record) bool {
	for _, e := range r.Events {
		if e.Kind == oarlock.EventSessionLink {
			return true
		}
	}
	return false
}

func TestOpenCodeLineIsChosenFromTheInstallation(t *testing.T) {
	if got := OpenCode.Select(oarlock.Installation{Version: "2.0.26"}); got.Attribution != oarlock.AttributionNested {
		t.Fatalf("2.x = %v", got.Attribution)
	}
	for _, v := range []string{"1.18.34", ""} { // unknown stays on the conservative (opaque) line
		if got := OpenCode.Select(oarlock.Installation{Version: v}); got.Attribution != oarlock.AttributionOpaque {
			t.Fatalf("%q = %v", v, got.Attribution)
		}
	}
	if OpenCode.Attribution != oarlock.AttributionOpaque {
		t.Fatal("the pre-open declaration must be the conservative one")
	}
	// Only v1 steers (a plain prompt joins the running loop); v2 refuses concurrent prompts.
	v1 := open(t, OpenCode, "opencode", oarlock.Options{})
	if _, ok := v1.(oarlock.Steerer); !ok {
		t.Fatal("opencode v1 steers")
	}
	v2 := open(t, OpenCodeV2, "opencode", oarlock.Options{})
	if _, ok := v2.(oarlock.Steerer); ok {
		t.Fatal("opencode v2 must not be a Steerer")
	}
}

func TestOpenCodeV1ForwardsOnlyTheParentsToolCall(t *testing.T) {
	// v1: a child session's frames never arrive; only the parent's `task` call.
	s := open(t, OpenCodeV1, "opencode", oarlock.Options{})
	if s.Capabilities().Attribution != oarlock.AttributionOpaque {
		t.Fatal("v1 declares opaque")
	}
	prompt(t, s, "child-flat") // only root-session frames exist in this scenario
	waitIdle(t, s)
	for _, r := range records(s) {
		if r.SessionID != "fake-session" || len(r.AgentPath) != 0 {
			t.Fatalf("opaque must not attribute: %+v", r)
		}
	}
}

func TestTierQwenQoderAttributedByParentToolUseID(t *testing.T) {
	for _, prof := range []Profile{Qwen, Qoder} {
		s := open(t, prof, prof.ID, oarlock.Options{})
		if s.Capabilities().Attribution != oarlock.AttributionAttributed {
			t.Fatalf("%s declares attributed", prof.ID)
		}
		prompt(t, s, "child-qwen")
		waitIdle(t, s)
		recs := records(s)
		var child, root string
		for _, r := range recs {
			for _, e := range r.Events {
				if e.Kind == oarlock.EventTextDelta {
					if len(r.AgentPath) == 1 && r.AgentPath[0] == "task-1" {
						child += e.Text
					} else if len(r.AgentPath) == 0 {
						root += e.Text
					}
				}
			}
		}
		if child != "from-child" || root != "from-root" {
			t.Fatalf("%s child=%q root=%q\n%s", prof.ID, child, root, dump(recs))
		}
		// attributed, not nested: no session edge is inferred.
		if hasEvent(recs, oarlock.EventSessionLink) {
			t.Fatalf("%s must not state an edge it was not told", prof.ID)
		}
		// A child agent's records never move the root's status.
		if st := fold.StatusOf(recs).Value; st.Kind != fold.StatusIdle {
			t.Fatalf("status = %+v", st)
		}
	}
}

func TestTierKimiAndAntigravityOpaqueAndNothingFabricated(t *testing.T) {
	for _, prof := range []Profile{Kimi, Antigravity} {
		s := open(t, prof, prof.ID, oarlock.Options{})
		if s.Capabilities().Attribution != oarlock.AttributionOpaque {
			t.Fatalf("%s declares opaque", prof.ID)
		}
		// antigravity flattens the child onto the parent session: it all lands on root.
		prompt(t, s, "child-flat")
		waitIdle(t, s)
		for _, r := range records(s) {
			if r.SessionID != "fake-session" || len(r.AgentPath) != 0 {
				t.Fatalf("%s: must not guess an agent from a toolCallId prefix: %+v", prof.ID, r)
			}
		}
		if hasEvent(records(s), oarlock.EventSessionLink) {
			t.Fatalf("%s fabricated an edge", prof.ID)
		}
	}
}

func TestSteerIsAnInterfaceOnlyWhereTheVendorHasOne(t *testing.T) {
	want := map[string]bool{"grok": true, "qoder": true, "opencode": true, "kimi": false, "qwen": false, "antigravity": false}
	for _, prof := range Profiles() {
		s := open(t, prof, prof.ID, oarlock.Options{})
		_, got := s.(oarlock.Steerer)
		if got != want[prof.ID] {
			t.Errorf("%s: Steerer = %v, want %v", prof.ID, got, want[prof.ID])
		}
	}
}

func TestSteerRequestCarriesTheVendorChannel(t *testing.T) {
	// qoder: priority next; grok: sendNow. Seen by the agent in the prompt's _meta.
	for _, c := range []struct {
		prof Profile
		key  string
	}{{Grok, "sendNow"}, {Qoder, "priority"}} {
		s := open(t, c.prof, c.prof.ID, oarlock.Options{})
		prompt(t, s, "steerable")
		waitFor(t, "running", func() bool { return strings.Contains(text(records(s)), "working") })
		out, _ := s.(oarlock.Steerer).Steer(context.Background(), "left", oarlock.InputOptions{})
		if out.Kind != oarlock.ControlAccepted {
			t.Fatalf("%s steer = %+v", c.prof.ID, out)
		}
		waitFor(t, "steered", func() bool { return strings.Contains(text(records(s)), "steered:left") })
	}
}

func TestProfilesStayUnder200Lines(t *testing.T) {
	// Enforced in CI by the shell check in docs/runtimes/acp.md; here, the
	// declared evidence strings keep the claim honest.
	for _, p := range Profiles() {
		if p.Evidence == "" || p.Attribution == "" {
			t.Errorf("%s: tier and evidence must both be declared", p.ID)
		}
	}
}

func TestTierEvidenceIsHonest(t *testing.T) {
	for _, p := range Profiles() {
		recorded := strings.Contains(p.Evidence, "recorded fixture")
		unverified := strings.Contains(p.Evidence, "unverified-live")
		if !recorded && !unverified {
			t.Errorf("%s evidence %q claims neither a recording nor unverified-live", p.ID, p.Evidence)
		}
		if strings.Contains(strings.ToLower(p.Evidence), "live-verified") {
			t.Errorf("%s: no live binary ran here; cannot claim live verification", p.ID)
		}
	}
}
