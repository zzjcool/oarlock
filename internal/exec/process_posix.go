//go:build !windows

package exec

import (
	"errors"
	"os"
	osexec "os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// sysState is the platform-specific state hanging off Cmd. POSIX needs none:
// the child's process group is fully named by the leader's pid, which Cmd
// already holds.
type sysState struct{}

// prepareStart runs before the child exists. There is nothing to create first
// on POSIX — Setpgid (see newSysProcAttr) is applied at fork time.
func prepareStart(c *Cmd) error { return nil }

// newSysProcAttr makes the child lead its own process group. Every descendant
// that does not call setpgid/setsid itself stays in that group, so
// kill(-pid, sig) reaches the whole tree with one syscall.
func newSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// attach is the post-Start half of the platform handshake; on POSIX the fork
// already did everything, so it is a no-op.
func attach(c *Cmd) error { return nil }

// releaseTree drops any platform handle owned by the tree. POSIX has none.
func releaseTree(c *Cmd) {}

// treeOwnershipError reports why the tree could not be owned. POSIX always
// owns the tree it starts (via the process group), so this is always nil.
func treeOwnershipError(c *Cmd) error { return nil }

// terminateSignal is the signal Close sends first: SIGTERM, so the child gets a
// chance to exit on its own during KillGrace. Windows has no SIGTERM and
// returns nil (see process_windows.go).
func terminateSignal() os.Signal { return syscall.SIGTERM }

// signalTree delivers sig to the whole process group started at pid.
//
// pid-reuse caveat: the group is addressed by the leader's pid, and by the time
// Close runs the leader may already have been reaped by Wait while the pgid
// still names a live group of descendants. That is precisely the case this
// path exists for, but it also means that if the entire group is gone and its
// pgid has been recycled to an unrelated group before we signal, the signal
// could reach that unrelated group. In practice Close signals immediately
// after Wait reaps and the kernel does not recycle a pgid while members remain,
// so the window is narrow; Cmd.Signal is a best-effort cancellation primitive,
// not a guarantee, and callers must not signal trees they have already Closed.
func signalTree(c *Cmd, sig os.Signal) error {
	pid := c.pid
	if pid <= 0 {
		return nil
	}
	s, ok := sig.(syscall.Signal)
	if !ok {
		// Not a syscall signal (an os.Signal implementation of the caller's
		// own): the group send cannot express it, use the leader alone.
		return signalProcess(c, sig)
	}
	err := syscall.Kill(-pid, s)
	if err == nil {
		return nil
	}
	// The group send failed: the leader may have left the group (setsid), or
	// we may lack permission on a descendant. Fall back to the leader alone.
	if perr := signalProcess(c, sig); perr == nil {
		return nil
	}
	if errors.Is(err, syscall.ESRCH) {
		// Nothing left to signal: "already gone" is success for Close/Signal.
		return nil
	}
	return err
}

// signalProcess signals the leader only, used when the group send fails.
func signalProcess(c *Cmd, sig os.Signal) error {
	if c.cmd == nil || c.cmd.Process == nil {
		return os.ErrProcessDone
	}
	return c.cmd.Process.Signal(sig)
}

// treeGone reports whether the process group is empty.
//
// kill(-pid, 0) is the primary probe: it succeeds while any member of the group
// exists and returns ESRCH once the last one is gone.
//
// There is one case that probe cannot see, and it is the common one: Close
// terminates but does not reap, so while Close polls, the leader it just
// signalled is usually an un-reaped zombie — and a zombie is still a group
// member, which would make Close wait out the whole KillGrace for a tree that
// is already dead. A process that has exited cannot run, holds no descriptors
// and cannot spawn, so it does not count as "gone". Linux exposes that
// distinction cheaply through /proc (the leader's state, then the group's live
// members); other POSIX kernels keep the coarse probe and may therefore use the
// full KillGrace in that case, which the KillGrace bound still caps.
func treeGone(c *Cmd) bool {
	pid := c.pid
	if pid <= 0 {
		return true
	}
	err := syscall.Kill(-pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return true
	}
	// nil (group exists) and EPERM (group exists, we may not signal it, e.g. a
	// setuid descendant) both mean "something is still there" as far as the
	// portable probe goes.
	if runtime.GOOS != "linux" {
		return false
	}
	return linuxGroupHasNoLiveMember(pid)
}

// linuxGroupHasNoLiveMember is the zombie-aware refinement of the group probe:
// false as soon as one member can still run.
func linuxGroupHasNoLiveMember(pgid int) bool {
	if state, ok := linuxProcState(pgid); ok && state != "Z" && state != "X" {
		// The leader itself is still running: no need to look further.
		return false
	}
	live, err := linuxCountLiveGroupMembers(pgid)
	if err != nil {
		// No /proc (or it became unreadable): fall back to the coarse answer
		// the caller already has.
		return false
	}
	return live == 0
}

// linuxProcState returns pid's process state letter ("R", "S", "Z", ...) from
// /proc/<pid>/stat. The comm field is parenthesised and may itself contain
// spaces and parentheses, so the fields are read after the last ')'.
func linuxProcState(pid int) (string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	fields, ok := linuxStatFields(string(data))
	if !ok {
		return "", false
	}
	return fields[0], true
}

// linuxCountLiveGroupMembers counts the processes in process group pgid that
// can still run, i.e. everything except zombies and dead entries.
func linuxCountLiveGroupMembers(pgid int) (int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	want := strconv.Itoa(pgid)
	live := 0
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue // not a pid directory
		}
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue // the process exited while we were looking
		}
		fields, ok := linuxStatFields(string(data))
		if !ok || len(fields) < 3 {
			continue
		}
		// fields: state ppid pgrp ...
		if fields[2] != want {
			continue
		}
		if fields[0] == "Z" || fields[0] == "X" {
			continue
		}
		live++
	}
	return live, nil
}

// linuxStatFields splits /proc/<pid>/stat into the fields that follow the
// parenthesised comm: [state, ppid, pgrp, session, ...].
func linuxStatFields(stat string) ([]string, bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 || i+1 >= len(stat) {
		return nil, false
	}
	fields := strings.Fields(stat[i+1:])
	if len(fields) == 0 {
		return nil, false
	}
	return fields, true
}

// killTree is the last-resort kill: SIGKILL to the whole group.
func killTree(c *Cmd) {
	if c.pid <= 0 {
		return
	}
	_ = signalTree(c, syscall.SIGKILL)
}

// exitStatus reads the leader's wait status. A signal death keeps the signal
// (Code -1) instead of collapsing into a number, so a caller can tell
// "cancelled by us" (SIGTERM/SIGKILL) from "exited non-zero".
func exitStatus(cmd *osexec.Cmd) ExitStatus {
	if cmd == nil || cmd.ProcessState == nil {
		return ExitStatus{Code: -1}
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return ExitStatus{Code: -1, Signal: ws.Signal()}
		}
		if ws.Exited() {
			return ExitStatus{Code: ws.ExitStatus()}
		}
	}
	return ExitStatus{Code: cmd.ProcessState.ExitCode()}
}
