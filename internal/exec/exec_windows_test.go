//go:build windows

package exec

// The tests in this file have NEVER been executed: this work was written on a
// Linux host with no Windows runtime, and the only evidence behind them is
// `GOOS=windows go build ./...` plus `GOOS=windows go vet ./...` — they are
// compile-checked, not verified. They exist so the windows-latest CI leg starts
// exercising the .cmd→.ps1 shim, the job object and the exit-status mapping for
// real; until that leg has run green at least once, treat every assertion here
// as a hypothesis about Win32 behaviour, not as a measured fact.
//
// Where a fixture would need a real Windows host (a PID file, a grandchild, a
// PowerShell script), the test uses this very test binary in helper mode, which
// keeps the assertions to things Go can observe portably on Windows.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// winHelperEnv points the re-executed test binary at the file it must write its
// own pid into before going to sleep.
const winHelperEnv = "OARLOCK_EXEC_WINDOWS_HELPER_PID_FILE"

// winSynchronize is SYNCHRONIZE, the access right WaitForSingleObject needs to
// watch a process handle. syscall exports the rest of the rights used below.
const winSynchronize = 0x00100000

// winWaitObject0 is WAIT_OBJECT_0, the "handle is signalled" answer, which for a
// process handle means "the process has exited".
const winWaitObject0 = 0x00000000

// TestWindowsTreeHelperProcess is not a test: it is the grandchild fixture for
// TestWindowsCloseKillsDescendants. It records its own pid and then stays alive
// until something terminates it. It always leaves via os.Exit so the testing
// framework never prints into a pipe the parent may be reading.
func TestWindowsTreeHelperProcess(t *testing.T) {
	pidFile := os.Getenv(winHelperEnv)
	if pidFile == "" {
		return // an ordinary `go test` run of this file
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\r\n"), 0o644); err != nil {
		os.Exit(90)
	}
	time.Sleep(60 * time.Second)
	os.Exit(0)
}

// winProcessExited reports whether pid has exited (or cannot be opened at all,
// which means the process object is gone).
//
// A process handle is the only portable way to ask about a pid on Windows: a
// terminated process whose handle is still held by a parent keeps its pid
// reserved, so "can I still open it" is not the question — "has it exited" is.
// WaitForSingleObject with a zero timeout answers exactly that, which is why the
// handle is requested with SYNCHRONIZE as well as PROCESS_QUERY_INFORMATION.
func winProcessExited(pid int) bool {
	h, err := syscall.OpenProcess(processQueryInformation|winSynchronize, false, uint32(pid))
	if err != nil {
		return true
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	event, err := syscall.WaitForSingleObject(h, 0)
	if err != nil {
		return true
	}
	return event == winWaitObject0
}

// winWaitProcessExited polls winProcessExited until it is true or the bound
// expires, and returns false when the process outlived the bound.
func winWaitProcessExited(pid int, limit time.Duration) bool {
	if pid <= 0 {
		return true
	}
	deadline := time.Now().Add(limit)
	for {
		if winProcessExited(pid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// winWriteFile writes a fixture file and fails the test if it cannot.
func winWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture %q = %v", path, err)
	}
}

// winReadPidFile parses the single pid the helper fixture records.
func winReadPidFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("pid file %q holds %q, which is not a pid", path, strings.TrimSpace(string(data)))
	}
	return pid, nil
}

// winWaitPidFile polls the helper's pid file until it holds a complete pid.
func winWaitPidFile(path string, limit time.Duration) (int, error) {
	deadline := time.Now().Add(limit)
	var lastErr error
	for {
		pid, err := winReadPidFile(path)
		if err == nil {
			return pid, nil
		}
		lastErr = err // the helper may have been caught mid-write
		if !time.Now().Before(deadline) {
			return 0, fmt.Errorf("waiting for a pid in %q: %w", path, lastErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestWindowsShimInvocationPreservesMultilinePromptArgument is the acceptance
// evidence for issue #16's "多行 prompt 经 .cmd 路径不被打散" criterion.
//
// It drives the real resolver — a real .cmd launcher with a real sibling .ps1 in
// a real directory — rather than a fake filesystem, because the bug being
// prevented (cmd.exe's %* re-tokenisation) is entirely about what the OS
// receives as a command line. The prompt is deliberately hostile: two line
// endings, embedded double quotes, %*, ^ and & — every one of which cmd.exe
// would reinterpret.
//
// NOT VERIFIED AT RUNTIME: written blind on Linux, compile-checked only.
func TestWindowsShimInvocationPreservesMultilinePromptArgument(t *testing.T) {
	dir := t.TempDir()
	launcher := filepath.Join(dir, "cursor-agent.cmd")
	script := filepath.Join(dir, "cursor-agent.ps1")
	winWriteFile(t, launcher, "@echo off\r\npowershell -NoProfile -ExecutionPolicy Bypass -File \"%~dp0cursor-agent.ps1\" %*\r\n")
	winWriteFile(t, script, "Write-Output $args.Count\r\n")

	if _, ok := powerShellPath(); !ok {
		t.Skip("no PowerShell host on PATH and none under %SystemRoot%\\System32\\WindowsPowerShell\\v1.0: the shim cannot resolve an entry point here")
	}

	prompt := "first line\nsecond \"quoted\" line && %* ^caret\r\nfourth line"
	exe, argv, err := resolveInvocation(launcher, []string{"--print", prompt})
	if err != nil {
		t.Fatalf("resolveInvocation(%q) = %v, want the PowerShell invocation through the .ps1 sibling", launcher, err)
	}

	// argv[0] is the host and the same string Start hands to the OS as Path.
	if len(argv) == 0 {
		t.Fatal("argv = empty, want argv[0] to be the PowerShell host")
	}
	if argv[0] != exe {
		t.Fatalf("argv[0] = %q, want the resolved executable %q (the os/exec convention Start relies on)", argv[0], exe)
	}
	switch host := strings.ToLower(filepath.Base(exe)); host {
	case "powershell.exe", "pwsh.exe":
	default:
		t.Fatalf("executable = %q, want the PowerShell host (powershell.exe or pwsh.exe)", exe)
	}

	// -File <ps1> must be present, in order, with the script as its own element.
	wantPrefix := []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script}
	if len(argv) < 1+len(wantPrefix)-1 {
		t.Fatalf("argv = %q, want the host, %q and the caller's arguments", argv, wantPrefix)
	}
	for i, want := range wantPrefix {
		if i+1 >= len(argv) {
			t.Fatalf("argv = %q, want %q at position %d", argv, want, i+1)
		}
		if argv[i+1] != want {
			t.Fatalf("argv[%d] = %q, want %q (argv = %q)", i+1, argv[i+1], want, argv)
		}
	}

	// Exactly the caller's arguments follow, and each one is one discrete
	// element: that is the whole point of the shim.
	rest := argv[1+len(wantPrefix):]
	if len(rest) != 2 {
		t.Fatalf("the caller's arguments arrived as %d elements (%q), want 2 (--print and the prompt)", len(rest), rest)
	}
	if rest[0] != "--print" {
		t.Fatalf("first caller argument = %q, want %q", rest[0], "--print")
	}
	if rest[1] != prompt {
		t.Fatalf("the prompt arrived as %q, want it byte-identical: a .cmd launcher must not re-tokenise it (want %q)", rest[1], prompt)
	}

	// No fragment leaked into a neighbouring element, and the prompt appears
	// exactly once. A re-tokenising path would split it at the newline or at
	// the quotes and produce several elements containing pieces of it.
	occurrences := 0
	for _, arg := range argv {
		if arg == prompt {
			occurrences++
		}
		if arg == prompt {
			continue
		}
		for _, fragment := range []string{"first line", "second", "quoted", "fourth line"} {
			if strings.Contains(arg, fragment) {
				t.Fatalf("argv element %q carries the fragment %q of the prompt: it was split", arg, fragment)
			}
		}
	}
	if occurrences != 1 {
		t.Fatalf("the prompt appears %d times in argv (%q), want exactly once", occurrences, argv)
	}
}

// TestWindowsShimWithoutPS1ReportsErrShim pins the "never fall back to cmd.exe
// re-parsing" rule: a lone .cmd with no .ps1 sibling must fail loudly.
//
// NOT VERIFIED AT RUNTIME: written blind on Linux, compile-checked only.
func TestWindowsShimWithoutPS1ReportsErrShim(t *testing.T) {
	dir := t.TempDir()
	launcher := filepath.Join(dir, "lone-agent.cmd")
	winWriteFile(t, launcher, "@echo off\r\necho %*\r\n")

	exe, argv, err := resolveInvocation(launcher, []string{"--print", "a prompt with spaces"})
	if err == nil {
		t.Fatalf("resolveInvocation(%q) = (%q, %q, nil), want an ErrShim failure: there is no .ps1 sibling to run", launcher, exe, argv)
	}
	if !errors.Is(err, ErrShim) {
		t.Fatalf("errors.Is(err, ErrShim) = false, want true (err = %T: %v)", err, err)
	}
	if exe != "" || argv != nil {
		t.Fatalf("resolveInvocation on failure = (%q, %q), want (\"\", nil): a refused launcher must not resolve to anything", exe, argv)
	}

	// The message must be actionable. Only the base name is asserted: the full
	// path goes through %q, which doubles every backslash, so it does not
	// appear verbatim in the message.
	msg := err.Error()
	for _, want := range []string{"lone-agent.cmd", ".ps1", "cmd.exe"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error message %q does not mention %q", msg, want)
		}
	}
}

// TestWindowsExitStatusFromCmdExe pins the exit-status mapping through a real
// child: Windows has no signals, so a plain non-zero exit is Code with a nil
// Signal, and Wait is not an error for it.
//
// NOT VERIFIED AT RUNTIME: written blind on Linux, compile-checked only.
func TestWindowsExitStatusFromCmdExe(t *testing.T) {
	cmd, err := Start(context.Background(), &Command{Path: "cmd.exe", Args: []string{"/c", "exit 3"}})
	if err != nil {
		t.Fatalf("Start(cmd.exe /c exit 3) = %v, want a running child", err)
	}
	defer func() { _ = cmd.Close(context.Background()) }()

	if pid := cmd.Pid(); pid <= 0 {
		t.Fatalf("Pid() = %d, want a positive pid", pid)
	}
	status, waitErr := cmd.Wait(context.Background())
	if waitErr != nil {
		t.Fatalf("Wait() error = %v, want nil (a non-zero exit is ExitStatus data)", waitErr)
	}
	if status.Code != 3 {
		t.Fatalf("ExitStatus.Code = %d, want 3", status.Code)
	}
	if status.Signal != nil {
		t.Fatalf("ExitStatus.Signal = %v, want nil: Windows has no signals", status.Signal)
	}
	if status.Success() {
		t.Fatal("ExitStatus.Success() = true, want false for exit code 3")
	}
	if err := cmd.Close(context.Background()); err != nil {
		t.Fatalf("Close() after the child exited = %v, want nil for an already gone tree", err)
	}
}

// TestWindowsCloseKillsDescendants is the acceptance evidence for the Windows
// half of issue #16's tree kill: the leader is cmd.exe, the grandchild is this
// very test binary in helper mode (it records its pid and sleeps), and the job
// object created with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE has to hold both.
//
// cmd.exe waits for its child, so both are alive when Close runs. Close has no
// SIGTERM to send on Windows — there is no such concept — so it waits out
// KillGrace and then terminates the job; KillGrace is deliberately short here
// and the overall poll bound deliberately generous.
//
// NOT VERIFIED AT RUNTIME: written blind on Linux, compile-checked only. The job
// object, the suspended start and the resume are unexercised code paths on this
// host.
func TestWindowsCloseKillsDescendants(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v, want the test binary path", err)
	}
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")

	// One argv element per token: cmd.exe is handed the helper's path without
	// any embedded quoting, so no cmd.exe /c quoting rule can bite here. Go
	// quotes the element itself if the path contains spaces.
	c := &Command{
		Path: "cmd.exe",
		Args: []string{"/c", exe, "-test.run=TestWindowsTreeHelperProcess$"},
		Env: append(os.Environ(),
			// GORACE only silences the race runtime's one-second at-exit
			// sleep, which would otherwise dominate the fixture launch.
			"GORACE=atexit_sleep_ms=0",
			winHelperEnv+"="+pidFile,
		),
		KillGrace: 500 * time.Millisecond, // spent by design: Windows has no SIGTERM
		WaitDelay: 10 * time.Second,
	}

	cmd, err := Start(context.Background(), c)
	if err != nil {
		t.Fatalf("Start(cmd.exe /c <helper>) = %v, want a running tree", err)
	}
	if ownErr := cmd.OwnershipError(); ownErr != nil {
		_ = cmd.Close(context.Background())
		t.Fatalf("OwnershipError() = %v, want nil: without the job object the tree cannot be killed as a tree", ownErr)
	}

	leader := cmd.Pid()
	grandchild, err := winWaitPidFile(pidFile, 15*time.Second)
	if err != nil {
		_ = cmd.Close(context.Background())
		t.Fatalf("the helper never recorded its pid: %v", err)
	}
	if grandchild == leader {
		t.Fatalf("the helper pid %d equals the leader pid %d: there is no grandchild to kill", grandchild, leader)
	}
	if winProcessExited(grandchild) {
		t.Fatalf("the grandchild %d had already exited before Close ran, want it alive", grandchild)
	}

	if err := cmd.Close(context.Background()); err != nil {
		t.Fatalf("Close() = %v, want nil for a tree it just terminated", err)
	}
	status, waitErr := cmd.Wait(context.Background())
	if waitErr != nil {
		t.Fatalf("Wait() error = %v, want nil (termination is ExitStatus data)", waitErr)
	}
	if status.Success() {
		t.Fatalf("ExitStatus = %+v, want a non-success outcome: the job was terminated", status)
	}

	// The leader is reaped by Wait, so the interesting obligation is the
	// grandchild: the job object must have killed it too.
	if !winWaitProcessExited(grandchild, 15*time.Second) {
		t.Fatalf("the grandchild %d was still running 15s after Close: it escaped the job object", grandchild)
	}
}
