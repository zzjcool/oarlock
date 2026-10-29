package cursor

// ops_test.go — the operating capabilities against fake cursor-agent
// executables (issue #23). Each test puts a POSIX shell script named
// cursor-agent at the front of PATH, so the adapter resolves the same name it
// uses in production through the same ops.Find path.
//
// The fakes are written as shell scripts, so the tests skip on Windows;
// Windows builds are covered by `GOOS=windows go build`, and the parsing logic
// they exercise is platform-independent.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/ops"
)

// fakeCursor writes a `cursor-agent` shell script and puts its directory first
// on PATH. It returns the path of the fake for stack traces.
func fakeCursor(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake executables are POSIX shell scripts; Windows builds are compile-checked only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, execName)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", execName, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

// argvFile is where the fake writes its argument vector, one argv word per
// line, so a test can assert what cursor-agent was asked to do.
func argvFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "argv")
}

func readArgv(t *testing.T, file string) []string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read fake argv: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// ---- models ------------------------------------------------------------------

func TestModels_ParsesHeaderBlankLinesAndIDNamePairs(t *testing.T) {
	argv := argvFile(t)
	fakeCursor(t, `
printf '%s\n' "$@" > `+argv+`
printf '\n'
printf 'Available models\n'
printf '\n'
printf 'auto - Auto (default)\n'
printf 'composer-2.5 - Composer 2.5\n'
`)
	models, err := (cursorModels{}).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []oarlock.Model{
		{ID: "auto", Name: "Auto (default)"},
		{ID: "composer-2.5", Name: "Composer 2.5"},
	}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %+v, want %+v", models, want)
	}
	if got := readArgv(t, argv); !reflect.DeepEqual(got, []string{"models"}) {
		t.Fatalf("argv = %q, want [models]", got)
	}
}

func TestModels_MalformedLineIsAnError(t *testing.T) {
	fakeCursor(t, `
printf 'Available models\n'
printf 'auto - Auto (default)\n'
printf 'this line is not a model\n'
`)
	models, err := (cursorModels{}).ListModels(context.Background())
	if err == nil {
		t.Fatalf("ListModels = %+v, want an error for the unparsable line", models)
	}
	if !strings.Contains(err.Error(), "this line is not a model") {
		t.Fatalf("error does not name the offending line: %v", err)
	}
	if models != nil {
		t.Fatalf("models = %+v on error, want nil", models)
	}
}

func TestModels_SkipsTheCLIsTrailingTipFooter(t *testing.T) {
	// cursor-agent 2026.09.18 prints this footer on stdout after the model
	// list; it is not a model and there is no flag to suppress it.
	fakeCursor(t, `
printf 'Available models\n'
printf '\n'
printf 'auto - Auto (default)\n'
printf 'Tip: use --model <id> (or /model <id> in interactive mode) to switch.\n'
`)
	models, err := (cursorModels{}).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "auto" {
		t.Fatalf("models = %+v", models)
	}
}

func TestModels_EmptyOutputIsAnError(t *testing.T) {
	for name, body := range map[string]string{
		"nothing":     ":\n",
		"header only": "printf 'Available models\\n'\n",
		"blank lines": "printf '\\n\\n\\n'\n",
	} {
		t.Run(name, func(t *testing.T) {
			fakeCursor(t, body)
			models, err := (cursorModels{}).ListModels(context.Background())
			if err == nil {
				t.Fatalf("ListModels = %+v, want an empty-list error", models)
			}
		})
	}
}

func TestModels_NonZeroExitIsAnError(t *testing.T) {
	fakeCursor(t, "echo 'not signed in' >&2; exit 2\n")
	if models, err := (cursorModels{}).ListModels(context.Background()); err == nil {
		t.Fatalf("ListModels = %+v, want an error for exit 2", models)
	}
}

// ---- status ------------------------------------------------------------------

// statusFake answers `status --format json` with body for both the status and
// logout read-backs.
func statusFake(t *testing.T, statusBody string, logoutExit int) {
	t.Helper()
	fakeCursor(t, `
case "$1" in
status) printf '%s\n' '`+statusBody+`' ;;
logout) exit `+strconv.Itoa(logoutExit)+` ;;
*) echo "fake cursor-agent: unexpected argv $*" >&2; exit 9 ;;
esac
`)
}

func TestStatus_Authenticated(t *testing.T) {
	argv := argvFile(t)
	fakeCursor(t, `
printf '%s\n' "$@" > `+argv+`
printf '%s\n' '{"status":"authenticated","isAuthenticated":true,"userInfo":{"email":"dev@example.com","userId":1}}'
`)
	status, err := (cursorLogin{}).AuthStatus(context.Background())
	if err != nil {
		t.Fatalf("AuthStatus: %v", err)
	}
	if !status.Authenticated || status.Account != "dev@example.com" {
		t.Fatalf("status = %+v", status)
	}
	if got := readArgv(t, argv); !reflect.DeepEqual(got, []string{"status", "--format", "json"}) {
		t.Fatalf("argv = %q", got)
	}
}

func TestStatus_Unauthenticated(t *testing.T) {
	statusFake(t, `{"status":"unauthenticated","isAuthenticated":false}`, 0)
	status, err := (cursorLogin{}).AuthStatus(context.Background())
	if err != nil {
		t.Fatalf("AuthStatus: %v", err)
	}
	if status.Authenticated {
		t.Fatalf("status = %+v, want authenticated=false", status)
	}
	if status.Account != "" {
		t.Fatalf("account = %q, want empty", status.Account)
	}
}

func TestStatus_UnreadableAnswersAreErrors(t *testing.T) {
	tests := map[string]struct{ body string }{
		// A required key the CLI stopped printing: an error, not "signed out".
		"missing isAuthenticated": {body: `{"status":"authenticated"}`},
		// No JSON at all: the exit code decides only that this is a failure.
		"no json, exit 3": {body: ""},
		"truncated json":  {body: `{"isAuthenticated":`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if tc.body == "" {
				fakeCursor(t, "echo 'status unavailable' >&2; exit 3\n")
			} else {
				fakeCursor(t, "printf '%s\\n' '"+tc.body+"'\n")
			}
			status, err := (cursorLogin{}).AuthStatus(context.Background())
			if err == nil {
				t.Fatalf("AuthStatus = %+v, want an error (never a claimed signed-out state)", status)
			}
			if status != (oarlock.AuthStatus{}) {
				t.Fatalf("status = %+v on error, want the zero value", status)
			}
		})
	}
}

// ---- login -------------------------------------------------------------------

// TestLogin_RelaysURLWithoutStdinOrWaiting is the non-interactive contract: the
// helper prints the URL, blocks with an open stdin pipe and never exits on its
// own; Login must return the URL promptly, must not have written stdin, and
// must not wait for the helper.
func TestLogin_RelaysURLWithoutStdinOrWaiting(t *testing.T) {
	dir := t.TempDir()
	stdinSeen := filepath.Join(dir, "stdin-seen")
	envSeen := filepath.Join(dir, "env-seen")
	argv := filepath.Join(dir, "argv")
	fakeCursor(t, `
printf '%s\n' "$@" > `+argv+`
printf '%s\n' "$NO_OPEN_BROWSER" > `+envSeen+`
# If oarlock ever wrote stdin, this read would return a line and record it.
if read line; then printf '%s\n' "$line" >> `+stdinSeen+`; fi
printf 'Opening your browser to sign in: https://cursor.com/loginDeepControl?challenge=abc123\n' >&2
sleep 60
`)

	done := make(chan struct{})
	var (
		challenge oarlock.LoginChallenge
		err       error
	)
	start := time.Now()
	go func() {
		defer close(done)
		challenge, err = (cursorLogin{}).Login(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Login did not return: it waited for the helper instead of stopping it")
	}
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if challenge.Kind != "url" || challenge.URL != "https://cursor.com/loginDeepControl?challenge=abc123" {
		t.Fatalf("challenge = %+v", challenge)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("Login took %v: it waited for the helper", elapsed)
	}
	if got := readArgv(t, argv); !reflect.DeepEqual(got, []string{"login"}) {
		t.Fatalf("argv = %q, want [login]", got)
	}
	env, err := os.ReadFile(envSeen)
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	if strings.TrimSpace(string(env)) != "1" {
		t.Fatalf("NO_OPEN_BROWSER = %q, want 1", strings.TrimSpace(string(env)))
	}
	if data, err := os.ReadFile(stdinSeen); err == nil {
		t.Fatalf("Login wrote stdin: %q", data)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read stdin marker: %v", err)
	}
}

func TestLogin_FailureWithoutChallengeIsAnError(t *testing.T) {
	fakeCursor(t, "echo 'login is unavailable offline' >&2; exit 5\n")
	if challenge, err := (cursorLogin{}).Login(context.Background()); err == nil {
		t.Fatalf("Login = %+v, want an error", challenge)
	}
}

// ---- logout ------------------------------------------------------------------

func TestLogout_NonZeroExitIsAnError(t *testing.T) {
	fakeCursor(t, `
case "$1" in
logout) echo 'logout failed' >&2; exit 4 ;;
status) printf '%s\n' '{"isAuthenticated":true,"userInfo":{"email":"dev@example.com"}}' ;;
esac
`)
	if err := (cursorLogin{}).Logout(context.Background()); err == nil {
		t.Fatal("Logout: want an error for exit 4")
	}
}

func TestLogout_StillLoggedInIsAnError(t *testing.T) {
	statusFake(t, `{"isAuthenticated":true,"userInfo":{"email":"dev@example.com"}}`, 0)
	err := (cursorLogin{}).Logout(context.Background())
	if err == nil {
		t.Fatal("Logout: an exit 0 that leaves the session open is an error")
	}
	if !strings.Contains(err.Error(), "still logged in") || !errors.Is(err, errStillLoggedIn) {
		t.Fatalf("error = %v, want a still-logged-in failure", err)
	}
}

func TestLogout_ReadsBackUnauthenticated(t *testing.T) {
	statusFake(t, `{"status":"unauthenticated","isAuthenticated":false}`, 0)
	if err := (cursorLogin{}).Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
}

// ---- upgrade -----------------------------------------------------------------

func TestUpgrade_UnchangedVersionIsAnError(t *testing.T) {
	argv := argvFile(t)
	fakeCursor(t, `
printf '%s\n' "$1" >> `+argv+`
case "$1" in
--version) echo 1.0.0 ;;
# update exits 0 and replaces nothing: the read-back is what decides.
update) : ;;
esac
`)
	err := (cursorUpgrade{}).Upgrade(context.Background())
	if err == nil {
		t.Fatal("Upgrade: want an error for an unchanged version")
	}
	if !errors.Is(err, ops.ErrVersionUnchanged) {
		t.Fatalf("error = %v, want ops.ErrVersionUnchanged", err)
	}
	if got := readArgv(t, argv); !reflect.DeepEqual(got, []string{"--version", "update", "--version"}) {
		t.Fatalf("argv = %q, want a version read on both sides of update", got)
	}
}

func TestUpgrade_ChangedVersionSucceeds(t *testing.T) {
	state := filepath.Join(t.TempDir(), "updated")
	fakeCursor(t, `
case "$1" in
--version) if [ -f `+state+` ]; then echo 2.0.0; else echo 1.0.0; fi ;;
update) : > `+state+` ;;
esac
`)
	if err := (cursorUpgrade{}).Upgrade(context.Background()); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("the fake update never ran: %v", err)
	}
}

// TestCheckUpdate_IsTypedUnsupported pins the refusal: `cursor-agent update`
// has no check-only sibling, so the check runs nothing at all (a fake on PATH
// would create the marker file) and never reports a made-up verdict.
func TestCheckUpdate_IsTypedUnsupported(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	fakeCursor(t, "touch '"+marker+"'\n")
	info, err := (cursorUpgrade{}).CheckUpdate(context.Background())
	if err == nil {
		t.Fatalf("CheckUpdate = %+v, want a typed UnsupportedOperation", info)
	}
	var unsupported *oarlock.UnsupportedOperation
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %v, want *oarlock.UnsupportedOperation", err)
	}
	if unsupported.Operation != "check-update" {
		t.Fatalf("operation = %q", unsupported.Operation)
	}
	// The reason names the gap in one line: the updater has no check-only
	// sibling subcommand, so there is no read to make.
	for _, want := range []string{"check-only", "cursor-agent update"} {
		if !strings.Contains(unsupported.Reason, want) {
			t.Errorf("reason = %q, want it to mention %q", unsupported.Reason, want)
		}
	}
	if info != (oarlock.UpdateInfo{}) {
		t.Fatalf("UpdateInfo = %+v, want none: a refusal is never an answer", info)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("CheckUpdate started a process: %v", statErr)
	}
}

// ---- inventory ---------------------------------------------------------------

func inventoryKinds(inv oarlock.Inventory) []oarlock.InventoryKind {
	kinds := make([]oarlock.InventoryKind, len(inv.Sections))
	for i, s := range inv.Sections {
		kinds[i] = s.Kind
	}
	return kinds
}

func TestInventory_ModelsFromCLIAndThreeExplicitUnsupported(t *testing.T) {
	fakeCursor(t, `
printf 'Available models\n'
printf 'auto - Auto (default)\n'
printf 'composer-2.5 - Composer 2.5\n'
`)
	inv, err := (cursorInventory{}).Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	want := []oarlock.InventoryKind{
		oarlock.InventoryModels, oarlock.InventorySkills,
		oarlock.InventoryCommands, oarlock.InventoryTools,
	}
	if got := inventoryKinds(inv); !reflect.DeepEqual(got, want) {
		t.Fatalf("sections = %v, want %v", got, want)
	}
	models := inv.Sections[0]
	if models.Status != oarlock.InventoryOK || len(models.Items) != 2 {
		t.Fatalf("models section = %+v", models)
	}
	if models.Items[0].Name != "auto" || models.Items[0].Description != "Auto (default)" {
		t.Fatalf("first model item = %+v", models.Items[0])
	}
	for _, section := range inv.Sections[1:] {
		if section.Status != oarlock.InventoryUnsupported {
			t.Fatalf("%s section = %+v, want unsupported", section.Kind, section)
		}
		if strings.TrimSpace(section.Reason) == "" {
			t.Fatalf("%s section has no reason", section.Kind)
		}
		if len(section.Items) != 0 {
			t.Fatalf("%s section carries items an unsupported section cannot have: %+v", section.Kind, section.Items)
		}
	}
}

func TestInventory_ModelsFailureIsPerSection(t *testing.T) {
	fakeCursor(t, "echo 'network unreachable' >&2; exit 7\n")
	inv, err := (cursorInventory{}).Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v (partial success is carried per section)", err)
	}
	if got := len(inv.Sections); got != 4 {
		t.Fatalf("sections = %d, want 4", got)
	}
	models := inv.Sections[0]
	if models.Status != oarlock.InventoryError {
		t.Fatalf("models section = %+v, want error", models)
	}
	if strings.TrimSpace(models.Reason) == "" {
		t.Fatal("models section has no reason")
	}
	for _, section := range inv.Sections[1:] {
		if section.Status != oarlock.InventoryUnsupported {
			t.Fatalf("%s section = %+v, want unsupported", section.Kind, section)
		}
	}
}

// ---- runtime declaration -----------------------------------------------------

func TestRuntime_WiresOpsAndKeepsSessionAndRefused(t *testing.T) {
	rt := Runtime()
	if rt.ID != "cursor" {
		t.Fatalf("ID = %q", rt.ID)
	}
	if rt.Session == nil || reflect.ValueOf(rt.Session).Pointer() != reflect.ValueOf(Open).Pointer() {
		t.Fatal("Session is not Open")
	}
	if !reflect.DeepEqual(rt.Refused, Refused()) {
		t.Fatal("Refused is not the table Refused() declares")
	}
	probe, ok := rt.Probe.(ops.Probe)
	if !ok || probe.Name != execName {
		t.Fatalf("Probe = %#v, want ops.Probe{Name: %q}", rt.Probe, execName)
	}
	if rt.Models == nil || rt.Login == nil || rt.Upgrade == nil || rt.Inventory == nil {
		t.Fatalf("runtime = %+v, want Models, Login, Upgrade and Inventory", rt)
	}
	if rt.Install != nil || rt.Usage != nil {
		t.Fatalf("runtime = %+v, want Install and Usage absent (nil is the capability)", rt)
	}
}
