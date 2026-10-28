package exec

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultKillGrace is how long Close waits for a politely signalled tree to
	// disappear before it kills it outright.
	DefaultKillGrace = 10 * time.Second

	// DefaultWaitDelay bounds how long Wait keeps waiting for the output pipes
	// to reach EOF after the leader has exited.
	DefaultWaitDelay = 2 * time.Second

	// closePollInterval is how often Close re-checks whether the tree is gone
	// while it is inside the KillGrace window.
	closePollInterval = 10 * time.Millisecond

	// cancelReapBound is the hard bound Wait puts on reaping the leader after a
	// cancellation has killed the tree. SIGKILL (or TerminateJobObject) is
	// immediate in practice; the bound only exists so a wedged kernel cannot
	// hang the caller past its deadline.
	cancelReapBound = 5 * time.Second
)

// ErrWaitDelay reports that the leader exited but at least one output pipe
// stayed open past Command.WaitDelay — typically a wrapper that exited while a
// grandchild inherited stdout and is still holding it. Wait force-closes the
// read ends and returns this sentinel so collection ends instead of hanging
// the host; the status of the leader is still returned next to it.
var ErrWaitDelay = errors.New("exec: output pipes stayed open after the process exited")

// Command describes one child process and the tree it may create.
type Command struct {
	Path      string        // executable; a bare name is resolved through PATH
	Args      []string      // arguments only; argv[0] is supplied by Start
	Dir       string        // working directory; "" = inherit
	Env       []string      // nil = inherit the parent environment
	Stdin     io.Reader     // optional: copied to the child by a goroutine; EOF closes the pipe
	KillGrace time.Duration // SIGTERM→SIGKILL grace; 0 = DefaultKillGrace
	WaitDelay time.Duration // drain bound after the leader exits; 0 = DefaultWaitDelay
}

// ExitStatus is how the leader ended. It is data, not an error: a non-zero
// code or a signal death is a normal outcome of a command, and only Start
// failures, ErrWaitDelay and real I/O faults come back as error.
type ExitStatus struct {
	Code   int       // exit code; -1 when killed by a signal
	Signal os.Signal // signal that killed it; nil otherwise
}

// Success reports whether the leader exited cleanly: no signal and code 0.
func (s ExitStatus) Success() bool { return s.Signal == nil && s.Code == 0 }

// Cmd is a started process tree: the leader's pid plus the pipes and handles
// needed to talk to it, bound its lifetime and collect its output.
type Cmd struct {
	cmd *osexec.Cmd
	pid int

	// stdinW is the parent's write end of the child's stdin pipe. It is closed
	// exactly once (by the copy goroutine on EOF, by Wait, or by Close) so the
	// child always sees EOF.
	stdinW *os.File

	stdout *stream
	stderr *stream

	killGrace time.Duration
	waitDelay time.Duration

	stdinOnce sync.Once

	closeOnce sync.Once
	closeErr  error

	// stopCancel is the context.AfterFunc stop handle for Start's ctx. It is
	// guarded because Start registers the AfterFunc before it stores the
	// handle: an already-done ctx runs the function on its own goroutine
	// immediately, so the write and the function's read can overlap.
	stopCancelMu sync.Mutex
	stopCancel   func() bool

	waitOnce sync.Once
	status   ExitStatus
	waitErr  error

	// sys holds platform-specific handles (the Windows job object); POSIX
	// needs none — the process group is fully named by pid.
	sys sysState
}

// Start launches c and returns once the child exists (on Windows: once it has
// been assigned to its job and resumed). It never blocks on Command.Stdin: the
// payload is copied by a goroutine after the child is running.
//
// ctx governs the lifetime of the whole tree: it is registered with
// context.AfterFunc, so cancelling it terminates the tree even if nobody is
// inside Wait. A ctx that is already done starts nothing at all and returns
// its error; a ctx that dies between the fork and Start's return is terminated
// and reaped here, so Start never hands back a *Cmd whose tree outlived it.
func Start(ctx context.Context, c *Command) (*Cmd, error) {
	if c == nil {
		return nil, errors.New("exec: Start(nil)")
	}
	if c.Path == "" {
		return nil, errors.New("exec: Command.Path is empty")
	}
	if ctx == nil {
		return nil, errors.New("exec: Start(nil context)")
	}
	// A ctx that is already done must start nothing at all: no fork, no pipe,
	// no marker file, nothing for a later Close to find.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	exePath := c.Path
	if filepath.Base(exePath) == exePath {
		// A bare name is resolved through PATH first, exactly as
		// os/exec.Command does: the child must be started from the path the
		// OS would have picked, and a not-found error belongs to Start.
		lp, err := osexec.LookPath(exePath)
		if err != nil {
			return nil, err
		}
		exePath = lp
	}

	// The shim layer rewrites a .cmd/.bat launcher onto its PowerShell entry
	// point on Windows and is the identity elsewhere. It receives the
	// already-resolved path and the caller's arguments without argv[0], and
	// returns argv with argv[0] included.
	exe, argv, err := resolveInvocation(exePath, c.Args)
	if err != nil {
		return nil, err
	}

	// Pipes are created by hand: WaitDelay needs read ends we own, and
	// os/exec.StdoutPipe/StdinPipe hand those to the package's own goroutines.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		closeAll(stdinR, stdinW)
		return nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		closeAll(stdinR, stdinW, stdoutR, stdoutW)
		return nil, err
	}

	cc := &Cmd{
		cmd: &osexec.Cmd{
			Path:        exe,
			Args:        argv,
			Dir:         c.Dir,
			Env:         c.Env,
			SysProcAttr: newSysProcAttr(),
			Stdin:       stdinR,
			Stdout:      stdoutW,
			Stderr:      stderrW,
		},
		stdinW:    stdinW,
		stdout:    newStream(stdoutR),
		stderr:    newStream(stderrR),
		killGrace: c.KillGrace,
		waitDelay: c.WaitDelay,
	}
	if cc.killGrace <= 0 {
		cc.killGrace = DefaultKillGrace
	}
	if cc.waitDelay <= 0 {
		cc.waitDelay = DefaultWaitDelay
	}

	// prepareStart runs before the child exists: on Windows it creates and
	// configures the kill-on-close job the suspended child is assigned to a
	// moment later. On POSIX it is a no-op.
	if err := prepareStart(cc); err != nil {
		closeAll(stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW)
		return nil, err
	}

	// Cheap second look: a ctx that died while the pipes and the platform
	// handles were being prepared should not cost a fork either.
	if err := ctx.Err(); err != nil {
		closeAll(stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW)
		releaseTree(cc)
		return nil, err
	}

	if err := cc.cmd.Start(); err != nil {
		closeAll(stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW)
		releaseTree(cc)
		return nil, wrapStartError(err)
	}
	cc.pid = cc.cmd.Process.Pid

	// os/exec does not close caller-supplied files (and the child has its own
	// duplicates by now), so the parent's copies of the child's ends are
	// closed here — without this the read ends never see EOF.
	closeAll(stdinR, stdoutW, stderrW)

	// attach is the other half of prepareStart: on Windows it assigns the
	// suspended child to the job and resumes its primary thread. If any step
	// after Start fails, the child must not be left running (or suspended).
	if err := attach(cc); err != nil {
		cc.abandon()
		return nil, err
	}

	// From here on ctx owns the tree's lifetime. Closing it must work even
	// when nobody is inside Wait (exec.CommandContext semantics) and must not
	// be aborted by the very ctx that triggered it, hence WithoutCancel.
	cc.setCancelWatch(context.AfterFunc(ctx, func() {
		_ = cc.Close(context.WithoutCancel(ctx))
	}))

	// The ctx may have died between the fork and the registration above, in
	// which case the AfterFunc has already started (or is starting) the
	// graceful Close. Start must not hand back a *Cmd whose tree outlived its
	// ctx, so terminate and reap here and report the abort.
	if err := ctx.Err(); err != nil {
		cc.abandonCancelled()
		return nil, err
	}

	if c.Stdin != nil {
		go cc.copyStdin(c.Stdin)
	}
	return cc, nil
}

// Pid is the leader's process id, 0 before Start has returned.
func (c *Cmd) Pid() int { return c.pid }

// Stdin is the parent's write end of the child's stdin pipe; closing it is
// EOF for the child. When Command.Stdin was set the package closes it on EOF
// of that reader, on Wait and on Close.
func (c *Cmd) Stdin() io.WriteCloser { return c.stdinW }

// Stdout is the child's standard output. The first Read claims the stream;
// a stream that is never read is drained to io.Discard by Wait.
func (c *Cmd) Stdout() io.ReadCloser { return c.stdout }

// Stderr is the child's standard error, with the same claim and drain rules as
// Stdout.
func (c *Cmd) Stderr() io.ReadCloser { return c.stderr }

// Signal sends sig to the whole tree. POSIX signals the process group and
// falls back to the leader alone; Windows has no signals and maps every signal
// to tree termination.
func (c *Cmd) Signal(sig os.Signal) error {
	if sig == nil {
		return errors.New("exec: Signal(nil)")
	}
	return signalTree(c, sig)
}

// OwnershipError reports why the tree could not be taken ownership of. It is
// nil in the normal case and always nil on POSIX; on Windows a non-nil value
// means the job-assignment step failed, so cancellation falls back to
// terminating the leader (or to taskkill) instead of the whole job.
func (c *Cmd) OwnershipError() error { return treeOwnershipError(c) }

// Wait reaps the leader and reports its ExitStatus. A non-zero exit or a
// signal death is not an error. Wait then waits for both output streams to
// reach EOF, bounded by Command.WaitDelay: on expiry the read ends are
// force-closed (reads yield io.EOF) and ErrWaitDelay is returned next to the
// status. Wait is idempotent and safe to call after Close.
//
// ctx bounds this blocking call: when it fires before the leader has exited,
// the tree is terminated (SIGTERM, then SIGKILL after KillGrace) and Wait
// returns ctx.Err() next to the killed status within KillGrace plus reap time.
// A leader that has already exited wins over a later cancellation: the real
// status is returned and ctx is ignored, because a result is a result.
func (c *Cmd) Wait(ctx context.Context) (ExitStatus, error) {
	c.waitOnce.Do(func() {
		c.status, c.waitErr = c.wait(ctx)
	})
	return c.status, c.waitErr
}

func (c *Cmd) wait(ctx context.Context) (ExitStatus, error) {
	// The reaper runs on its own goroutine so Wait can race it against
	// ctx.Done. It writes rawErr and then closes done; that close is the
	// happens-before edge that makes rawErr and c.cmd.ProcessState safe to
	// read afterwards, so no lock is needed and no read may happen before
	// <-done.
	done := make(chan struct{})
	var rawErr error
	go func() {
		rawErr = c.cmd.Wait()
		close(done)
	}()

	// A ctx that is already dead at entry wins by definition: waiting for a
	// leader that this very ctx asked to be killed would turn an abort into a
	// status instead of the writer's error.
	if ctx.Err() != nil {
		return c.waitAborted(ctx, done)
	}

	select {
	case <-done:
		// Leader exit wins: a result is a result. Cancellation from here on
		// must not retroactively become an error. Stop the watcher now that
		// the leader is reaped so a late deadline cannot signal a recycled pid.
		c.stopCancelWatch()
	case <-ctx.Done():
		// The leader may have exited at the same moment; prefer the result
		// that is already in hand.
		select {
		case <-done:
			c.stopCancelWatch()
		default:
			return c.waitAborted(ctx, done)
		}
	}

	status := exitStatus(c.cmd)
	// Closing the write end is what tells a child still reading stdin that
	// the conversation is over.
	c.closeStdin()

	// A leading *osexec.ExitError carries the exit code we already extracted;
	// anything else is a real fault we must not hide.
	var exitErr *osexec.ExitError
	var waitErr error
	if rawErr != nil && !errors.As(rawErr, &exitErr) {
		waitErr = rawErr
	}

	// Streams the caller never read are drained here so Wait is bounded even
	// when stderr is ignored. Streams the caller is reading are waited on: the
	// caller's reader closes done at EOF or at the force-close below.
	var drain []<-chan struct{}
	var claimed []<-chan struct{}
	for _, s := range []*stream{c.stdout, c.stderr} {
		if s == nil {
			continue
		}
		if s.claimed.Load() {
			claimed = append(claimed, s.done)
			continue
		}
		s.claimed.Store(true)
		drained := make(chan struct{})
		go func(s *stream) {
			defer close(drained)
			_, _ = io.Copy(io.Discard, s)
		}(s)
		drain = append(drain, drained)
	}

	timer := time.NewTimer(c.waitDelay)
	defer timer.Stop()
	timedOut := false
	for _, ch := range append(claimed, drain...) {
		if timedOut {
			break
		}
		select {
		case <-ch:
		case <-timer.C:
			timedOut = true
		}
	}
	if timedOut {
		// Force-closing the read ends unblocks both a caller blocked in Read
		// and our drain goroutines; their reads come back as io.EOF.
		for _, s := range []*stream{c.stdout, c.stderr} {
			if s != nil {
				s.forceClose()
			}
		}
		for _, ch := range drain {
			// Bounded: every drain goroutine is inside Read, and its fd is
			// closed now.
			<-ch
		}
		if waitErr != nil {
			return status, errors.Join(ErrWaitDelay, waitErr)
		}
		return status, ErrWaitDelay
	}
	return status, waitErr
}

// waitAborted is the ctx-wins half of Wait: the tree is terminated (with an
// already-dead ctx Close escalates straight to SIGKILL), the leader is reaped
// within a hard bound, and the call reports ctx.Err() instead of pretending
// the kill was a normal exit. Output streams are force-closed so the abort
// leaves no blocked reader and no fd behind.
func (c *Cmd) waitAborted(ctx context.Context, done <-chan struct{}) (ExitStatus, error) {
	// Disarm the lifetime watcher first: Wait is taking over the termination,
	// and a later deadline must not signal a recycled pid.
	c.stopCancelWatch()
	closeErr := c.Close(ctx)

	reaped := waitDoneWithin(done, cancelReapBound)
	c.stopCancelWatch()
	c.closeStdin()
	for _, s := range []*stream{c.stdout, c.stderr} {
		if s != nil {
			s.forceClose()
		}
	}

	err := ctx.Err()
	if err == nil {
		// Unreachable for a well-behaved Context (Done closed implies Err is
		// set); be explicit rather than returning nil for an abort.
		err = context.Canceled
	}
	if closeErr != nil && !errors.Is(closeErr, err) {
		err = errors.Join(err, closeErr)
	}

	// Reading the status is only safe once the reaper has finished; if the
	// hard bound expired the tree was still dying, so report "unknown" (-1)
	// instead of racing the reaper over c.cmd.ProcessState.
	if !reaped {
		return ExitStatus{Code: -1}, err
	}
	return exitStatus(c.cmd), err
}

// waitDoneWithin reports whether done closed within bound; it is the hard
// reap bound after a cancellation.
func waitDoneWithin(done <-chan struct{}, bound time.Duration) bool {
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// Close terminates the tree but does not reap: the caller still calls Wait.
// It is idempotent and safe concurrently with Wait.
//
// Close closes stdin, asks the tree to exit, polls every 10ms until it is gone
// for at most Command.KillGrace, and then kills what is left. A tree that is
// already gone is not an error. Output read ends are deliberately left open:
// closing them would discard buffered data, and a dead tree produces EOF on
// its own.
//
// ctx bounds this blocking call: the grace loop also stops at ctx.Done, an
// already-cancelled ctx skips the polite SIGTERM and kills immediately, and
// the result is ctx.Err() when ctx caused the escalation (joined with a
// preceding signal error, if any). A cancelled ctx never means "return early
// without killing": the tree is always terminated, and Close never blocks past
// min(KillGrace, ctx deadline) plus the kill itself.
func (c *Cmd) Close(ctx context.Context) error {
	c.closeOnce.Do(func() {
		c.closeErr = c.close(ctx)
	})
	return c.closeErr
}

func (c *Cmd) close(ctx context.Context) error {
	// A late deadline must not signal a recycled pid: the watcher is disarmed
	// before anything is signalled and before releaseTree drops the handle.
	c.stopCancelWatch()
	c.closeStdin()
	if treeGone(c) {
		releaseTree(c)
		return nil
	}

	// A ctx that is already dead skips the polite phase; it must never skip
	// the kill — cancelled means "abort now", not "leak".
	if err := ctx.Err(); err != nil {
		killTree(c)
		releaseTree(c)
		return err
	}

	// terminateSignal is SIGTERM where that exists. Windows has no
	// SIGTERM, so nothing is sent and the grace period itself is the notice.
	var firstErr error
	if sig := terminateSignal(); sig != nil {
		if err := signalTree(c, sig); err != nil {
			firstErr = err
		}
	}

	deadline := time.Now().Add(c.killGrace)
	ticker := time.NewTicker(closePollInterval)
	defer ticker.Stop()
	var ctxErr error
	for !treeGone(c) {
		if !time.Now().Before(deadline) {
			killTree(c)
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			ctxErr = ctx.Err()
			if ctxErr == nil {
				ctxErr = context.Canceled
			}
			killTree(c)
		}
		if ctxErr != nil {
			break
		}
	}
	releaseTree(c)

	if ctxErr != nil {
		if firstErr != nil {
			return errors.Join(ctxErr, firstErr)
		}
		return ctxErr
	}
	return firstErr
}

// copyStdin delivers Command.Stdin to the child and closes the write end so the
// child sees EOF. It runs in its own goroutine, so Start never blocks on the
// payload and a 1MB prompt cannot deadlock against a child that floods stdout
// before it reads stdin.
func (c *Cmd) copyStdin(r io.Reader) {
	_, _ = io.Copy(c.stdinW, r)
	c.closeStdin()
}

// closeStdin closes the stdin write end exactly once.
func (c *Cmd) closeStdin() {
	c.stdinOnce.Do(func() {
		if c.stdinW != nil {
			_ = c.stdinW.Close()
		}
	})
}

// setCancelWatch stores the context.AfterFunc stop handle for Start's ctx. It
// is a setter because an already-done ctx may run the watched function before
// AfterFunc returns.
func (c *Cmd) setCancelWatch(stop func() bool) {
	c.stopCancelMu.Lock()
	c.stopCancel = stop
	c.stopCancelMu.Unlock()
}

// stopCancelWatch disarms Start's context watcher; a nil watcher is a no-op
// and repeated calls are harmless. It is called by Wait (after the leader is
// reaped) and by Close, so a late deadline cannot signal a recycled pid.
func (c *Cmd) stopCancelWatch() {
	c.stopCancelMu.Lock()
	stop := c.stopCancel
	c.stopCancelMu.Unlock()
	if stop != nil {
		stop()
	}
}

// abandonCancelled tears down a child whose Start ctx died between the fork
// and the return: the tree is killed outright (the ctx is already dead, so no
// polite phase is left), the leader is reaped, every fd is closed and the
// watcher is disarmed, so Start can return ctx.Err() without leaking a process
// or a pipe. It deliberately does not go through Close: the AfterFunc may
// already be inside Close's graceful path, and a cancelled Start must not wait
// out KillGrace behind it.
func (c *Cmd) abandonCancelled() {
	c.stopCancelWatch()
	killTree(c)
	releaseTree(c)
	c.closeStdin()
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Wait()
	}
	if c.stdout != nil {
		_ = c.stdout.closeFile()
	}
	if c.stderr != nil {
		_ = c.stderr.closeFile()
	}
}

// abandon tears down a started child whose attach step failed: kill it, reap
// it, close every fd and drop the tree handle, so Start can return an error
// without leaking a process or a pipe.
func (c *Cmd) abandon() {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	c.closeStdin()
	if c.stdout != nil {
		_ = c.stdout.closeFile()
	}
	if c.stderr != nil {
		_ = c.stderr.closeFile()
	}
	releaseTree(c)
}

// stream is one of the child's output pipes. It translates the error a closed
// file produces into io.EOF, so a caller's bufio.Scanner terminates normally
// both at the natural end and after a WaitDelay force-close, and it signals
// its done channel exactly once at that point.
type stream struct {
	f *os.File

	done     chan struct{}
	doneOnce sync.Once

	fileOnce sync.Once
	claimed  atomic.Bool
}

func newStream(f *os.File) *stream {
	return &stream{f: f, done: make(chan struct{})}
}

// Read implements io.Reader. The first Read claims the stream: from then on
// Wait waits for this stream instead of draining it.
func (s *stream) Read(p []byte) (int, error) {
	s.claimed.Store(true)
	n, err := s.f.Read(p)
	if err != nil {
		if isClosedStreamError(err) {
			err = io.EOF
		}
		// The natural end and the forced end are both terminal: close the fd
		// and let Wait/(a later Read) proceed.
		s.finish()
		_ = s.closeFile()
	}
	return n, err
}

// Close implements io.ReadCloser. Callers use it when they are done early;
// reads afterwards report io.EOF.
func (s *stream) Close() error {
	s.claimed.Store(true)
	s.finish()
	return s.closeFile()
}

// forceClose closes the read end from under a blocked Read. The blocked read
// comes back as io.EOF (see Read), which is what makes WaitDelay collection
// terminate instead of hanging.
func (s *stream) forceClose() {
	s.finish()
	_ = s.closeFile()
}

// finish marks the stream terminal exactly once.
func (s *stream) finish() {
	s.doneOnce.Do(func() { close(s.done) })
}

// closeFile closes the read end exactly once; a second close is not an error
// and never reaches the caller.
func (s *stream) closeFile() error {
	var err error
	s.fileOnce.Do(func() {
		err = s.f.Close()
		if isClosedStreamError(err) {
			err = nil
		}
	})
	return err
}

// isClosedStreamError reports whether err is one of the "the file is closed"
// spellings a forced close produces. os.ErrClosed is an alias for
// fs.ErrClosed; both are named here on purpose.
func isClosedStreamError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, os.ErrClosed) ||
		errors.Is(err, fs.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe)
}

// closeAll closes every non-nil file, ignoring errors: the closing paths here
// run on both the success and the failure path of Start, where some fds are
// already gone.
func closeAll(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}
