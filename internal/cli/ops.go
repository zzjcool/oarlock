package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/ops"
)

// The read-only commands answer questions about a runtime without running it:
// who is signed in, what quota the account has left, whether an update exists,
// and what the runtime can offer this machine. They share one shape — an
// optional target, one goroutine per runtime, results in registry order — and
// differ only in the operation they consult and in what makes them exit 1.
//
// Nothing here installs, upgrades, logs in or prompts. A read that did not
// answer stays absent or unsupported; it is never filled with a plausible
// number, and "not implemented", "not registered", "not signed in" and "not
// installed" are answers, not failures.

// targetOrAll is the optional <runtime|all> positional. Omitted and "all" both
// mean every registered runtime.
func targetOrAll(positional []string) string {
	if len(positional) == 1 {
		return positional[0]
	}
	return "all"
}

// runStatus reports the login state of one or every runtime. A signed-out
// machine is a successful read, so only an operation that failed exits 1.
func (a *App) runStatus(args []string) int {
	fs := a.newFlagSet("status")
	jsonOut := fs.Bool("json", false, "emit the stable JSON DTO")
	positional, code, ok := a.parseCommand(fs, args)
	if !ok {
		return code
	}
	if len(positional) > 1 {
		fmt.Fprintln(a.stderr(), "usage: oarlock status [<runtime>|all] [--json]")
		return ExitUsage
	}

	ids := a.selectIDs(targetOrAll(positional))
	dto := StatusDTO{Schema: Schema, Command: "status", Results: parallelMap(ids, a.statusOne)}

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
	renderStatus(a.stdout(), dto.Results)
	return exit
}

// statusOne asks one runtime whether this machine is signed in. It reads the
// vendor's login state only: no probe, no session, no token.
func (a *App) statusOne(id string) StatusResultDTO {
	rt, err := a.lookup(id)
	if err != nil {
		if errors.Is(err, oarlock.ErrRuntimeNotFound) {
			return StatusResultDTO{Runtime: id, Unsupported: err.Error()}
		}
		return StatusResultDTO{Runtime: id, Error: err.Error()}
	}
	result := StatusResultDTO{Runtime: rt.ID}

	if rt.Login == nil {
		result.Unsupported = "login status is not implemented"
		return result
	}
	auth, err := rt.Login.AuthStatus(context.Background())
	if err != nil {
		var unsupported *oarlock.UnsupportedOperation
		if errors.As(err, &unsupported) {
			result.Unsupported = err.Error()
		} else {
			result.Error = err.Error()
		}
		return result
	}
	result.Auth = &AuthDTO{Authenticated: auth.Authenticated, Account: auth.Account, Detail: auth.Detail}
	return result
}

// runUsage reports the account-level quota of one or every runtime. A read the
// account cannot provide is unsupported, not a failure; a read that broke is
// an error.
func (a *App) runUsage(args []string) int {
	fs := a.newFlagSet("usage")
	jsonOut := fs.Bool("json", false, "emit the stable JSON DTO")
	positional, code, ok := a.parseCommand(fs, args)
	if !ok {
		return code
	}
	if len(positional) > 1 {
		fmt.Fprintln(a.stderr(), "usage: oarlock usage [<runtime>|all] [--json]")
		return ExitUsage
	}

	ids := a.selectIDs(targetOrAll(positional))
	dto := UsageDTO{Schema: Schema, Command: "usage", Results: parallelMap(ids, a.usageOne)}

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
	renderUsage(a.stdout(), dto.Results)
	return exit
}

// usageOne reads one runtime's account usage. The answer is mirrored verbatim:
// an account the vendor reports without windows stays a usage object with no
// windows, and a failed read carries no usage at all.
func (a *App) usageOne(id string) UsageResultDTO {
	rt, err := a.lookup(id)
	if err != nil {
		if errors.Is(err, oarlock.ErrRuntimeNotFound) {
			return UsageResultDTO{Runtime: id, Unsupported: err.Error()}
		}
		return UsageResultDTO{Runtime: id, Error: err.Error()}
	}
	result := UsageResultDTO{Runtime: rt.ID}

	if rt.Usage == nil {
		result.Unsupported = "account usage is not implemented"
		return result
	}
	usage, err := rt.Usage.AccountUsage(context.Background())
	if err != nil {
		var unsupported *oarlock.UnsupportedOperation
		if errors.As(err, &unsupported) || usageUnsupported(err) {
			result.Unsupported = err.Error()
		} else {
			result.Error = err.Error()
		}
		return result
	}
	result.Usage = accountUsageDTO(usage)
	return result
}

// usageUnsupported reports whether an account-usage failure means "there is
// nothing here to read" rather than "the read failed": no usage surface at all,
// or an account that would first have to log in. Both follow the models
// precedent of reporting "not signed in" as unsupported.
func usageUnsupported(err error) bool {
	var usageErr *ops.UsageError
	if !errors.As(err, &usageErr) {
		return false
	}
	return usageErr.Kind == ops.UsageKindUnsupported || usageErr.Kind == ops.UsageKindReauth
}

// runUpdate is check-only: --check is required and the native upgrade is never
// called, so the command is safe to run at any time. Missing --check is a usage
// error, decided before any runtime is touched and before any DTO exists.
func (a *App) runUpdate(args []string) int {
	fs := a.newFlagSet("update")
	jsonOut := fs.Bool("json", false, "emit the stable JSON DTO")
	check := fs.Bool("check", false, "only look for an update; never upgrade")
	positional, code, ok := a.parseCommand(fs, args)
	if !ok {
		return code
	}
	if len(positional) > 1 || !*check {
		fmt.Fprintln(a.stderr(), "usage: oarlock update [<runtime>|all] --check [--json]")
		return ExitUsage
	}

	ids := a.selectIDs(targetOrAll(positional))
	dto := UpdateDTO{Schema: Schema, Command: "update", Results: parallelMap(ids, a.updateOne)}

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
	renderUpdate(a.stdout(), dto.Results)
	return exit
}

// updateOne performs one read-only update check. CheckUpdate is the only call
// made here; RunUpdate deliberately never reaches Upgrader.Upgrade, because a
// read-only command must not change the machine.
func (a *App) updateOne(id string) UpdateResultDTO {
	rt, err := a.lookup(id)
	if err != nil {
		if errors.Is(err, oarlock.ErrRuntimeNotFound) {
			return UpdateResultDTO{Runtime: id, Unsupported: err.Error()}
		}
		return UpdateResultDTO{Runtime: id, Error: err.Error()}
	}
	result := UpdateResultDTO{Runtime: rt.ID}

	if rt.Upgrade == nil {
		result.Unsupported = "update check is not implemented"
		return result
	}
	info, err := rt.Upgrade.CheckUpdate(context.Background())
	if err != nil {
		var unsupported *oarlock.UnsupportedOperation
		if errors.As(err, &unsupported) {
			result.Unsupported = err.Error()
		} else {
			result.Error = err.Error()
		}
		return result
	}
	result.Update = updateInfoDTO(info)
	return result
}

// runInventory reports what one or every runtime can offer this machine.
// Sections keep their own status, so a partially readable runtime still shows
// the sections that answered. A section the runtime itself marks "error" is a
// failed read and exits 1 even when the call returned no error.
func (a *App) runInventory(args []string) int {
	fs := a.newFlagSet("inventory")
	jsonOut := fs.Bool("json", false, "emit the stable JSON DTO")
	positional, code, ok := a.parseCommand(fs, args)
	if !ok {
		return code
	}
	if len(positional) > 1 {
		fmt.Fprintln(a.stderr(), "usage: oarlock inventory [<runtime>|all] [--json]")
		return ExitUsage
	}

	ids := a.selectIDs(targetOrAll(positional))
	dto := InventoryDTO{Schema: Schema, Command: "inventory", Results: parallelMap(ids, a.inventoryOne)}

	exit := ExitOK
	for _, result := range dto.Results {
		if result.Error != "" || sectionFailed(result.Sections) {
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
	renderInventory(a.stdout(), dto.Results)
	return exit
}

// inventoryOne reads one runtime's native inventories. A call that failed after
// reading some sections keeps them, mirroring probe keeping an installation it
// read; the failure itself is reported beside them. A typed refusal is the
// same "nothing to read here" answer the other three commands give: the
// per-section status is inventory's normal unsupported channel, so a typed
// *oarlock.UnsupportedOperation from the call itself is never a failure.
func (a *App) inventoryOne(id string) InventoryResultDTO {
	rt, err := a.lookup(id)
	if err != nil {
		if errors.Is(err, oarlock.ErrRuntimeNotFound) {
			return InventoryResultDTO{Runtime: id, Unsupported: err.Error()}
		}
		return InventoryResultDTO{Runtime: id, Error: err.Error()}
	}
	result := InventoryResultDTO{Runtime: rt.ID}

	if rt.Inventory == nil {
		result.Unsupported = "inventory is not implemented"
		return result
	}
	inv, err := rt.Inventory.Inventory(context.Background())
	result.Sections = inventorySectionDTOs(inv.Sections)
	if err != nil {
		var unsupported *oarlock.UnsupportedOperation
		if errors.As(err, &unsupported) {
			result.Unsupported = err.Error()
		} else {
			result.Error = err.Error()
		}
	}
	return result
}

// sectionFailed reports whether the runtime marked any section as a read it
// attempted and failed. That verdict is the runtime's own, so it exits 1 even
// when the call as a whole returned no error.
func sectionFailed(sections []InventorySectionDTO) bool {
	for _, section := range sections {
		if section.Status == string(oarlock.InventoryError) {
			return true
		}
	}
	return false
}
