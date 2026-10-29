package codex

// The version window, the schema checks and the refused-option table
// (issue #20, docs/compat-matrix.md "Version policy").
//
// Window: [MinVersion, MaxVersionExclusive). MinVersion is the oldest app-server
// OAR evidenced (0.149.0: child-thread delivery, subAgentActivity); the upper
// bound is the line the fixtures were recorded on (0.165.x). A codex outside the
// window is a *VersionError BEFORE the session process starts, never a warning
// followed by a run. A codex that cannot answer `--version` is a
// *VersionProbeError, also before anything starts.
//
// The initialize reply is checked again (userAgent names the running build) and
// the thread reply must have the schema the projection reads; either failure
// kills the process and fails the open (*VersionError / *SchemaError).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/exec"
)

const (
	// MinVersion is the oldest supported codex (inclusive).
	MinVersion = "0.149.0"
	// MaxVersionExclusive is the first codex version outside the window.
	MaxVersionExclusive = "0.166.0"
	// VersionTimeout bounds `codex --version`.
	VersionTimeout = 15 * time.Second

	versionOutputCap  = 64 << 10
	versionQuoteBytes = 200
)

var (
	// ErrVersionOutOfWindow is the sentinel of VersionError.
	ErrVersionOutOfWindow = errors.New("codex: unsupported codex version")
	// ErrVersionProbe is the sentinel of VersionProbeError.
	ErrVersionProbe = errors.New("codex: cannot read the codex version")
	// ErrSchema is the sentinel of SchemaError.
	ErrSchema = errors.New("codex: app-server reply does not match the pinned schema")
)

// VersionError says the codex that was asked for is outside the window.
type VersionError struct {
	Command string
	Min     string // inclusive
	Max     string // exclusive
	Actual  string
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("codex: unsupported codex version %q: oarlock supports codex >= %s and < %s (the app-server schema is pinned; `%s --version`): install a version inside the window",
		e.Actual, e.Min, e.Max, e.Command)
}
func (e *VersionError) Is(target error) bool { return target == ErrVersionOutOfWindow }

// VersionProbeError says `codex --version` gave no answer to compare.
type VersionProbeError struct {
	Command    string
	Timeout    bool
	ExitCode   int
	StderrTail string
	Cause      error
}

func (e *VersionProbeError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "codex: cannot read the codex version: `%s --version` ", e.Command)
	switch {
	case e.Timeout:
		b.WriteString("did not exit in time; it was killed")
	case e.Cause != nil:
		fmt.Fprintf(&b, "failed: %v", e.Cause)
	case e.ExitCode != 0:
		fmt.Fprintf(&b, "exited with code %d", e.ExitCode)
	default:
		b.WriteString("gave no answer")
	}
	if t := strings.TrimSpace(e.StderrTail); t != "" {
		fmt.Fprintf(&b, "; stderr: %s", t)
	}
	return b.String()
}
func (e *VersionProbeError) Is(target error) bool { return target == ErrVersionProbe }
func (e *VersionProbeError) Unwrap() error        { return e.Cause }

// SchemaError says a reply lacks a member the adapter's projection depends on.
type SchemaError struct {
	Method string
	Detail string
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("codex: %s reply does not match the pinned app-server schema: %s", e.Method, e.Detail)
}
func (e *SchemaError) Is(target error) bool { return target == ErrSchema }

var versionRE = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

type semver [3]int

func parseVersion(s string) (semver, bool) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return semver{}, false
	}
	var v semver
	for i := 0; i < 3; i++ {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return semver{}, false
		}
		v[i] = n
	}
	return v, true
}

func (a semver) less(b semver) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// InWindow reports whether a version string (any text containing x.y.z, such as
// `codex-cli 0.150.0` or a userAgent) is inside the supported window.
func InWindow(text string) bool {
	v, ok := parseVersion(text)
	if !ok {
		return false
	}
	lo, _ := parseVersion(MinVersion)
	hi, _ := parseVersion(MaxVersionExclusive)
	return !v.less(lo) && v.less(hi)
}

// VersionCheck describes one probe.
type VersionCheck struct {
	Command   string
	Args      []string // arguments before --version (a launcher's own)
	Dir       string
	Env       []string
	Timeout   time.Duration // 0 = VersionTimeout
	KillGrace time.Duration
	WaitDelay time.Duration
}

// CheckVersion runs `<Command> --version` and returns the text it printed when
// that is inside the window. Anything else is an error and nothing outlives the
// call.
func CheckVersion(ctx context.Context, c VersionCheck) (string, error) {
	if ctx == nil {
		return "", errors.New("codex: CheckVersion(nil context)")
	}
	command := c.Command
	if command == "" {
		command = "codex"
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = VersionTimeout
	}
	grace := c.KillGrace
	if grace <= 0 {
		grace = 2 * time.Second
	}
	fail := func(e *VersionProbeError) (string, error) { e.Command = command; return "", e }

	cmd, err := exec.Start(ctx, &exec.Command{Path: command, Args: append(append([]string(nil), c.Args...), "--version"), Dir: c.Dir, Env: c.Env, KillGrace: grace, WaitDelay: c.WaitDelay})
	if err != nil {
		return fail(&VersionProbeError{Cause: err})
	}
	defer func() { _ = cmd.Close(context.Background()) }()
	_, _ = cmd.Stdout().Read(nil)
	_, _ = cmd.Stderr().Read(nil)

	var out, errTail []byte
	done := make(chan struct{}, 2)
	go func() {
		out, _ = io.ReadAll(io.LimitReader(cmd.Stdout(), versionOutputCap))
		_, _ = io.Copy(io.Discard, cmd.Stdout())
		done <- struct{}{}
	}()
	go func() {
		errTail, _ = io.ReadAll(io.LimitReader(cmd.Stderr(), versionOutputCap))
		_, _ = io.Copy(io.Discard, cmd.Stderr())
		done <- struct{}{}
	}()
	var state atomic.Int32 // 0 running, 1 finished, 2 timer fired
	timer := time.AfterFunc(timeout, func() {
		if state.CompareAndSwap(0, 2) {
			_ = cmd.Signal(os.Kill)
		}
	})
	status, werr := cmd.Wait(ctx)
	timedOut := !state.CompareAndSwap(0, 1)
	timer.Stop()
	<-done
	<-done

	switch {
	case timedOut:
		return fail(&VersionProbeError{Timeout: true, ExitCode: status.Code, StderrTail: string(errTail), Cause: context.DeadlineExceeded})
	case werr != nil:
		return fail(&VersionProbeError{ExitCode: status.Code, StderrTail: string(errTail), Cause: werr})
	case status.Signal != nil:
		return fail(&VersionProbeError{ExitCode: status.Code, StderrTail: string(errTail), Cause: fmt.Errorf("killed by signal %v", status.Signal)})
	case status.Code != 0:
		return fail(&VersionProbeError{ExitCode: status.Code, StderrTail: string(errTail)})
	}
	actual := strings.TrimSpace(string(out))
	if actual == "" {
		return fail(&VersionProbeError{StderrTail: string(errTail), Cause: errors.New("it printed nothing on stdout")})
	}
	if !InWindow(actual) {
		return "", &VersionError{Command: command, Min: MinVersion, Max: MaxVersionExclusive, Actual: clipVersion(actual)}
	}
	return actual, nil
}

func clipVersion(s string) string {
	if len(s) <= versionQuoteBytes {
		return s
	}
	cut := versionQuoteBytes
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// ---- option table -------------------------------------------------------------

// honouredOptions are the frozen options Open implements. Every option in
// oarlock.AllRefusableOptions is honoured as of issue #54: the option surface
// (MCP servers and their credentials, tool denials, userConfiguration) travels
// as native codex state — a config override, codex's own disabled_tools, and an
// isolated CODEX_HOME — so none of it is filtered or dropped on the oarlock
// side.
var honouredOptions = []oarlock.Option{
	oarlock.OptionSystemPrompt,
	oarlock.OptionAppendSystemPrompt,
	oarlock.OptionEnv,
	oarlock.OptionMCPServers,
	oarlock.OptionMCPServerCredentials,
	oarlock.OptionDisallowedTools,
	oarlock.OptionLaunchArgs,
	oarlock.OptionServiceTier,
	oarlock.OptionAdditionalDirectories,
	oarlock.OptionOutputSchema,
	oarlock.OptionName,
	oarlock.OptionUserConfiguration,
}

// refusedReasons holds the options Open refuses, each with its reason: the table
// below is only what Open refuses. An option in honouredOptions is never
// refused; an option in neither table is refused with undecidedReason. It is
// empty because codex refuses nothing on the frozen option surface (issue #54
// added the last four); a refusal belongs here when an option's native seam is
// missing or broken.
var refusedReasons = map[oarlock.Option]string{}

const undecidedReason = "the codex adapter has no decision for this option yet; it is refused rather than dropped"

var refusedOptions = buildRefusedOptions(oarlock.AllRefusableOptions, honouredOptions, refusedReasons)

func buildRefusedOptions(all, honoured []oarlock.Option, reasons map[oarlock.Option]string) oarlock.RefusedOptions {
	kept := make(map[oarlock.Option]bool, len(honoured))
	for _, o := range honoured {
		kept[o] = true
	}
	table := make(oarlock.RefusedOptions, len(all))
	for _, o := range all {
		if kept[o] {
			continue
		}
		reason := reasons[o]
		if reason == "" {
			reason = undecidedReason
		}
		table[o] = reason
	}
	return table
}

// Refused returns a copy of the table Open checks.
func Refused() oarlock.RefusedOptions { return maps.Clone(refusedOptions) }
