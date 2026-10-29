package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
)

func writeScript(t *testing.T, name, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake executables are POSIX shell scripts; Windows builds are compile-checked only")
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	return path
}

func TestRun_NonZeroExitIsDataNotError(t *testing.T) {
	script := writeScript(t, "cli", "echo out; echo err >&2; exit 3\n")
	res, err := Run(context.Background(), Command{Path: script}, 5*time.Second)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 3 || res.Stdout != "out\n" || res.Stderr != "err\n" {
		t.Fatalf("result = %+v", res)
	}
}

func TestVersion_FirstNonEmptyLine(t *testing.T) {
	script := writeScript(t, "cli", "echo; echo 1.2.3; echo ignored\n")
	got, err := Version(context.Background(), script)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != "1.2.3" {
		t.Fatalf("version = %q", got)
	}
}

func TestProbe_FoundMissingAndUnreadableVersion(t *testing.T) {
	script := writeScript(t, "oar-test-probe", "echo 9.9.9\n")
	t.Setenv("PATH", filepath.Dir(script)+string(os.PathListSeparator)+os.Getenv("PATH"))

	inst, err := (Probe{Name: "oar-test-probe"}).Installation(context.Background())
	if err != nil {
		t.Fatalf("Probe found: %v", err)
	}
	if !inst.Available || inst.Version != "9.9.9" || inst.Source != "path" {
		t.Fatalf("installation = %+v", inst)
	}

	missing, err := (Probe{Name: "oar-test-absent"}).Installation(context.Background())
	if err != nil {
		t.Fatalf("Probe absent: %v", err)
	}
	if missing.Available {
		t.Fatalf("absent probe = %+v", missing)
	}

	broken := writeScript(t, "oar-test-broken", "exit 7\n")
	t.Setenv("PATH", filepath.Dir(broken)+string(os.PathListSeparator)+os.Getenv("PATH"))
	inst, err = (Probe{Name: "oar-test-broken"}).Installation(context.Background())
	if err == nil {
		t.Fatal("unreadable version: want an error")
	}
	if !inst.Available || inst.Path == "" {
		t.Fatalf("unreadable version should still be available: %+v", inst)
	}
}

// fileProbe reports an installation once the marker file exists.
type fileProbe struct{ marker string }

func (p fileProbe) Installation(context.Context) (oarlock.Installation, error) {
	if _, err := os.Stat(p.marker); err == nil {
		return oarlock.Installation{Available: true, Version: "2.0.0", Path: p.marker}, nil
	}
	return oarlock.Installation{Available: false}, nil
}

func TestInstaller_PlanDoesNotRunAndInstallIsIdempotent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the installer line is a POSIX shell command; the runner is covered on POSIX")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "installed")
	runs := filepath.Join(dir, "runs")
	inst := Installer{
		Line:  "echo run >> " + runs + "; touch " + marker,
		Probe: fileProbe{marker: marker},
	}

	plan, err := inst.InstallPlan(context.Background(), oarlock.InstallRequest{})
	if err != nil {
		t.Fatalf("InstallPlan: %v", err)
	}
	if !plan.ChangesMachine || len(plan.Command) != 3 || plan.Command[0] != "sh" || plan.Command[1] != "-c" {
		t.Fatalf("plan = %+v", plan)
	}
	if _, err := os.Stat(runs); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plan executed the installer: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plan created the marker: %v", err)
	}

	result, err := inst.Install(context.Background(), oarlock.InstallRequest{})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !result.Installation.Available || result.Installation.Version != "2.0.0" {
		t.Fatalf("result = %+v", result)
	}
	if data, err := os.ReadFile(runs); err != nil || strings.Count(string(data), "run") != 1 {
		t.Fatalf("installer runs = %q, %v", data, err)
	}

	// Second call: the probe says available, so nothing runs again.
	if _, err := inst.Install(context.Background(), oarlock.InstallRequest{}); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if data, err := os.ReadFile(runs); err != nil || strings.Count(string(data), "run") != 1 {
		t.Fatalf("idempotent install ran again: %q, %v", data, err)
	}
}

func TestInstaller_ExitZeroWithoutInstallationIsFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the installer line is a POSIX shell command; the runner is covered on POSIX")
	}
	inst := Installer{
		Line:  "echo pretending to install",
		Probe: fileProbe{marker: filepath.Join(t.TempDir(), "never")},
	}
	if _, err := inst.Install(context.Background(), oarlock.InstallRequest{}); err == nil {
		t.Fatal("install without a resulting installation: want an error")
	}
}

func TestInstaller_NonZeroInstallerWithInstallationIsSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the installer line is a POSIX shell command; the runner is covered on POSIX")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "installed")
	inst := Installer{
		Line:  "touch " + marker + "; exit 9",
		Probe: fileProbe{marker: marker},
	}
	if _, err := inst.Install(context.Background(), oarlock.InstallRequest{}); err != nil {
		t.Fatalf("probe decides success, not the exit code: %v", err)
	}
}

func TestUpgrade_VersionDecides(t *testing.T) {
	dir := t.TempDir()
	versionFile := filepath.Join(dir, "version")
	if err := os.WriteFile(versionFile, []byte("1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeScript(t, "upgrader", `
case "$1" in
--version) cat "$OAR_TEST_VERSION_FILE";;
update) printf '2.0.0\n' > "$OAR_TEST_VERSION_FILE"; echo updated;;
esac
`)
	t.Setenv("OAR_TEST_VERSION_FILE", versionFile)

	result, err := Upgrade(context.Background(), script, []string{"update"}, 5*time.Second)
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if result.From != "1.0.0" || result.To != "2.0.0" {
		t.Fatalf("result = %+v", result)
	}
}

func TestUpgrade_UnchangedVersionIsExplicitFailure(t *testing.T) {
	dir := t.TempDir()
	versionFile := filepath.Join(dir, "version")
	if err := os.WriteFile(versionFile, []byte("1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := writeScript(t, "upgrader", `
case "$1" in
--version) cat "$OAR_TEST_VERSION_FILE";;
update) echo "all good"; exit 0;;
esac
`)
	t.Setenv("OAR_TEST_VERSION_FILE", versionFile)

	result, err := Upgrade(context.Background(), script, []string{"update"}, 5*time.Second)
	if !errors.Is(err, ErrVersionUnchanged) {
		t.Fatalf("err = %v, want ErrVersionUnchanged", err)
	}
	if result.From != "1.0.0" || result.To != "1.0.0" {
		t.Fatalf("result = %+v", result)
	}
}

func TestCaptureLogin_ReturnsURLWithoutWaitingForStdin(t *testing.T) {
	script := writeScript(t, "login", `
echo "Open https://example.test/device and sign in"
# Keep running with stdin open and never read it: the capture must not wait.
sleep 30
`)
	started := time.Now()
	challenge, output, err := CaptureLogin(context.Background(),
		Command{Path: script},
		regexpForTest(),
		10*time.Second)
	if err != nil {
		t.Fatalf("CaptureLogin: %v", err)
	}
	if challenge.Kind != "url" || challenge.URL != "https://example.test/device" {
		t.Fatalf("challenge = %+v", challenge)
	}
	if !strings.Contains(output, "Open https://example.test/device") {
		t.Fatalf("output = %q", output)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("CaptureLogin waited for the helper: %v", elapsed)
	}
}

func TestCaptureLogin_NoChallengeIsAnError(t *testing.T) {
	script := writeScript(t, "login", "echo nope >&2; exit 4\n")
	if _, _, err := CaptureLogin(context.Background(), Command{Path: script}, regexpForTest(), 5*time.Second); err == nil {
		t.Fatal("no challenge: want an error")
	}
}

func TestCaptureLogin_TimeoutIsAnError(t *testing.T) {
	script := writeScript(t, "login", "sleep 30\n")
	if _, _, err := CaptureLogin(context.Background(), Command{Path: script}, regexpForTest(), 200*time.Millisecond); err == nil {
		t.Fatal("timeout: want an error")
	}
}

func TestFallbackPath(t *testing.T) {
	if got := FallbackPath(".local/bin/tool"); got != "" && !strings.HasSuffix(got, filepath.Join(".local", "bin", "tool")) {
		t.Fatalf("FallbackPath = %q", got)
	}
}

func regexpForTest() *regexp.Regexp { return regexp.MustCompile(`https?://\S+`) }
