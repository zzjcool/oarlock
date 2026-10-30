package cli

import (
	"fmt"
	"sort"
)

// runList prints every registered runtime and the operations it declares. It
// cannot fail: an empty registry is an honest empty table.
func (a *App) runList(args []string) int {
	fs := a.newFlagSet("list")
	jsonOut := fs.Bool("json", false, "emit the stable JSON DTO")
	positional, code, ok := a.parseCommand(fs, args)
	if !ok {
		return code
	}
	if len(positional) != 0 {
		fmt.Fprintln(a.stderr(), "usage: oarlock list [--json]")
		return ExitUsage
	}

	ids := append([]string(nil), a.registered()...)
	sort.Strings(ids)
	dto := ListDTO{Schema: Schema, Command: "list", Runtimes: make([]RuntimeDTO, 0, len(ids))}
	for _, id := range ids {
		rt, err := a.lookup(id)
		if err != nil {
			// The registry listed an id it cannot resolve. That is a broken
			// registry, not a runtime capability: report it and fail.
			fmt.Fprintf(a.stderr(), "oarlock: list: %s: %v\n", id, err)
			return ExitError
		}
		dto.Runtimes = append(dto.Runtimes, RuntimeDTO{ID: rt.ID, Capabilities: capabilitiesDTO(rt)})
	}

	if *jsonOut {
		return a.writeJSON(dto)
	}
	renderList(a.stdout(), dto.Runtimes)
	return ExitOK
}
