package acp

// Install plans and installs against fake executables (issue #69, docs/ops.md
// §3/§5). The whole surface is argv plus PATH, so a #!/bin/sh script at the
// front of PATH pins it: a plan that executes anything runs a fake and leaves
// its marker, a plan that executes nothing leaves the marker missing. The fakes
// are POSIX shell scripts, so the behavioural tests skip on Windows (the
// package still compiles there).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
)

// fakePATH is a temp directory of `#!/bin/sh` fakes at the front of an
// otherwise minimal PATH, with an empty HOME: a real vendor binary on the host
// can neither answer a probe nor be run by an install.
type fakePATH struct{ dir string }

// newFakePATH creates the directory, puts it first on PATH and points HOME at a
// fresh temp directory (so no known install location from the host is found).
func newFakePATH(t *testing.T) *fakePATH {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake executables are POSIX shell scripts; Windows builds are compile-checked only")
	}
	f := &fakePATH{dir: t.TempDir()}
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", strings.Join([]string{f.dir, "/bin", "/usr/bin"}, sep))
	t.Setenv("HOME", t.TempDir())
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

// runCount is how many times a fake appended its "ran" line; a missing log is 0.
func runCount(t *testing.T, log string) int {
	t.Helper()
	data, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("read %s: %v", log, err)
	}
	return strings.Count(string(data), "ran")
}

// TestInstallPlan_IsPureAndExact pins the argv of every plan and proves a plan
// runs nothing: `sh`, `curl`, `bash` and `npm` are all fakes that would leave a
// marker if the plan had executed. The two opencode lines are here because they
// go through the same `sh -c` shape as the four scripts.
func TestInstallPlan_IsPureAndExact(t *testing.T) {
	cases := []struct {
		id   string
		line string
		want []string
	}{
		{"grok", "", []string{"sh", "-c", `curl -fsSL https://x.ai/cli/install.sh | bash`}},
		{"kimi", "", []string{"sh", "-c", `curl -fsSL https://code.kimi.com/kimi-code/install.sh | bash`}},
		{"qwen", "", []string{"sh", "-c", `curl -fsSL https://qwen-code-assets.oss-cn-hangzhou.aliyuncs.com/installation/install-qwen-standalone.sh | bash -s -- --method standalone --no-modify-path`}},
		{"qoder", "", []string{"sh", "-c", `curl -fsSL https://qoder.com/install | bash -s -- --skip-path`}},
		{"opencode", "v1", []string{"sh", "-c", "npm install -g " + opencodeV1Package}},
		{"opencode", "v2", []string{"sh", "-c", "npm install -g " + opencodeV2Package}},
	}
	for _, tc := range cases {
		name := tc.id
		if tc.line != "" {
			name += "-" + tc.line
		}
		t.Run(name, func(t *testing.T) {
			f := newFakePATH(t)
			marker := filepath.Join(t.TempDir(), "executed")
			for _, exe := range []string{"sh", "curl", "bash", "npm"} {
				f.add(t, exe, "touch '"+marker+"'\n")
			}

			rt := Profile{ID: tc.id}.Runtime()
			if rt.Install == nil {
				t.Fatalf("%s has no Installer; every ACP profile but antigravity has one", tc.id)
			}
			plan, err := rt.Install.InstallPlan(context.Background(), oarlock.InstallRequest{Line: tc.line})
			if err != nil {
				t.Fatalf("InstallPlan: %v", err)
			}
			if strings.Join(plan.Command, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("plan.Command = %q, want %q", plan.Command, tc.want)
			}
			if !plan.ChangesMachine {
				t.Error("ChangesMachine = false: running an installer does change the machine")
			}
			if plan.Reason == "" {
				t.Error("plan.Reason is empty")
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("InstallPlan executed the installer (the fake shell left %s)", marker)
			}
		})
	}
}

// TestOpenCodeInstallPlan_RefusesUnnamedAndUnknownLines pins the two typed
// refusals of the two-line runtime, for both InstallPlan and Install: an
// unnamed line must not be guessed, and neither refusal may run anything.
func TestOpenCodeInstallPlan_RefusesUnnamedAndUnknownLines(t *testing.T) {
	f := newFakePATH(t)
	marker := filepath.Join(t.TempDir(), "executed")
	for _, exe := range []string{"sh", "npm"} {
		f.add(t, exe, "touch '"+marker+"'\n")
	}
	rt := Profile{ID: "opencode"}.Runtime()

	cases := []struct {
		line string
		want string
	}{
		{"", "this runtime ships two install lines (v1: npm " + opencodeV1Package + ", v2: npm " + opencodeV2Package + "); InstallRequest.Line must name one"},
		{"v3", `unknown install line "v3": opencode installs v1 or v2`},
	}
	for _, tc := range cases {
		name := tc.line
		if name == "" {
			name = "empty-line"
		}
		t.Run(name, func(t *testing.T) {
			_, planErr := rt.Install.InstallPlan(context.Background(), oarlock.InstallRequest{Line: tc.line})
			_, installErr := rt.Install.Install(context.Background(), oarlock.InstallRequest{Line: tc.line})
			for _, err := range []error{planErr, installErr} {
				var unsupported *oarlock.UnsupportedOperation
				if !errors.As(err, &unsupported) {
					t.Fatalf("err = %v, want *oarlock.UnsupportedOperation", err)
				}
				if unsupported.Operation != "install" {
					t.Fatalf("Operation = %q, want install", unsupported.Operation)
				}
				if unsupported.Reason != tc.want {
					t.Fatalf("Reason = %q, want %q", unsupported.Reason, tc.want)
				}
			}
		})
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused line ran something (the fake shell left %s)", marker)
	}
}

// TestInstall_SingleLineProfileRefusesCallerLine keeps the other half of the
// line rule: the four vendors that ship one line refuse a named line instead of
// running the caller's choice.
func TestInstall_SingleLineProfileRefusesCallerLine(t *testing.T) {
	f := newFakePATH(t)
	marker := filepath.Join(t.TempDir(), "executed")
	f.add(t, "sh", "touch '"+marker+"'\n")

	_, err := Profile{ID: "grok"}.Runtime().Install.InstallPlan(context.Background(), oarlock.InstallRequest{Line: "npm install -g grok"})
	var unsupported *oarlock.UnsupportedOperation
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want *oarlock.UnsupportedOperation", err)
	}
	if unsupported.Operation != "install" || !strings.Contains(unsupported.Reason, "one release line") {
		t.Fatalf("unsupported = %+v, want the one-release-line refusal", unsupported)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused line ran something (the fake shell left %s)", marker)
	}
}

// TestInstall_RunsOnceAndReProbeDecides proves the install contract end to end:
// the line runs exactly once, the re-probe (the vendor's known install path) is
// what reports success, and a second call short-circuits because the probe now
// finds the copy the installer wrote.
func TestInstall_RunsOnceAndReProbeDecides(t *testing.T) {
	f := newFakePATH(t)
	home := os.Getenv("HOME")
	runs := filepath.Join(t.TempDir(), "runs")
	// The fake shell stands in for grok's installer: it writes the file the real
	// one links at ~/.grok/bin/grok, which is where the probe looks.
	f.add(t, "sh", "echo ran >> '"+runs+"'\nmkdir -p \"$HOME/.grok/bin\" && : > \"$HOME/.grok/bin/grok\"\n")

	rt := Profile{ID: "grok"}.Runtime()
	before, err := rt.Probe.Installation(context.Background())
	if err != nil {
		t.Fatalf("probe before install: %v", err)
	}
	if before.Available {
		t.Fatalf("installation before install = %+v, want absent", before)
	}

	got, err := rt.Install.Install(context.Background(), oarlock.InstallRequest{})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	want := filepath.Join(home, ".grok", "bin", "grok")
	if !got.Installation.Available || got.Installation.Path != want || got.Installation.Source != "known" {
		t.Fatalf("installation after install = %+v, want the known path %s", got.Installation, want)
	}
	if got.Installation.Version != "" {
		t.Fatalf("Version = %q: the ACP probe reads no version", got.Installation.Version)
	}
	if n := runCount(t, runs); n != 1 {
		t.Fatalf("the installer ran %d times, want exactly once", n)
	}

	// The probe already finds a copy, so the second call runs nothing.
	again, err := rt.Install.Install(context.Background(), oarlock.InstallRequest{})
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if !again.Installation.Available {
		t.Fatalf("second installation = %+v, want available", again.Installation)
	}
	if n := runCount(t, runs); n != 1 {
		t.Fatalf("an already installed runtime ran the installer again (%d runs)", n)
	}
}

// TestInstall_ReProbeFailureIsAnError is the other half: the pipeline exits 0
// and installs nothing, so the probe decides failure. The result carries the
// re-probe (after), the probe before the call was absent, and the exit code is
// quoted in the error rather than trusted.
func TestInstall_ReProbeFailureIsAnError(t *testing.T) {
	f := newFakePATH(t)
	runs := filepath.Join(t.TempDir(), "runs")
	f.add(t, "sh", "echo ran >> '"+runs+"'\nexit 0\n")

	rt := Profile{ID: "kimi"}.Runtime()
	before, err := rt.Probe.Installation(context.Background())
	if err != nil {
		t.Fatalf("probe before install: %v", err)
	}
	if before.Available {
		t.Fatalf("installation before install = %+v, want absent", before)
	}

	got, err := rt.Install.Install(context.Background(), oarlock.InstallRequest{})
	if err == nil {
		t.Fatal("installer exited 0 without installing anything: want an error")
	}
	if got.Installation.Available {
		t.Fatalf("installation after install = %+v, want the re-probe's absence", got.Installation)
	}
	if !strings.Contains(err.Error(), "exited 0") || !strings.Contains(err.Error(), "found no installation") {
		t.Fatalf("err = %v, want the exit code and the re-probe's verdict", err)
	}
	if n := runCount(t, runs); n != 1 {
		t.Fatalf("the installer ran %d times, want exactly once", n)
	}
}

// TestAntigravityHasNoInstallerAndNoLogin keeps absence honest: no
// refusal-shaped fake installer and no invented login channel.
func TestAntigravityHasNoInstallerAndNoLogin(t *testing.T) {
	rt := Antigravity.Runtime()
	if rt.Install != nil {
		t.Fatalf("Install = %#v, want nil: antigravity has no command-line installer (OAR requires_gui)", rt.Install)
	}
	if rt.Login != nil {
		t.Fatalf("Login = %#v, want nil: antigravity has no verified auth channel", rt.Login)
	}
	if rt.Probe == nil {
		t.Fatal("Probe is nil: every ACP profile still probes its installation")
	}
}

// TestRuntimesWiringMatchesTheVendorTables walks all six profiles: an Installer
// exactly where a vendor line exists (all but antigravity) and a LoginProvider
// exactly where the channel is verified (opencode only).
func TestRuntimesWiringMatchesTheVendorTables(t *testing.T) {
	for _, id := range []string{"grok", "kimi", "opencode", "qwen", "qoder", "antigravity"} {
		t.Run(id, func(t *testing.T) {
			rt := Profile{ID: id}.Runtime()
			_, hasLine := installLines[id]
			wantInstall := hasLine || id == "opencode"
			if (rt.Install != nil) != wantInstall {
				t.Fatalf("Install present = %v, want %v", rt.Install != nil, wantInstall)
			}
			_, hasKnownPath := knownInstallPaths[id]
			probed, ok := rt.Probe.(pathProbe)
			if !ok {
				t.Fatalf("Probe = %T, want pathProbe", rt.Probe)
			}
			if probed.command != id {
				t.Fatalf("probe command = %q, want %q", probed.command, id)
			}
			if (len(probed.fallbacks) > 0) != hasKnownPath {
				t.Fatalf("probe fallbacks = %q, want exactly one entry for %v", probed.fallbacks, hasKnownPath)
			}
			if wantLogin := id == "opencode"; (rt.Login != nil) != wantLogin {
				t.Fatalf("Login present = %v, want %v", rt.Login != nil, wantLogin)
			}
		})
	}
}

// TestProbe_PathThenKnownLocationAndRunsNothing pins the probe rules: PATH
// first (Source "path"), then the vendor's known install path (Source "known"),
// neither is Available:false with no error — and the probe reads no version and
// starts no process, which is why opencode's v1/v2 Select still works from an
// empty Version.
func TestProbe_PathThenKnownLocationAndRunsNothing(t *testing.T) {
	f := newFakePATH(t)
	home := os.Getenv("HOME")
	marker := filepath.Join(t.TempDir(), "ran")
	onPATH := f.add(t, "grok", "touch '"+marker+"'\n")

	inst, err := Profile{ID: "grok"}.Runtime().Probe.Installation(context.Background())
	if err != nil {
		t.Fatalf("probe on PATH: %v", err)
	}
	if !inst.Available || inst.Source != "path" || inst.Path != onPATH || inst.Version != "" {
		t.Fatalf("installation = %+v, want the PATH binary with no version", inst)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the probe started the binary (it left %s)", marker)
	}

	// Not on PATH any more: the vendor's default install location answers.
	if err := os.Remove(onPATH); err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(home, ".grok", "bin", "grok")
	if err := os.MkdirAll(filepath.Dir(known), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(known, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	inst, err = Profile{ID: "grok"}.Runtime().Probe.Installation(context.Background())
	if err != nil {
		t.Fatalf("probe known path: %v", err)
	}
	if !inst.Available || inst.Source != "known" || inst.Path != known || inst.Version != "" {
		t.Fatalf("installation = %+v, want the known path %s with no version", inst, known)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the probe started the binary (it left %s)", marker)
	}

	// Neither: absent, and still no error.
	if err := os.Remove(known); err != nil {
		t.Fatal(err)
	}
	inst, err = Profile{ID: "grok"}.Runtime().Probe.Installation(context.Background())
	if err != nil {
		t.Fatalf("probe absent: %v", err)
	}
	if inst.Available || inst.Path != "" {
		t.Fatalf("installation = %+v, want absent", inst)
	}
}
