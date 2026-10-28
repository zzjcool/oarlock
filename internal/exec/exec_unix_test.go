//go:build unix

package exec

// Runtime acceptance evidence for issue #16. These are the tests that need a
// real POSIX process table, a real execve and a real pipe buffer, so they are
// restricted to unix and deliberately use /bin/sh fixtures in addition to the
// portable, re-executed-test-binary fixtures of exec_test.go.
//
// Every case is self-bounding: each one runs inside bounded(), which fails the
// test if the case has not finished within its hard deadline. `go test -timeout`
// alone would turn a wedged pipe into a whole-package crash with no message.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// bounded runs one acceptance case on its own goroutine and fails the test when
// the case has not returned within limit.
//
// body must not call any *testing.T method: the testing package forbids
// t.Fatal off the test goroutine (go vet's testinggoroutine check rejects it
// too), so body reports its assertion failures as errors and this helper — on
// the test goroutine — is what calls t.Fatal. A timed-out goroutine is
// abandoned: the case has already failed, and the test binary is what gets
// thrown away.
func bounded(t *testing.T, limit time.Duration, body func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- body() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(limit):
		t.Fatalf("no result within the hard deadline of %v: a blocking call did not return (deadlock?)", limit)
	}
}

// unixProcState reads the process state letter ("R", "S", "Z", ...) from
// /proc/<pid>/stat. The comm field is parenthesised and may itself contain
// spaces and parentheses, so the fields are read after the last ')'.
//
// This is a second, independent implementation of the read that
// process_posix.go already has: acceptance evidence must not lean on the code
// under test being right.
func unixProcState(pid int) (string, bool) {
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

// pidGone reports whether pid no longer exists, i.e. whether the OS is free to
// reuse it.
//
// syscall.Kill(pid, 0) is the portable probe: ESRCH means "no such process".
// EPERM means "it exists but is not ours", which is still alive. Any other
// answer counts as alive, so a broken probe fails the test instead of passing
// it.
//
// On Linux the probe alone is not enough for the fixtures here. Close
// terminates a tree and deliberately does not reap it, so the grandchildren it
// kills are reparented to PID 1 — and PID 1 in a container is often not a
// reaper (a shell script, an init without SIGCHLD handling). Such a child stays
// visible forever as a zombie: kill(pid, 0) keeps succeeding and a test waiting
// for the pid to vanish would flake for a reason that has nothing to do with
// the tree kill. A zombie has exited, cannot run, holds no descriptors and
// cannot spawn, so it counts as gone; Linux reports that state as Z (exited,
// not reaped) or X (dead). Other POSIX kernels have no cheap portable
// post-mortem state, so they keep the coarse probe and their tests rely on a
// generous bound instead.
func pidGone(pid int) bool {
	if pid <= 0 {
		return true
	}
	err := syscall.Kill(pid, 0)
	if err != nil {
		return errors.Is(err, syscall.ESRCH)
	}
	if state, ok := unixProcState(pid); ok && (state == "Z" || state == "X") {
		return true
	}
	return false
}

// waitPidsGone polls until every pid is gone and returns the ones still there
// when the bound expires (nil means all gone).
func waitPidsGone(pids []int, limit time.Duration) []int {
	deadline := time.Now().Add(limit)
	for {
		var left []int
		for _, pid := range pids {
			if !pidGone(pid) {
				left = append(left, pid)
			}
		}
		if len(left) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return left
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// parsePidLines parses the one-pid-per-line fixture format. A file that does
// not end in a newline is a write caught in progress, not a short list: it is
// rejected so a truncated pid ("7573" out of "75735") can never be mistaken for
// a real one.
func parsePidLines(data []byte) ([]int, error) {
	text := string(data)
	if !strings.HasSuffix(text, "\n") {
		return nil, fmt.Errorf("incomplete write: %q does not end with a newline", text)
	}
	var pids []int
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("pid line %q is not a pid", line)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// waitPidFile polls path until it holds exactly want complete pid lines, e.g.
// the fixture's $$ and $!.
func waitPidFile(path string, want int, limit time.Duration) ([]int, error) {
	deadline := time.Now().Add(limit)
	var lastErr error
	for {
		if data, err := os.ReadFile(path); err == nil {
			pids, perr := parsePidLines(data)
			switch {
			case perr != nil:
				lastErr = perr
			case len(pids) != want:
				lastErr = fmt.Errorf("%q holds %d pids, want %d", path, len(pids), want)
			default:
				return pids, nil
			}
		} else {
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("waiting for %d pids in %q: %w", want, path, lastErr)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// closeTreeScript is the process-tree fixture: a background grandchild that
// stays in the leader's process group, both pids recorded one per line, then a
// foreground sleep that keeps the leader alive until someone signals the group.
//
// $$ is the shell's own pid, i.e. the pid Start launched, and `sleep 300 &` is
// started with job control off (non-interactive shell), so it inherits the
// shell's process group — exactly the escape that Setpgid plus kill(-pgid, sig)
// is supposed to cover.
const closeTreeScript = `sleep 300 &
echo $$ > "$OARLOCK_PID_FILE"
echo $! >> "$OARLOCK_PID_FILE"
sleep 300
`

// closeTreeIgnoresTERMScript keeps the group alive past SIGTERM: the script
// installs an ignoring trap for SIGTERM, which makes the signal a no-op for the
// shell, and the loop restarts the foreground sleep that SIGTERM does kill. Only the SIGKILL escalation can end this tree,
// which is what proves Close actually escalates after KillGrace.
const closeTreeIgnoresTERMScript = `trap '' TERM
sleep 300 &
echo $$ > "$OARLOCK_PID_FILE"
echo $! >> "$OARLOCK_PID_FILE"
while :; do sleep 300; done
`

// treeCaseResult is what one Close/Wait cycle over the fixture observed. It is
// filled on the case goroutine and asserted on the test goroutine; the channel
// inside bounded is the happens-before edge between the two.
type treeCaseResult struct {
	pids      []int
	closeErr  error
	closeTook time.Duration
	status    ExitStatus
	waitErr   error
	left      []int // pids still alive 10s after Close; empty means "no leftovers"
}

// runCloseTreeCase starts the fixture, waits for both recorded pids, then runs
// Close, Wait and the leftover poll. It never touches *testing.T.
func runCloseTreeCase(c *Command, pidFile string) (treeCaseResult, error) {
	var res treeCaseResult
	err := func() error {
		cmd, err := Start(context.Background(), c)
		if err != nil {
			return fmt.Errorf("Start() = %w", err)
		}
		defer func() { _ = cmd.Close(context.Background()) }()

		pids, err := waitPidFile(pidFile, 2, 10*time.Second)
		if err != nil {
			return err
		}
		res.pids = pids

		started := time.Now()
		res.closeErr = cmd.Close(context.Background())
		res.closeTook = time.Since(started)

		res.status, res.waitErr = cmd.Wait(context.Background())

		// The issue's criterion is "ps is clean afterwards, grandchildren
		// included", so this is the assertion that matters; it runs on the
		// case goroutine only because it polls.
		res.left = waitPidsGone(pids, 10*time.Second)
		return nil
	}()
	return res, err
}

// TestCloseKillsProcessGroupWithDescendants is the acceptance evidence for
// issue #16's "父进程 kill 后 ps 中无残留（含孙进程）" criterion.
func TestCloseKillsProcessGroupWithDescendants(t *testing.T) {
	t.Run("SIGTERM", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "pids")
		c := &Command{
			Path: "/bin/sh",
			Args: []string{"-c", closeTreeScript},
			Env:  append(os.Environ(), "OARLOCK_PID_FILE="+pidFile),
			// Short on purpose: this tree dies from the first SIGTERM, and a
			// long grace would only make the test slow. macOS's coarse group
			// probe may spend the grace even for a dead tree.
			KillGrace: 1 * time.Second,
			WaitDelay: 10 * time.Second,
		}

		var res treeCaseResult
		bounded(t, 40*time.Second, func() error {
			var err error
			res, err = runCloseTreeCase(c, pidFile)
			return err
		})

		if got := len(res.pids); got != 2 {
			t.Fatalf("the fixture recorded %d pids, want exactly 2 (the shell and its grandchild): %v", got, res.pids)
		}
		if res.closeErr != nil {
			t.Fatalf("Close() = %v, want nil for a tree it just terminated", res.closeErr)
		}
		if res.waitErr != nil {
			t.Fatalf("Wait() error = %v, want nil (the kill is ExitStatus data, not an error)", res.waitErr)
		}
		if res.status.Success() {
			t.Fatalf("ExitStatus = %+v, want a non-success outcome: Close terminated the tree", res.status)
		}
		if res.status.Signal != syscall.SIGTERM && res.status.Signal != syscall.SIGKILL {
			t.Fatalf("ExitStatus.Signal = %v (%T), want SIGTERM or SIGKILL: the whole group was signalled", res.status.Signal, res.status.Signal)
		}
		if len(res.left) != 0 {
			t.Fatalf("still alive 10s after Close: pids %v (fixture recorded %v): a descendant survived the process-group kill", res.left, res.pids)
		}
	})

	t.Run("SIGTERMIgnoredEscalatesToSIGKILL", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "pids")
		const killGrace = 500 * time.Millisecond
		c := &Command{
			Path: "/bin/sh",
			Args: []string{"-c", closeTreeIgnoresTERMScript},
			Env:  append(os.Environ(), "OARLOCK_PID_FILE="+pidFile),
			// The shell ignores SIGTERM, so Close must spend this grace and
			// then SIGKILL the group.
			KillGrace: killGrace,
			WaitDelay: 10 * time.Second,
		}

		var res treeCaseResult
		bounded(t, 40*time.Second, func() error {
			var err error
			res, err = runCloseTreeCase(c, pidFile)
			return err
		})

		if got := len(res.pids); got != 2 {
			t.Fatalf("the fixture recorded %d pids, want exactly 2: %v", got, res.pids)
		}
		if res.closeErr != nil {
			t.Fatalf("Close() = %v, want nil", res.closeErr)
		}
		if res.waitErr != nil {
			t.Fatalf("Wait() error = %v, want nil", res.waitErr)
		}
		if res.status.Signal != syscall.SIGKILL {
			t.Fatalf("ExitStatus.Signal = %v, want SIGKILL: the leader ignored SIGTERM, so only the escalation can end it", res.status.Signal)
		}
		if res.closeTook < killGrace {
			t.Fatalf("Close() returned after %v, want at least the %v KillGrace before the SIGKILL escalation", res.closeTook, killGrace)
		}
		if len(res.left) != 0 {
			t.Fatalf("still alive 10s after Close: pids %v (fixture recorded %v)", res.left, res.pids)
		}
	})
}

// Fixture modes for the re-executed test binary. Re-executing this very binary
// keeps the 1 MiB fixture exact (a Go process counting bytes, not a shell's wc)
// and portable over the unixes CI covers.
const (
	// unixFixtureEnv selects the fixture behaviour in the re-executed binary;
	// empty means "an ordinary go test run", in which case every fixture test
	// is a no-op.
	unixFixtureEnv = "OARLOCK_EXEC_UNIX_FIXTURE"

	// fixtureFloodStdoutThenDrainStdin is the 1 MiB stdin interlock: flood
	// stdout past the pipe buffer first, then drain stdin and report the count.
	fixtureFloodStdoutThenDrainStdin = "flood-stdout-then-drain-stdin"

	// stdinFloodBytes is how much the fixture writes to stdout before it looks
	// at stdin: several times the 64 KiB pipe buffer, so a stdin write issued
	// on the Start path would block there while the child blocks here — the
	// two-way pipe deadlock this test exists to catch.
	stdinFloodBytes = 600 << 10

	// stdinCountMarker prefixes the fixture's report line. The flood is nothing
	// but 'f' bytes, so this marker can only come from the report.
	stdinCountMarker = "STDIN-BYTES="

	// oneMegabyte is the payload size the acceptance criterion names.
	oneMegabyte = 1 << 20
)

// TestUnixFixtureProcess is not a test: it is the child body of the re-executed
// test binary. It always leaves via os.Exit so the testing framework never
// prints its banner into the pipe the parent is reading.
func TestUnixFixtureProcess(t *testing.T) {
	mode := os.Getenv(unixFixtureEnv)
	if mode == "" {
		return // an ordinary `go test` run of this file
	}
	switch mode {
	case fixtureFloodStdoutThenDrainStdin:
		// os.Stdout is unbuffered and os.File.Write loops over partial writes,
		// so this one call puts the whole flood on the pipe before the first
		// byte of stdin is touched.
		if _, err := os.Stdout.Write(bytes.Repeat([]byte("f"), stdinFloodBytes)); err != nil {
			os.Exit(90)
		}
		n, err := io.Copy(io.Discard, os.Stdin)
		if err != nil {
			os.Exit(91)
		}
		if _, err := fmt.Fprintf(os.Stdout, "\n%s%d\n", stdinCountMarker, n); err != nil {
			os.Exit(92)
		}
		os.Exit(0)
	}
	os.Exit(97)
}

// unixFixtureCommand builds a Command that re-runs this test binary in fixture
// mode.
func unixFixtureCommand(t *testing.T, mode string) *Command {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() = %v, want the test binary path", err)
	}
	return &Command{
		Path: exe,
		Args: []string{"-test.run=TestUnixFixtureProcess$"},
		// GORACE only silences the race runtime's one-second at-exit sleep,
		// which would otherwise dominate every fixture launch.
		Env: append(os.Environ(),
			"GORACE=atexit_sleep_ms=0",
			unixFixtureEnv+"="+mode,
		),
		KillGrace: 2 * time.Second,
		WaitDelay: 20 * time.Second,
	}
}

// parseStdinCount finds the fixture's report line in the collected stdout.
func parseStdinCount(data []byte) (int64, error) {
	i := bytes.LastIndex(data, []byte(stdinCountMarker))
	if i < 0 {
		return 0, fmt.Errorf("no %q marker in %d collected bytes", stdinCountMarker, len(data))
	}
	rest := data[i+len(stdinCountMarker):]
	end := bytes.IndexByte(rest, '\n')
	if end < 0 {
		return 0, fmt.Errorf("the %q report line is truncated: %q", stdinCountMarker, rest)
	}
	n, err := strconv.ParseInt(string(rest[:end]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("the %q report line is not a number: %w", stdinCountMarker, err)
	}
	return n, nil
}

// stdinCaseResult is everything one 1 MiB subunit observed.
type stdinCaseResult struct {
	reported    int64 // bytes the child says it read from stdin
	reportErr   error
	stdoutBytes int // bytes the caller's reader collected
	status      ExitStatus
	waitErr     error
	writerErr   error
	elapsed     time.Duration // Start → Wait
}

// runOneMegabyteStdinCase drives one 1 MiB case. It never touches *testing.T.
//
// writeThroughStdin selects the second acceptance variant: with false the
// payload is handed to Command.Stdin (the package's own copier goroutine); with
// true Command.Stdin stays nil and the caller writes through Cmd.Stdin() from a
// goroutine and closes it for EOF.
func runOneMegabyteStdinCase(c *Command, payload []byte, writeThroughStdin bool) (stdinCaseResult, error) {
	var res stdinCaseResult
	err := func() error {
		started := time.Now()
		cmd, err := Start(context.Background(), c)
		if err != nil {
			return fmt.Errorf("Start() = %w", err)
		}
		defer func() { _ = cmd.Close(context.Background()) }()

		// The caller's write side, when this is that variant. It runs while
		// the child is flooding stdout, which is the interlock.
		writerCh := make(chan error, 1)
		if writeThroughStdin {
			go func() {
				w := cmd.Stdin()
				if _, err := w.Write(payload); err != nil {
					writerCh <- fmt.Errorf("writing the payload to Stdin() = %w", err)
					return
				}
				writerCh <- w.Close()
			}()
		}

		// Claim the stream before Wait: a stream is claimed on its first Read.
		// Reading one byte here, on this goroutine, makes the claim
		// deterministic instead of dependent on goroutine scheduling; if Wait
		// saw an unclaimed stream it would drain it to io.Discard and the data
		// would be gone. The fixture floods stdout immediately, so this read is
		// short (and the whole case is inside bounded()).
		stdout := cmd.Stdout()
		first := make([]byte, 1)
		n, err := stdout.Read(first)
		if err != nil {
			return fmt.Errorf("first Read(Stdout()) = %w, want data from the flood", err)
		}

		type readResult struct {
			data []byte
			err  error
		}
		readCh := make(chan readResult, 1)
		go func() {
			data, err := io.ReadAll(stdout)
			readCh <- readResult{data, err}
		}()

		res.status, res.waitErr = cmd.Wait(context.Background())
		res.elapsed = time.Since(started)

		select {
		case got := <-readCh:
			if got.err != nil {
				return fmt.Errorf("io.ReadAll(Stdout()) = %v, want the stream to end with io.EOF", got.err)
			}
			collected := append(first[:n:n], got.data...)
			res.stdoutBytes = len(collected)
			res.reported, res.reportErr = parseStdinCount(collected)
		case <-time.After(30 * time.Second):
			return errors.New("the stdout reader did not finish within 30s of Wait returning")
		}

		if writeThroughStdin {
			select {
			case res.writerErr = <-writerCh:
			case <-time.After(30 * time.Second):
				return errors.New("the Cmd.Stdin writer goroutine did not finish within 30s")
			}
		}
		return nil
	}()
	return res, err
}

// assertOneMegabyteStdinCase is the shared acceptance body of the two 1 MiB
// subunits: the child really received all 1 MiB, the caller really read the
// flood, and nothing took long enough to be a pipe deadlock.
func assertOneMegabyteStdinCase(t *testing.T, res stdinCaseResult, payload []byte) {
	t.Helper()
	if res.waitErr != nil {
		t.Fatalf("Wait() error = %v, want nil (a clean exit is ExitStatus data)", res.waitErr)
	}
	if !res.status.Success() {
		t.Fatalf("ExitStatus = %+v, want a clean exit", res.status)
	}
	if res.stdoutBytes < stdinFloodBytes {
		t.Fatalf("the caller collected %d bytes of stdout, want at least the %d-byte flood the child wrote before reading stdin", res.stdoutBytes, stdinFloodBytes)
	}
	if res.reportErr != nil {
		t.Fatalf("the child's byte-count report: %v", res.reportErr)
	}
	if want := int64(oneMegabyte); res.reported != want {
		t.Fatalf("the child received %d bytes on stdin, want %d (payload is %d bytes)", res.reported, want, len(payload))
	}
	if res.writerErr != nil {
		t.Fatalf("writing the payload through Cmd.Stdin() = %v, want nil", res.writerErr)
	}
	if res.elapsed >= 30*time.Second {
		t.Fatalf("Start→Wait took %v, want well under 30s: a 64 KiB pipe deadlock would not return at all", res.elapsed)
	}
}

// TestStdinOneMegabyteDoesNotDeadlock is the acceptance evidence for issue
// #16's "大 prompt（1MB）经 stdin 成功；不出现死锁" criterion. Both variants send
// 1 MiB to a child that has already written 600 KiB to stdout before it reads a
// single byte of stdin.
func TestStdinOneMegabyteDoesNotDeadlock(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), oneMegabyte/16)
	if len(payload) != oneMegabyte {
		t.Fatalf("payload is %d bytes, want %d", len(payload), oneMegabyte)
	}

	t.Run("CommandStdin", func(t *testing.T) {
		c := unixFixtureCommand(t, fixtureFloodStdoutThenDrainStdin)
		c.Stdin = bytes.NewReader(payload) // the package's copier goroutine

		var res stdinCaseResult
		bounded(t, 60*time.Second, func() error {
			var err error
			res, err = runOneMegabyteStdinCase(c, payload, false)
			return err
		})
		assertOneMegabyteStdinCase(t, res, payload)
	})

	t.Run("CmdStdinWriter", func(t *testing.T) {
		c := unixFixtureCommand(t, fixtureFloodStdoutThenDrainStdin) // Stdin stays nil

		var res stdinCaseResult
		bounded(t, 60*time.Second, func() error {
			var err error
			res, err = runOneMegabyteStdinCase(c, payload, true)
			return err
		})
		assertOneMegabyteStdinCase(t, res, payload)
	})
}

// grandchildHoldsStdoutScript is the WaitDelay fixture: a background grandchild
// inherits the stdout/stderr write ends, the leader prints the grandchild's pid
// plus a marker and exits 0 — the leader is gone while the pipe's write end is
// still held.
const grandchildHoldsStdoutScript = `sleep 300 &
echo $! > "$OARLOCK_PID_FILE"
echo "OARLOCK-GRANDCHILD=$!"
echo "OARLOCK-MARKER the leader exited and the grandchild still holds this pipe"
`

// TestWaitDelayBoundsOutputWhenGrandchildHoldsStdout is the acceptance evidence
// for issue #16's "wrapper 退出但孙进程存活 → 采集正常结束" criterion.
func TestWaitDelayBoundsOutputWhenGrandchildHoldsStdout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	const waitDelay = 300 * time.Millisecond
	c := &Command{
		Path:      "/bin/sh",
		Args:      []string{"-c", grandchildHoldsStdoutScript},
		Env:       append(os.Environ(), "OARLOCK_PID_FILE="+pidFile),
		WaitDelay: waitDelay,
		KillGrace: 5 * time.Second,
	}

	type caseResult struct {
		grandchild int
		collected  string
		readErr    error
		status     ExitStatus
		waitErr    error
		elapsed    time.Duration
		closeErr   error
		left       []int
	}
	var res caseResult

	bounded(t, 40*time.Second, func() error {
		cmd, err := Start(context.Background(), c)
		if err != nil {
			return fmt.Errorf("Start() = %w", err)
		}
		pids, err := waitPidFile(pidFile, 1, 10*time.Second)
		if err != nil {
			_ = cmd.Close(context.Background())
			return err
		}
		res.grandchild = pids[0]

		// Claim the stream BEFORE Wait. A stream is claimed on its first Read,
		// and a stream claimed only after Wait has been drained to io.Discard —
		// then WaitDelay would never be exercised. Reading one byte here, on
		// this goroutine, makes the claim deterministic; the fixture prints
		// immediately, so this read is short.
		stdout := cmd.Stdout()
		first := make([]byte, 1)
		n, err := stdout.Read(first)
		if err != nil {
			_ = cmd.Close(context.Background())
			return fmt.Errorf("first Read(Stdout()) = %w, want the fixture's first byte", err)
		}

		var collected []byte
		var readErr error
		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			collected, readErr = io.ReadAll(stdout)
		}()

		started := time.Now()
		res.status, res.waitErr = cmd.Wait(context.Background())
		res.elapsed = time.Since(started)

		select {
		case <-readDone:
			res.collected = string(append(first[:n:n], collected...))
			res.readErr = readErr
		case <-time.After(10 * time.Second):
			return errors.New("the stdout reader was still blocked 10s after Wait returned")
		}

		// Close still has to reach the tree the leader left behind.
		res.closeErr = cmd.Close(context.Background())
		res.left = waitPidsGone([]int{res.grandchild}, 10*time.Second)
		return nil
	})

	if res.waitErr == nil {
		t.Fatalf("Wait() error = nil, want ErrWaitDelay: the grandchild held the pipe for longer than the %v WaitDelay", waitDelay)
	}
	if !errors.Is(res.waitErr, ErrWaitDelay) {
		t.Fatalf("Wait() error = %v, want it to be (or wrap) ErrWaitDelay", res.waitErr)
	}
	if res.elapsed >= 2*time.Second {
		t.Fatalf("Wait() took %v, want under 2s: the %v WaitDelay is what bounds collection", res.elapsed, waitDelay)
	}
	if res.status.Code != 0 || res.status.Signal != nil {
		t.Fatalf("ExitStatus = %+v, want the leader's clean exit (code 0, no signal) even though the drain timed out", res.status)
	}
	if !strings.Contains(res.collected, "OARLOCK-MARKER") {
		t.Fatalf("the caller collected %q, want the marker the leader printed before it exited", res.collected)
	}
	if !strings.Contains(res.collected, "OARLOCK-GRANDCHILD=") {
		t.Fatalf("the caller collected %q, want the grandchild pid line too (nothing the leader wrote may be lost)", res.collected)
	}
	if res.readErr != nil {
		t.Fatalf("io.ReadAll(Stdout()) = %v, want nil: the WaitDelay force-close must surface as io.EOF, not as an error", res.readErr)
	}
	if res.closeErr != nil {
		t.Fatalf("Close() = %v, want nil", res.closeErr)
	}
	if len(res.left) != 0 {
		t.Fatalf("the grandchild %v was still alive 10s after Close: the leader is reaped, so Close has to reach the rest of the tree alone", res.left)
	}
}

// TestWaitAndCloseLeaveNoGoroutinesOrFileDescriptors pins contract semantics 9
// (#9's "no goroutine or fd leaks"): after a Wait+Close cycle the goroutine
// count and the open file descriptors return to their pre-Start values, and a
// caller that only ever calls Wait — never Close — does not leak the stdin
// write end either.
//
// The Wait-only half exists because it is the one closure a reviewer proved the
// suite could lose silently: delete `c.closeStdin()` from `wait()` and every
// other test stays green while each command leaks one descriptor.
func TestWaitAndCloseLeaveNoGoroutinesOrFileDescriptors(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("fd accounting reads /proc/self/fd")
	}
	countFDs := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatalf("read /proc/self/fd: %v", err)
		}
		return len(entries)
	}
	// settle tolerates the runtime closing things asynchronously; it fails only
	// if the count is still above the baseline after the budget.
	settle := func(fn func() int, before int, what string) {
		deadline := time.Now().Add(3 * time.Second)
		for {
			got := fn()
			if got <= before {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s grew from %d to %d: a start/wait/close cycle leaked", what, before, got)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	start := func() *Cmd {
		cmd, err := Start(context.Background(), &Command{Path: "/bin/sh", Args: []string{"-c", "exit 0"}})
		if err != nil {
			t.Fatalf("Start() = %v", err)
		}
		return cmd
	}

	// Warm up the runtime's own lazily created descriptors (epoll, /proc reads).
	warm := start()
	_, _ = io.ReadAll(warm.Stdout())
	if _, err := warm.Wait(context.Background()); err != nil {
		t.Fatalf("warm-up Wait() = %v", err)
	}
	_ = warm.Close(context.Background())

	beforeFDs, beforeGoroutines := countFDs(), runtime.NumGoroutine()
	for i := 0; i < 10; i++ {
		cmd := start()
		_, _ = io.ReadAll(cmd.Stdout())
		_, _ = io.ReadAll(cmd.Stderr())
		if _, err := cmd.Wait(context.Background()); err != nil {
			t.Fatalf("Wait() = %v", err)
		}
		if err := cmd.Close(context.Background()); err != nil {
			t.Fatalf("Close() = %v", err)
		}
	}
	settle(countFDs, beforeFDs, "open fds after Wait+Close")
	settle(runtime.NumGoroutine, beforeGoroutines, "goroutines after Wait+Close")

	// Wait alone must close the stdin write end: a consumer that never calls
	// Close is the normal #9 shape.
	beforeFDs = countFDs()
	for i := 0; i < 10; i++ {
		cmd := start()
		_, _ = io.ReadAll(cmd.Stdout())
		if _, err := cmd.Wait(context.Background()); err != nil {
			t.Fatalf("Wait() = %v", err)
		}
	}
	settle(countFDs, beforeFDs, "open fds after Wait without Close")
	settle(runtime.NumGoroutine, beforeGoroutines, "goroutines after Wait without Close")
}
