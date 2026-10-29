package acp

// Inventories for the ACP family: the protocol exposes session-scoped
// capabilities (modes, permission options) after `initialize`, but no
// vendor-neutral query for the four oarlock inventory sections. The adapter
// therefore answers explicitly: four `unsupported` sections with a reason.
// It never returns an empty `ok` list (which would claim "nothing installed")
// and never an error (which would claim the question was asked and failed).
// Adding a native reader per vendor is issue #23 follow-up work.

import (
	"context"
	"fmt"

	"github.com/zzjcool/oarlock"
)

// inventories is the explicit refusal for one ACP profile. id is the profile
// id so the reason names the runtime the answer belongs to.
type inventories struct{ id string }

var _ oarlock.InventoryReader = inventories{}

// Inventory returns the four sections, each unsupported, in the fixed order
// models, skills, commands, tools.
func (i inventories) Inventory(context.Context) (oarlock.Inventory, error) {
	return oarlock.Inventory{Sections: []oarlock.InventorySection{
		unsupportedSection(i.id, oarlock.InventoryModels),
		unsupportedSection(i.id, oarlock.InventorySkills),
		unsupportedSection(i.id, oarlock.InventoryCommands),
		unsupportedSection(i.id, oarlock.InventoryTools),
	}}, nil
}

func unsupportedSection(id string, kind oarlock.InventoryKind) oarlock.InventorySection {
	return oarlock.InventorySection{
		Kind:   kind,
		Status: oarlock.InventoryUnsupported,
		Reason: fmt.Sprintf("the ACP adapter for %s does not read %s inventories; no native query is wired yet (issue #23)", id, kind),
	}
}
