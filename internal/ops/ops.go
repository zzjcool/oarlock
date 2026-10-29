// Package ops runs the vendor CLIs behind oarlock's operating capabilities:
// installation probing, installers, login challenges, auth status and
// self-update (issue #23).
//
// It is the only place that starts a vendor CLI for an operating capability.
// Every command goes through internal/exec, so the whole process tree is owned
// and killed on timeout, and stdin is always closed before the child can block
// waiting for a prompt. Nothing here writes configuration: an installer and an
// updater are the vendor's own programs, and oarlock only decides whether they
// worked by re-reading the installation/version afterwards.
//
// The rules are the ones in /tmp/oar-research docs/spec/{install,login,
// update}.md, reduced to the Go interfaces in runtime.go: a plan is read-only,
// an install/upgrade is explicit and judged by a probe, a login challenge is
// returned without waiting on stdin, and a missing surface is either an absent
// interface or *oarlock.Unsupported, never a fake success.
package ops

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/exec"
)

const (
	// DefaultTimeout bounds one read-only vendor command (OAR uses 15 s).
	DefaultTimeout = 15 * time.Second
	// InstallTimeout bounds one installer run (OAR uses 10 min).
	InstallTimeout = 10 * time.Minute
	// UpgradeTimeout bounds one updater run (OAR uses 10 min).
	UpgradeTimeout = 10 * time.Minute
	// LoginTimeout bounds one login-challenge capture. The helper is stopped
	// as soon as the challenge line is seen, so this is only a backstop.
	LoginTimeout = 2 * time.Minute

	outputTail = 2 << 10
)

// ErrVersionUnchanged is the explicit result of an upgrade whose read-back
// version did not move. It is an error, never a success: a vendor updater can
// exit 0 without replacing anything.
var ErrVersionUnchanged = errors.New("version unchanged after upgrade")

// Command is one vendor CLI invocation.
type Command struct {
	Path string
	Args []string
	Dir  string
	Env  []string
}

// Result is the captured output of one command. A non-zero ExitCode is data,
// not an error: callers decide what an exit means for their vendor.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Run executes c with no stdin and returns its complete output. It returns an
// error only when the process could not be started, the context expired, or
// output could not be collected; a non-zero exit is reported in Result.
func Run(ctx context.Context, c Command, timeout time.Duration) (Result, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, err := exec.Start(ctx, &exec.Command{
		Path: c.Path,
		Args: c.Args,
		Dir:  c.Dir,
		Env:  c.Env,
		// A reader at EOF closes the child's stdin immediately. This is the
		// "no stdin, no terminal" rule: a vendor CLI that prompts gets EOF,
		// never a host process blocked on a pipe.
		Stdin: strings.NewReader(""),
	})
	if err != nil {
		return Result{}, err
	}
	// Claim both streams before Wait starts. A goroutine that has not reached
	// Read yet must not lose the output to Wait's unclaimed-stream drain; the
	// zero-length Read claims without consuming (internal/exec handles it).
	_, _ = cmd.Stdout().Read(nil)
	_, _ = cmd.Stderr().Read(nil)

	var (
		wg             sync.WaitGroup
		stdout, stderr strings.Builder
	)
	wg.Add(2)
	go func() { defer wg.Done(); b, _ := io.ReadAll(cmd.Stdout()); stdout.Write(b) }()
	go func() { defer wg.Done(); b, _ := io.ReadAll(cmd.Stderr()); stderr.Write(b) }()

	status, waitErr := cmd.Wait(ctx)
	wg.Wait()
	_ = cmd.Close(context.Background())

	res := Result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: status.Code}
	if waitErr != nil {
		return res, waitErr
	}
	return res, nil
}

// Version reads `<path> --version` and returns the first non-empty line of
// stdout (falling back to stderr). It is the read that decides whether an
// upgrade changed anything.
func Version(ctx context.Context, path string) (string, error) {
	return VersionWith(ctx, path, nil, DefaultTimeout)
}

// VersionWith is Version with explicit arguments and a custom timeout.
func VersionWith(ctx context.Context, path string, args []string, timeout time.Duration) (string, error) {
	if len(args) == 0 {
		args = []string{"--version"}
	}
	res, err := Run(ctx, Command{Path: path, Args: args}, timeout)
	if err != nil {
		return "", err
	}
	version := firstLine(res.Stdout)
	if version == "" {
		version = firstLine(res.Stderr)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%s %s: exit %d: %s", path, strings.Join(args, " "), res.ExitCode, tail(res.Stderr))
	}
	if version == "" {
		return "", fmt.Errorf("%s %s printed no version", path, strings.Join(args, " "))
	}
	return version, nil
}

// Find looks for an executable on PATH, then at the given absolute
// fallbacks. The source is "path" for PATH and "known" for a fallback.
func Find(name string, fallbacks ...string) (path, source string, ok bool) {
	if path, err := osexec.LookPath(name); err == nil {
		return path, "path", true
	}
	for _, candidate := range fallbacks {
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, "known", true
		}
	}
	return "", "", false
}

// Probe is an InstallationProbe built on the vendor's `--version`. It performs
// no account or network I/O. A found executable with an unreadable version is
// Available=true plus the read error, so the host can tell "absent" from
// "present but broken".
type Probe struct {
	Name      string
	Fallbacks []string
	// VersionArgs defaults to --version.
	VersionArgs []string
	// Timeout bounds the version read; 0 = DefaultTimeout.
	Timeout time.Duration
}

func (p Probe) Installation(ctx context.Context) (oarlock.Installation, error) {
	path, source, ok := Find(p.Name, p.Fallbacks...)
	if !ok {
		return oarlock.Installation{Available: false, Source: "path"}, nil
	}
	version, err := VersionWith(ctx, path, p.VersionArgs, p.Timeout)
	if err != nil {
		return oarlock.Installation{Available: true, Path: path, Source: source}, err
	}
	return oarlock.Installation{Available: true, Version: version, Path: path, Source: source}, nil
}

// Installer is an oarlock.Installer for a vendor's own installer script. The
// plan is pure; Install is the only method that changes the machine, and it is
// only reached when a caller invokes it explicitly.
type Installer struct {
	// Line is the vendor's official install line, run by the shell.
	Line string
	// Probe decides success. It is mandatory.
	Probe oarlock.InstallationProbe
	// Shell overrides the default ["sh", "-c"] (tests use a fake script).
	Shell []string
	// Timeout bounds the installer; 0 = InstallTimeout.
	Timeout time.Duration
}

var _ oarlock.Installer = Installer{}

// InstallPlan returns the command that would run, without running it.
func (i Installer) InstallPlan(_ context.Context, req oarlock.InstallRequest) (oarlock.InstallPlan, error) {
	if i.Line == "" {
		return oarlock.InstallPlan{}, &oarlock.UnsupportedOperation{Operation: "install", Reason: "this runtime has no installer"}
	}
	if req.Line != "" {
		return oarlock.InstallPlan{}, &oarlock.UnsupportedOperation{Operation: "install", Reason: "this runtime ships one release line; InstallRequest.Line must be empty"}
	}
	command := append(i.shell(), i.Line)
	return oarlock.InstallPlan{
		Command:        command,
		ChangesMachine: true,
		Reason:         "vendor's official installer; runs only after an explicit Install call",
	}, nil
}

// Install runs the vendor's installer once, then re-probes. It is idempotent:
// an already-available installation returns without executing anything.
//
// Success is decided by the probe, never by the installer's exit code: a
// `curl ... | sh` pipeline can exit 0 after a failed download.
func (i Installer) Install(ctx context.Context, req oarlock.InstallRequest) (oarlock.InstallResult, error) {
	if i.Probe == nil {
		return oarlock.InstallResult{}, errors.New("ops: Installer.Probe is required")
	}
	before, err := i.Probe.Installation(ctx)
	if err != nil {
		return oarlock.InstallResult{}, fmt.Errorf("probe before install: %w", err)
	}
	if before.Available {
		return oarlock.InstallResult{Installation: before}, nil
	}
	plan, err := i.InstallPlan(ctx, req)
	if err != nil {
		return oarlock.InstallResult{}, err
	}
	timeout := i.Timeout
	if timeout <= 0 {
		timeout = InstallTimeout
	}
	res, runErr := Run(ctx, Command{Path: plan.Command[0], Args: plan.Command[1:]}, timeout)
	if runErr != nil {
		return oarlock.InstallResult{Installation: before}, fmt.Errorf("installer did not finish: %w", runErr)
	}
	after, probeErr := i.Probe.Installation(ctx)
	if probeErr == nil && after.Available {
		return oarlock.InstallResult{Installation: after}, nil
	}
	failure := fmt.Errorf("installer exited %d but the probe found no installation: %s", res.ExitCode, tail(res.Stdout+"\n"+res.Stderr))
	if probeErr != nil {
		failure = errors.Join(failure, fmt.Errorf("probe after install: %w", probeErr))
	}
	return oarlock.InstallResult{Installation: after}, failure
}

func (i Installer) shell() []string {
	if len(i.Shell) > 0 {
		return append([]string(nil), i.Shell...)
	}
	return []string{"sh", "-c"}
}

// LoginChallenge is the captured non-interactive part of a vendor login.
// Login stops the helper as soon as a challenge line appears; it never writes
// stdin. The type is oarlock's; this alias keeps adapter code short.
type LoginChallenge = oarlock.LoginChallenge

// CaptureLogin starts c, returns the first line matching re as a URL
// challenge, and stops the process tree immediately. It never waits for stdin
// and never forwards the child's output anywhere except the returned string.
// If no line matches and the process ends, exit status and a bounded output
// tail are in the error.
func CaptureLogin(ctx context.Context, c Command, urlPattern *regexp.Regexp, timeout time.Duration) (LoginChallenge, string, error) {
	if urlPattern == nil {
		return LoginChallenge{}, "", errors.New("ops: CaptureLogin needs a URL pattern")
	}
	if timeout <= 0 {
		timeout = LoginTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, err := exec.Start(ctx, &exec.Command{
		Path:  c.Path,
		Args:  c.Args,
		Dir:   c.Dir,
		Env:   c.Env,
		Stdin: strings.NewReader(""),
	})
	if err != nil {
		return LoginChallenge{}, "", err
	}
	// Claim the streams synchronously, before the scanners and before any Wait
	// can drain an unclaimed stream (same seam as Run).
	_, _ = cmd.Stdout().Read(nil)
	_, _ = cmd.Stderr().Read(nil)

	var (
		mu  sync.Mutex
		out strings.Builder
	)
	found := make(chan string, 1)
	done := make(chan struct{})
	scan := func(r io.Reader) {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			mu.Lock()
			out.WriteString(line)
			out.WriteByte('\n')
			mu.Unlock()
			if urlPattern.MatchString(line) {
				select {
				case found <- line:
				default:
				}
				return
			}
		}
	}
	go func() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); scan(cmd.Stdout()) }()
		go func() { defer wg.Done(); scan(cmd.Stderr()) }()
		wg.Wait()
		close(done)
	}()

	output := func() string {
		mu.Lock()
		defer mu.Unlock()
		return out.String()
	}
	stop := func() {
		cancel()
		_ = cmd.Close(context.Background())
		_, _ = cmd.Wait(context.Background())
		<-done
	}

	select {
	case line := <-found:
		stop()
		return LoginChallenge{Kind: "url", URL: strings.TrimSpace(urlPattern.FindString(line)), Instructions: "open this URL to sign in; the vendor CLI completes the login"}, output(), nil
	case <-done:
		status, _ := cmd.Wait(context.Background())
		_ = cmd.Close(context.Background())
		return LoginChallenge{}, output(), fmt.Errorf("login helper ended (exit %d) without a challenge: %s", status.Code, tail(output()))
	case <-ctx.Done():
		stop()
		return LoginChallenge{}, output(), fmt.Errorf("no login challenge within %s: %w", timeout, ctx.Err())
	}
}

// UpgradeResult is what the version read-back saw.
type UpgradeResult struct {
	From     string
	To       string
	Output   string
	ExitCode int
}

// Upgrade runs the vendor's updater and judges it only by a version read-back.
// Same version is ErrVersionUnchanged even when the updater exited 0. A
// changed version is success even when the updater exited non-zero: the
// vendor's exit code is not evidence about the machine.
func Upgrade(ctx context.Context, path string, args []string, timeout time.Duration) (UpgradeResult, error) {
	from, err := Version(ctx, path)
	if err != nil {
		return UpgradeResult{}, fmt.Errorf("read version before upgrade: %w", err)
	}
	if timeout <= 0 {
		timeout = UpgradeTimeout
	}
	res, runErr := Run(ctx, Command{Path: path, Args: args}, timeout)
	result := UpgradeResult{From: from, Output: tail(res.Stdout + "\n" + res.Stderr), ExitCode: res.ExitCode}

	to, versionErr := Version(ctx, path)
	result.To = to
	if versionErr != nil {
		if runErr != nil {
			return result, errors.Join(fmt.Errorf("updater: %w", runErr), fmt.Errorf("read version after upgrade: %w", versionErr))
		}
		return result, fmt.Errorf("read version after upgrade: %w", versionErr)
	}
	if to == from {
		return result, fmt.Errorf("%w: %s stayed at %s (updater exit %d): %s", ErrVersionUnchanged, path, to, res.ExitCode, result.Output)
	}
	if runErr != nil {
		return result, fmt.Errorf("updater did not finish, but the version changed %s -> %s: %w", from, to, runErr)
	}
	return result, nil
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= outputTail {
		return s
	}
	return "...[truncated]...\n" + s[len(s)-outputTail:]
}

// FallbackPath returns home/relative for a known install location. It never
// fails: an empty home returns "" and Find skips it.
func FallbackPath(rel string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, rel)
}
