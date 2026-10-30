package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/ops"
)

// The read-only ops commands (issue #97): status, usage, update --check and
// inventory, driven through App.Run over fake runtimes only. cli_test.go owns
// the helpers (ctApp, ctRun, ctDecode, ctRuntime); every fake type here is
// prefixed opsT so it cannot collide with that file.
//
// The rules these tests pin: a read that did not answer is unsupported (exit
// 0), a read that failed is an error (exit 1), a usage error (exit 2) never
// produces a DTO, nothing fabricates a quota the vendor did not report, and
// `update --check` never reaches Upgrader.Upgrade.

// opsTLogin is a Runtime.LoginProvider whose AuthStatus is the fake. Login and
// Logout exist only because the interface has them: a read command must not
// call either.
type opsTLogin func(context.Context) (oarlock.AuthStatus, error)

func (f opsTLogin) Login(context.Context) (oarlock.LoginChallenge, error) {
	return oarlock.LoginChallenge{}, nil
}

func (f opsTLogin) Logout(context.Context) error { return nil }

func (f opsTLogin) AuthStatus(ctx context.Context) (oarlock.AuthStatus, error) {
	return f(ctx)
}

// opsTUsage is a Runtime.AccountUsageReader built from a function.
type opsTUsage func(context.Context) (oarlock.AccountUsage, error)

func (f opsTUsage) AccountUsage(ctx context.Context) (oarlock.AccountUsage, error) { return f(ctx) }

// opsTUpgrade is a Runtime.Upgrader built from a check function. upgrades
// counts Upgrade calls, so `update --check` can prove it never upgraded.
type opsTUpgrade struct {
	check    func(context.Context) (oarlock.UpdateInfo, error)
	upgrades *atomic.Int64
}

func (f opsTUpgrade) CheckUpdate(ctx context.Context) (oarlock.UpdateInfo, error) {
	return f.check(ctx)
}

func (f opsTUpgrade) Upgrade(context.Context) error {
	if f.upgrades != nil {
		f.upgrades.Add(1)
	}
	return nil
}

// opsTInventory is a Runtime.InventoryReader built from a function.
type opsTInventory func(context.Context) (oarlock.Inventory, error)

func (f opsTInventory) Inventory(ctx context.Context) (oarlock.Inventory, error) { return f(ctx) }

// opsTJSON runs one command with --json and enforces the frozen document
// contract: exactly one line, schema oarlock.cli/v1, the expected command, no
// human table text, and (when v is non-nil) a decode into the DTO type.
func opsTJSON(t *testing.T, app *App, wantCommand string, v any, args ...string) (int, string) {
	t.Helper()
	code, out, _ := ctRun(t, app, args...)
	if Schema != "oarlock.cli/v1" {
		t.Fatalf("Schema = %q, want the frozen oarlock.cli/v1", Schema)
	}
	var envelope struct {
		Schema  string `json:"schema"`
		Command string `json:"command"`
	}
	ctDecode(t, out, &envelope) // exactly one line, valid JSON
	if envelope.Schema != Schema {
		t.Fatalf("schema = %q, want %q", envelope.Schema, Schema)
	}
	if envelope.Command != wantCommand {
		t.Fatalf("command = %q, want %q", envelope.Command, wantCommand)
	}
	if !strings.HasPrefix(out, "{") {
		t.Fatalf("--json stdout is not one JSON object:\n%s", out)
	}
	// Defence in depth: encoding/json escapes every control character, so a
	// raw tab can only come from a writer that bypassed the DTO encoder. The
	// prefix and one-line checks above are what actually catch a leaked table
	// (tabwriter pads with spaces, so a table carries no raw tab).
	if strings.Contains(out, "\t") {
		t.Fatalf("--json stdout carries a raw tab:\n%s", out)
	}
	if v != nil {
		ctDecode(t, out, v)
	}
	return code, out
}

// opsTRawResults decodes a DTO's results as raw objects, so a test can assert a
// field is absent rather than zero.
func opsTRawResults(t *testing.T, out string) []map[string]json.RawMessage {
	t.Helper()
	var doc struct {
		Results []map[string]json.RawMessage `json:"results"`
	}
	ctDecode(t, out, &doc)
	return doc.Results
}

// opsTLineContaining returns the first output line mentioning want.
func opsTLineContaining(t *testing.T, out, want string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	t.Fatalf("no table line mentions %q:\n%s", want, out)
	return ""
}

// opsTStatusRuntimes is the status fixture: signed in, signed out, no login
// provider, a typed refusal and (when failing) an exploded read.
func opsTStatusRuntimes(failing bool) []oarlock.Runtime {
	authed := ctRuntime("authed")
	authed.Login = opsTLogin(func(context.Context) (oarlock.AuthStatus, error) {
		return oarlock.AuthStatus{Authenticated: true, Account: "me@example.com", Detail: "token in the keychain"}, nil
	})

	signedOut := ctRuntime("signed-out")
	signedOut.Login = opsTLogin(func(context.Context) (oarlock.AuthStatus, error) {
		return oarlock.AuthStatus{}, nil
	})

	unimplemented := ctRuntime("unimplemented")

	typedUnsupported := ctRuntime("typed-unsupported")
	typedUnsupported.Login = opsTLogin(func(context.Context) (oarlock.AuthStatus, error) {
		return oarlock.AuthStatus{}, &oarlock.UnsupportedOperation{Operation: "auth status", Reason: "no native login flow"}
	})

	runtimes := []oarlock.Runtime{authed, signedOut, unimplemented, typedUnsupported}
	if failing {
		boom := ctRuntime("boom")
		boom.Login = opsTLogin(func(context.Context) (oarlock.AuthStatus, error) {
			return oarlock.AuthStatus{}, errors.New("auth status exploded")
		})
		runtimes = append(runtimes, boom)
	}
	return runtimes
}

func TestOpsStatusJSON(t *testing.T) {
	t.Parallel()
	app, _, _ := ctApp(opsTStatusRuntimes(true)...)
	code, out := opsTJSON(t, app, "status", &StatusDTO{}, "status", "--json")
	if code != ExitError {
		t.Fatalf("exit = %d, want 1 because one auth read failed", code)
	}

	var dto StatusDTO
	ctDecode(t, out, &dto)
	wantOrder := []string{"authed", "boom", "signed-out", "typed-unsupported", "unimplemented"}
	if len(dto.Results) != len(wantOrder) {
		t.Fatalf("results = %d, want %d: %+v", len(dto.Results), len(wantOrder), dto.Results)
	}
	byID := make(map[string]StatusResultDTO, len(dto.Results))
	for i, want := range wantOrder {
		if dto.Results[i].Runtime != want {
			t.Fatalf("results[%d] = %q, want %q (registry order)", i, dto.Results[i].Runtime, want)
		}
		byID[dto.Results[i].Runtime] = dto.Results[i]
	}

	authed := byID["authed"]
	if authed.Auth == nil || !authed.Auth.Authenticated || authed.Auth.Account != "me@example.com" || authed.Auth.Detail != "token in the keychain" {
		t.Fatalf("authed = %+v", authed)
	}
	if authed.Unsupported != "" || authed.Error != "" {
		t.Fatalf("authed carries a non-answer: %+v", authed)
	}

	signedOut := byID["signed-out"]
	if signedOut.Auth == nil || signedOut.Auth.Authenticated {
		t.Fatalf("signed-out = %+v, want an auth object saying authenticated:false", signedOut)
	}
	if signedOut.Unsupported != "" || signedOut.Error != "" {
		t.Fatalf("signed out is data, never an error: %+v", signedOut)
	}

	unimplemented := byID["unimplemented"]
	if unimplemented.Unsupported != "login status is not implemented" || unimplemented.Auth != nil || unimplemented.Error != "" {
		t.Fatalf("unimplemented = %+v", unimplemented)
	}

	typed := byID["typed-unsupported"]
	wantUnsupported := (&oarlock.UnsupportedOperation{Operation: "auth status", Reason: "no native login flow"}).Error()
	if typed.Unsupported != wantUnsupported || typed.Auth != nil || typed.Error != "" {
		t.Fatalf("typed-unsupported = %+v, want unsupported %q", typed, wantUnsupported)
	}

	boom := byID["boom"]
	if !strings.Contains(boom.Error, "auth status exploded") || boom.Auth != nil || boom.Unsupported != "" {
		t.Fatalf("boom = %+v", boom)
	}

	// The auth object is present iff AuthStatus answered.
	raw := opsTRawResults(t, out)
	wantAuth := []bool{true, false, true, false, false} // matches wantOrder
	for i, want := range wantAuth {
		_, ok := raw[i]["auth"]
		if ok != want {
			t.Fatalf("results[%d] (%s) auth present = %v, want %v:\n%s", i, wantOrder[i], ok, want, out)
		}
	}
	// authenticated:false is the answer of a signed-out machine, so the field
	// must survive even at its zero value (no omitempty).
	if auth := string(raw[2]["auth"]); !strings.Contains(auth, `"authenticated":false`) {
		t.Fatalf("signed-out auth = %s, want an explicit authenticated:false", auth)
	}
}

func TestOpsStatusUnsupportedIsNotAnError(t *testing.T) {
	t.Parallel()
	app, _, _ := ctApp(opsTStatusRuntimes(false)...)
	code, out := opsTJSON(t, app, "status", nil, "status", "all", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0: unsupported and signed-out are answers, not failures:\n%s", code, out)
	}
	var dto StatusDTO
	ctDecode(t, out, &dto)
	if len(dto.Results) != 4 {
		t.Fatalf("results = %d, want 4", len(dto.Results))
	}
	for _, result := range dto.Results {
		if result.Error != "" {
			t.Fatalf("%s: unsupported is not a failure: %+v", result.Runtime, result)
		}
	}
}

func TestOpsStatusDoesNotProbe(t *testing.T) {
	t.Parallel()
	probed := new(atomic.Int64)
	rt := ctRuntime("alpha")
	rt.Probe = ctProbe(func(context.Context) (oarlock.Installation, error) {
		probed.Add(1)
		return oarlock.Installation{Available: true}, nil
	})
	rt.Login = opsTLogin(func(context.Context) (oarlock.AuthStatus, error) {
		return oarlock.AuthStatus{Authenticated: true}, nil
	})

	app, _, _ := ctApp(rt)
	code, out := opsTJSON(t, app, "status", &StatusDTO{}, "status", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0:\n%s", code, out)
	}
	if got := probed.Load(); got != 0 {
		t.Fatalf("InstallationProbe called %d times, want 0 (status reads the login state only)", got)
	}
}

// TestOpsUnknownRuntimeIsUnsupported pins the rule once per command: an id the
// registry does not know is the models-precedent unsupported answer, never a
// failure. It is a table because the four collectors each carry their own copy
// of the ErrRuntimeNotFound branch.
func TestOpsUnknownRuntimeIsUnsupported(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		command string
		args    []string
		dto     any
	}{
		{command: "status", args: []string{"status", "nope", "--json"}, dto: &StatusDTO{}},
		{command: "usage", args: []string{"usage", "nope", "--json"}, dto: &UsageDTO{}},
		{command: "update", args: []string{"update", "nope", "--check", "--json"}, dto: &UpdateDTO{}},
		{command: "inventory", args: []string{"inventory", "nope", "--json"}, dto: &InventoryDTO{}},
	} {
		t.Run(tc.command, func(t *testing.T) {
			t.Parallel()
			app, _, _ := ctApp()
			code, out := opsTJSON(t, app, tc.command, tc.dto, tc.args...)
			if code != ExitOK {
				t.Fatalf("%s exit = %d, want 0 (the models precedent):\n%s", tc.command, code, out)
			}
			raw := opsTRawResults(t, out)
			if len(raw) != 1 {
				t.Fatalf("%s results = %d, want 1:\n%s", tc.command, len(raw), out)
			}
			if _, ok := raw[0]["error"]; ok {
				t.Fatalf("%s reported an unknown id as an error:\n%s", tc.command, out)
			}
			if reason, ok := raw[0]["unsupported"]; !ok || len(reason) == 0 {
				t.Fatalf("%s reported an unknown id as neither error nor unsupported:\n%s", tc.command, out)
			}
		})
	}
}

// reset2026 is the reset instant the usage fixture reports and the test expects
// back as RFC3339.
func reset2026() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

// opsTUsageRuntimes is the usage fixture: a plan with two windows (one with a
// reset instant, one without), an empty snapshot, the two unsupported
// classifications and (when failing) a transport failure plus an untyped one.
func opsTUsageRuntimes(failing bool) []oarlock.Runtime {
	reset := reset2026()

	ok := ctRuntime("ok")
	ok.Usage = opsTUsage(func(context.Context) (oarlock.AccountUsage, error) {
		return oarlock.AccountUsage{Plan: "max", Windows: []oarlock.UsageWindow{
			{Name: "5h", Used: 42.5, Limit: 100, Unit: "percent", ResetsAt: &reset},
			{Name: "weekly", Used: 7, Limit: 100, Unit: "percent"},
		}}, nil
	})

	empty := ctRuntime("empty")
	empty.Usage = opsTUsage(func(context.Context) (oarlock.AccountUsage, error) {
		return oarlock.AccountUsage{}, nil
	})

	unimplemented := ctRuntime("unimplemented")

	typedUnsupported := ctRuntime("typed-unsupported")
	typedUnsupported.Usage = opsTUsage(func(context.Context) (oarlock.AccountUsage, error) {
		return oarlock.AccountUsage{}, &ops.UsageError{Runtime: "typed-unsupported", Kind: ops.UsageKindUnsupported, Reason: ops.ReasonEndpointUnavailable}
	})

	reauth := ctRuntime("reauth")
	reauth.Usage = opsTUsage(func(context.Context) (oarlock.AccountUsage, error) {
		return oarlock.AccountUsage{}, &ops.UsageError{Runtime: "reauth", Kind: ops.UsageKindReauth, Reason: ops.ReasonCredentialsMissing}
	})

	runtimes := []oarlock.Runtime{ok, empty, unimplemented, typedUnsupported, reauth}
	if failing {
		transport := ctRuntime("transport")
		transport.Usage = opsTUsage(func(context.Context) (oarlock.AccountUsage, error) {
			return oarlock.AccountUsage{}, &ops.UsageError{Runtime: "transport", Kind: ops.UsageKindTransport, Status: 500}
		})
		boom := ctRuntime("boom")
		boom.Usage = opsTUsage(func(context.Context) (oarlock.AccountUsage, error) {
			return oarlock.AccountUsage{}, errors.New("usage read exploded")
		})
		runtimes = append(runtimes, transport, boom)
	}
	return runtimes
}

func TestOpsUsageJSON(t *testing.T) {
	t.Parallel()
	app, _, _ := ctApp(opsTUsageRuntimes(true)...)
	code, out := opsTJSON(t, app, "usage", &UsageDTO{}, "usage", "--json")
	if code != ExitError {
		t.Fatalf("exit = %d, want 1 because one read failed", code)
	}

	var dto UsageDTO
	ctDecode(t, out, &dto)
	wantOrder := []string{"boom", "empty", "ok", "reauth", "transport", "typed-unsupported", "unimplemented"}
	if len(dto.Results) != len(wantOrder) {
		t.Fatalf("results = %d, want %d: %+v", len(dto.Results), len(wantOrder), dto.Results)
	}
	byID := make(map[string]UsageResultDTO, len(dto.Results))
	for i, want := range wantOrder {
		if dto.Results[i].Runtime != want {
			t.Fatalf("results[%d] = %q, want %q (registry order)", i, dto.Results[i].Runtime, want)
		}
		byID[dto.Results[i].Runtime] = dto.Results[i]
	}

	ok := byID["ok"]
	if ok.Usage == nil || ok.Usage.Plan != "max" || len(ok.Usage.Windows) != 2 {
		t.Fatalf("ok = %+v", ok)
	}
	if ok.Error != "" || ok.Unsupported != "" {
		t.Fatalf("ok carries a non-answer: %+v", ok)
	}
	if w := ok.Usage.Windows[0]; w.Name != "5h" || w.Used != 42.5 || w.Limit != 100 || w.Unit != "percent" {
		t.Fatalf("windows[0] = %+v", w)
	}
	parsed, err := time.Parse(time.RFC3339, ok.Usage.Windows[0].ResetsAt)
	if err != nil {
		t.Fatalf("windows[0].resetsAt = %q is not RFC3339: %v", ok.Usage.Windows[0].ResetsAt, err)
	}
	if !parsed.Equal(reset2026()) {
		t.Fatalf("windows[0].resetsAt = %q, want %s", ok.Usage.Windows[0].ResetsAt, reset2026().Format(time.RFC3339))
	}
	if ok.Usage.Windows[1].Name != "weekly" || ok.Usage.Windows[1].ResetsAt != "" {
		t.Fatalf("windows[1] = %+v, want an absent reset", ok.Usage.Windows[1])
	}

	empty := byID["empty"]
	if empty.Usage == nil || empty.Usage.Plan != "" || len(empty.Usage.Windows) != 0 {
		t.Fatalf("empty = %+v, want an empty usage object, never an invented window", empty)
	}
	if empty.Error != "" || empty.Unsupported != "" {
		t.Fatalf("empty carries a non-answer: %+v", empty)
	}

	unimplemented := byID["unimplemented"]
	if unimplemented.Unsupported != "account usage is not implemented" || unimplemented.Usage != nil || unimplemented.Error != "" {
		t.Fatalf("unimplemented = %+v", unimplemented)
	}

	for _, id := range []string{"typed-unsupported", "reauth"} {
		result := byID[id]
		if result.Unsupported == "" || result.Usage != nil || result.Error != "" {
			t.Fatalf("%s = %+v, want unsupported and no usage", id, result)
		}
	}
	if got, want := byID["typed-unsupported"].Unsupported, (&ops.UsageError{Runtime: "typed-unsupported", Kind: ops.UsageKindUnsupported, Reason: ops.ReasonEndpointUnavailable}).Error(); got != want {
		t.Fatalf("typed-unsupported.Unsupported = %q, want %q", got, want)
	}
	if got, want := byID["reauth"].Unsupported, (&ops.UsageError{Runtime: "reauth", Kind: ops.UsageKindReauth, Reason: ops.ReasonCredentialsMissing}).Error(); got != want {
		t.Fatalf("reauth.Unsupported = %q, want %q", got, want)
	}

	transport := byID["transport"]
	if !strings.Contains(transport.Error, string(ops.UsageKindTransport)) || transport.Unsupported != "" || transport.Usage != nil {
		t.Fatalf("transport = %+v, want an error and no usage", transport)
	}
	boom := byID["boom"]
	if !strings.Contains(boom.Error, "usage read exploded") || boom.Unsupported != "" || boom.Usage != nil {
		t.Fatalf("boom = %+v, want an error and no usage", boom)
	}

	// No fabricated quota on any failure: a failed read never carries a usage
	// object, and an empty snapshot carries neither plan nor windows.
	raw := opsTRawResults(t, out)
	for i, result := range dto.Results {
		_, hasUsage := raw[i]["usage"]
		if result.Error != "" && hasUsage {
			t.Fatalf("results[%d] (%s) fabricated a quota on error:\n%s", i, result.Runtime, out)
		}
	}
	if _, ok := raw[1]["usage"]; !ok { // results[1] is empty (wantOrder above)
		t.Fatalf("empty snapshot lost its usage object:\n%s", out)
	}
	var emptyUsage map[string]json.RawMessage
	if err := json.Unmarshal(raw[1]["usage"], &emptyUsage); err != nil {
		t.Fatalf("usage object: %v", err)
	}
	for _, key := range []string{"plan", "windows"} {
		if _, ok := emptyUsage[key]; ok {
			t.Fatalf("empty snapshot invented %q:\n%s", key, out)
		}
	}
}

// reset2026 lives above the usage fixture.

func TestOpsUsageUnsupportedIsNotAnError(t *testing.T) {
	t.Parallel()
	app, _, _ := ctApp(opsTUsageRuntimes(false)...)
	code, out := opsTJSON(t, app, "usage", nil, "usage", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0: unsupported and reauth are answers, not failures:\n%s", code, out)
	}
	var dto UsageDTO
	ctDecode(t, out, &dto)
	if len(dto.Results) != 5 {
		t.Fatalf("results = %d, want 5", len(dto.Results))
	}
	for _, result := range dto.Results {
		if result.Error != "" {
			t.Fatalf("%s: unsupported is not a failure: %+v", result.Runtime, result)
		}
	}
}

// opsTUpdateRuntimes is the update fixture. Every fake shares one upgrade
// counter: `update --check` must leave it at zero.
func opsTUpdateRuntimes(failing bool) ([]oarlock.Runtime, *atomic.Int64) {
	upgrades := new(atomic.Int64)

	current := ctRuntime("current")
	current.Upgrade = opsTUpgrade{upgrades: upgrades, check: func(context.Context) (oarlock.UpdateInfo, error) {
		return oarlock.UpdateInfo{Available: true, Current: "1.2.3", Latest: "1.4.0"}, nil
	}}

	upToDate := ctRuntime("up-to-date")
	upToDate.Upgrade = opsTUpgrade{upgrades: upgrades, check: func(context.Context) (oarlock.UpdateInfo, error) {
		return oarlock.UpdateInfo{Available: false, Current: "2.0.0", Latest: "2.0.0"}, nil
	}}

	unimplemented := ctRuntime("unimplemented")

	typedUnsupported := ctRuntime("typed-unsupported")
	typedUnsupported.Upgrade = opsTUpgrade{upgrades: upgrades, check: func(context.Context) (oarlock.UpdateInfo, error) {
		return oarlock.UpdateInfo{}, &oarlock.UnsupportedOperation{Operation: "update", Reason: "no check-only mode"}
	}}

	runtimes := []oarlock.Runtime{current, upToDate, unimplemented, typedUnsupported}
	if failing {
		boom := ctRuntime("boom")
		boom.Upgrade = opsTUpgrade{upgrades: upgrades, check: func(context.Context) (oarlock.UpdateInfo, error) {
			return oarlock.UpdateInfo{}, errors.New("update check exploded")
		}}
		runtimes = append(runtimes, boom)
	}
	return runtimes, upgrades
}

func TestOpsUpdateCheckJSON(t *testing.T) {
	t.Parallel()
	fakes, upgrades := opsTUpdateRuntimes(true)
	app, _, _ := ctApp(fakes...)
	code, out := opsTJSON(t, app, "update", &UpdateDTO{}, "update", "--check", "--json")
	if code != ExitError {
		t.Fatalf("exit = %d, want 1 because one check failed", code)
	}

	var dto UpdateDTO
	ctDecode(t, out, &dto)
	wantOrder := []string{"boom", "current", "typed-unsupported", "unimplemented", "up-to-date"}
	if len(dto.Results) != len(wantOrder) {
		t.Fatalf("results = %d, want %d: %+v", len(dto.Results), len(wantOrder), dto.Results)
	}
	byID := make(map[string]UpdateResultDTO, len(dto.Results))
	for i, want := range wantOrder {
		if dto.Results[i].Runtime != want {
			t.Fatalf("results[%d] = %q, want %q (registry order)", i, dto.Results[i].Runtime, want)
		}
		byID[dto.Results[i].Runtime] = dto.Results[i]
	}

	current := byID["current"]
	if current.Update == nil || !current.Update.Available || current.Update.Current != "1.2.3" || current.Update.Latest != "1.4.0" {
		t.Fatalf("current = %+v", current)
	}
	if current.Error != "" || current.Unsupported != "" {
		t.Fatalf("current carries a non-answer: %+v", current)
	}

	upToDate := byID["up-to-date"]
	if upToDate.Update == nil || upToDate.Update.Available || upToDate.Update.Current != "2.0.0" || upToDate.Update.Latest != "2.0.0" {
		t.Fatalf("up-to-date = %+v, want available:false with versions as data", upToDate)
	}
	if upToDate.Error != "" || upToDate.Unsupported != "" {
		t.Fatalf("available:false is data, never an error: %+v", upToDate)
	}

	unimplemented := byID["unimplemented"]
	if unimplemented.Unsupported != "update check is not implemented" || unimplemented.Update != nil || unimplemented.Error != "" {
		t.Fatalf("unimplemented = %+v", unimplemented)
	}

	typed := byID["typed-unsupported"]
	wantUnsupported := (&oarlock.UnsupportedOperation{Operation: "update", Reason: "no check-only mode"}).Error()
	if typed.Unsupported != wantUnsupported || typed.Update != nil || typed.Error != "" {
		t.Fatalf("typed-unsupported = %+v, want unsupported %q", typed, wantUnsupported)
	}

	boom := byID["boom"]
	if !strings.Contains(boom.Error, "update check exploded") || boom.Update != nil || boom.Unsupported != "" {
		t.Fatalf("boom = %+v", boom)
	}

	// available:false must stay a report, not collapse into absence.
	raw := opsTRawResults(t, out) // results[4] is up-to-date (wantOrder above)
	if !strings.Contains(string(raw[4]["update"]), `"available":false`) {
		t.Fatalf("up-to-date update object = %s, want available:false", raw[4]["update"])
	}

	// The command is check-only: no fake may have been upgraded.
	if got := upgrades.Load(); got != 0 {
		t.Fatalf("Upgrade() called %d times, want 0 (update is check-only)", got)
	}
}

func TestOpsUpdateUnsupportedIsNotAnError(t *testing.T) {
	t.Parallel()
	fakes, upgrades := opsTUpdateRuntimes(false)
	app, _, _ := ctApp(fakes...)
	code, out := opsTJSON(t, app, "update", nil, "update", "--check", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0: unsupported and available:false are answers:\n%s", code, out)
	}
	var dto UpdateDTO
	ctDecode(t, out, &dto)
	if len(dto.Results) != 4 {
		t.Fatalf("results = %d, want 4", len(dto.Results))
	}
	for _, result := range dto.Results {
		if result.Error != "" {
			t.Fatalf("%s: unsupported is not a failure: %+v", result.Runtime, result)
		}
	}
	if got := upgrades.Load(); got != 0 {
		t.Fatalf("Upgrade() called %d times, want 0", got)
	}
}

func TestOpsUpdateRequiresCheck(t *testing.T) {
	t.Parallel()
	const wantUsage = "usage: oarlock update [<runtime>|all] --check [--json]"
	fakes, upgrades := opsTUpdateRuntimes(false)
	for _, args := range [][]string{
		{"update", "--json"},            // --check missing
		{"update"},                      // --check missing, no --json
		{"update", "--check", "a", "b"}, // too many positionals
		{"update", "--check", "--json", "current", "extra"},
	} {
		app, _, _ := ctApp(fakes...)
		code, out, stderr := ctRun(t, app, args...)
		if code != ExitUsage {
			t.Fatalf("%v exit = %d, want 2", args, code)
		}
		if out != "" {
			t.Fatalf("%v wrote a DTO on a usage error:\n%s", args, out)
		}
		if !strings.Contains(stderr, wantUsage) {
			t.Fatalf("%v stderr = %q, want %q", args, stderr, wantUsage)
		}
	}
	if got := upgrades.Load(); got != 0 {
		t.Fatalf("Upgrade() called %d times, want 0", got)
	}

	// One runtime, --check present: one result and exit 0.
	app, _, _ := ctApp(fakes...)
	code, out := opsTJSON(t, app, "update", &UpdateDTO{}, "update", "--check", "current", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0:\n%s", code, out)
	}
	var dto UpdateDTO
	ctDecode(t, out, &dto)
	if len(dto.Results) != 1 || dto.Results[0].Runtime != "current" || dto.Results[0].Update == nil {
		t.Fatalf("dto = %+v", dto.Results)
	}
}

// opsTInventoryFixture is the four-section inventory: items in models and
// tools, a section the runtime marks unsupported (skills), and an ok section
// that reported nothing (commands).
func opsTInventoryFixture() oarlock.Inventory {
	return oarlock.Inventory{Sections: []oarlock.InventorySection{
		{Kind: oarlock.InventoryModels, Status: oarlock.InventoryOK, Items: []oarlock.InventoryItem{
			{Name: "m1", Source: "native", Description: "Model One"},
			{Name: "m2"},
		}},
		{Kind: oarlock.InventorySkills, Status: oarlock.InventoryUnsupported, Reason: "skills are not readable"},
		{Kind: oarlock.InventoryCommands, Status: oarlock.InventoryOK},
		{Kind: oarlock.InventoryTools, Status: oarlock.InventoryOK, Items: []oarlock.InventoryItem{
			{Name: "shell", Source: "builtin", Description: "run a command"},
		}},
	}}
}

func opsTInventoryRuntime(id string, inv oarlock.Inventory) oarlock.Runtime {
	rt := ctRuntime(id)
	rt.Inventory = opsTInventory(func(context.Context) (oarlock.Inventory, error) { return inv, nil })
	return rt
}

func TestOpsInventoryJSON(t *testing.T) {
	t.Parallel()
	app, _, _ := ctApp(opsTInventoryRuntime("sections", opsTInventoryFixture()))
	code, out := opsTJSON(t, app, "inventory", &InventoryDTO{}, "inventory", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0: a section-level unsupported is not a failure:\n%s", code, out)
	}

	var dto InventoryDTO
	ctDecode(t, out, &dto)
	if len(dto.Results) != 1 {
		t.Fatalf("results = %+v", dto.Results)
	}
	result := dto.Results[0]
	if result.Unsupported != "" || result.Error != "" {
		t.Fatalf("result = %+v", result)
	}
	wantKinds := []string{"models", "skills", "commands", "tools"}
	if len(result.Sections) != len(wantKinds) {
		t.Fatalf("sections = %d, want %d: %+v", len(result.Sections), len(wantKinds), result.Sections)
	}
	for i, want := range wantKinds {
		if result.Sections[i].Kind != want {
			t.Fatalf("sections[%d].kind = %q, want %q (runtime order preserved)", i, result.Sections[i].Kind, want)
		}
	}

	models := result.Sections[0]
	if models.Status != "ok" || len(models.Items) != 2 {
		t.Fatalf("models section = %+v", models)
	}
	if m1 := models.Items[0]; m1.Name != "m1" || m1.Source != "native" || m1.Description != "Model One" {
		t.Fatalf("models.items[0] = %+v", m1)
	}
	if m2 := models.Items[1]; m2.Name != "m2" || m2.Source != "" || m2.Description != "" {
		t.Fatalf("models.items[1] = %+v, want only the name reported", m2)
	}

	skills := result.Sections[1]
	if skills.Status != "unsupported" || skills.Reason != "skills are not readable" || len(skills.Items) != 0 {
		t.Fatalf("skills section = %+v", skills)
	}

	commands := result.Sections[2]
	if commands.Status != "ok" || len(commands.Items) != 0 {
		t.Fatalf("commands section = %+v, want an empty ok section", commands)
	}

	tools := result.Sections[3]
	if tools.Status != "ok" || len(tools.Items) != 1 {
		t.Fatalf("tools section = %+v", tools)
	}
	if item := tools.Items[0]; item.Name != "shell" || item.Source != "builtin" || item.Description != "run a command" {
		t.Fatalf("tools.items[0] = %+v", item)
	}

	// Absent fields stay absent rather than becoming empty strings or slices.
	raw := opsTRawResults(t, out)
	var sectionsRaw []map[string]json.RawMessage
	if err := json.Unmarshal(raw[0]["sections"], &sectionsRaw); err != nil {
		t.Fatalf("sections: %v", err)
	}
	if _, ok := sectionsRaw[2]["items"]; ok {
		t.Fatalf("empty ok section invented items:\n%s", out)
	}
	var itemsRaw []map[string]json.RawMessage
	if err := json.Unmarshal(sectionsRaw[0]["items"], &itemsRaw); err != nil {
		t.Fatalf("items: %v", err)
	}
	for _, key := range []string{"source", "description"} {
		if _, ok := itemsRaw[1][key]; ok {
			t.Fatalf("undescribed item invented %q:\n%s", key, out)
		}
	}
}

func TestOpsInventoryUnsupportedErrorAndPartial(t *testing.T) {
	t.Parallel()
	unimplemented := ctRuntime("unimplemented")

	// A plain failure is an error, exit 1; only a typed *oarlock.
	// UnsupportedOperation is the "nothing to read" answer (see the test below).
	boom := ctRuntime("boom")
	boom.Inventory = opsTInventory(func(context.Context) (oarlock.Inventory, error) {
		return oarlock.Inventory{}, errors.New("inventory exploded")
	})

	// partial answers before it fails: the sections it read must survive.
	partial := ctRuntime("partial")
	partial.Inventory = opsTInventory(func(context.Context) (oarlock.Inventory, error) {
		return oarlock.Inventory{Sections: []oarlock.InventorySection{
			{Kind: oarlock.InventoryModels, Status: oarlock.InventoryOK, Items: []oarlock.InventoryItem{{Name: "m1"}}},
			{Kind: oarlock.InventoryTools, Status: oarlock.InventoryError, Reason: "tools reader failed"},
		}}, errors.New("inventory partially exploded")
	})

	app, _, _ := ctApp(unimplemented, boom, partial)
	code, out := opsTJSON(t, app, "inventory", &InventoryDTO{}, "inventory", "all", "--json")
	if code != ExitError {
		t.Fatalf("exit = %d, want 1 because one call failed", code)
	}
	var dto InventoryDTO
	ctDecode(t, out, &dto)
	wantOrder := []string{"boom", "partial", "unimplemented"}
	if len(dto.Results) != len(wantOrder) {
		t.Fatalf("results = %+v", dto.Results)
	}
	byID := make(map[string]InventoryResultDTO, len(dto.Results))
	for i, want := range wantOrder {
		if dto.Results[i].Runtime != want {
			t.Fatalf("results[%d] = %q, want %q", i, dto.Results[i].Runtime, want)
		}
		byID[dto.Results[i].Runtime] = dto.Results[i]
	}

	if got := byID["unimplemented"]; got.Unsupported != "inventory is not implemented" || got.Error != "" || got.Sections != nil {
		t.Fatalf("unimplemented = %+v", got)
	}
	failed := byID["boom"]
	if !strings.Contains(failed.Error, "inventory exploded") || failed.Unsupported != "" || failed.Sections != nil {
		t.Fatalf("boom = %+v", failed)
	}
	// A call that failed after reading keeps what it read, mirroring probe.
	kept := byID["partial"]
	if !strings.Contains(kept.Error, "inventory partially exploded") || len(kept.Sections) != 2 {
		t.Fatalf("partial = %+v, want the sections kept beside the error", kept)
	}
	if kept.Sections[0].Kind != "models" || len(kept.Sections[0].Items) != 1 || kept.Sections[0].Items[0].Name != "m1" {
		t.Fatalf("partial.sections[0] = %+v", kept.Sections[0])
	}
	if kept.Sections[1].Status != "error" || kept.Sections[1].Reason != "tools reader failed" {
		t.Fatalf("partial.sections[1] = %+v", kept.Sections[1])
	}
	raw := opsTRawResults(t, out)
	if _, ok := raw[1]["sections"]; !ok {
		t.Fatalf("partial lost its sections:\n%s", out)
	}
	if _, ok := raw[1]["error"]; !ok {
		t.Fatalf("partial lost its error:\n%s", out)
	}
	if len(raw) != len(wantOrder) {
		t.Fatalf("raw results = %d, want %d:\n%s", len(raw), len(wantOrder), out)
	}

	// The human table keeps the sections that answered and then shows the
	// failure, instead of hiding one behind the other.
	humanApp, _, _ := ctApp(partial)
	humanCode, table, _ := ctRun(t, humanApp, "inventory")
	if humanCode != ExitError {
		t.Fatalf("human exit = %d, want 1", humanCode)
	}
	for _, want := range []string{"m1", "tools", "error", "tools reader failed", "(error:"} {
		if !strings.Contains(table, want) {
			t.Fatalf("human inventory missing %q:\n%s", want, table)
		}
	}
}

// TestOpsInventoryTypedUnsupportedIsNotAnError pins the global rule for the one
// command whose unsupported channel is normally the per-section status: a typed
// refusal from Inventory itself is not a failed read.
func TestOpsInventoryTypedUnsupportedIsNotAnError(t *testing.T) {
	t.Parallel()
	refused := ctRuntime("refused")
	refused.Inventory = opsTInventory(func(context.Context) (oarlock.Inventory, error) {
		return oarlock.Inventory{}, &oarlock.UnsupportedOperation{Operation: "inventory", Reason: "no native inventory"}
	})

	app, _, _ := ctApp(refused)
	code, out := opsTJSON(t, app, "inventory", &InventoryDTO{}, "inventory", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (a typed refusal is not a failure)", code)
	}
	var dto InventoryDTO
	ctDecode(t, out, &dto)
	if len(dto.Results) != 1 {
		t.Fatalf("results = %+v", dto.Results)
	}
	if got := dto.Results[0]; got.Error != "" || !strings.Contains(got.Unsupported, "no native inventory") || got.Sections != nil {
		t.Fatalf("refused = %+v, want unsupported and no error", got)
	}
}

func TestOpsInventorySectionErrorExitsOne(t *testing.T) {
	t.Parallel()
	failed := ctRuntime("failed")
	failed.Inventory = opsTInventory(func(context.Context) (oarlock.Inventory, error) {
		return oarlock.Inventory{Sections: []oarlock.InventorySection{
			{Kind: oarlock.InventoryModels, Status: oarlock.InventoryOK, Items: []oarlock.InventoryItem{{Name: "m1", Source: "native"}}},
			{Kind: oarlock.InventorySkills, Status: oarlock.InventoryError, Reason: "skills reader failed"},
			{Kind: oarlock.InventoryTools, Status: oarlock.InventoryOK, Items: []oarlock.InventoryItem{{Name: "shell"}}},
		}}, nil
	})
	ok := opsTInventoryRuntime("ok", opsTInventoryFixture())

	app, _, _ := ctApp(failed, ok)
	code, out := opsTJSON(t, app, "inventory", &InventoryDTO{}, "inventory", "--json")
	if code != ExitError {
		t.Fatalf("exit = %d, want 1 because a section the runtime marked error is a failed read", code)
	}
	var dto InventoryDTO
	ctDecode(t, out, &dto)
	if len(dto.Results) != 2 || dto.Results[0].Runtime != "failed" || dto.Results[1].Runtime != "ok" {
		t.Fatalf("results = %+v", dto.Results)
	}
	if dto.Results[0].Error != "" {
		t.Fatalf("a section error is not a call error: %+v", dto.Results[0])
	}
	if len(dto.Results[0].Sections) != 3 {
		t.Fatalf("sections = %+v, want all three kept", dto.Results[0].Sections)
	}
	if got := dto.Results[0].Sections[1]; got.Status != "error" || got.Reason != "skills reader failed" {
		t.Fatalf("sections[1] = %+v", got)
	}
	if dto.Results[1].Error != "" || dto.Results[1].Unsupported != "" {
		t.Fatalf("one runtime's failed section erased another: %+v", dto.Results[1])
	}

	// The human table still shows the sections that answered.
	app, _, _ = ctApp(failed, ok)
	code, human, _ := ctRun(t, app, "inventory")
	if code != ExitError {
		t.Fatalf("human exit = %d, want 1", code)
	}
	for _, want := range []string{"m1", "shell", "skills", "error", "skills reader failed"} {
		if !strings.Contains(human, want) {
			t.Fatalf("human inventory missing %q:\n%s", want, human)
		}
	}
}

// opsTConcurrentRuntimes builds three runtimes that answer every read-only op
// after a different delay, so completion order (charlie, bravo, alpha) is the
// reverse of the sorted registry order. bravo fails every read.
func opsTConcurrentRuntimes(t *testing.T) []oarlock.Runtime {
	t.Helper()
	build := func(id string, sleep time.Duration, failing bool) oarlock.Runtime {
		rt := ctRuntime(id)
		rt.Login = opsTLogin(func(context.Context) (oarlock.AuthStatus, error) {
			time.Sleep(sleep)
			if failing {
				return oarlock.AuthStatus{}, errors.New(id + ": auth status exploded")
			}
			return oarlock.AuthStatus{Authenticated: true, Account: id + "@example.com"}, nil
		})
		rt.Usage = opsTUsage(func(context.Context) (oarlock.AccountUsage, error) {
			time.Sleep(sleep)
			if failing {
				return oarlock.AccountUsage{}, errors.New(id + ": usage read exploded")
			}
			return oarlock.AccountUsage{Plan: "plan-" + id, Windows: []oarlock.UsageWindow{
				{Name: "5h", Used: 1, Limit: 100, Unit: "percent"},
			}}, nil
		})
		rt.Upgrade = opsTUpgrade{check: func(context.Context) (oarlock.UpdateInfo, error) {
			time.Sleep(sleep)
			if failing {
				return oarlock.UpdateInfo{}, errors.New(id + ": update check exploded")
			}
			return oarlock.UpdateInfo{Available: true, Current: "1.0.0", Latest: "1.1.0"}, nil
		}}
		rt.Inventory = opsTInventory(func(context.Context) (oarlock.Inventory, error) {
			time.Sleep(sleep)
			if failing {
				return oarlock.Inventory{}, errors.New(id + ": inventory exploded")
			}
			return oarlock.Inventory{Sections: []oarlock.InventorySection{
				{Kind: oarlock.InventoryModels, Status: oarlock.InventoryOK, Items: []oarlock.InventoryItem{{Name: id + "-model"}}},
			}}, nil
		})
		return rt
	}
	return []oarlock.Runtime{
		build("charlie", 0, false),
		build("alpha", 40*time.Millisecond, false),
		build("bravo", 5*time.Millisecond, true),
	}
}

func TestOpsConcurrentOrderAndIsolation(t *testing.T) {
	t.Parallel()
	payloadKey := map[string]string{
		"status":    "auth",
		"usage":     "usage",
		"update":    "update",
		"inventory": "sections",
	}
	for _, command := range []string{"status", "usage", "update", "inventory"} {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			app, _, _ := ctApp(opsTConcurrentRuntimes(t)...)
			args := []string{command, "--json"}
			if command == "update" {
				args = []string{command, "--check", "--json"}
			}
			code, out := opsTJSON(t, app, command, nil, args...)
			if code != ExitError {
				t.Fatalf("exit = %d, want 1 because bravo failed:\n%s", code, out)
			}

			raw := opsTRawResults(t, out)
			wantOrder := []string{"alpha", "bravo", "charlie"}
			if len(raw) != len(wantOrder) {
				t.Fatalf("results = %d, want %d:\n%s", len(raw), len(wantOrder), out)
			}
			for i, want := range wantOrder {
				var id string
				if err := json.Unmarshal(raw[i]["runtime"], &id); err != nil {
					t.Fatalf("results[%d].runtime: %v", i, err)
				}
				if id != want {
					t.Fatalf("results[%d] = %q, want %q (registry order, not completion order)", i, id, want)
				}
			}

			// One failing fake must not erase the others.
			for _, i := range []int{0, 2} {
				if len(raw[i][payloadKey[command]]) == 0 {
					t.Fatalf("results[%d] (%s) lost its %q payload:\n%s", i, wantOrder[i], payloadKey[command], out)
				}
				if _, ok := raw[i]["error"]; ok {
					t.Fatalf("results[%d] (%s) carries an error:\n%s", i, wantOrder[i], out)
				}
			}
			if _, ok := raw[1]["error"]; !ok {
				t.Fatalf("bravo's failure was erased:\n%s", out)
			}
		})
	}
}

// opsTFullRuntime answers every read-only op, so one registry can serve all
// four commands in the JSON contract table.
func opsTFullRuntime(id string) oarlock.Runtime {
	rt := ctRuntime(id)
	rt.Login = opsTLogin(func(context.Context) (oarlock.AuthStatus, error) {
		return oarlock.AuthStatus{Authenticated: true, Account: id + "@example.com"}, nil
	})
	rt.Usage = opsTUsage(func(context.Context) (oarlock.AccountUsage, error) {
		return oarlock.AccountUsage{Plan: "max", Windows: []oarlock.UsageWindow{
			{Name: "5h", Used: 10, Limit: 100, Unit: "percent"},
		}}, nil
	})
	rt.Upgrade = opsTUpgrade{check: func(context.Context) (oarlock.UpdateInfo, error) {
		return oarlock.UpdateInfo{Available: false, Current: "1.0.0", Latest: "1.0.0"}, nil
	}}
	rt.Inventory = opsTInventory(func(context.Context) (oarlock.Inventory, error) {
		return oarlock.Inventory{Sections: []oarlock.InventorySection{
			{Kind: oarlock.InventoryModels, Status: oarlock.InventoryOK, Items: []oarlock.InventoryItem{{Name: "m1"}}},
		}}, nil
	})
	return rt
}

func TestOpsJSONContract(t *testing.T) {
	t.Parallel()
	cases := []struct {
		command string
		args    []string
		dto     any
	}{
		{"status", []string{"status", "--json"}, &StatusDTO{}},
		{"usage", []string{"usage", "--json"}, &UsageDTO{}},
		{"update", []string{"update", "--check", "--json"}, &UpdateDTO{}},
		{"inventory", []string{"inventory", "--json"}, &InventoryDTO{}},
	}
	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			t.Parallel()
			app, _, _ := ctApp(opsTFullRuntime("alpha"))
			code, out := opsTJSON(t, app, tc.command, tc.dto, tc.args...)
			if code != ExitOK {
				t.Fatalf("exit = %d, want 0:\n%s", code, out)
			}
			// One JSON object with a results array, and one line only.
			var doc map[string]json.RawMessage
			ctDecode(t, out, &doc)
			if _, ok := doc["results"]; !ok {
				t.Fatalf("no results array:\n%s", out)
			}
			if _, ok := doc["schema"]; !ok {
				t.Fatalf("no schema:\n%s", out)
			}
			if _, ok := doc["command"]; !ok {
				t.Fatalf("no command:\n%s", out)
			}
		})
	}
}

func TestOpsHumanTables(t *testing.T) {
	t.Parallel()

	t.Run("status", func(t *testing.T) {
		t.Parallel()
		app, _, _ := ctApp(opsTStatusRuntimes(true)...)
		code, out, _ := ctRun(t, app, "status")
		if code != ExitError {
			t.Fatalf("exit = %d, want 1", code)
		}
		for _, want := range []string{"RUNTIME", "AUTHENTICATED", "ACCOUNT", "DETAIL", "(unsupported:", "(error:"} {
			if !strings.Contains(out, want) {
				t.Fatalf("status table missing %q:\n%s", want, out)
			}
		}
		if line := opsTLineContaining(t, out, "authed"); !strings.Contains(line, "yes") {
			t.Fatalf("authed row = %q, want AUTHENTICATED yes", line)
		}
		if line := opsTLineContaining(t, out, "signed-out"); !strings.Contains(line, "no") {
			t.Fatalf("signed-out row = %q, want AUTHENTICATED no", line)
		}
	})

	t.Run("usage", func(t *testing.T) {
		t.Parallel()
		app, _, _ := ctApp(opsTUsageRuntimes(false)...)
		code, out, _ := ctRun(t, app, "usage")
		if code != ExitOK {
			t.Fatalf("exit = %d, want 0", code)
		}
		for _, want := range []string{"RUNTIME", "PLAN", "WINDOW", "USED", "LIMIT", "UNIT", "RESETS AT", "max", "5h", "weekly", "(no windows)", "(unsupported:"} {
			if !strings.Contains(out, want) {
				t.Fatalf("usage table missing %q:\n%s", want, out)
			}
		}
		if line := opsTLineContaining(t, out, "empty"); !strings.Contains(line, "(no windows)") {
			t.Fatalf("empty row = %q, want (no windows)", line)
		}
	})

	t.Run("update", func(t *testing.T) {
		t.Parallel()
		fakes, upgrades := opsTUpdateRuntimes(false)
		app, _, _ := ctApp(fakes...)
		code, out, _ := ctRun(t, app, "update", "--check")
		if code != ExitOK {
			t.Fatalf("exit = %d, want 0", code)
		}
		for _, want := range []string{"RUNTIME", "UPDATE AVAILABLE", "CURRENT", "LATEST", "1.4.0", "2.0.0", "(unsupported:"} {
			if !strings.Contains(out, want) {
				t.Fatalf("update table missing %q:\n%s", want, out)
			}
		}
		if line := opsTLineContaining(t, out, "current"); !strings.Contains(line, "yes") {
			t.Fatalf("current row = %q, want UPDATE AVAILABLE yes", line)
		}
		if line := opsTLineContaining(t, out, "up-to-date"); !strings.Contains(line, "no") {
			t.Fatalf("up-to-date row = %q, want UPDATE AVAILABLE no", line)
		}
		if got := upgrades.Load(); got != 0 {
			t.Fatalf("Upgrade() called %d times, want 0", got)
		}
	})

	t.Run("inventory", func(t *testing.T) {
		t.Parallel()
		app, _, _ := ctApp(opsTInventoryRuntime("sections", opsTInventoryFixture()))
		code, out, _ := ctRun(t, app, "inventory")
		if code != ExitOK {
			t.Fatalf("exit = %d, want 0", code)
		}
		// The frozen header has no DESCRIPTION column: the item description is a
		// --json-only field.
		for _, want := range []string{"RUNTIME", "SECTION", "STATUS", "NAME", "SOURCE", "REASON", "m1", "native", "skills are not readable", "shell", "builtin"} {
			if !strings.Contains(out, want) {
				t.Fatalf("inventory table missing %q:\n%s", want, out)
			}
		}
		if line := opsTLineContaining(t, out, "commands"); !strings.Contains(line, "-") {
			t.Fatalf("empty ok section row = %q, want the NAME=- placeholder", line)
		}
	})
}

func TestOpsTooManyPositionalsIsExit2(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"status", "alpha", "beta"},
		{"status", "alpha", "beta", "--json"},
		{"usage", "alpha", "beta"},
		{"update", "--check", "alpha", "beta"},
		{"inventory", "alpha", "beta"},
		{"inventory", "alpha", "beta", "--json"},
	} {
		app, _, _ := ctApp(opsTFullRuntime("alpha"))
		code, out, stderr := ctRun(t, app, args...)
		if code != ExitUsage {
			t.Fatalf("%v exit = %d, want 2", args, code)
		}
		if out != "" {
			t.Fatalf("%v wrote a DTO on a usage error:\n%s", args, out)
		}
		if !strings.Contains(stderr, "usage: oarlock "+args[0]) {
			t.Fatalf("%v stderr = %q", args, stderr)
		}
	}
}
