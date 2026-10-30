package claude

// Operating capabilities (issue #23) against a fake `claude`: the whole ops
// surface is argv, exit codes and one line of output, so a #!/bin/sh script at
// the front of PATH pins all of it. No real claude is needed or used, and
// nothing reaches the network: the installer test fakes `curl` and `bash` as
// well, so a real `curl ... | sh` can never run.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/ops"
)

// fakePATH is a temp directory holding POSIX `#!/bin/sh` fakes, kept at the
// front of PATH. Fakes are written into one directory so a test can shadow
// several vendor commands (`claude`, `curl`, `bash`) at once.
type fakePATH struct{ dir string }

// newFakePATH creates the directory and prepends it to PATH. The tests are
// compile-checked on Windows (GOOS=windows go build ./...) but the fakes are
// shell scripts, so the behavioural tests skip there.
func newFakePATH(t *testing.T) *fakePATH {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake executables are POSIX shell scripts; Windows builds are compile-checked only")
	}
	f := &fakePATH{dir: t.TempDir()}
	t.Setenv("PATH", f.dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

// add writes one fake executable into the directory and returns its path.
func (f *fakePATH) add(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(f.dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	return path
}

// fakeClaude is the common setup: one fake `claude`, and an isolated HOME so a
// real ~/.local/bin/claude on the test host cannot answer instead of the fake.
func fakeClaude(t *testing.T, body string) *fakePATH {
	t.Helper()
	f := newFakePATH(t)
	f.add(t, "claude", body)
	t.Setenv("HOME", t.TempDir())
	return f
}

// isolatedPATH leaves only dir (plus the POSIX shell directories) on PATH:
// used where a real `claude` elsewhere on the host must not be found.
func isolatedPATH(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", strings.Join([]string{dir, "/bin", "/usr/bin"}, string(os.PathListSeparator)))
}

func TestRuntime_WiresEveryOperation(t *testing.T) {
	rt := Runtime()
	if rt.ID != "claude" {
		t.Fatalf("ID = %q", rt.ID)
	}
	if rt.Session == nil {
		t.Fatal("Session is nil")
	}
	if ell := len(rt.Refused); ell == 0 || ell != len(Refused()) {
		t.Fatalf("Refused has %d entries, Refused() has %d", ell, len(Refused()))
	}
	// Refused is the table Open checks, so a refused option is refused through
	// the runtime too.
	if err := rt.CheckOptions(oarlock.Options{ServiceTier: "fast"}); err == nil {
		t.Fatal("CheckOptions(serviceTier) = nil, want a refusal")
	}

	probe, ok := rt.Probe.(ops.Probe)
	if !ok || probe.Name != "claude" {
		t.Fatalf("Probe = %#v (%T), want ops.Probe{Name: claude}", rt.Probe, rt.Probe)
	}
	if _, ok := rt.Install.(installer); !ok {
		t.Fatalf("Install = %T, want installer", rt.Install)
	}
	if _, ok := rt.Login.(loginProvider); !ok {
		t.Fatalf("Login = %T, want loginProvider", rt.Login)
	}
	if _, ok := rt.Upgrade.(upgrader); !ok {
		t.Fatalf("Upgrade = %T, want upgrader", rt.Upgrade)
	}
	if _, ok := rt.Inventory.(inventories); !ok {
		t.Fatalf("Inventory = %T, want inventories", rt.Inventory)
	}
	// Models is present since issue #98: the catalogue comes from claude's own
	// `list_models` control request (models.go), not from a placeholder list. A
	// failure there is typed (ErrNotAuthenticated / UnsupportedOperation), so an
	// empty answer can only ever be claude saying it has no models.
	if _, ok := rt.Models.(claudeModels); !ok {
		t.Fatalf("Models = %T, want claudeModels", rt.Models)
	}
	if _, ok := rt.Usage.(accountUsageReader); !ok {
		t.Fatalf("Usage = %T, want accountUsageReader", rt.Usage)
	}
}

func TestProbe_FoundAndMissing(t *testing.T) {
	fakeClaude(t, `if [ "$1" = --version ]; then echo "2.0.9 (Claude Code)"; exit 0; fi
exit 2
`)

	inst, err := Runtime().Probe.Installation(context.Background())
	if err != nil {
		t.Fatalf("probe found: %v", err)
	}
	if !inst.Available || inst.Version != "2.0.9 (Claude Code)" || inst.Source != "path" || inst.Path == "" {
		t.Fatalf("installation = %+v", inst)
	}

	// Nothing on PATH and nothing in ~/.local/bin: absent, no error.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	missing, err := Runtime().Probe.Installation(context.Background())
	if err != nil {
		t.Fatalf("probe missing: %v", err)
	}
	if missing.Available || missing.Path != "" {
		t.Fatalf("missing installation = %+v", missing)
	}
}

func TestInstallPlan_IsPureAndOfficial(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "executed")
	// Anything that ran the plan would run this fake curl.
	f := fakeClaude(t, "exit 1\n")
	f.add(t, "curl", "touch '"+marker+"'\n")

	plan, err := Runtime().Install.InstallPlan(context.Background(), oarlock.InstallRequest{})
	if err != nil {
		t.Fatalf("InstallPlan: %v", err)
	}
	want := []string{"sh", "-c", `curl -fsSL https://claude.ai/install.sh | bash`}
	if strings.Join(plan.Command, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("plan.Command = %q, want %q", plan.Command, want)
	}
	if !plan.ChangesMachine {
		t.Fatal("ChangesMachine = false: installing does change the machine")
	}
	if plan.Reason == "" {
		t.Fatal("plan.Reason is empty")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("InstallPlan executed the installer: %v", err)
	}
}

func TestInstallPlan_RefusesCallerLine(t *testing.T) {
	fakeClaude(t, "exit 1\n")
	_, err := Runtime().Install.InstallPlan(context.Background(), oarlock.InstallRequest{Line: "npm install -g claude"})
	var unsupported *oarlock.UnsupportedOperation
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want *oarlock.UnsupportedOperation", err)
	}
	if unsupported.Operation != "install" || unsupported.Reason == "" {
		t.Fatalf("unsupported = %+v", unsupported)
	}
}

// TestInstall_ExitZeroWithoutInstallationIsAnErrorAndRetryable runs the official
// line with faked `curl`/`bash`: the pipeline succeeds but installs nothing, so
// the probe decides failure. A second attempt runs again (nothing was recorded
// locally) and the isolated HOME stays empty (oarlock writes no config).
func TestInstall_ExitZeroWithoutInstallationIsAnErrorAndRetryable(t *testing.T) {
	f := newFakePATH(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	isolatedPATH(t, f.dir)

	curlLog := filepath.Join(t.TempDir(), "curl.log")
	staged := filepath.Join(t.TempDir(), "staged")
	f.add(t, "curl", "echo \"$@\" >> '"+curlLog+"'\nexit 0\n")
	f.add(t, "bash", "cat > '"+staged+"'\nexit 0\n")

	ctx := context.Background()
	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := Runtime().Install.Install(ctx, oarlock.InstallRequest{}); err == nil {
			t.Fatalf("attempt %d: install without an installation succeeded", attempt)
		}
		log, err := os.ReadFile(curlLog)
		if err != nil {
			t.Fatalf("attempt %d: the official line did not run: %v", attempt, err)
		}
		if got := strings.Count(string(log), "https://claude.ai/install.sh"); got != attempt {
			t.Fatalf("attempt %d: curl ran %d times: %q", attempt, got, log)
		}
	}

	if data, err := os.ReadFile(staged); err != nil || len(data) != 0 {
		t.Fatalf("installer body = %q, %v; want the fake curl to pipe nothing", data, err)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a failed install wrote local state: %v", entries)
	}
}

func TestAuthStatus_LoggedIn(t *testing.T) {
	fakeClaude(t, `if [ "$1" = auth ] && [ "$2" = status ] && [ "$3" = --json ]; then
  echo '{"loggedIn":true,"email":"dev@example.test","subscriptionType":"max"}'
  exit 0
fi
exit 9
`)
	status, err := Runtime().Login.AuthStatus(context.Background())
	if err != nil {
		t.Fatalf("AuthStatus: %v", err)
	}
	if !status.Authenticated || status.Account != "dev@example.test" || status.Detail != "max" {
		t.Fatalf("status = %+v", status)
	}
}

func TestAuthStatus_AuthMethodIsTheDetailFallback(t *testing.T) {
	fakeClaude(t, `if [ "$1" = auth ] && [ "$2" = status ]; then
  echo '{"loggedIn":true,"email":"dev@example.test","authMethod":"oauth_token"}'
  exit 0
fi
exit 9
`)
	status, err := (loginProvider{}).AuthStatus(context.Background())
	if err != nil {
		t.Fatalf("AuthStatus: %v", err)
	}
	if !status.Authenticated || status.Detail != "oauth_token" {
		t.Fatalf("status = %+v", status)
	}
}

func TestAuthStatus_ExitOneIsLoggedOut(t *testing.T) {
	fakeClaude(t, `if [ "$1" = auth ] && [ "$2" = status ]; then
  echo "Not logged in" >&2
  exit 1
fi
exit 9
`)
	status, err := (loginProvider{}).AuthStatus(context.Background())
	if err != nil {
		t.Fatalf("exit 1 is Claude's documented logged-out answer, got %v", err)
	}
	if status.Authenticated {
		t.Fatalf("status = %+v, want logged out", status)
	}
}

func TestAuthStatus_UnreadableJSONIsAnError(t *testing.T) {
	fakeClaude(t, `if [ "$1" = auth ] && [ "$2" = status ]; then
  echo "signed in, probably"
  exit 0
fi
exit 9
`)
	if _, err := (loginProvider{}).AuthStatus(context.Background()); err == nil {
		t.Fatal("unreadable JSON: want an error, not a logged-out answer")
	}
}

func TestAuthStatus_MissingLoggedInIsAnError(t *testing.T) {
	fakeClaude(t, `if [ "$1" = auth ] && [ "$2" = status ]; then
  echo '{"email":"dev@example.test"}'
  exit 0
fi
exit 9
`)
	if _, err := (loginProvider{}).AuthStatus(context.Background()); err == nil {
		t.Fatal("missing loggedIn: want an error, not a guessed state")
	}
}

func TestAuthStatus_UnexpectedExitIsAnError(t *testing.T) {
	fakeClaude(t, `if [ "$1" = auth ] && [ "$2" = status ]; then
  echo "boom" >&2
  exit 3
fi
exit 9
`)
	status, err := (loginProvider{}).AuthStatus(context.Background())
	if err == nil {
		t.Fatal("exit 3: want an error, never a logged-out claim")
	}
	if status.Authenticated {
		t.Fatalf("status = %+v", status)
	}
}

func TestAuthStatus_TimeoutIsAnErrorNotLoggedOut(t *testing.T) {
	fakeClaude(t, `if [ "$1" = auth ] && [ "$2" = status ]; then sleep 30; fi
exit 9
`)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	status, err := (loginProvider{}).AuthStatus(ctx)
	if err == nil {
		t.Fatal("a timeout says nothing about the credential: want an error, never a logged-out answer")
	}
	if status.Authenticated {
		t.Fatalf("status = %+v", status)
	}
}

func TestLogout_NonZeroIsAnError(t *testing.T) {
	fakeClaude(t, `if [ "$1" = auth ] && [ "$2" = logout ]; then
  echo "cannot sign out" >&2
  exit 4
fi
exit 9
`)
	if err := (loginProvider{}).Logout(context.Background()); err == nil {
		t.Fatal("logout exit 4: want an error")
	}
}

func TestLogout_StillLoggedInIsAnError(t *testing.T) {
	fakeClaude(t, `if [ "$1" = auth ] && [ "$2" = logout ]; then exit 0; fi
if [ "$1" = auth ] && [ "$2" = status ]; then
  echo '{"loggedIn":true,"email":"dev@example.test"}'
  exit 0
fi
exit 9
`)
	err := (loginProvider{}).Logout(context.Background())
	if err == nil {
		t.Fatal("logout exited 0 but the status still says logged in: want an error")
	}
	if !strings.Contains(err.Error(), "still logged in") {
		t.Fatalf("err = %v, want it to say still logged in", err)
	}
}

func TestLogout_SignsOut(t *testing.T) {
	fakeClaude(t, `if [ "$1" = auth ] && [ "$2" = logout ]; then exit 0; fi
if [ "$1" = auth ] && [ "$2" = status ]; then
  echo '{"loggedIn":false}'
  exit 0
fi
exit 9
`)
	if err := (loginProvider{}).Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
}

// TestLogin_ReturnsURLWithoutWaitingForStdin is the non-interactive login rule:
// the helper sleeps with stdin open, and the URL must still come back at once,
// with BROWSER suppressed so the host does not get a browser window.
func TestLogin_ReturnsURLWithoutWaitingForStdin(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "browser")
	fakeClaude(t, `if [ "$1" = auth ] && [ "$2" = login ]; then
  echo "$BROWSER" > '`+seen+`'
  echo "Opening a browser at https://claude.ai/oauth/authorize?code=oar-test to finish signing in"
  sleep 30
fi
exit 9
`)

	started := time.Now()
	challenge, err := Runtime().Login.Login(context.Background())
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Login waited for the helper: %v", elapsed)
	}
	if challenge.Kind != "url" || challenge.URL != "https://claude.ai/oauth/authorize?code=oar-test" {
		t.Fatalf("challenge = %+v", challenge)
	}
	if challenge.Instructions == "" {
		t.Fatal("challenge.Instructions is empty")
	}
	browser, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("the fake login did not record BROWSER: %v", err)
	}
	if strings.TrimSpace(string(browser)) != "true" {
		t.Fatalf("BROWSER = %q, want true (the CLI must not open a browser on the host)", browser)
	}
}

func TestLogin_NoChallengeIsAnError(t *testing.T) {
	fakeClaude(t, `if [ "$1" = auth ] && [ "$2" = login ]; then
  echo "no url on this line"
  exit 3
fi
exit 9
`)
	if _, err := (loginProvider{}).Login(context.Background()); err == nil {
		t.Fatal("login without a URL: want an error")
	}
}

func TestLogin_TimeoutIsAnError(t *testing.T) {
	fakeClaude(t, `if [ "$1" = auth ] && [ "$2" = login ]; then sleep 30; fi
exit 9
`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := (loginProvider{}).Login(ctx); err == nil {
		t.Fatal("an unresponsive helper: want a bounded error, not a hang")
	}
}

func TestUpgrade_UnchangedVersionIsExplicitFailure(t *testing.T) {
	fakeClaude(t, `if [ "$1" = --version ]; then echo "1.0.0"; exit 0; fi
if [ "$1" = update ]; then echo "all up to date"; exit 0; fi
exit 9
`)
	if err := Runtime().Upgrade.Upgrade(context.Background()); !errors.Is(err, ops.ErrVersionUnchanged) {
		t.Fatalf("err = %v, want ops.ErrVersionUnchanged", err)
	}
}

func TestUpgrade_ChangedVersionIsSuccess(t *testing.T) {
	versionFile := filepath.Join(t.TempDir(), "version")
	if err := os.WriteFile(versionFile, []byte("1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeClaude(t, `if [ "$1" = --version ]; then cat '`+versionFile+`'; exit 0; fi
if [ "$1" = update ]; then echo "2.0.0" > '`+versionFile+`'; exit 0; fi
exit 9
`)
	if err := (upgrader{}).Upgrade(context.Background()); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
}

// TestCheckUpdate_IsTypedUnsupported pins the refusal and the live evidence
// behind it: `claude update` checks and installs in one step (claude 2.1.312's
// `update --help` lists no --check) and the native channel read is an HTTP
// release lookup, so there is no local command to run — running the updater
// anyway would install, not check.
func TestCheckUpdate_IsTypedUnsupported(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	fakeClaude(t, "touch '"+marker+"'\nexit 0\n")
	info, err := Runtime().Upgrade.CheckUpdate(context.Background())
	var unsupported *oarlock.UnsupportedOperation
	if !errors.As(err, &unsupported) {
		t.Fatalf("CheckUpdate = %+v, %v; want *oarlock.UnsupportedOperation", info, err)
	}
	if unsupported.Operation != "check-update" {
		t.Fatalf("operation = %q, want check-update", unsupported.Operation)
	}
	// The reason is the gap, in one line, and it matches the live evidence.
	for _, want := range []string{"--check", "one step", "release lookup"} {
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

func TestInventory_FourExplicitUnsupportedSections(t *testing.T) {
	inv, err := Runtime().Inventory.Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	want := []oarlock.InventoryKind{
		oarlock.InventoryModels,
		oarlock.InventorySkills,
		oarlock.InventoryCommands,
		oarlock.InventoryTools,
	}
	if len(inv.Sections) != len(want) {
		t.Fatalf("sections = %d, want %d: %+v", len(inv.Sections), len(want), inv.Sections)
	}
	for i, section := range inv.Sections {
		if section.Kind != want[i] {
			t.Fatalf("section %d = %q, want %q", i, section.Kind, want[i])
		}
		if section.Status != oarlock.InventoryUnsupported {
			t.Fatalf("section %q status = %q, want unsupported", section.Kind, section.Status)
		}
		if strings.TrimSpace(section.Reason) == "" {
			t.Fatalf("section %q has no reason naming the gap", section.Kind)
		}
		if len(section.Items) != 0 {
			t.Fatalf("section %q carries items: %+v", section.Kind, section.Items)
		}
	}
}
