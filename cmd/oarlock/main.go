// Command oarlock is the Phase 1 CLI: list, probe, models and run over every
// registered runtime. The blank imports below are how an adapter registers
// itself; without them `list` would see an empty registry.
package main

import (
	"os"

	_ "github.com/zzjcool/oarlock/adapters/acp"
	_ "github.com/zzjcool/oarlock/adapters/claude"
	_ "github.com/zzjcool/oarlock/adapters/codex"
	_ "github.com/zzjcool/oarlock/adapters/cursor"
	_ "github.com/zzjcool/oarlock/adapters/pi"

	"github.com/zzjcool/oarlock/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
