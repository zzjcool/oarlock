package pi

// Version gate and refused-option tests (issue #15).
//
// The fake Pi is the one of resume_test.go: this very test binary re-executed.
// helperFakePi answers `--version` through helperFakeVersion below, so a probe
// is a real child process in a real process group, and every test can tell a
// probe from a session: the session writes markerEnv, the probe writes
// probeEnv.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
)

const (
	// versionEnv picks what the fake's `--version` does; unset prints SupportedVersion.
	versionEnv = "OARLOCK_PI_TEST_VERSION"
	// probeEnv names the file the fake's `--version` records itself in.
	probeEnv = "OARLOCK_PI_TEST_PROBE"
	// leaderEnv names the file the hanging fake writes its own pid to.
	leaderEnv = "OARLOCK_PI_TEST_LEADER_PID"
	// grandchildEnv names the file the hang-tree grandchild writes its pid to. It
	// is not markerEnv: that file means "a session process started".
	grandchildEnv = "OARLOCK_PI_TEST_GRANDCHILD_PID"
)

// helperFakeVersion is `pi --version` of the fake Pi. versionEnv selects:
//
//	"" or "ok"   print the supported version
//	"padded"     the supported version with surrounding blanks and CRLF
//	"hang"       never exit
//	"hang-tree"  never exit, and a grandchild in the same group does not either
//	"exit3"      fail on stderr with exit code 3
//	"empty"      exit 0 and print nothing
//	"flood"      print 8 MiB
//	anything else is printed verbatim (a wrong version)
func helperFakeVersion(args []string) int {
	if p := os.Getenv(probeEnv); p != "" {
		cwd, _ := os.Getwd()
		rec := cwd + "\n" + os.Getenv(envAgentDir) + "\n" + strings.Join(args, "\n")
		_ = os.WriteFile(p, []byte(rec), 0o600)
	}
	switch v := os.Getenv(versionEnv); v {
	case "", "ok":
		emit(SupportedVersion)
	case "padded":
		_, _ = os.Stdout.WriteString("  " + SupportedVersion + "  \r\n\n")
	case "hang", "hang-tree":
		if v == "hang-tree" {
			exe, err := os.Executable()
			if err != nil {
				return 95
			}
			gc := osexec.Command(exe, "-test.run=TestHelperProcess$")
			// The later entry wins: the grandchild must not write the session marker.
			gc.Env = append(os.Environ(), modeEnv+"=record-pid", markerEnv+"="+os.Getenv(grandchildEnv))
			if err := gc.Start(); err != nil {
				return 98
			}
		}
		if p := os.Getenv(leaderEnv); p != "" {
			_ = os.WriteFile(p, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600)
		}
		time.Sleep(time.Minute)
	case "exit3":
		fmt.Fprintln(os.Stderr, "pi: cannot find module 'missing-dependency'")
		return 3
	case "empty":
	case "flood":
		chunk := strings.Repeat("x", 1<<20)
		for i := 0; i < 8; i++ {
			_, _ = os.Stdout.WriteString(chunk)
		}
	default:
		emit(v)
	}
	return 0
}

// fakeVersionCheck probes the fake Pi directly, without Open.
func fakeVersionCheck(t *testing.T, version string, env ...string) VersionCheck {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	base := append(os.Environ(), "GORACE=atexit_sleep_ms=0", modeEnv+"=fake-pi", versionEnv+"="+version)
	return VersionCheck{
		Command:   exe,
		Args:      []string{"-test.run=TestHelperProcess$", "--"},
		Env:       append(base, env...),
		Timeout:   10 * time.Second,
		KillGrace: time.Second,
		WaitDelay: 2 * time.Second,
	}
}

// newGateEnv is a fakeEnv whose fake Pi also records its `--version` probe.
func newGateEnv(t *testing.T, version string) (*fakeEnv, string) {
	t.Helper()
	e := newFakeEnv(t, nil)
	probe := filepath.Join(filepath.Dir(e.argv), "probe")
	e.env[probeEnv] = &probe
	if version != "" {
		e.env[versionEnv] = &version
	}
	return e, probe
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ---- the version gate -----------------------------------------------------------

func TestCheckVersionAcceptsExactlyTheSupportedVersion(t *testing.T) {
	defer watchdog(t, 60*time.Second)()
	if SupportedVersion != "1.4.0" {
		t.Fatalf("SupportedVersion = %q; the evidence baseline is 1.4.0 (docs/runtimes/pi.md §1)", SupportedVersion)
	}
	for _, mode := range []string{"ok", "padded"} {
		got, err := CheckVersion(context.Background(), fakeVersionCheck(t, mode))
		if err != nil || got != SupportedVersion {
			t.Errorf("mode %q: CheckVersion = %q, %v; want %q, nil", mode, got, err, SupportedVersion)
		}
	}
	noLeaks(t)
}

// Acceptance 1: a different version is a typed error that carries both versions.
func TestCheckVersionMismatchIsTypedWithExpectedAndActual(t *testing.T) {
	defer watchdog(t, 60*time.Second)()
	for _, actual := range []string{"1.3.9", "1.4.1", "1.5.0", "2.0.0", "1.4.0-beta.1", "v1.4.0", "pi 1.4.0", "1.4"} {
		got, err := CheckVersion(context.Background(), fakeVersionCheck(t, actual))
		var ve *VersionError
		if !errors.As(err, &ve) {
			t.Errorf("version %q: err = %v, want *VersionError", actual, err)
			continue
		}
		if got != "" {
			t.Errorf("version %q: returned %q beside an error; a refused Pi must not yield a version to run with", actual, got)
		}
		if ve.Expected != "1.4.0" || ve.Actual != actual {
			t.Errorf("version %q: VersionError{Expected: %q, Actual: %q}", actual, ve.Expected, ve.Actual)
		}
		msg := err.Error()
		if !strings.Contains(msg, "expected 1.4.0") || !strings.Contains(msg, fmt.Sprintf("actual %q", actual)) {
			t.Errorf("version %q: the message must name expected and actual: %s", actual, msg)
		}
		if !errors.Is(err, ErrVersionMismatch) || errors.Is(err, ErrVersionProbe) {
			t.Errorf("version %q: Is(ErrVersionMismatch) = %v, Is(ErrVersionProbe) = %v", actual, errors.Is(err, ErrVersionMismatch), errors.Is(err, ErrVersionProbe))
		}
	}
	noLeaks(t)
}

// A probe that cannot answer is not a mismatch, and says what to do.
func TestCheckVersionProbeFailuresAreActionable(t *testing.T) {
	defer watchdog(t, 60*time.Second)()

	t.Run("non-zero exit carries code and stderr", func(t *testing.T) {
		c := fakeVersionCheck(t, "exit3")
		_, err := CheckVersion(context.Background(), c)
		var pe *VersionProbeError
		if !errors.As(err, &pe) || pe.ExitCode != 3 || pe.Timeout {
			t.Fatalf("err = %v, want *VersionProbeError{ExitCode: 3}", err)
		}
		if !strings.Contains(pe.StderrTail, "missing-dependency") || !strings.Contains(err.Error(), "missing-dependency") {
			t.Errorf("the stderr tail must reach the error: %v", err)
		}
		if !strings.Contains(err.Error(), "--version` yourself") || !strings.Contains(err.Error(), SupportedVersion) {
			t.Errorf("the error must say how to check by hand and which Pi to select: %v", err)
		}
		if !errors.Is(err, ErrVersionProbe) || errors.Is(err, ErrVersionMismatch) {
			t.Errorf("Is(ErrVersionProbe) = %v, Is(ErrVersionMismatch) = %v", errors.Is(err, ErrVersionProbe), errors.Is(err, ErrVersionMismatch))
		}
	})

	t.Run("empty output", func(t *testing.T) {
		_, err := CheckVersion(context.Background(), fakeVersionCheck(t, "empty"))
		if !errors.Is(err, ErrVersionProbe) || !strings.Contains(err.Error(), "printed nothing") {
			t.Fatalf("err = %v, want a probe error that says it printed nothing", err)
		}
	})

	t.Run("not startable", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "no-such-pi")
		_, err := CheckVersion(context.Background(), VersionCheck{Command: missing})
		var pe *VersionProbeError
		if !errors.As(err, &pe) || pe.Cause == nil || !strings.Contains(err.Error(), missing) {
			t.Fatalf("err = %v, want a probe error that names %s and keeps the start error", err, missing)
		}
	})

	t.Run("flood is bounded", func(t *testing.T) {
		_, err := CheckVersion(context.Background(), fakeVersionCheck(t, "flood"))
		var ve *VersionError
		if !errors.As(err, &ve) {
			t.Fatalf("err = %v, want *VersionError (8 MiB of x is not a version)", err)
		}
		if len(ve.Actual) > versionQuoteBytes+len("…") {
			t.Errorf("Actual holds %d bytes; an error must not carry a flood", len(ve.Actual))
		}
	})

	t.Run("negative timeout", func(t *testing.T) {
		if _, err := CheckVersion(context.Background(), VersionCheck{Timeout: -time.Second}); err == nil {
			t.Fatal("a negative timeout must be an error")
		}
	})
	noLeaks(t)
}

// Acceptance 3: a `--version` that never exits does not hang the caller.
func TestCheckVersionTimeoutDoesNotHang(t *testing.T) {
	defer watchdog(t, 60*time.Second)()
	c := fakeVersionCheck(t, "hang")
	c.Timeout = 300 * time.Millisecond
	start := time.Now()
	got, err := CheckVersion(context.Background(), c)
	elapsed := time.Since(start)

	var pe *VersionProbeError
	if !errors.As(err, &pe) || !pe.Timeout || pe.After != 300*time.Millisecond {
		t.Fatalf("CheckVersion = %q, %v; want *VersionProbeError{Timeout, After: 300ms}", got, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrVersionProbe) {
		t.Errorf("a timeout must satisfy DeadlineExceeded and ErrVersionProbe: %v", err)
	}
	if !strings.Contains(err.Error(), "killed") || !strings.Contains(err.Error(), "300ms") {
		t.Errorf("the message must say what was killed and the bound: %v", err)
	}
	if elapsed > 15*time.Second {
		t.Errorf("returned after %v; the probe must be bounded by its timeout plus the kill", elapsed)
	}
	noLeaks(t)
}

func TestCheckVersionContextCancellationKillsTheProbe(t *testing.T) {
	defer watchdog(t, 60*time.Second)()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	c := fakeVersionCheck(t, "hang")
	c.Timeout = time.Minute
	start := time.Now()
	_, err := CheckVersion(ctx, c)
	var pe *VersionProbeError
	if !errors.As(err, &pe) || pe.Timeout || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a probe error caused by context.Canceled and not flagged Timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("returned after %v", elapsed)
	}
	noLeaks(t)
}

// ---- the refused-option table -----------------------------------------------------

// presentOptions has, for every frozen option, an Options value that sets it. A
// frozen option without an entry fails TestRefusedTableDecidesEveryFrozenOption,
// so adding one to oarlock.AllRefusableOptions forces a decision here.
var presentOptions = map[oarlock.Option]oarlock.Options{
	oarlock.OptionSystemPrompt:       {SystemPrompt: "be brief"},
	oarlock.OptionAppendSystemPrompt: {AppendSystemPrompt: "and kind"},
	oarlock.OptionEnv:                {Env: map[string]*string{"OAR_TEST": strp("1")}},
	oarlock.OptionMCPServers:         {MCPServers: []oarlock.MCPServer{{Name: "s", Command: "x"}}},
	oarlock.OptionMCPServerCredentials: {MCPServers: []oarlock.MCPServer{
		{Name: "s", Command: "x", Env: map[string]string{"TOKEN": "v"}},
	}},
	oarlock.OptionDisallowedTools:       {DisallowedTools: []string{"bash"}},
	oarlock.OptionLaunchArgs:            {LaunchArgs: []string{"--no-session"}},
	oarlock.OptionServiceTier:           {ServiceTier: "priority"},
	oarlock.OptionAdditionalDirectories: {AdditionalDirectories: []string{"/tmp"}},
	oarlock.OptionOutputSchema:          {OutputSchema: json.RawMessage(`{"type":"object"}`)},
	oarlock.OptionName:                  {Name: "my session"},
	oarlock.OptionUserConfiguration:     {UserConfiguration: oarlock.UserConfigurationExclude},
}

// refusalOf is the option a present option is reported as: a credential-bearing
// MCP server is refused as mcpServers, which the table also declares and which
// oarlock.RefusedOptions.Check tests first.
func refusalOf(o oarlock.Option) oarlock.Option {
	if o == oarlock.OptionMCPServerCredentials {
		return oarlock.OptionMCPServers
	}
	return o
}

func TestRefusedTableDecidesEveryFrozenOption(t *testing.T) {
	for _, o := range oarlock.AllRefusableOptions {
		if _, ok := presentOptions[o]; !ok {
			t.Errorf("frozen option %s has no entry in presentOptions: decide it in gate.go and add one here", o)
		}
		reason, refused := refusedOptions[o]
		honoured := slices.Contains(honouredOptions, o)
		if refused == honoured {
			t.Errorf("option %s: refused=%v honoured=%v; exactly one must hold", o, refused, honoured)
		}
		if refused && (reason == "" || reason == undecidedReason) {
			t.Errorf("option %s is refused without a reason of its own", o)
		}
	}
	for o := range refusedReasons {
		if !slices.Contains(oarlock.AllRefusableOptions, o) || slices.Contains(honouredOptions, o) {
			t.Errorf("refusedReasons has a reason for %s, which is not a refused frozen option", o)
		}
	}
	for o := range refusedOptions {
		if !slices.Contains(oarlock.AllRefusableOptions, o) {
			t.Errorf("the table refuses %s, which is not a frozen option", o)
		}
	}
	// The issue's own list, so a reader can see it was not dropped.
	for _, o := range []oarlock.Option{
		oarlock.OptionLaunchArgs, oarlock.OptionSystemPrompt, oarlock.OptionAppendSystemPrompt,
		oarlock.OptionMCPServers, oarlock.OptionMCPServerCredentials, oarlock.OptionDisallowedTools,
		oarlock.OptionAdditionalDirectories, oarlock.OptionOutputSchema, oarlock.OptionName,
		oarlock.OptionUserConfiguration, oarlock.OptionServiceTier,
	} {
		if _, ok := refusedOptions[o]; !ok {
			t.Errorf("option %s must be refused", o)
		}
	}
	if _, ok := refusedOptions[oarlock.OptionEnv]; ok {
		t.Error("env is honoured (EnvFor): it must not be refused")
	}
}

// Acceptance 2, both directions: whatever the table declares is refused when
// present, whatever it does not declare is not refused by it.
func TestRefusedTableDeclaredIsRefusedAndUndeclaredIsNot(t *testing.T) {
	declared := Refused()
	rt := oarlock.Runtime{ID: "pi", Session: Open, Refused: declared}
	for _, o := range oarlock.AllRefusableOptions {
		opt := presentOptions[o]
		err := refusedOptions.Check(opt)
		if rtErr := rt.CheckOptions(opt); fmt.Sprint(rtErr) != fmt.Sprint(err) {
			t.Errorf("option %s: Runtime.CheckOptions = %v, the open path's table says %v: declaration and check are not one table", o, rtErr, err)
		}
		reason, isDeclared := declared[o]
		var uo *oarlock.UnsupportedOption
		switch {
		case isDeclared:
			if !errors.As(err, &uo) {
				t.Errorf("declared option %s present: Check = %v, want *UnsupportedOption", o, err)
				continue
			}
			want := refusalOf(o)
			if uo.Option != want || uo.Reason != declared[want] || declared[want] == "" {
				t.Errorf("declared option %s: refused as %s with %q, want %s with the table's reason", o, uo.Option, uo.Reason, want)
			}
			_ = reason
		default:
			if err != nil {
				t.Errorf("undeclared option %s present: Check = %v, want nil (a refusal the table does not declare is drift)", o, err)
			}
		}
	}

	// A declared option that is absent is not an error: one Options can go to every runtime.
	if err := refusedOptions.Check(oarlock.Options{}); err != nil {
		t.Errorf("the zero Options = %v, want nil", err)
	}
	// The options Open always honours are never refused by the table, with their values set.
	honoured := oarlock.Options{Cwd: "/work", Model: "p/m", Effort: "high", Resume: "an-id", Env: map[string]*string{"A": strp("1"), "B": nil}}
	if err := refusedOptions.Check(honoured); err != nil {
		t.Errorf("Cwd/Model/Effort/Resume/Env = %v, want nil", err)
	}
	// userConfiguration refuses only exclude: include and omission are accepted.
	if err := refusedOptions.Check(oarlock.Options{UserConfiguration: oarlock.UserConfigurationInclude}); err != nil {
		t.Errorf("userConfiguration include = %v, want nil", err)
	}
	// mcpServerCredentials is derived from mcpServers: a server without credentials is still mcpServers.
	var uo *oarlock.UnsupportedOption
	if err := refusedOptions.Check(presentOptions[oarlock.OptionMCPServers]); !errors.As(err, &uo) || uo.Option != oarlock.OptionMCPServers {
		t.Errorf("a plain MCP server = %v, want refused as mcpServers", err)
	}
}

func TestRefusedReturnsACopy(t *testing.T) {
	got := Refused()
	delete(got, oarlock.OptionLaunchArgs)
	got[oarlock.OptionEnv] = "mutated"
	if _, ok := refusedOptions[oarlock.OptionLaunchArgs]; !ok {
		t.Error("deleting from Refused()'s result removed the option from the table Open checks")
	}
	if _, ok := refusedOptions[oarlock.OptionEnv]; ok {
		t.Error("writing to Refused()'s result added an option to the table Open checks")
	}
}

// A frozen option nobody has decided about is refused, never dropped.
func TestBuildRefusedOptionsRefusesWhatNobodyDecided(t *testing.T) {
	const future = oarlock.Option("futureOption")
	all := append(slices.Clone(oarlock.AllRefusableOptions), future)
	table := buildRefusedOptions(all, honouredOptions, refusedReasons)
	if table[future] != undecidedReason {
		t.Errorf("an undecided option = %q, want the undecided reason", table[future])
	}
	for o, want := range refusedOptions {
		if table[o] != want {
			t.Errorf("option %s: %q, want %q", o, table[o], want)
		}
	}
}

// ---- Open ---------------------------------------------------------------------------

// Acceptance 2 + 4 of the issue (goal 4): the refusal happens before ANY process,
// the version probe included, and through the table Refused() declares.
func TestOpenRefusesEveryDeclaredOptionBeforeAnyProcess(t *testing.T) {
	defer watchdog(t, 60*time.Second)()
	e, probe := newGateEnv(t, "")
	declared := Refused()
	if len(declared) == 0 {
		t.Fatal("the table declares nothing")
	}
	for _, o := range oarlock.AllRefusableOptions {
		if _, ok := declared[o]; !ok {
			continue
		}
		opt := presentOptions[o]
		base := e.options("")
		opt.Cwd, opt.Env = base.Cwd, base.Env
		s, err := open(context.Background(), e.spec(t), opt)
		var uo *oarlock.UnsupportedOption
		if s != nil || !errors.As(err, &uo) {
			t.Errorf("option %s: open = %v, %v; want *UnsupportedOption and no session", o, s, err)
			continue
		}
		if want := refusalOf(o); uo.Option != want || uo.Reason != declared[want] {
			t.Errorf("option %s: refused as %s (%q), want %s (%q)", o, uo.Option, uo.Reason, want, declared[want])
		}
		if e.started() || fileExists(probe) {
			t.Fatalf("option %s: a process was started (session=%v, version probe=%v) for a refused option", o, e.started(), fileExists(probe))
		}
	}

	// Through the public entry, with a Pi that cannot even be started: the table
	// answers first, so the error is the refusal and not a start failure.
	missing := filepath.Join(t.TempDir(), "no-such-pi")
	_, err := Open(context.Background(), oarlock.Installation{Path: missing}, oarlock.Options{LaunchArgs: []string{"--x"}})
	var uo *oarlock.UnsupportedOption
	if !errors.As(err, &uo) || uo.Option != oarlock.OptionLaunchArgs {
		t.Errorf("Open(launchArgs) with an unstartable Pi = %v, want *UnsupportedOption{launchArgs}", err)
	}
	noLeaks(t)
}

// A resume target that is not there is refused before the probe too.
func TestOpenResumeNotFoundSkipsTheVersionProbe(t *testing.T) {
	defer watchdog(t, 60*time.Second)()
	e, probe := newGateEnv(t, "")
	_, err := e.open(t, "no-such-session-00000")
	if _, ok := asSessionNotFound(err); !ok {
		t.Fatalf("open = %v, want SessionNotFound", err)
	}
	if e.started() || fileExists(probe) {
		t.Fatalf("a process was started (session=%v, probe=%v) for a session that does not exist", e.started(), fileExists(probe))
	}
	noLeaks(t)
}

// Acceptance 1 at the Open level: a Pi of another version never becomes a session.
func TestOpenRefusesAVersionMismatchBeforeLaunch(t *testing.T) {
	defer watchdog(t, 60*time.Second)()
	e, probe := newGateEnv(t, "1.3.9")
	s, err := e.open(t, "")
	var ve *VersionError
	if s != nil || !errors.As(err, &ve) {
		t.Fatalf("open = %v, %v; want *VersionError and no session (not a warning, not a run)", s, err)
	}
	if ve.Expected != "1.4.0" || ve.Actual != "1.3.9" || !errors.Is(err, ErrVersionMismatch) {
		t.Errorf("VersionError = %+v", *ve)
	}
	if !fileExists(probe) {
		t.Error("the probe never ran: the mismatch path was not exercised")
	}
	if e.started() {
		t.Fatal("a session process was started for a Pi of the wrong version")
	}
	noLeaks(t)
}

func TestOpenVersionProbeFailureIsActionable(t *testing.T) {
	defer watchdog(t, 60*time.Second)()
	e, _ := newGateEnv(t, "exit3")
	s, err := e.open(t, "")
	var pe *VersionProbeError
	if s != nil || !errors.As(err, &pe) || pe.ExitCode != 3 || !strings.Contains(err.Error(), "missing-dependency") {
		t.Fatalf("open = %v, %v; want a *VersionProbeError with exit code 3 and the stderr", s, err)
	}
	if e.started() {
		t.Fatal("a session process was started although the probe failed")
	}
	noLeaks(t)
}

// Acceptance 3 at the Open level: no hang, nothing started.
func TestOpenVersionTimeoutDoesNotHang(t *testing.T) {
	defer watchdog(t, 60*time.Second)()
	e, _ := newGateEnv(t, "hang")
	spec := e.spec(t)
	spec.versionTimeout = 300 * time.Millisecond
	start := time.Now()
	s, err := open(context.Background(), spec, e.options(""))
	var pe *VersionProbeError
	if s != nil || !errors.As(err, &pe) || !pe.Timeout {
		t.Fatalf("open = %v, %v; want a timed-out *VersionProbeError and no session", s, err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("open returned after %v; it must be bounded by the probe timeout", elapsed)
	}
	if e.started() {
		t.Fatal("a session process was started although the probe timed out")
	}
	noLeaks(t)
}

// The probe asks the very Pi the session would run: same cwd, same agent dir.
func TestOpenProbesTheSessionCwdAndEnvAndKeepsTheVerifiedVersion(t *testing.T) {
	defer watchdog(t, 60*time.Second)()
	e, probe := newGateEnv(t, "")
	s := e.mustOpen(t, "")
	_ = idOf(t, s)
	if got := s.PiVersion(); got != SupportedVersion {
		t.Errorf("PiVersion() = %q, want %q", got, SupportedVersion)
	}
	rec := strings.Split(string(mustRead(t, probe)), "\n")
	if len(rec) < 3 || rec[0] != e.cwd || rec[1] != e.agentDir || rec[len(rec)-1] != "--version" {
		t.Errorf("the probe saw cwd/agentDir/args %q, want %q, %q and ... --version", rec, e.cwd, e.agentDir)
	}
}

// Gap, on purpose (docs/runtimes/pi.md §13.3): Pi has no frame that reports its
// version and oarlock never synthesizes a frame, so the stream holds only what Pi
// wrote. This test documents that the gate adds nothing to it.
func TestTheVersionGateAddsNothingToTheRecordStream(t *testing.T) {
	defer watchdog(t, 60*time.Second)()
	e, _ := newGateEnv(t, "")
	s := e.mustOpen(t, "")
	_ = idOf(t, s)
	for _, r := range recordsOf(s) {
		if r.Kind != oarlock.KindFrame || len(r.Events) != 0 || bytes.Contains(r.Native, []byte(SupportedVersion)) {
			t.Errorf("record %d (%s) holds something the gate put there: %s", r.Seq, r.Kind, r.Native)
		}
	}
}

// ---- docs ---------------------------------------------------------------------------

// Acceptance 4: the window is in docs/compat-matrix.md and links the issue.
func TestCompatMatrixStatesTheWindowAndLinksTheIssue(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "compat-matrix.md"))
	if err != nil {
		t.Fatal(err)
	}
	var row string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "| pi |") {
			row = line
			break
		}
	}
	switch {
	case row == "":
		t.Fatal("docs/compat-matrix.md has no `| pi |` row in the version policy table")
	case !strings.Contains(row, SupportedVersion):
		t.Errorf("the pi version-policy row does not state %s: %s", SupportedVersion, row)
	case !strings.Contains(strings.ToLower(row), "exact"):
		t.Errorf("the pi version-policy row does not say the match is exact: %s", row)
	case !strings.Contains(row, "issues/0015-pi-version-gate.md"):
		t.Errorf("the pi version-policy row does not link issue #15: %s", row)
	}
	if _, err := os.Stat(filepath.Join("..", "..", "issues", "0015-pi-version-gate.md")); err != nil {
		t.Errorf("the linked issue file is missing: %v", err)
	}
}
