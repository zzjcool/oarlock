package claude

// Runtime: the Claude adoption unit (issue #23). Session stays the stream-json
// entry point of issue #19; Refused is the same table Open checks, copied, so
// the declaration and the behaviour cannot drift; the operating capabilities
// live in ops.go and are Claude's own CLI surface (`claude --version`,
// `claude auth login|logout|status --json`, `claude update`, the official
// install script).
//
// Models and account usage stay nil on purpose: this adapter reads no native
// model list, and Claude reports account usage only over the control channel,
// which is a documented gap. An absent interface is the honest answer; a
// placeholder that returns an empty list or a zero plan would be a claim.

import (
	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/ops"
)

// Runtime returns the production adoption unit. Session is the only required
// field; every operation is an optional interface whose nil value means "this
// adapter does not have it", so a host branches on presence, never on a flag.
func Runtime() oarlock.Runtime {
	return oarlock.Runtime{
		ID:        "claude",
		Session:   Open,
		Refused:   Refused(),
		Probe:     installationProbe(),
		Install:   installer{},
		Login:     loginProvider{},
		Upgrade:   upgrader{},
		Inventory: inventories{},
	}
}

// installationProbe is the one installation read: `claude --version`, with the
// official installer's ~/.local/bin/claude as the known fallback. It performs
// no account or network I/O. It is built per call so the fallback follows the
// home directory in force now instead of the one the process started under.
func installationProbe() ops.Probe {
	return ops.Probe{Name: "claude", Fallbacks: []string{ops.FallbackPath(".local/bin/claude")}}
}
