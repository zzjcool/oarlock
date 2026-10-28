package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests below never shell out to /bin/sh or cmd.exe: the child is this very
// test binary re-executed in a helper mode. That keeps them portable (the same
// binary runs on linux, macOS and windows) and dependency-free, which is the
// only way "ProcessStart/Wait/Close" can be asserted on all three platforms
// without a fixture CLI.
const (
	// helperEnv selects the helper behaviour in the re-executed binary.
	helperEnv = "EXEC_TEST_HELPER_MODE"

	// helperStdin is the payload the echo helpers send back; small on purpose,
	// the large-payload case has its own test with a concurrent reader.
	helperStdin = "hello from stdin\nwith a second line\n"

	// helperMarkerEnv names the file the pid-recording fixture writes. It is
	// how the cancellation tests learn which pids they must see die.
	helperMarkerEnv = "OARLOCK_EXEC_TEST_MARKER"
)

// helperCommand builds a Command that re-runs this test binary in helper mode.
func helperCommand(t *testing.T, mode string) *Command {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v, want the test binary path", err)
	}
	return &Command{
		Path: exe,
		Args: []string{"-test.run=TestHelperProcess$"},
		// GORACE only quiets the race runtime's one-second at-exit sleep, which
		// would otherwise dominate every helper launch; it changes no
		// behaviour under test.
		Env:       append(os.Environ(), "GORACE=atexit_sleep_ms=0", helperEnv+"="+mode),
		KillGrace: 2 * time.Second,
		WaitDelay: 5 * time.Second,
	}
}

// TestHelperProcess is not a test: it is the child body of the re-executed test
// binary. Each mode exits with the well-known status the parent asserts on, and
// always via os.Exit so the testing framework never prints its PASS banner into
// the pipe the parent is reading.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		return // an ordinary `go test` run of this file
	}
	switch mode {
	case "exit0":
		os.Exit(0)
	case "exit3":
		os.Exit(3)
	case "echo-stdin":
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			os.Exit(9)
		}
		os.Exit(0)
	case "echo-both":
		fmt.Fprint(os.Stdout, "stdout-payload")
		fmt.Fprint(os.Stderr, "stderr-payload")
		os.Exit(0)
	case "sleep":
		// Alive but idle until someone signals or terminates the tree.
		time.Sleep(60 * time.Second)
		os.Exit(0)
	case "record-pid":
		// Record our pid, then stay alive until the tree is terminated: the
		// cancellation tests use this to prove no orphan survived.
		if path := os.Getenv(helperMarkerEnv); path != "" {
			if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
				os.Exit(96)
			}
		}
		time.Sleep(60 * time.Second)
		os.Exit(0)
	case "spawn-grandchild-then-exit":
		// The wrapper-exits-while-a-grandchild-holds-stdout case: the leader
		// leaves a descendant behind that inherited the stdout/stderr write
		// ends and then exits itself.
		exe, err := os.Executable()
		if err != nil {
			os.Exit(95)
		}
		grandchild := osexec.Command(exe, "-test.run=TestHelperProcess$")
		grandchild.Env = append(os.Environ(), helperEnv+"=sleep")
		grandchild.Stdout = os.Stdout
		grandchild.Stderr = os.Stderr
		if err := grandchild.Start(); err != nil {
			os.Exit(98)
		}
		os.Exit(0)
	}
	os.Exit(97)
}

// readAll drains rc completely, failing the test on a read error.
func readAll(t *testing.T, rc io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("io.ReadAll = %v, want the full stream and io.EOF", err)
	}
	return string(b)
}

// TestStartWaitExitStatusSuccess pins the happy path: one child, exit code 0,
// no signal, no error, and a real pid.
func TestStartWaitExitStatusSuccess(t *testing.T) {
	cmd, err := Start(context.Background(), helperCommand(t, "exit0"))
	if err != nil {
		t.Fatalf("Start() = %v, want a running child", err)
	}
	defer func() { _ = cmd.Close(context.Background()) }()

	if pid := cmd.Pid(); pid <= 0 {
		t.Fatalf("Pid() = %d, want a positive pid", pid)
	}

	status, err := cmd.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v, want nil (exit code lives in ExitStatus)", err)
	}
	if status.Code != 0 {
		t.Fatalf("ExitStatus.Code = %d, want 0", status.Code)
	}
	if status.Signal != nil {
		t.Fatalf("ExitStatus.Signal = %v, want nil", status.Signal)
	}
	if !status.Success() {
		t.Fatal("ExitStatus.Success() = false, want true for a clean exit")
	}
}

// TestWaitReportsNonZeroExitAsExitStatusNotError pins that a failing child is
// data, not an error: only Start failures, ErrWaitDelay and real faults are
// errors.
func TestWaitReportsNonZeroExitAsExitStatusNotError(t *testing.T) {
	cmd, err := Start(context.Background(), helperCommand(t, "exit3"))
	if err != nil {
		t.Fatalf("Start() = %v, want a running child", err)
	}
	defer func() { _ = cmd.Close(context.Background()) }()

	status, err := cmd.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v, want nil for a non-zero exit", err)
	}
	if status.Code != 3 {
		t.Fatalf("ExitStatus.Code = %d, want 3", status.Code)
	}
	if status.Signal != nil {
		t.Fatalf("ExitStatus.Signal = %v, want nil (the child exited, it was not killed)", status.Signal)
	}
	if status.Success() {
		t.Fatal("ExitStatus.Success() = true, want false for exit code 3")
	}
}

// TestWaitIsIdempotentAfterDoubleWait pins the sync.Once contract: the second
// Wait returns the stored result instead of "Wait was already called", and it
// works after Close too.
func TestWaitIsIdempotentAfterDoubleWait(t *testing.T) {
	cmd, err := Start(context.Background(), helperCommand(t, "exit3"))
	if err != nil {
		t.Fatalf("Start() = %v, want a running child", err)
	}

	first, firstErr := cmd.Wait(context.Background())
	if firstErr != nil {
		t.Fatalf("first Wait() error = %v, want nil", firstErr)
	}
	second, secondErr := cmd.Wait(context.Background())
	if second != first || secondErr != firstErr {
		t.Fatalf("second Wait() = (%+v, %v), want the stored (%+v, %v)", second, secondErr, first, firstErr)
	}

	if err := cmd.Close(context.Background()); err != nil {
		t.Fatalf("Close() after Wait = %v, want nil for an already exited tree", err)
	}
	third, thirdErr := cmd.Wait(context.Background())
	if third != first || thirdErr != firstErr {
		t.Fatalf("Wait() after Close = (%+v, %v), want the stored (%+v, %v)", third, thirdErr, first, firstErr)
	}
}

// TestStdoutAndStderrReturnStableStreamsWithOutput pins that Stdout()/Stderr()
// return the same stream on every call and that the child's output arrives.
func TestStdoutAndStderrReturnStableStreamsWithOutput(t *testing.T) {
	cmd, err := Start(context.Background(), helperCommand(t, "echo-both"))
	if err != nil {
		t.Fatalf("Start() = %v, want a running child", err)
	}
	defer func() { _ = cmd.Close(context.Background()) }()

	if cmd.Stdout() != cmd.Stdout() {
		t.Fatal("Stdout() returned two different streams, want the same one")
	}
	if cmd.Stderr() != cmd.Stderr() {
		t.Fatal("Stderr() returned two different streams, want the same one")
	}

	if got := readAll(t, cmd.Stdout()); got != "stdout-payload" {
		t.Fatalf("stdout = %q, want %q", got, "stdout-payload")
	}
	if got := readAll(t, cmd.Stderr()); got != "stderr-payload" {
		t.Fatalf("stderr = %q, want %q", got, "stderr-payload")
	}
	if _, err := cmd.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}
}

// TestStdinCloseIsEOFForTheChild pins the nil-Command.Stdin case: the caller
// owns the write end, and closing it is what ends the child's input.
func TestStdinCloseIsEOFForTheChild(t *testing.T) {
	cmd, err := Start(context.Background(), helperCommand(t, "echo-stdin"))
	if err != nil {
		t.Fatalf("Start() = %v, want a running child", err)
	}
	defer func() { _ = cmd.Close(context.Background()) }()

	if cmd.Stdin() != cmd.Stdin() {
		t.Fatal("Stdin() returned two different writers, want the same one")
	}
	if _, err := io.WriteString(cmd.Stdin(), helperStdin); err != nil {
		t.Fatalf("writing to Stdin() = %v, want the payload accepted", err)
	}
	if err := cmd.Stdin().Close(); err != nil {
		t.Fatalf("Stdin().Close() = %v, want nil (and EOF for the child)", err)
	}

	if got := readAll(t, cmd.Stdout()); got != helperStdin {
		t.Fatalf("echoed stdin = %q, want %q", got, helperStdin)
	}
	if status, err := cmd.Wait(context.Background()); err != nil || !status.Success() {
		t.Fatalf("Wait() = (%+v, %v), want a clean exit", status, err)
	}
}

// TestCommandStdinIsCopiedToTheChild pins the Command.Stdin case: the package
// copies the reader in and turns its EOF into the pipe's EOF, without Start
// ever blocking on the payload.
func TestCommandStdinIsCopiedToTheChild(t *testing.T) {
	c := helperCommand(t, "echo-stdin")
	c.Stdin = strings.NewReader(helperStdin)

	cmd, err := Start(context.Background(), c)
	if err != nil {
		t.Fatalf("Start() = %v, want a running child", err)
	}
	defer func() { _ = cmd.Close(context.Background()) }()

	if got := readAll(t, cmd.Stdout()); got != helperStdin {
		t.Fatalf("echoed stdin = %q, want %q", got, helperStdin)
	}
	if status, err := cmd.Wait(context.Background()); err != nil || !status.Success() {
		t.Fatalf("Wait() = (%+v, %v), want a clean exit", status, err)
	}
}

// TestCommandStdinLargePayloadDoesNotDeadlock pins the 64KiB-pipe rule: a
// payload larger than the pipe buffer is delivered while the child is writing
// back, so neither side can wedge the other.
func TestCommandStdinLargePayloadDoesNotDeadlock(t *testing.T) {
	payload := strings.Repeat("0123456789abcdef", 1<<14) // 256 KiB
	c := helperCommand(t, "echo-stdin")
	c.Stdin = strings.NewReader(payload)
	c.WaitDelay = 10 * time.Second

	cmd, err := Start(context.Background(), c)
	if err != nil {
		t.Fatalf("Start() = %v, want a running child", err)
	}
	defer func() { _ = cmd.Close(context.Background()) }()

	// Reading concurrently with the copy is what a real caller does; the
	// package must not need it to survive Start (Start returns before the
	// payload is written) but it must survive it here.
	got := readAll(t, cmd.Stdout())
	if got != payload {
		t.Fatalf("echoed %d bytes, want %d", len(got), len(payload))
	}
	if status, err := cmd.Wait(context.Background()); err != nil || !status.Success() {
		t.Fatalf("Wait() = (%+v, %v), want a clean exit", status, err)
	}
}

// TestWaitDelayBoundsCollectionWhenAGrandchildHoldsStdout pins the WaitDelay
// rule end to end: a wrapper exits, a grandchild keeps the output pipe open,
// and collection still ends — with io.EOF for the caller and ErrWaitDelay for
// Wait — instead of blocking the host forever.
func TestWaitDelayBoundsCollectionWhenAGrandchildHoldsStdout(t *testing.T) {
	c := helperCommand(t, "spawn-grandchild-then-exit")
	c.KillGrace = 500 * time.Millisecond
	c.WaitDelay = 300 * time.Millisecond

	cmd, err := Start(context.Background(), c)
	if err != nil {
		t.Fatalf("Start() = %v, want a running child", err)
	}

	// Reading claims the stream: from here on Wait waits for EOF rather than
	// draining it, which is exactly the case WaitDelay has to bound.
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, cmd.Stdout())
		readDone <- err
	}()

	status, err := cmd.Wait(context.Background())
	if !errors.Is(err, ErrWaitDelay) {
		t.Fatalf("Wait() error = %v, want ErrWaitDelay when a grandchild holds stdout", err)
	}
	if !status.Success() {
		t.Fatalf("ExitStatus = %+v, want the leader's clean exit even when the drain timed out", status)
	}

	select {
	case rerr := <-readDone:
		if rerr != nil {
			t.Fatalf("reading stdout = %v, want io.EOF after the force-close", rerr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stdout reader still blocked after Wait returned, want the force-close to release it with io.EOF")
	}

	// Close is what actually reaps the tree the wrapper left behind.
	if err := cmd.Close(context.Background()); err != nil {
		t.Fatalf("Close() = %v, want nil for a tree it just terminated", err)
	}
}

// TestCloseIsIdempotent pins that Close can be called twice, and returns nil
// for a tree that is already gone.
func TestCloseIsIdempotent(t *testing.T) {
	cmd, err := Start(context.Background(), helperCommand(t, "exit0"))
	if err != nil {
		t.Fatalf("Start() = %v, want a running child", err)
	}
	if _, err := cmd.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}
	for i := 0; i < 3; i++ {
		if err := cmd.Close(context.Background()); err != nil {
			t.Fatalf("Close() #%d = %v, want nil for an already gone tree", i+1, err)
		}
	}
}

// TestCloseTerminatesARunningTree pins that Close ends a child that is still
// running, and that the outcome shows up in Wait's ExitStatus rather than as an
// error.
func TestCloseTerminatesARunningTree(t *testing.T) {
	c := helperCommand(t, "sleep")
	c.KillGrace = 200 * time.Millisecond

	cmd, err := Start(context.Background(), c)
	if err != nil {
		t.Fatalf("Start() = %v, want a running child", err)
	}

	if err := cmd.Close(context.Background()); err != nil {
		t.Fatalf("Close() = %v, want nil for a tree it just terminated", err)
	}
	status, err := cmd.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v, want nil (termination lives in ExitStatus)", err)
	}
	if status.Success() {
		t.Fatalf("ExitStatus = %+v, want a non-success outcome after Close terminated the tree", status)
	}
}

// TestCloseIsSafeConcurrentlyWithWait pins the frozen rule that Close may run
// while another goroutine is inside Wait: no double reap, no lost status, no
// race between signalling and reaping.
func TestCloseIsSafeConcurrentlyWithWait(t *testing.T) {
	c := helperCommand(t, "sleep")
	c.KillGrace = 500 * time.Millisecond

	cmd, err := Start(context.Background(), c)
	if err != nil {
		t.Fatalf("Start() = %v, want a running child", err)
	}

	type result struct {
		status ExitStatus
		err    error
	}
	waited := make(chan result, 1)
	go func() {
		status, err := cmd.Wait(context.Background())
		waited <- result{status, err}
	}()

	// Not required for correctness: it just makes the interleaving where Wait
	// is blocked on the reaper when Close signals the likely one.
	time.Sleep(20 * time.Millisecond)
	if err := cmd.Close(context.Background()); err != nil {
		t.Fatalf("Close() concurrent with Wait = %v, want nil", err)
	}

	select {
	case got := <-waited:
		if got.err != nil {
			t.Fatalf("Wait() error = %v, want nil (termination is an ExitStatus)", got.err)
		}
		if got.status.Success() {
			t.Fatalf("ExitStatus = %+v, want a non-success outcome after Close", got.status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return after Close, want it to observe the terminated tree")
	}
	if err := cmd.Close(context.Background()); err != nil {
		t.Fatalf("second Close() = %v, want nil", err)
	}
}

// TestCloseReturnsPromptlyWhenTheLeaderIsAnUnreapedZombie pins the liveness
// probe against the mistake that makes Close useless in practice: Close does
// not reap, so the leader it has just killed is an un-reaped zombie, and a
// group probe that counts zombies as alive would make every Close burn the
// whole KillGrace (10s by default) on a tree that is already dead.
//
// The zombie refinement is Linux-only (/proc). Other POSIX kernels fall back to
// the coarse group probe, which cannot tell a zombie from a live member and so
// spends the full KillGrace by design (contract limitation 3) — asserting
// promptness there would be a test of the kernel, not of this package.
//
// Windows is skipped on purpose: it has no SIGTERM, so Close deliberately
// spends KillGrace waiting for a voluntary exit before it terminates the job.
func TestCloseReturnsPromptlyWhenTheLeaderIsAnUnreapedZombie(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the zombie-aware group probe reads /proc and exists only on Linux")
	}

	cmd, err := Start(context.Background(), helperCommand(t, "exit0")) // default KillGrace: 10s
	if err != nil {
		t.Fatalf("Start() = %v, want a running child", err)
	}
	// Drain stdout to EOF: the leader has exited, and nobody has reaped it yet.
	if got := readAll(t, cmd.Stdout()); got != "" {
		t.Fatalf("stdout = %q, want empty", got)
	}

	started := time.Now()
	if err := cmd.Close(context.Background()); err != nil {
		t.Fatalf("Close() = %v, want nil for a tree that already exited", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Close() took %v, want well under the %v KillGrace for a dead tree (the leader is an un-reaped zombie, not a live process)", elapsed, DefaultKillGrace)
	}
	if _, err := cmd.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}
}

// TestSignalOnExitedTreeIsNotAnError pins "already gone is not an error" for
// Signal, which Close relies on.
func TestSignalOnExitedTreeIsNotAnError(t *testing.T) {
	cmd, err := Start(context.Background(), helperCommand(t, "exit0"))
	if err != nil {
		t.Fatalf("Start() = %v, want a running child", err)
	}
	defer func() { _ = cmd.Close(context.Background()) }()
	if _, err := cmd.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}
	if err := cmd.Signal(os.Kill); err != nil {
		t.Fatalf("Signal(os.Kill) on an exited tree = %v, want nil", err)
	}
}

// TestStdoutReportsEOFAfterCallerClose pins the io.ReadCloser half of the
// stream contract: closing it early turns later reads into io.EOF instead of
// "file already closed".
func TestStdoutReportsEOFAfterCallerClose(t *testing.T) {
	c := helperCommand(t, "sleep")
	c.KillGrace = 200 * time.Millisecond

	cmd, err := Start(context.Background(), c)
	if err != nil {
		t.Fatalf("Start() = %v, want a running child", err)
	}
	if err := cmd.Stdout().Close(); err != nil {
		t.Fatalf("Stdout().Close() = %v, want nil", err)
	}
	if b, err := io.ReadAll(cmd.Stdout()); err != io.EOF && len(b) != 0 {
		t.Fatalf("Read after Close = (%q, %v), want (empty, io.EOF)", b, err)
	}
	if err := cmd.Close(context.Background()); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	if _, err := cmd.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}
}

// TestOwnershipErrorIsNilWhenTreeIsOwned pins the normal case on every
// platform: POSIX always owns its process group, and Windows owns the job
// unless assignment failed.
func TestOwnershipErrorIsNilWhenTreeIsOwned(t *testing.T) {
	cmd, err := Start(context.Background(), helperCommand(t, "exit0"))
	if err != nil {
		t.Fatalf("Start() = %v, want a running child", err)
	}
	defer func() { _ = cmd.Close(context.Background()) }()

	if err := cmd.OwnershipError(); err != nil {
		t.Fatalf("OwnershipError() = %v, want nil for a tree we started and own", err)
	}
	if _, err := cmd.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}
}

// TestExitStatusSuccessIsAFunctionOfCodeAndSignal pins the predicate itself;
// the type is data and must not depend on any process.
func TestExitStatusSuccessIsAFunctionOfCodeAndSignal(t *testing.T) {
	cases := []struct {
		name   string
		status ExitStatus
		want   bool
	}{
		{"CleanExit", ExitStatus{}, true},
		{"NonZeroExit", ExitStatus{Code: 1}, false},
		{"Signalled", ExitStatus{Code: -1, Signal: os.Kill}, false},
		{"SignalWithZeroCode", ExitStatus{Code: 0, Signal: os.Interrupt}, false},
	}
	for _, tc := range cases {
		if got := tc.status.Success(); got != tc.want {
			t.Errorf("%s: ExitStatus%+v.Success() = %v, want %v", tc.name, tc.status, got, tc.want)
		}
	}
}

// TestStartOfMissingBareNameFails pins that a bare name goes through PATH and
// that Start reports the failure instead of starting something else.
func TestStartOfMissingBareNameFails(t *testing.T) {
	cmd, err := Start(context.Background(), &Command{Path: "oarlock-no-such-binary-16c0ffee"})
	if err == nil {
		if cmd != nil {
			_ = cmd.Close(context.Background())
			_, _ = cmd.Wait(context.Background())
		}
		t.Fatal("Start(bare missing name) = nil error, want a PATH resolution failure")
	}
	if cmd != nil {
		t.Fatalf("Start(bare missing name) = (%v, %v), want a nil *Cmd on failure", cmd, err)
	}
}

// TestStartRejectsEmptyPath pins the only structural validation Start does.
func TestStartRejectsEmptyPath(t *testing.T) {
	if _, err := Start(context.Background(), &Command{}); err == nil {
		t.Fatal("Start(context.Background(), &Command{}) = nil error, want a path validation error")
	}
	if _, err := Start(context.Background(), nil); err == nil {
		t.Fatal("Start(context.Background(), nil) = nil error, want an error")
	}
}

// TestStreamReadAfterForceCloseReportsEOF pins the WaitDelay mechanism in
// isolation: forceClose makes a blocked/next read report io.EOF, never a
// "closed file" error that would escape into a caller's scanner.
func TestStreamReadAfterForceCloseReportsEOF(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() = %v", err)
	}
	defer func() { _ = w.Close() }()

	s := newStream(r)
	if _, err := io.WriteString(w, "leftover\n"); err != nil {
		t.Fatalf("writing to the pipe = %v", err)
	}
	first := make([]byte, 16)
	if n, err := s.Read(first); err != nil || n == 0 {
		t.Fatalf("first Read = (%d, %v), want data and nil", n, err)
	}
	s.forceClose()
	if n, err := s.Read(first); n != 0 || err != io.EOF {
		t.Fatalf("Read after forceClose = (%d, %v), want (0, io.EOF)", n, err)
	}
	select {
	case <-s.done:
	default:
		t.Fatal("done is not closed after a terminal read, want it closed exactly once")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() after forceClose = %v, want nil", err)
	}
}

// TestStreamDiscardsClosedPipeAsEOF pins the other closed-file spelling: a read
// on a pipe whose write end vanished reports io.EOF, not io.ErrClosedPipe.
func TestStreamDiscardsClosedPipeAsEOF(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() = %v", err)
	}
	s := newStream(r)
	if err := w.Close(); err != nil {
		t.Fatalf("closing the write end = %v", err)
	}
	if b, err := io.ReadAll(s); err != io.EOF && len(b) != 0 {
		t.Fatalf("ReadAll on a closed pipe = (%q, %v), want (empty, io.EOF)", b, err)
	}
}

// TestStdinCopierClosesTheWriteEndOnEOF pins that Command.Stdin's EOF becomes
// the child's EOF even when the child never reads: Wait must not hang, and the
// copy goroutine must be able to finish.
func TestStdinCopierClosesTheWriteEndOnEOF(t *testing.T) {
	c := helperCommand(t, "exit0")
	c.Stdin = bytes.NewReader(make([]byte, 0))
	cmd, err := Start(context.Background(), c)
	if err != nil {
		t.Fatalf("Start() = %v, want a running child", err)
	}
	defer func() { _ = cmd.Close(context.Background()) }()
	if _, err := cmd.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}
}

// TestStartReturnsErrorBeforeAnyGoroutine pins that a failed Start leaves
// nothing behind: no pid, no stream that pretends to work.
func TestStartReturnsErrorBeforeAnyGoroutine(t *testing.T) {
	cmd, err := Start(context.Background(), &Command{Path: "/nonexistent/oarlock-16-missing"})
	if err == nil {
		if cmd != nil {
			_ = cmd.Close(context.Background())
			_, _ = cmd.Wait(context.Background())
		}
		t.Fatal("Start(nonexistent absolute path) = nil error, want a start failure")
	}
	if !strings.Contains(err.Error(), "no such file") && !os.IsNotExist(errors.Unwrap(err)) {
		t.Logf("Start() error = %v (informational: the message is platform specific)", err)
	}
}

// ---------------------------------------------------------------------------
// context.Context cancellation tests (contract §10)
//
// Every case below is self-bounding: the blocking calls run through ctxRun,
// which fails the test when the call has not returned within an explicit
// deadline, so a hang fails with a message instead of relying on `go test
// -timeout` to kill the whole package. The fixtures record their pids in a file,
// and every liveness assertion polls with an explicit bound of its own.
// ---------------------------------------------------------------------------

// ctxStartResult is one Start call's outcome, carried across goroutines.
type ctxStartResult struct {
	cmd *Cmd
	err error
}

// ctxCallResult is one Wait or Close call's outcome, carried across goroutines.
type ctxCallResult struct {
	status ExitStatus
	err    error
}

// ctxRun runs fn on its own goroutine and fails the test when it has not
// returned within limit. fn must report through its return value, never
// through *testing.T: the testing package forbids Fatal from another
// goroutine.
func ctxRun[T any](t *testing.T, limit time.Duration, what string, fn func() T) T {
	t.Helper()
	done := make(chan T, 1)
	go func() { done <- fn() }()
	select {
	case v := <-done:
		return v
	case <-time.After(limit):
		var zero T
		t.Fatalf("%s did not return within the hard deadline of %v: a blocking call did not return (deadlock?)", what, limit)
		return zero
	}
}

// ctxReapLimit bounds reaping waits in the cancellation tests' clean-up
// paths, so a failed assertion cannot turn into a blocked Wait inside a defer.
const ctxReapLimit = 10 * time.Second

// ctxReapCtx returns a bounded context for a reaping Wait.
func ctxReapCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), ctxReapLimit)
}

// ctxParsePIDs parses the fixture's one-pid-per-line marker format. A marker
// that does not end in a newline is a write caught in progress, not a short
// list, so it is rejected instead of being mistaken for a complete one.
func ctxParsePIDs(data []byte) ([]int, error) {
	text := string(data)
	if !strings.HasSuffix(text, "\n") {
		return nil, fmt.Errorf("incomplete marker: %q does not end with a newline", text)
	}
	var pids []int
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("marker line %q is not a pid", line)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// ctxWaitForPIDs waits until the marker holds exactly want pids.
func ctxWaitForPIDs(t *testing.T, marker string, want int, bound time.Duration) []int {
	t.Helper()
	deadline := time.Now().Add(bound)
	var lastErr error
	for {
		if data, err := os.ReadFile(marker); err == nil {
			pids, perr := ctxParsePIDs(data)
			switch {
			case perr != nil:
				lastErr = perr
			case len(pids) != want:
				lastErr = fmt.Errorf("%s holds %d pids, want %d", marker, len(pids), want)
			default:
				return pids
			}
		} else {
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("fixture did not record %d pids in %s within %v: %v", want, marker, bound, lastErr)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ctxPIDsIfRecorded returns the pids the fixture managed to record within
// bound, or nil when it was killed before it could write any. It is the
// non-fatal variant used where a fast kill legitimately races the first write.
func ctxPIDsIfRecorded(marker string, bound time.Duration) []int {
	deadline := time.Now().Add(bound)
	for {
		if data, err := os.ReadFile(marker); err == nil {
			if pids, perr := ctxParsePIDs(data); perr == nil && len(pids) > 0 {
				return pids
			}
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ctxProcState reads the process state letter from /proc/<pid>/stat. The comm
// field is parenthesised, so the fields are read after the last ')'.
func ctxProcState(pid int) (string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	stat := string(data)
	i := strings.LastIndexByte(stat, ')')
	if i < 0 || i+1 >= len(stat) {
		return "", false
	}
	fields := strings.Fields(stat[i+1:])
	if len(fields) == 0 {
		return "", false
	}
	return fields[0], true
}

// ctxPIDAlive reports whether pid still names a process that has not exited.
// The probe is signal 0; on Linux a zombie (state Z/X) counts as gone because
// it cannot run, holds no descriptors and cannot spawn, and PID 1 in a
// container may never reap it. On Windows, where the tests using this probe
// are skipped, Signal(0) is not implemented and the probe answers false.
func ctxPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return errors.Is(err, syscall.EPERM)
	}
	if runtime.GOOS == "linux" {
		if state, ok := ctxProcState(pid); ok && (state == "Z" || state == "X") {
			return false
		}
	}
	return true
}

// ctxWaitPIDsGone polls until every pid is gone, failing the test at the bound.
func ctxWaitPIDsGone(t *testing.T, pids []int, bound time.Duration) {
	t.Helper()
	deadline := time.Now().Add(bound)
	for {
		var alive []int
		for _, pid := range pids {
			if ctxPIDAlive(pid) {
				alive = append(alive, pid)
			}
		}
		if len(alive) == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("pids %v are still alive %v after cancellation, want the whole tree dead", alive, bound)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ctxSIGTERMIgnoringScript is the fixture that cannot be stopped politely: the
// trap makes SIGTERM a no-op for the shell, and the exec'd sleep inherits the
// ignored disposition, so only the SIGKILL escalation can end the tree.
const ctxSIGTERMIgnoringScript = `trap '' TERM
echo $$ > "$OARLOCK_EXEC_TEST_MARKER"
sleep 300 &
echo $! >> "$OARLOCK_EXEC_TEST_MARKER"
wait
`

// ctxTreeCommand builds the SIGTERM-ignoring /bin/sh fixture.
func ctxTreeCommand(marker string, killGrace time.Duration) *Command {
	return &Command{
		Path:      "/bin/sh",
		Args:      []string{"-c", ctxSIGTERMIgnoringScript},
		Env:       append(os.Environ(), helperMarkerEnv+"="+marker),
		KillGrace: killGrace,
		WaitDelay: 2 * time.Second,
	}
}

// TestStartRefusesCancelledContext pins §10's "starts nothing": a ctx that is
// already done must not fork, so the fixture never creates its marker file and
// Start returns no *Cmd at all.
func TestStartRefusesCancelledContext(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pid")
	c := helperCommand(t, "record-pid")
	c.Env = append(c.Env, helperMarkerEnv+"="+marker)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := ctxRun(t, 5*time.Second, "Start with a pre-cancelled ctx", func() ctxStartResult {
		cmd, err := Start(ctx, c)
		return ctxStartResult{cmd: cmd, err: err}
	})
	if !errors.Is(res.err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context.Canceled", res.err)
	}
	if res.cmd != nil {
		_ = res.cmd.Close(context.Background())
		reapCtx, reapCancel := ctxReapCtx()
		_, _ = res.cmd.Wait(reapCtx)
		reapCancel()
		t.Fatalf("Start() = (%v, %v), want a nil *Cmd: a cancelled Start must start nothing", res.cmd, res.err)
	}
	// Give a wrongly started fixture time to appear: the marker is the proof
	// that nothing was ever forked.
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("fixture marker %s exists (stat error %v): Start forked despite a cancelled ctx", marker, err)
	}
}

// TestStartWithCancelledContextLeavesNoProcess pins §10's "no orphan" rule over
// both timings: a ctx already dead before the call, and a ctx cancelled right
// after Start returns. Either outcome Start can produce is acceptable — an
// error, or a Cmd that is then killed — but a recorded pid that survives is
// not.
func TestStartWithCancelledContextLeavesNoProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the pid liveness probe is POSIX-specific; Windows is compile-checked only")
	}

	const iterations = 20
	observed := 0
	for i := 0; i < iterations; i++ {
		observed += ctxStartCancellationIteration(t, i)
	}
	if observed == 0 {
		t.Fatal("no iteration ever observed a recorded pid: the kill path was never exercised")
	}
}

// ctxStartCancellationIteration runs one Start/cancel timing and returns how
// many fixture pids it observed. It is a function rather than a loop body so
// the per-iteration cancel is deferred on every path (go vet's lostcancel).
func ctxStartCancellationIteration(t *testing.T, i int) int {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "pid")
	c := helperCommand(t, "record-pid")
	c.Env = append(c.Env, helperMarkerEnv+"="+marker)
	c.KillGrace = 300 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	timing := i % 4 // 0,2: dead before the call; 1: cancel right after; 3: cancel once recorded
	if timing == 0 || timing == 2 {
		cancel()
	}

	res := ctxRun(t, 10*time.Second, "Start under cancellation", func() ctxStartResult {
		cmd, err := Start(ctx, c)
		return ctxStartResult{cmd: cmd, err: err}
	})

	switch {
	case res.cmd == nil && res.err == nil:
		t.Fatalf("iteration %d (timing %d): Start() = (nil, nil)", i, timing)
	case res.cmd == nil && !errors.Is(res.err, context.Canceled):
		t.Fatalf("iteration %d (timing %d): Start() error = %v, want context.Canceled", i, timing, res.err)
	case res.cmd != nil && res.err != nil:
		t.Fatalf("iteration %d (timing %d): Start() = (%v, %v), want either a Cmd or an error", i, timing, res.cmd, res.err)
	}

	switch timing {
	case 1:
		// The AfterFunc owns the kill from here: no Wait is in flight.
		cancel()
	case 3:
		// Same "cancel after Start returned" path, made deterministic: let
		// the fixture record its pid first, then cancel.
		ctxWaitForPIDs(t, marker, 1, 5*time.Second)
		cancel()
	}

	if res.cmd == nil {
		// A cancelled Start started nothing: no pid may ever be recorded.
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("iteration %d (timing %d): fixture marker %s exists (stat error %v) after Start refused the cancelled ctx", i, timing, marker, err)
		}
		return 0
	}

	pids := ctxPIDsIfRecorded(marker, 500*time.Millisecond)

	// Reap and close before probing liveness, so the leader the test just
	// killed is not still an un-reaped zombie.
	waitRes := ctxRun(t, ctxReapLimit, "Wait after Start cancellation", func() ctxCallResult {
		reapCtx, reapCancel := ctxReapCtx()
		defer reapCancel()
		status, err := res.cmd.Wait(reapCtx)
		return ctxCallResult{status: status, err: err}
	})
	if waitRes.err != nil {
		t.Fatalf("iteration %d (timing %d): Wait() after cancellation = %v, want nil (the kill is ExitStatus data)", i, timing, waitRes.err)
	}
	if timing == 1 || timing == 3 {
		// The fixture sleeps for 60s, so a clean outcome would mean the tree
		// was never terminated.
		if waitRes.status.Success() {
			t.Fatalf("iteration %d (timing %d): Wait() = %+v, want a non-success outcome: the fixture was still sleeping when the ctx was cancelled", i, timing, waitRes.status)
		}
	}
	if closeErr := ctxRun(t, 10*time.Second, "Close after Start cancellation", func() error {
		return res.cmd.Close(context.Background())
	}); closeErr != nil && !errors.Is(closeErr, context.Canceled) {
		t.Fatalf("iteration %d (timing %d): Close() after cancellation = %v, want nil", i, timing, closeErr)
	}

	ctxWaitPIDsGone(t, pids, 5*time.Second)
	return len(pids)
}

// TestWaitReturnsBoundedAfterContextCancel pins §10's Wait rule with the
// unfriendly fixture: it ignores SIGTERM, KillGrace is 300ms, and Wait must
// still return context.Canceled in well under 3s — which is only possible if
// Close escalated to SIGKILL and the tree really died.
func TestWaitReturnsBoundedAfterContextCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture ignores SIGTERM; Windows has no SIGTERM and is compile-checked only")
	}

	marker := filepath.Join(t.TempDir(), "pids")
	const killGrace = 300 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	startRes := ctxRun(t, 10*time.Second, "Start", func() ctxStartResult {
		cmd, err := Start(ctx, ctxTreeCommand(marker, killGrace))
		return ctxStartResult{cmd: cmd, err: err}
	})
	if startRes.err != nil {
		cancel()
		t.Fatalf("Start() = %v, want a running tree", startRes.err)
	}
	cmd := startRes.cmd
	defer func() {
		cancel()
		_ = cmd.Close(context.Background())
		reapCtx, reapCancel := ctxReapCtx()
		_, _ = cmd.Wait(reapCtx)
		reapCancel()
	}()

	// The fixture must have installed its trap and recorded both pids before
	// the cancel, or SIGTERM alone would have been enough.
	pids := ctxWaitForPIDs(t, marker, 2, 5*time.Second)

	started := time.Now()
	cancel()
	waitRes := ctxRun(t, 3*time.Second, "Wait with a cancelled ctx", func() ctxCallResult {
		status, err := cmd.Wait(ctx)
		return ctxCallResult{status: status, err: err}
	})
	elapsed := time.Since(started)

	if !errors.Is(waitRes.err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context.Canceled", waitRes.err)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("Wait() returned after %v, want well under 3s: Close has to escalate to SIGKILL after the %v KillGrace", elapsed, killGrace)
	}
	if waitRes.status.Signal != syscall.SIGKILL {
		t.Fatalf("Wait() status = %+v, want SIGKILL (signal %v): the fixture ignores SIGTERM, so only the escalation can end it", waitRes.status, syscall.SIGKILL)
	}
	ctxWaitPIDsGone(t, pids, 5*time.Second)
}

// TestCloseReturnsBoundedAfterContextCancel pins §10's Close rule: an
// already-cancelled ctx skips the polite SIGTERM but still kills the tree, and
// Close returns context.Canceled in well under 3s even though the fixture
// ignores SIGTERM.
func TestCloseReturnsBoundedAfterContextCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture ignores SIGTERM; Windows has no SIGTERM and is compile-checked only")
	}

	marker := filepath.Join(t.TempDir(), "pids")
	const killGrace = 300 * time.Millisecond

	startRes := ctxRun(t, 10*time.Second, "Start", func() ctxStartResult {
		cmd, err := Start(context.Background(), ctxTreeCommand(marker, killGrace))
		return ctxStartResult{cmd: cmd, err: err}
	})
	if startRes.err != nil {
		t.Fatalf("Start() = %v, want a running tree", startRes.err)
	}
	cmd := startRes.cmd
	defer func() {
		_ = cmd.Close(context.Background())
		reapCtx, reapCancel := ctxReapCtx()
		_, _ = cmd.Wait(reapCtx)
		reapCancel()
	}()

	pids := ctxWaitForPIDs(t, marker, 2, 5*time.Second)

	closeCtx, cancelClose := context.WithCancel(context.Background())
	cancelClose() // dead before the call: the polite phase is already over

	started := time.Now()
	closeErr := ctxRun(t, 3*time.Second, "Close with a cancelled ctx", func() error {
		return cmd.Close(closeCtx)
	})
	elapsed := time.Since(started)

	if !errors.Is(closeErr, context.Canceled) {
		t.Fatalf("Close() error = %v, want context.Canceled", closeErr)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("Close() returned after %v, want well under 3s: a cancelled Close kills immediately, it does not wait out KillGrace", elapsed)
	}

	// Close never reaps; Wait does, so the liveness probe cannot see the
	// leader's zombie. The Wait is bounded for the same reason every other
	// blocking call in these tests is.
	reapRes := ctxRun(t, ctxReapLimit, "Wait after Close", func() ctxCallResult {
		reapCtx, reapCancel := ctxReapCtx()
		defer reapCancel()
		status, err := cmd.Wait(reapCtx)
		return ctxCallResult{status: status, err: err}
	})
	if reapRes.err != nil {
		t.Fatalf("Wait() after Close = %v, want nil (the kill is ExitStatus data)", reapRes.err)
	}
	if reapRes.status.Signal != syscall.SIGKILL {
		t.Fatalf("Wait() status = %+v, want SIGKILL (signal %v): the fixture ignores SIGTERM, so the cancelled Close must kill", reapRes.status, syscall.SIGKILL)
	}
	ctxWaitPIDsGone(t, pids, 5*time.Second)
}

// TestWaitPrefersLeaderExitOverLateContextCancel pins §10's "a result is a
// result": once Wait has observed the leader's exit, a cancellation arriving
// afterwards must not rewrite the stored result.
func TestWaitPrefersLeaderExitOverLateContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	startRes := ctxRun(t, 10*time.Second, "Start", func() ctxStartResult {
		cmd, err := Start(ctx, helperCommand(t, "exit0"))
		return ctxStartResult{cmd: cmd, err: err}
	})
	if startRes.err != nil {
		cancel()
		t.Fatalf("Start() = %v, want a running child", startRes.err)
	}
	cmd := startRes.cmd
	defer func() {
		cancel()
		_ = cmd.Close(context.Background())
	}()

	first := ctxRun(t, 10*time.Second, "Wait with a live ctx", func() ctxCallResult {
		status, err := cmd.Wait(ctx)
		return ctxCallResult{status: status, err: err}
	})
	if first.err != nil {
		t.Fatalf("Wait(live ctx) error = %v, want nil", first.err)
	}
	if first.status.Code != 0 || first.status.Signal != nil {
		t.Fatalf("Wait(live ctx) = %+v, want a clean exit (code 0, no signal)", first.status)
	}

	// The leader is gone; a cancel arriving now must not turn the result into
	// an error, and the stored result must win over the dead ctx.
	cancel()
	second := ctxRun(t, 10*time.Second, "Wait with a cancelled ctx after the exit", func() ctxCallResult {
		status, err := cmd.Wait(ctx)
		return ctxCallResult{status: status, err: err}
	})
	if second.err != nil {
		t.Fatalf("second Wait(cancelled ctx) error = %v, want the stored nil", second.err)
	}
	if second.status != first.status {
		t.Fatalf("second Wait = %+v, want the stored %+v", second.status, first.status)
	}
}

func TestStream_ZeroLengthReadClaimsWithoutBlocking(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	s := newStream(r)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if n, err := s.Read(nil); n != 0 || err != nil {
			t.Errorf("zero-length Read = (%d, %v), want (0, nil)", n, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("zero-length Read blocked; the stream was not claimed without an OS read")
	}
	if !s.claimed.Load() {
		t.Fatal("zero-length Read did not claim the stream")
	}
}
