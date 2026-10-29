package codex

// Tests for the operating capabilities (ops.go): probe, install plan and run,
// auth status, login/logout, the `codex doctor --json` update check, upgrade
// and inventory.
//
// Every vendor CLI here is a fake `#!/bin/sh` script on PATH that appends each
// invocation to a log file, so a test can prove a command ran - or that it did
// not - without a codex binary, a credential store or a network. Windows skips
// the script tests and only compile-checks.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/ops"
)

// skipScripts keeps the POSIX-script tests off Windows.
func skipScripts(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake executables are POSIX shell scripts; Windows is compile-checked only")
	}
}

// shQuote quotes s for /bin/sh.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeShScript writes a `#!/bin/sh` executable into dir.
func writeShScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// fakeCodexScript puts a fake `codex` on PATH, prepends its directory and gives
// the test an empty HOME so the probe's known location cannot answer from the
// real one. body runs after a line that logs "$@" to the returned log path.
func fakeCodexScript(t *testing.T, body string) (dir, log string) {
	t.Helper()
	skipScripts(t)
	dir = t.TempDir()
	log = filepath.Join(dir, "invocations")
	writeShScript(t, dir, executableName, "echo \"$@\" >> "+shQuote(log)+"\n"+body)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	return dir, log
}

// logLines reads the invocation log; an absent log is no invocations.
func logLines(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read invocations: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// absoluteBinary resolves a system tool before PATH is narrowed to the fake's
// directory.
func absoluteBinary(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("no %s on this host: %v", name, err)
	}
	return path
}

// pathWithoutCodex narrows PATH to the fake's directory, so a codex installed
// elsewhere on the machine cannot answer for the fake. The install tests need
// this because their fake executable does not exist until the installer runs;
// they use absoluteBinary for any system tool the fake needs.
func pathWithoutCodex(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", dir)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
}

func TestProbeFindsCodexAndReportsAbsence(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		fakeCodexScript(t, `echo "codex-cli 0.165.0"`)
		inst, err := Runtime().Probe.Installation(context.Background())
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if !inst.Available || inst.Version != "codex-cli 0.165.0" || inst.Source != "path" || inst.Path == "" {
			t.Fatalf("installation = %+v", inst)
		}
	})
	t.Run("missing", func(t *testing.T) {
		// An empty PATH and an empty home: neither the PATH lookup nor the
		// installer's known location can answer.
		t.Setenv("PATH", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		t.Setenv("USERPROFILE", os.Getenv("HOME"))
		inst, err := Runtime().Probe.Installation(context.Background())
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if inst.Available {
			t.Fatalf("installation = %+v, want absent", inst)
		}
	})
}

func TestRuntimeWiresEveryOperatingField(t *testing.T) {
	r := Runtime()
	if r.ID != "codex" || r.Session == nil || r.Refused == nil {
		t.Fatalf("runtime = %+v", r)
	}
	wantProbe := ops.Probe{Name: "codex", Fallbacks: []string{ops.FallbackPath(".local/bin/codex")}}
	if !reflect.DeepEqual(r.Probe, wantProbe) {
		t.Fatalf("probe = %#v, want %#v", r.Probe, wantProbe)
	}

	inst, ok := r.Install.(ops.Installer)
	if !ok {
		t.Fatalf("install = %T, want ops.Installer", r.Install)
	}
	if inst.Line != installLine {
		t.Fatalf("install line = %q, want %q", inst.Line, installLine)
	}
	if inst.Line != "curl -fsSL https://chatgpt.com/codex/install.sh | CODEX_NON_INTERACTIVE=1 sh" {
		t.Fatalf("install line = %q, want OpenAI's official one", inst.Line)
	}
	// The installer judges itself with the same probe the runtime declares.
	if !reflect.DeepEqual(inst.Probe, wantProbe) {
		t.Fatalf("installer probe = %#v, want %#v", inst.Probe, wantProbe)
	}

	if _, ok := r.Login.(loginProvider); !ok {
		t.Fatalf("login = %T, want loginProvider", r.Login)
	}
	if _, ok := r.Upgrade.(upgrader); !ok {
		t.Fatalf("upgrade = %T, want upgrader", r.Upgrade)
	}
	if _, ok := r.Inventory.(inventories); !ok {
		t.Fatalf("inventory = %T, want inventories", r.Inventory)
	}
	// Absent, not refused: no model-list or account-usage command exists on the
	// CLI this adapter drives.
	if r.Models != nil || r.Usage != nil {
		t.Fatalf("models/usage must stay nil, got %T/%T", r.Models, r.Usage)
	}
}

func TestInstallPlanIsOfficialAndRunsNothing(t *testing.T) {
	_, log := fakeCodexScript(t, `echo "the plan must not run me" >&2; exit 64`)
	plan, err := Runtime().Install.InstallPlan(context.Background(), oarlock.InstallRequest{})
	if err != nil {
		t.Fatalf("InstallPlan: %v", err)
	}
	want := []string{"sh", "-c", installLine}
	if !reflect.DeepEqual(plan.Command, want) {
		t.Fatalf("plan.Command = %q, want %q", plan.Command, want)
	}
	if !plan.ChangesMachine {
		t.Fatal("the vendor installer changes the machine")
	}
	if plan.Reason == "" {
		t.Fatal("the plan must say why")
	}
	if lines := logLines(t, log); len(lines) != 0 {
		t.Fatalf("InstallPlan executed something: %q", lines)
	}
}

func TestInstallRunsTheOfficialLineOnceAndLetsTheProbeDecide(t *testing.T) {
	skipScripts(t)
	chmod := absoluteBinary(t, "chmod")
	dir := t.TempDir()
	log := filepath.Join(dir, "installer.log")
	target := filepath.Join(dir, executableName)
	// The fake installer creates the executable the probe looks for, so success
	// comes from the probe, and a second Install finds it and no-ops.
	install := writeShScript(t, dir, "install.sh", strings.Join([]string{
		"echo \"$@\" >> " + shQuote(log),
		"printf '#!/bin/sh\\necho 0.165.0\\n' > " + shQuote(target),
		chmod + " +x " + shQuote(target),
	}, "\n"))
	pathWithoutCodex(t, dir)

	inst := installer
	inst.Shell = []string{install}
	inst.Probe = codexProbe()

	res, err := inst.Install(context.Background(), oarlock.InstallRequest{})
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if !res.Installation.Available || res.Installation.Version != "0.165.0" || res.Installation.Source != "path" {
		t.Fatalf("installation = %+v", res.Installation)
	}
	if _, err := inst.Install(context.Background(), oarlock.InstallRequest{}); err != nil {
		t.Fatalf("second install must reuse the installation: %v", err)
	}
	lines := logLines(t, log)
	if len(lines) != 1 || lines[0] != installLine {
		t.Fatalf("installer invocations = %q, want the official line exactly once", lines)
	}
}

func TestInstallFailureIsAnErrorAndRetryable(t *testing.T) {
	skipScripts(t)
	chmod := absoluteBinary(t, "chmod")
	dir := t.TempDir()
	pathWithoutCodex(t, dir)

	inst := installer
	inst.Shell = []string{writeShScript(t, dir, "install-fail.sh", "echo 'download failed' >&2\nexit 3")}
	inst.Probe = codexProbe()

	res, err := inst.Install(context.Background(), oarlock.InstallRequest{})
	if err == nil {
		t.Fatal("installer exit 3 with no installation must be an error")
	}
	if res.Installation.Available {
		t.Fatalf("installation = %+v after a failed installer", res.Installation)
	}
	if !strings.Contains(err.Error(), "no installation") {
		t.Fatalf("error = %v, want it to name the missing installation", err)
	}

	// Nothing was written by oarlock, so the next attempt can succeed.
	inst.Shell = []string{writeShScript(t, dir, "install-retry.sh", strings.Join([]string{
		"printf '#!/bin/sh\\necho 0.165.0\\n' > " + shQuote(filepath.Join(dir, executableName)),
		chmod + " +x " + shQuote(filepath.Join(dir, executableName)),
	}, "\n"))}
	if _, err := inst.Install(context.Background(), oarlock.InstallRequest{}); err != nil {
		t.Fatalf("retry after a failure: %v", err)
	}
}

// codexStatusScript is a fake `codex` whose `login status` branch is <body>.
const codexStatusScript = `case "$*" in
"login status") <body> ;;
*) echo "unexpected arguments: $*" >&2; exit 64 ;;
esac`

func TestAuthStatusReadsCodexLoginStatus(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    oarlock.AuthStatus
		wantErr string // substring; empty means no error
	}{
		{
			name: "chatgpt login",
			body: `echo "Logged in using ChatGPT" >&2; exit 0`,
			want: oarlock.AuthStatus{Authenticated: true, Account: "chatgpt"},
		},
		{
			name: "api key login",
			body: `echo "Logged in using an API key - sk-test" >&2; exit 0`,
			want: oarlock.AuthStatus{Authenticated: true, Account: "api-key"},
		},
		{
			name: "api key configured",
			body: `echo "API key configured - openai" >&2; exit 0`,
			want: oarlock.AuthStatus{Authenticated: true, Account: "api-key"},
		},
		{
			name: "not logged in",
			body: `echo "Not logged in" >&2; exit 1`,
			want: oarlock.AuthStatus{},
		},
		{
			// Exit 0 is not an answer on its own.
			name:    "exit 0 without a status",
			body:    `echo "hello" >&2; exit 0`,
			wantErr: "exited 0",
		},
		{
			// A broken configuration must never be reported as signed out.
			name:    "unreadable configuration",
			body:    `echo "Error loading configuration: invalid TOML" >&2; exit 1`,
			wantErr: "exited 1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeCodexScript(t, strings.Replace(codexStatusScript, "<body>", tc.body, 1))
			got, err := loginProvider{}.AuthStatus(context.Background())
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("AuthStatus = %+v, %v; want an error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("AuthStatus: %v", err)
			}
			if got.Authenticated != tc.want.Authenticated || got.Account != tc.want.Account {
				t.Fatalf("AuthStatus = %+v, want %+v", got, tc.want)
			}
			if got.Authenticated && got.Detail == "" {
				t.Fatal("a signed-in answer must carry the credential/metering detail")
			}
		})
	}
}

func TestLoginIsTypedUnsupportedAndRunsNothing(t *testing.T) {
	_, log := fakeCodexScript(t, `echo "login must not run" >&2; exit 0`)
	challenge, err := loginProvider{}.Login(context.Background())
	if err == nil {
		t.Fatal("Login must refuse: the device-code flow is app-server-only")
	}
	var unsupported *oarlock.UnsupportedOperation
	if !errors.As(err, &unsupported) {
		t.Fatalf("Login error = %T (%v), want *oarlock.UnsupportedOperation", err, err)
	}
	if unsupported.Operation != "login" {
		t.Fatalf("operation = %q, want login", unsupported.Operation)
	}
	if want := "codex device-code login needs the app-server account/login/start flow, which this adapter does not drive; run `codex login` in a terminal"; unsupported.Reason != want {
		t.Fatalf("reason = %q, want %q", unsupported.Reason, want)
	}
	if challenge != (oarlock.LoginChallenge{}) {
		t.Fatalf("challenge = %+v, want none", challenge)
	}
	if lines := logLines(t, log); len(lines) != 0 {
		t.Fatalf("Login started a process: %q", lines)
	}
}

// codexLogoutScript is a fake `codex`: <logout> answers `codex logout` and
// <status> answers `codex login status`.
const codexLogoutScript = `case "$*" in
logout) <logout> ;;
"login status") <status> ;;
*) echo "unexpected arguments: $*" >&2; exit 64 ;;
esac`

func TestLogoutNeedsAZeroExitAndAnUnauthenticatedStatus(t *testing.T) {
	t.Run("signed out", func(t *testing.T) {
		_, log := fakeCodexScript(t, strings.NewReplacer(
			"<logout>", `echo "Successfully logged out" >&2; exit 0`,
			"<status>", `echo "Not logged in" >&2; exit 1`,
		).Replace(codexLogoutScript))
		if err := (loginProvider{}).Logout(context.Background()); err != nil {
			t.Fatalf("Logout: %v", err)
		}
		if got, want := logLines(t, log), []string{"logout", "login status"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("invocations = %q, want %q", got, want)
		}
	})

	t.Run("non-zero exit", func(t *testing.T) {
		_, log := fakeCodexScript(t, strings.NewReplacer(
			"<logout>", `echo "Error logging out: failed to delete auth.json: Permission denied" >&2; exit 1`,
			"<status>", `echo "Logged in using ChatGPT" >&2; exit 0`,
		).Replace(codexLogoutScript))
		err := loginProvider{}.Logout(context.Background())
		if err == nil || !strings.Contains(err.Error(), "exited 1") {
			t.Fatalf("Logout = %v, want a non-zero exit error", err)
		}
		if got, want := logLines(t, log), []string{"logout"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("invocations = %q, want %q: a failed logout does not consult the status", got, want)
		}
	})

	t.Run("still signed in", func(t *testing.T) {
		fakeCodexScript(t, strings.NewReplacer(
			"<logout>", `echo "Successfully logged out" >&2; exit 0`,
			"<status>", `echo "Logged in using ChatGPT" >&2; exit 0`,
		).Replace(codexLogoutScript))
		err := loginProvider{}.Logout(context.Background())
		if err == nil || !strings.Contains(err.Error(), "still reads signed in") {
			t.Fatalf("Logout = %v, want it to refuse an exit 0 that left the machine signed in", err)
		}
	})
}

func TestUpgradeUnchangedVersionIsAnError(t *testing.T) {
	fakeCodexScript(t, `case "$1" in
--version) echo "codex-cli 0.165.0" ;;
update) echo "codex is up to date"; exit 0 ;;
*) echo "unexpected arguments: $*" >&2; exit 64 ;;
esac`)
	err := upgrader{}.Upgrade(context.Background())
	if !errors.Is(err, ops.ErrVersionUnchanged) {
		t.Fatalf("Upgrade = %v, want ops.ErrVersionUnchanged: `codex update` can exit 0 without replacing anything", err)
	}
}

func TestUpgradeSucceedsWhenTheVersionMoves(t *testing.T) {
	dir, log := fakeCodexScript(t, `case "$1" in
--version) cat "$(dirname "$0")/version" ;;
update) echo 0.166.0 > "$(dirname "$0")/version"; echo "installed 0.166.0" ;;
*) echo "unexpected arguments: $*" >&2; exit 64 ;;
esac`)
	if err := os.WriteFile(filepath.Join(dir, "version"), []byte("0.165.0\n"), 0o644); err != nil {
		t.Fatalf("write version: %v", err)
	}
	if err := (upgrader{}).Upgrade(context.Background()); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if got, want := logLines(t, log), []string{"--version", "update", "--version"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("invocations = %q, want %q", got, want)
	}
}

// codexDoctorScript is a fake `codex` that answers `--version` and
// `doctor --json` from the files $FAKE_CODEX_VERSION and $FAKE_CODEX_DOCTOR
// point at, and exits <exit> on doctor: on a live host doctor exits 1 whenever
// any other row fails while the JSON is still valid.
const codexDoctorScript = `case "$*" in
--version) cat "$FAKE_CODEX_VERSION" ;;
"doctor --json") cat "$FAKE_CODEX_DOCTOR"; exit <exit> ;;
*) echo "unexpected arguments: $*" >&2; exit 64 ;;
esac`

// fakeCodexDoctor writes the two answer files and the fake that reads them, so
// a test can drive `codex doctor --json` from a table. The answer files are env
// var values, not argv, so a payload never has to survive shell quoting. It
// returns the invocation log.
func fakeCodexDoctor(t *testing.T, doctor string, exit int) string {
	t.Helper()
	dir, log := fakeCodexScript(t, strings.Replace(codexDoctorScript, "<exit>", strconv.Itoa(exit), 1))
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	t.Setenv("FAKE_CODEX_VERSION", write("version.txt", "codex-cli 0.165.0\n"))
	t.Setenv("FAKE_CODEX_DOCTOR", write("doctor.json", doctor))
	return log
}

// doctorBody is a `codex doctor --json` report whose updates row carries
// details, plus a second row the adapter must ignore.
func doctorBody(details string) string {
	return `{"checks":{"auth":{"status":"fail","details":{"logged in":"false"}},` +
		`"updates.status":{"status":"fail","details":{` + details + `}}}}`
}

// TestCheckUpdateReadsCodexDoctor pins the parse rules: the JSON decides even
// when doctor exits 1, the vendor's own verdict beats the version comparison,
// and a report that cannot answer is an error - never a fabricated "up to
// date".
func TestCheckUpdateReadsCodexDoctor(t *testing.T) {
	tests := []struct {
		name    string
		doctor  string
		exit    int
		want    oarlock.UpdateInfo
		wantErr []string // substrings; empty means no error
	}{
		{
			name: "newer version is available",
			doctor: doctorBody(`"latest version":"0.166.0",` +
				`"latest version status":"a newer version is available: 0.166.0",` +
				`"update action":"npm install -g @openai/codex"`),
			want: oarlock.UpdateInfo{Available: true, Current: "0.165.0", Latest: "0.166.0"},
		},
		{
			// The vendor's verdict wins over the comparison: doctor says this
			// copy is not behind, so the check must not invent an update from
			// two version strings that differ.
			name: "not older",
			doctor: doctorBody(`"latest version":"0.166.0",` +
				`"latest version status":"the latest version 0.166.0 is not older than the installed version",` +
				`"update action":"npm install -g @openai/codex"`),
			want: oarlock.UpdateInfo{Available: false, Current: "0.165.0", Latest: "0.166.0"},
		},
		{
			// No verdict in the row: Latest != Current is the fallback.
			name:   "no verdict falls back to the version comparison",
			doctor: doctorBody(`"latest version":"0.166.0"`),
			want:   oarlock.UpdateInfo{Available: true, Current: "0.165.0", Latest: "0.166.0"},
		},
		{
			// Live behaviour: doctor exits 1 whenever any row fails, and the
			// valid JSON it printed is still the answer.
			name: "exit 1 with valid JSON still answers",
			exit: 1,
			doctor: doctorBody(`"latest version":"0.166.0",` +
				`"latest version status":"a newer version is available: 0.166.0"`),
			want: oarlock.UpdateInfo{Available: true, Current: "0.165.0", Latest: "0.166.0"},
		},
		{
			// The live 0.165.0 shape on a blocked network: the probe detail is
			// the only explanation, doctor prints no latest version at all, and
			// the row mixes strings with an array.
			name: "missing latest version",
			doctor: doctorBody(`"check for update on startup":"true",` +
				`"latest version probe":"HTTP 403 Forbidden",` +
				`"npm update target":"not inspected: the release probe failed",` +
				`"update action":"npm install -g @openai/codex",` +
				`"version cache":["0.165.0"]`),
			wantErr: []string{`no "latest version"`, "latest version probe: HTTP 403 Forbidden"},
		},
		{
			// A copied binary or an app bundle: `codex update` cannot move it,
			// so a version comparison would promise an update it cannot install.
			name: "manual or unknown update action",
			doctor: doctorBody(`"latest version":"0.166.0",` +
				`"latest version status":"a newer version is available: 0.166.0",` +
				`"update action":"manual or unknown"`),
			wantErr: []string{"manual or unknown", "cannot update this copy"},
		},
		{
			name:    "no JSON in stdout",
			doctor:  "doctor: could not read the update row\n",
			wantErr: []string{"printed no JSON"},
		},
		{
			name:    "unreadable JSON",
			doctor:  `{"checks": }`,
			wantErr: []string{"printed unreadable JSON"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			log := fakeCodexDoctor(t, tc.doctor, tc.exit)
			info, err := upgrader{}.CheckUpdate(context.Background())
			// The check ran both reads whatever the outcome: the version first,
			// the report second.
			if got, want := logLines(t, log), []string{"--version", "doctor --json"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("invocations = %q, want %q", got, want)
			}
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("CheckUpdate = %+v, want an error", info)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %v, want it to contain %q", err, want)
					}
				}
				if info != (oarlock.UpdateInfo{}) {
					t.Errorf("UpdateInfo = %+v on error, want the zero value", info)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckUpdate: %v", err)
			}
			if info != tc.want {
				t.Fatalf("UpdateInfo = %+v, want %+v", info, tc.want)
			}
		})
	}
}

func TestVersionTokenStripsTheProductPrefixAndV(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"codex-cli 0.165.0", "0.165.0"},
		{"codex-cli v0.166.0", "0.166.0"},
		{"0.165.0", "0.165.0"},
		{"", ""},
	} {
		if got := versionToken(tc.line); got != tc.want {
			t.Errorf("versionToken(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

func TestInventoryDeclaresFourUnsupportedSections(t *testing.T) {
	inv, err := inventories{}.Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	want := []oarlock.InventoryKind{
		oarlock.InventoryModels, oarlock.InventorySkills, oarlock.InventoryCommands, oarlock.InventoryTools,
	}
	if len(inv.Sections) != len(want) {
		t.Fatalf("sections = %d, want %d: %+v", len(inv.Sections), len(want), inv.Sections)
	}
	for i, kind := range want {
		section := inv.Sections[i]
		if section.Kind != kind {
			t.Errorf("section %d = %q, want %q", i, section.Kind, kind)
		}
		if section.Status != oarlock.InventoryUnsupported {
			t.Errorf("section %s status = %q, want %q", kind, section.Status, oarlock.InventoryUnsupported)
		}
		if strings.TrimSpace(section.Reason) == "" {
			t.Errorf("section %s must name the gap", kind)
		}
		if len(section.Items) != 0 {
			t.Errorf("section %s carries items although it is unsupported", kind)
		}
	}
}
