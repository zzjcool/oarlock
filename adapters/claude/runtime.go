package claude

// Runtime: the Claude adoption unit (issue #23). Session stays the stream-json
// entry point of issue #19; Refused is the same table Open checks, copied, so
// the declaration and the behaviour cannot drift; the operating capabilities
// live in ops.go and are Claude's own CLI surface (`claude --version`,
// `claude auth login|logout|status --json`, `claude update`, the official
// install script).
//
// Models is present as of issue #98 (models.go): the catalogue comes from
// claude's own `list_models` control request, read over one throwaway
// stream-json process and never from a placeholder — a signed-out account is the
// typed ErrNotAuthenticated and a build without the subtype is an
// UnsupportedOperation, so no failure can be mistaken for "no models". The read
// is UNVERIFIED against a live claude.
//
// Account usage is present as of issue #89 (accountusage.go): Claude's quota is
// read from its own HTTP endpoint, so the field names a reader that returns
// either at least one window or a typed error and never a zero snapshot. The
// read is UNVERIFIED against a live subscription, and it reads one home
// directory's credential document rather than every credential store Claude may
// use, so a host that needs certainty still treats the result as best effort.

import (
	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/ops"
)

// Runtime returns the production adoption unit. Session is the only required
// field; every operation is an optional interface whose nil value means "this
// adapter does not have it", so a host branches on presence, never on a flag.
func Runtime() oarlock.Runtime {
	return oarlock.Runtime{
		ID:      "claude",
		Session: Open,
		Refused: Refused(),
		Probe:   installationProbe(),
		Install: installer{},
		Login:   loginProvider{},
		// Usage is the zero-valued reader: it defaults to Claude's endpoint, the
		// process environment, the home directory in force and the shared read
		// timeout, so there is nothing to configure here.
		Usage:     accountUsageReader{},
		Upgrade:   upgrader{},
		Inventory: inventories{},
		// Models is the zero-valued lister: claude is found the way the probe
		// finds it, the process environment is inherited and the exchange is
		// bounded by modelsTimeout, so there is nothing to configure here.
		Models: claudeModels{},
	}
}

// installationProbe is the one installation read: `claude --version`, with the
// official installer's ~/.local/bin/claude as the known fallback. It performs
// no account or network I/O. It is built per call so the fallback follows the
// home directory in force now instead of the one the process started under.
func installationProbe() ops.Probe {
	return ops.Probe{Name: "claude", Fallbacks: []string{ops.FallbackPath(".local/bin/claude")}}
}
