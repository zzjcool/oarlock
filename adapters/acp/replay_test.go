package acp

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/zzjcool/oarlock"
)

// The one real recording we have for the family: opencode 2.0.26 forwarding a
// child's updates on the root envelope (testdata/opencode-acp-v2-child.json,
// from OAR's replay fixtures, ids normalised). It is replayed through the
// projection with no process at all.
func TestOpenCodeV2RecordedChildReplay(t *testing.T) {
	raw, err := os.ReadFile("testdata/opencode-acp-v2-child.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Notifications []json.RawMessage `json:"notifications"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	p := newProjector(OpenCodeV2, "root")
	var child, root, links, unknown int
	for _, n := range fx.Notifications {
		line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": json.RawMessage(n)})
		w, ok := parseWire(line)
		if !ok {
			t.Fatal("unparseable")
		}
		sid, _, events := p.notification(w)
		for _, e := range events {
			switch {
			case e.Kind == oarlock.EventSessionLink:
				links++
				if e.Parent != "root" || e.Child != "child-1" {
					t.Fatalf("edge = %+v", e)
				}
			case sid == "child-1":
				child++
			default:
				root++
			}
		}
	}
	unknown = len(p.tally())
	// 3 child tool events + the child's text land under child-1; the root keeps its own.
	if child < 4 || root < 4 || links != 5 {
		t.Fatalf("child=%d root=%d links=%d", child, root, links)
	}
	if unknown != 0 {
		t.Fatalf("unknown updates: %v", p.tally())
	}
}

// Live smoke: skipped everywhere this repo's CI runs. A host with the real
// vendor binary and a login sets OARLOCK_LIVE_ACP=<id> to opt in.
func TestLiveSmoke(t *testing.T) {
	for _, p := range Profiles() {
		p := p
		t.Run(p.ID, func(t *testing.T) {
			if os.Getenv("OARLOCK_LIVE_ACP") != p.ID {
				t.Skipf("live %s smoke is opt-in (OARLOCK_LIVE_ACP=%s) and needs the vendor binary and a login: UNVERIFIED", p.ID, p.ID)
			}
			inst, err := p.Runtime().Installation(context.Background())
			if err != nil || !inst.Available {
				t.Skipf("%s binary not on PATH: UNVERIFIED", p.ID)
			}
			t.Skip("live recording harness not implemented: UNVERIFIED")
		})
	}
}
