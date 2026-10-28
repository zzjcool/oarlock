// Package exec starts one child process and owns the whole tree it creates.
//
// A coding-agent CLI is rarely a single process: an npm shim spawns
// cmd.exe, which spawns powershell.exe, which spawns node.exe, which spawns
// the actual agent. Killing the pid we started leaves the rest running, and
// closing the pipes we hold is not enough either — a grandchild inherits the
// stdout/stderr write ends and keeps them open, so a naive io.ReadAll hangs
// after the leader is long gone. This package makes both problems finite:
//
//   - POSIX: the child leads its own process group (Setpgid), so cancel sends
//     a signal to kill(-pgid) and reaches every descendant that stayed in the
//     group.
//   - Windows: the child is created suspended, assigned to a Job Object with
//     JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, then resumed, so not even a .cmd
//     wrapper can spawn a grandchild outside the job.
//   - stdin is delivered by a goroutine, never on the Start path, so a large
//     payload cannot deadlock against a child that writes stdout first.
//   - Wait is bounded: output streams that the caller does not read are
//     drained, and the ones it does read are force-closed after WaitDelay.
//     A wrapper that exited while a grandchild holds stdout therefore ends
//     collection with ErrWaitDelay instead of hanging the host forever.
//   - An over-long command line surfaces as ErrArgvLimit with a remedy, on
//     both E2BIG (POSIX) and error 206 (Windows).
//   - Every blocking call takes a context.Context: Start's ctx owns the tree's
//     lifetime, and Wait/Close abort by killing the tree, never by walking
//     away from it.
//
// Inside this package os/exec is imported as osexec, because the type named
// Command here is about the whole tree, not one process.
//
// # Frozen API
//
//	// Command describes one child process and the tree it may create.
//	type Command struct {
//		Path      string        // executable; a bare name is resolved through PATH
//		Args      []string      // arguments only; argv[0] is supplied by Start
//		Dir       string        // working directory; "" = inherit
//		Env       []string      // nil = inherit the parent environment
//		Stdin     io.Reader     // optional: copied to the child by a goroutine; EOF closes the pipe
//		KillGrace time.Duration // SIGTERM→SIGKILL grace; 0 = DefaultKillGrace
//		WaitDelay time.Duration // drain bound after the leader exits; 0 = DefaultWaitDelay
//	}
//
//	func Start(ctx context.Context, c *Command) (*Cmd, error)
//
//	func (c *Cmd) Pid() int
//	func (c *Cmd) Stdin() io.WriteCloser
//	func (c *Cmd) Stdout() io.ReadCloser
//	func (c *Cmd) Stderr() io.ReadCloser
//	func (c *Cmd) Wait(ctx context.Context) (ExitStatus, error)
//	func (c *Cmd) Close(ctx context.Context) error
//	func (c *Cmd) Signal(sig os.Signal) error
//	func (c *Cmd) OwnershipError() error
//
//	type ExitStatus struct {
//		Code   int       // exit code; -1 when killed by a signal
//		Signal os.Signal // signal that killed it; nil otherwise
//	}
//	func (s ExitStatus) Success() bool // Signal == nil && Code == 0
//
//	var ErrArgvLimit error // sentinel: command line exceeded the OS limit
//	var ErrWaitDelay error // sentinel: pipes stayed open past WaitDelay (Wait only)
//	var ErrShim error      // sentinel: .cmd/.bat launcher with no usable .ps1 sibling
//
//	type ArgvLimitError struct{ Err error } // As target; Is(ErrArgvLimit); message carries the remedy
//
//	const DefaultKillGrace = 10 * time.Second
//	const DefaultWaitDelay = 2 * time.Second
//
// # Usage
//
//	cmd, err := exec.Start(ctx, &exec.Command{Path: "cursor-agent", Args: []string{"--print"}, Stdin: strings.NewReader(prompt)})
//	out, err := io.ReadAll(cmd.Stdout())          // read while the child runs; io.EOF ends the loop
//	status, err := cmd.Wait(ctx)                // Wait reaps, Close kills anything left
//	defer cmd.Close(ctx)
//
// # Context
//
// Start's ctx governs the lifetime of the tree it starts: it is registered
// with context.AfterFunc, so cancelling it terminates the whole tree even when
// nobody is inside Wait. Start refuses to start anything at all when ctx is
// already done, and a ctx that dies between the fork and Start's return is
// terminated and reaped before Start reports the error.
//
// Wait's and Close's ctx bound that one blocking call. When it fires, the tree
// is killed — SIGTERM first, then SIGKILL after KillGrace — and the call
// returns ctx.Err(); an already-cancelled ctx skips the polite phase but never
// skips the kill. A leader that has already exited wins over a later
// cancellation: Wait returns the real ExitStatus and no error, because a
// result is a result.
//
// # Semantics worth knowing before calling
//
//   - A non-zero exit or a signal death is not an error: it is ExitStatus.
//     The error results are Start failures, ErrWaitDelay, and genuine I/O
//     faults.
//   - Wait is idempotent (the first result is stored) and safe after Close.
//     A ctx that fires after the leader has exited is ignored.
//   - Close terminates the tree but never reaps: call Wait as well. It is
//     idempotent, and a tree that is already gone is not an error. Its ctx
//     bounds the grace window, not the kill: Close always kills before it
//     returns.
//   - The first Read of Stdout()/Stderr() claims that stream; a stream the
//     caller never reads is drained to io.Discard by Wait so that Wait stays
//     bounded even when stderr is ignored.
//   - Reads return io.EOF at the natural end and after a WaitDelay
//     force-close, so bufio.Scanner terminates normally in both cases.
//   - OwnershipError reports why the tree could not be owned (Windows job
//     assignment); on POSIX it is always nil.
//   - Command.Stdin copying is bounded in one direction only: closing the
//     write end on EOF, on Wait or on Close ends the copy, but a reader that
//     blocks forever inside Read cannot be interrupted (no API can), so it
//     keeps its copier goroutine until it returns. Use a reader that can be
//     closed together with the tree if that matters.
package exec
