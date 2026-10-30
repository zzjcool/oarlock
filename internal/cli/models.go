package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/zzjcool/oarlock"
)

// runModels lists what each runtime can run now. The same per-runtime isolation
// and ordering as probe applies. "Not implemented" and "not registered" are
// unsupported answers, not failures; a typed list error is honest and exits 1.
func (a *App) runModels(args []string) int {
	fs := a.newFlagSet("models")
	jsonOut := fs.Bool("json", false, "emit the stable JSON DTO")
	positional, code, ok := a.parseCommand(fs, args)
	if !ok {
		return code
	}
	if len(positional) > 1 {
		fmt.Fprintln(a.stderr(), "usage: oarlock models [<runtime>|all] [--json]")
		return ExitUsage
	}
	target := "all"
	if len(positional) == 1 {
		target = positional[0]
	}

	ids := a.selectIDs(target)
	dto := ModelsDTO{Schema: Schema, Command: "models", Results: parallelMap(ids, a.modelsOne)}

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
	renderModels(a.stdout(), dto.Results)
	return exit
}

func (a *App) modelsOne(id string) ModelsResultDTO {
	rt, err := a.lookup(id)
	if err != nil {
		if errors.Is(err, oarlock.ErrRuntimeNotFound) {
			return ModelsResultDTO{Runtime: id, Unsupported: err.Error()}
		}
		return ModelsResultDTO{Runtime: id, Error: err.Error()}
	}
	result := ModelsResultDTO{Runtime: rt.ID}

	if rt.Probe != nil {
		inst, err := rt.Installation(context.Background())
		result.Installation = installationDTO(inst)
		if err != nil {
			result.Error = fmt.Sprintf("probing %s: %v", rt.ID, err)
			return result
		}
		if !inst.Available {
			result.Unsupported = "runtime is not installed"
			return result
		}
	}

	if rt.Models == nil {
		result.Unsupported = "model listing is not implemented"
		return result
	}
	models, err := rt.Models.ListModels(context.Background())
	if err != nil {
		var unsupported *oarlock.UnsupportedOperation
		if errors.As(err, &unsupported) {
			result.Unsupported = err.Error()
		} else {
			result.Error = err.Error()
		}
		return result
	}
	result.Models = modelDTOs(models)
	return result
}
