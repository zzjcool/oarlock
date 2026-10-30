package cli

import (
	"context"
	"fmt"

	"github.com/zzjcool/oarlock"
)

// runProbe answers "is this runtime installed?" for one or every runtime.
// Results are collected concurrently and kept in registry order; a probe error
// is isolated to its own result and makes the command exit 1. An unavailable
// installation is a successful probe: the command ran and the answer is "no".
func (a *App) runProbe(args []string) int {
	fs := a.newFlagSet("probe")
	jsonOut := fs.Bool("json", false, "emit the stable JSON DTO")
	positional, code, ok := a.parseCommand(fs, args)
	if !ok {
		return code
	}
	if len(positional) > 1 {
		fmt.Fprintln(a.stderr(), "usage: oarlock probe [<runtime>|all] [--json]")
		return ExitUsage
	}
	target := "all"
	if len(positional) == 1 {
		target = positional[0]
	}

	ids := a.selectIDs(target)
	dto := ProbeDTO{Schema: Schema, Command: "probe", Results: parallelMap(ids, a.probeOne)}

	exit := ExitOK
	for _, result := range dto.Results {
		if result.Error != "" {
			exit = ExitError
			break
		}
	}

	if *jsonOut {
		if code := a.writeJSON(dto); code != ExitOK {
			return code
		}
		return exit
	}
	renderProbe(a.stdout(), dto.Results)
	return exit
}

func (a *App) probeOne(id string) ProbeResultDTO {
	rt, err := a.lookup(id)
	if err != nil {
		return ProbeResultDTO{Runtime: id, Error: err.Error()}
	}
	result := ProbeResultDTO{Runtime: rt.ID}
	if rt.Probe == nil {
		// The runtime declares no probe: the honest answer is "not verified as
		// installed", not an error.
		result.Installation = &InstallationDTO{Available: false}
		return result
	}
	inst, err := rt.Installation(context.Background())
	if err != nil {
		result.Error = fmt.Sprintf("probing %s: %v", rt.ID, err)
		// A version-gate refusal still carries the installation it found; a
		// failure that read nothing leaves the field absent.
		if inst != (oarlock.Installation{}) {
			result.Installation = installationDTO(inst)
		}
		return result
	}
	result.Installation = installationDTO(inst)
	return result
}
