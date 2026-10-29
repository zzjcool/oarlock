package acp

import (
	"context"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
)

// The ACP family cannot read any of the four native inventories, so the
// runtime must say so explicitly per profile: four unsupported sections with
// a one-line reason, never an empty ok list and never an error. Everything
// else op-wise stays absent (nil field in runtime.go).
func TestProfileRuntimeInventoryIsExplicitlyUnsupported(t *testing.T) {
	want := []oarlock.InventoryKind{
		oarlock.InventoryModels,
		oarlock.InventorySkills,
		oarlock.InventoryCommands,
		oarlock.InventoryTools,
	}
	for _, id := range []string{"grok", "kimi"} {
		t.Run(id, func(t *testing.T) {
			rt := Profile{ID: id}.Runtime()

			if rt.ID != id {
				t.Fatalf("ID = %q, want %q", rt.ID, id)
			}
			if rt.Session == nil {
				t.Fatal("Session is nil; Runtime must still be able to open")
			}
			if rt.Probe == nil {
				t.Fatal("Probe is nil; the LookPath probe must stay")
			}

			// Nil field means absence. The ACP family implements none of these.
			for _, absent := range []struct {
				name string
				op   any
			}{
				{"Models", rt.Models},
				{"Install", rt.Install},
				{"Login", rt.Login},
				{"Usage", rt.Usage},
				{"Upgrade", rt.Upgrade},
			} {
				if absent.op != nil {
					t.Errorf("%s is present; the ACP family must leave it absent", absent.name)
				}
			}

			if rt.Inventory == nil {
				t.Fatal("Inventory is nil; the family must answer explicitly")
			}
			inv, err := rt.Inventory.Inventory(context.Background())
			if err != nil {
				t.Fatalf("Inventory: %v (refusal must not be an error)", err)
			}
			if len(inv.Sections) != len(want) {
				t.Fatalf("got %d sections, want %d: %+v", len(inv.Sections), len(want), inv.Sections)
			}
			for i, kind := range want {
				section := inv.Sections[i]
				if section.Kind != kind {
					t.Errorf("section %d kind = %q, want %q", i, section.Kind, kind)
				}
				if section.Status != oarlock.InventoryUnsupported {
					t.Errorf("%s status = %q, want %q", kind, section.Status, oarlock.InventoryUnsupported)
				}
				if section.Reason == "" {
					t.Errorf("%s has an empty reason", kind)
				}
				if !strings.Contains(section.Reason, id) {
					t.Errorf("%s reason does not name the profile %q: %q", kind, id, section.Reason)
				}
				if !strings.Contains(section.Reason, string(kind)) {
					t.Errorf("%s reason does not name the section: %q", kind, section.Reason)
				}
				if strings.Contains(section.Reason, "\n") {
					t.Errorf("%s reason is not one line: %q", kind, section.Reason)
				}
				if len(section.Items) != 0 {
					t.Errorf("%s is unsupported but carries items: %+v", kind, section.Items)
				}
			}
		})
	}
}
