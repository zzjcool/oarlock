package pi

// The version gate and the refused-option table (issue #15).
//
// Version policy (docs/compat-matrix.md): EXACT match. The RPC dialect changes
// from one Pi release to the next, so a Pi that is not SupportedVersion is
// refused before any session process exists; it is never a warning followed by
// a run. CheckVersion runs `<entry> --version` (OAR's runtimes/pi/rpc/launch.ts
// does the same with timeoutMs 15_000), compares the trimmed stdout and fails
// closed on everything else:
//
//   - a Pi of another version is a *VersionError carrying Expected and Actual
//     (errors.Is(err, ErrVersionMismatch));
//   - a probe that cannot answer (not startable, non-zero exit, no output, the
//     ctx ended, or no exit within the timeout) is a *VersionProbeError
//     (errors.Is(err, ErrVersionProbe)). On timeout the whole process group is
//     SIGKILLed (Job Object on Windows), so a hung `--version` neither blocks
//     the host nor leaves a process behind.
//
// Refused options: refusedOptions is the ONE table. Refused() hands a copy to a
// host (oarlock.Runtime.Refused) and open checks the very same table, so the
// declaration cannot drift from the behaviour. Every frozen option is decided
// here (honoured or refused with a reason); an option somebody adds to
// oarlock.AllRefusableOptions later is refused until it is decided, never
// silently dropped.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/exec"
)

const (
	// SupportedVersion is the one Pi version this adapter is evidenced against
	// (docs/runtimes/pi.md §1) and therefore the only one it will start.
	SupportedVersion = "1.4.0"

	// VersionTimeout bounds `<entry> --version` (OAR launch.ts: 15_000 ms).
	VersionTimeout = 15 * time.Second

	// versionKillGrace is the SIGTERM→SIGKILL grace when the caller's ctx ends the
	// probe. A timeout needs no grace: `--version` has nothing to flush, so the
	// group is SIGKILLed at once.
	versionKillGrace = 2 * time.Second

	// versionOutputCap is how much of the probe's stdout is kept; the rest is
	// read and discarded so a flooding child can neither grow memory nor block.
	versionOutputCap = 64 << 10

	// versionQuoteBytes is the most bytes of a wrong answer an error carries.
	versionQuoteBytes = 200
)

var (
	// ErrVersionMismatch is the sentinel of VersionError.
	ErrVersionMismatch = errors.New("pi: unsupported Pi version")

	// ErrVersionProbe is the sentinel of VersionProbeError.
	ErrVersionProbe = errors.New("pi: cannot read the Pi version")
)

// VersionError says the Pi that was asked for is not the supported version.
// It is returned before any session process starts.
type VersionError struct {
	Command  string // the executable that was probed
	Expected string // the supported version
	Actual   string // what `--version` printed, trimmed and clipped
}

// Error names both versions and what to do about it.
func (e *VersionError) Error() string {
	return fmt.Sprintf("pi: unsupported Pi version: expected %s, actual %q (oarlock supports Pi %s exactly because the RPC dialect changes between versions; `%s --version` reported %q): install Pi %s, or select a Pi executable of that version",
		e.Expected, e.Actual, e.Expected, e.Command, e.Actual, e.Expected)
}

// Is reports whether target is ErrVersionMismatch.
func (e *VersionError) Is(target error) bool { return target == ErrVersionMismatch }

// VersionProbeError says `<entry> --version` gave no answer to compare.
type VersionProbeError struct {
	Command    string        // the executable that was probed
	Timeout    bool          // no exit within After: the process group was killed
	After      time.Duration // the timeout that fired (Timeout only)
	ExitCode   int           // the exit code when it ran and failed; -1 for a signal death
	StderrTail string        // the end of stderr, raw
	Cause      error         // the native reason: a start error, ctx.Err(), context.DeadlineExceeded (Timeout), ...
}

// Error says what happened and how to check it by hand.
func (e *VersionProbeError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "pi: cannot read the Pi version: `%s --version` ", e.Command)
	switch {
	case e.Timeout:
		fmt.Fprintf(&b, "did not exit within %v; it was killed with its whole process group", e.After)
	case e.Cause != nil:
		fmt.Fprintf(&b, "failed: %v", e.Cause)
	case e.ExitCode != 0:
		fmt.Fprintf(&b, "exited with code %d", e.ExitCode)
	default:
		b.WriteString("gave no answer")
	}
	if tail := strings.TrimSpace(e.StderrTail); tail != "" {
		fmt.Fprintf(&b, "; stderr: %s", tail)
	}
	fmt.Fprintf(&b, "; run `%s --version` yourself, and select a working Pi %s executable", e.Command, SupportedVersion)
	return b.String()
}

// Is reports whether target is ErrVersionProbe.
func (e *VersionProbeError) Is(target error) bool { return target == ErrVersionProbe }

// Unwrap returns Cause, so errors.Is(err, context.DeadlineExceeded) holds for a
// timeout and errors.Is(err, context.Canceled) for a cancelled ctx.
func (e *VersionProbeError) Unwrap() error { return e.Cause }

// VersionCheck describes one probe. The zero value runs `pi --version` for
// SupportedVersion with the default timeout.
type VersionCheck struct {
	Command   string        // "" = DefaultExecutable, resolved through PATH by internal/exec
	Args      []string      // arguments before --version (a launcher's own)
	Dir       string        // working directory; "" = inherit
	Env       []string      // nil = inherit the parent environment
	Expected  string        // "" = SupportedVersion
	Timeout   time.Duration // 0 = VersionTimeout; negative is an error
	KillGrace time.Duration // SIGTERM→SIGKILL grace when ctx ends the probe; 0 = 2s
	WaitDelay time.Duration // drain bound after the leader exits; 0 = exec's default
}

// CheckVersion runs `<Command> <Args...> --version` and returns the version it
// printed when that equals Expected. Anything else is an error and nothing
// outlives the call: the tree is closed on every path.
func CheckVersion(ctx context.Context, c VersionCheck) (string, error) {
	if ctx == nil {
		return "", errors.New("pi: CheckVersion(nil context)")
	}
	if c.Timeout < 0 {
		return "", fmt.Errorf("pi: VersionCheck.Timeout is negative (%v)", c.Timeout)
	}
	command := c.Command
	if command == "" {
		command = DefaultExecutable
	}
	expected := c.Expected
	if expected == "" {
		expected = SupportedVersion
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = VersionTimeout
	}
	grace := c.KillGrace
	if grace == 0 {
		grace = versionKillGrace
	}
	fail := func(e *VersionProbeError) (string, error) {
		e.Command = command
		return "", e
	}

	args := append(append([]string(nil), c.Args...), "--version")
	cmd, err := exec.Start(ctx, &exec.Command{Path: command, Args: args, Dir: c.Dir, Env: c.Env, KillGrace: grace, WaitDelay: c.WaitDelay})
	if err != nil {
		return fail(&VersionProbeError{Cause: err})
	}
	// Whatever happens below, no member of the tree may outlive the probe. After
	// a normal exit the group is already empty and this returns at once.
	defer func() { _ = cmd.Close(context.Background()) }()

	// A zero-length Read claims a stream synchronously, so Wait cannot drain
	// output of a child that is gone before the readers are scheduled (see Launch).
	_, _ = cmd.Stdout().Read(nil)
	_, _ = cmd.Stderr().Read(nil)
	var (
		out  []byte
		tail = &tailRing{buf: make([]byte, DefaultStderrTailBytes)}
		wg   sync.WaitGroup
	)
	wg.Add(2)
	go func() { defer wg.Done(); out = readHead(cmd.Stdout(), versionOutputCap) }()
	go func() { defer wg.Done(); _, _ = io.Copy(tail, cmd.Stderr()) }()

	// state: 0 running, 1 finished, 2 the timer fired. The callback kills only a
	// probe that has not finished, and the return path learns which one won.
	var state atomic.Int32
	timer := time.AfterFunc(timeout, func() {
		if state.CompareAndSwap(0, 2) {
			_ = cmd.Signal(os.Kill)
		}
	})
	status, werr := cmd.Wait(ctx)
	timedOut := !state.CompareAndSwap(0, 1)
	timer.Stop()
	wg.Wait() // Wait returns only when both streams ended or were force-closed

	switch {
	case timedOut:
		return fail(&VersionProbeError{Timeout: true, After: timeout, ExitCode: status.Code, StderrTail: tail.String(), Cause: context.DeadlineExceeded})
	case werr != nil:
		// ctx ended the probe, or the output pipes stayed open past WaitDelay:
		// the answer may be incomplete, so it is not compared.
		return fail(&VersionProbeError{ExitCode: status.Code, StderrTail: tail.String(), Cause: werr})
	case status.Signal != nil:
		return fail(&VersionProbeError{ExitCode: status.Code, StderrTail: tail.String(), Cause: fmt.Errorf("killed by signal %v", status.Signal)})
	case status.Code != 0:
		return fail(&VersionProbeError{ExitCode: status.Code, StderrTail: tail.String()})
	}
	actual := strings.TrimSpace(string(out))
	if actual == "" {
		return fail(&VersionProbeError{StderrTail: tail.String(), Cause: errors.New("it printed nothing on stdout")})
	}
	if actual != expected {
		return "", &VersionError{Command: command, Expected: expected, Actual: clipVersion(actual)}
	}
	return actual, nil
}

// readHead keeps the first max bytes of r and discards the rest.
func readHead(r io.Reader, max int) []byte {
	head, _ := io.ReadAll(io.LimitReader(r, int64(max)))
	_, _ = io.Copy(io.Discard, r)
	return head
}

// clipVersion bounds a wrong answer for an error, on a rune boundary.
func clipVersion(s string) string {
	if len(s) <= versionQuoteBytes {
		return s
	}
	cut := versionQuoteBytes
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// PiVersion is the Pi version the gate verified before this session's process
// started ("" for a session built with NewSession and no gate). Pi has no init
// or handshake frame that carries its version and oarlock never synthesizes a
// frame (docs/spec/record-stream.md, invariant 1), so the version is NOT in the
// record stream: this accessor is how a host puts it into its own run metadata.
// docs/runtimes/pi.md §13.3 records that as a gap.
func (s *Session) PiVersion() string { return s.piVersion }

// ---- refused options ----------------------------------------------------------

// honouredOptions are the frozen options Open implements. Cwd, Model, Effort and
// Resume are not refusable options at all: they are always honoured.
var honouredOptions = []oarlock.Option{
	oarlock.OptionEnv,
}

// refusedReasons is the only place that decides why Pi refuses an option. A
// reason says what Pi lacks or what the adapter does not build yet; an option
// is refused rather than dropped, because a session that runs without it would
// make the host's configuration a lie. Sources: OAR runtimes/pi/open.ts
// (piRefusedSessionOptions, piRpcRefusedSessionOptions) and pi 1.4.0 `--help`.
var refusedReasons = map[oarlock.Option]string{
	oarlock.OptionLaunchArgs:            "oarlock owns Pi's RPC startup arguments (--mode rpc, --session-dir, --session, --model, --thinking); arbitrary CLI flags are not supported",
	oarlock.OptionSystemPrompt:          "Pi has --system-prompt, but the adapter does not pass it yet (OAR hands it over in a private file, because Pi reads an unreadable path as literal prompt text); refused, never dropped",
	oarlock.OptionAppendSystemPrompt:    "Pi has --append-system-prompt, but the adapter does not pass it yet (see systemPrompt); refused, never dropped",
	oarlock.OptionMCPServers:            "per-session MCP servers need a Pi RPC extension (OAR loads one with -e and starts Pi with --no-mcp); the Go adapter has none yet",
	oarlock.OptionMCPServerCredentials:  "MCP server credentials would have to reach Pi through that same extension (OAR reads them over a private pipe); the Go adapter has none yet",
	oarlock.OptionDisallowedTools:       "Pi has --exclude-tools, but the adapter does not pass it yet and its effect on the RPC tool set is not verified",
	oarlock.OptionAdditionalDirectories: "Pi configures one working directory and has no additional-directory option",
	oarlock.OptionOutputSchema:          "Pi has no final-output schema option",
	oarlock.OptionName:                  "Pi has --name, but the adapter does not pass it yet, and it must not rename a resumed session that already has a name; refused, never dropped",
	oarlock.OptionUserConfiguration:     "Pi has no verified per-session way to exclude user configuration and keep its native login (--no-extensions also removes host-registered providers, docs/runtimes/pi.md §1); only exclude is refused",
	oarlock.OptionServiceTier:           "Pi has no verified per-session service-tier setting with a read-back",
}

// undecidedReason refuses a frozen option that nobody has decided about yet.
const undecidedReason = "the Pi adapter has no decision for this option yet; it is refused rather than dropped"

// refusedOptions is the table both Refused() and open use.
var refusedOptions = buildRefusedOptions(oarlock.AllRefusableOptions, honouredOptions, refusedReasons)

// buildRefusedOptions refuses every option in all that is not honoured, with its
// decided reason or, when nobody decided, undecidedReason. It takes its inputs so
// a test can prove that a frozen option added later is refused, not dropped.
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

// Refused returns a copy of the table Open checks: the declaration for
// oarlock.Runtime.Refused. A host that must decide before opening reads it; a
// host that does not simply calls Open and falls back on *oarlock.UnsupportedOption.
func Refused() oarlock.RefusedOptions { return maps.Clone(refusedOptions) }
