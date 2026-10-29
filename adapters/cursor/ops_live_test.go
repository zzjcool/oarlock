package cursor

// ops_live_test.go — the operating capabilities against the installed
// cursor-agent (issue #23). Opt-in: OAR_CURSOR_LIVE=1. Everything here is
// read-only — probe, model list, auth status, inventory — and nothing calls
// Login, Logout or Upgrade, which would open a browser or change the machine.
//
// Each claim is checked and logged even when an earlier one fails, so one CLI
// surprise does not hide the state of the others.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
)

func TestLiveOps(t *testing.T) {
	if os.Getenv("OAR_CURSOR_LIVE") != "1" {
		t.Skip("NOT VERIFIED: set OAR_CURSOR_LIVE=1 (needs a logged-in cursor-agent) to read the real ops surfaces")
	}
	rt := Runtime()
	if rt.Probe == nil || rt.Models == nil || rt.Login == nil || rt.Inventory == nil {
		t.Fatalf("runtime = %+v, want a probe, a model lister, a login provider and an inventory", rt)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	inst, err := rt.Probe.Installation(ctx)
	if err != nil {
		t.Fatalf("Installation: %v", err)
	}
	if !inst.Available || inst.Version == "" {
		t.Fatalf("installation = %+v, want an available cursor-agent with a version", inst)
	}
	t.Logf("cursor-agent %s at %s (%s)", inst.Version, inst.Path, inst.Source)

	models, modelsErr := rt.Models.ListModels(ctx)
	status, statusErr := rt.Login.AuthStatus(ctx)

	if modelsErr != nil {
		t.Errorf("ListModels: %v", modelsErr)
	} else if len(models) == 0 {
		t.Error("ListModels listed nothing on a logged-in CLI")
	} else {
		for i, model := range models {
			if i == 5 {
				t.Logf("... %d models in total", len(models))
				break
			}
			t.Logf("model %q %q", model.ID, model.Name)
		}
	}

	if statusErr != nil {
		t.Fatalf("AuthStatus: %v", statusErr)
	}
	if !status.Authenticated {
		t.Skipf("NOT VERIFIED: cursor-agent is not logged in (status %q); run `cursor-agent login`", status.Detail)
	}
	t.Logf("authenticated as %q (status %q)", status.Account, status.Detail)

	// The inventory's models section is the same read, so it only says
	// something new once the list itself parsed.
	if modelsErr != nil {
		return
	}
	inv, err := rt.Inventory.Inventory(ctx)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if len(inv.Sections) != 4 {
		t.Fatalf("inventory sections = %d, want 4", len(inv.Sections))
	}
	if inv.Sections[0].Status != oarlock.InventoryOK || len(inv.Sections[0].Items) == 0 {
		t.Fatalf("models section = %+v, want ok with items", inv.Sections[0])
	}
	for _, section := range inv.Sections[1:] {
		if section.Status != oarlock.InventoryUnsupported {
			t.Fatalf("%s section = %+v, want unsupported", section.Kind, section)
		}
	}
	t.Logf("inventory: %d models, %d unsupported sections", len(inv.Sections[0].Items), len(inv.Sections)-1)
}
