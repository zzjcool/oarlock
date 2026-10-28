//go:build windows

package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Win32 constants. They are spelled out here because syscall does not export
// all of them and this package must not depend on golang.org/x/sys.
const (
	createSuspended = 0x00000004 // CREATE_SUSPENDED
	createNoWindow  = 0x08000000 // CREATE_NO_WINDOW

	processTerminate        = 0x0001 // PROCESS_TERMINATE
	processSetQuota         = 0x0100 // PROCESS_SET_QUOTA
	processQueryInformation = 0x0400 // PROCESS_QUERY_INFORMATION

	// JobObjectBasicAccountingInformation and JobObjectExtendedLimitInformation,
	// the two JOBOBJECTINFOCLASS values used here.
	jobClassBasicAccounting = 1
	jobClassExtendedLimit   = 9

	jobObjectLimitKillOnJobClose = 0x2000

	toolhelpSnapThread  = 0x00000004 // TH32CS_SNAPTHREAD
	threadSuspendResume = 0x0002     // THREAD_SUSPEND_RESUME

	// taskkillTimeout bounds the fallback tree kill, so Close can never hang on
	// a wedged taskkill.
	taskkillTimeout = 10 * time.Second
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCreateJobObjectW          = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject   = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject  = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject        = kernel32.NewProc("TerminateJobObject")
	procQueryInformationJobObject = kernel32.NewProc("QueryInformationJobObject")
	procOpenProcess               = kernel32.NewProc("OpenProcess")
	procCreateToolhelp32Snapshot  = kernel32.NewProc("CreateToolhelp32Snapshot")
	procThread32First             = kernel32.NewProc("Thread32First")
	procThread32Next              = kernel32.NewProc("Thread32Next")
	procOpenThread                = kernel32.NewProc("OpenThread")
	procResumeThread              = kernel32.NewProc("ResumeThread")
)

// JOBOBJECT_BASIC_LIMIT_INFORMATION. Field order and types are the Win32
// layout; unsafe.Sizeof of the enclosing struct is what
// SetInformationJobObject is given as the buffer size.
type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

// IO_COUNTERS.
type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

// JOBOBJECT_EXTENDED_LIMIT_INFORMATION, used only to set
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE.
type jobObjectExtendedLimitInformation struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IOInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION: ActiveProcesses is how tree
// liveness is read — the job is gone when nothing is left inside it.
type jobObjectBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// THREADENTRY32.
type threadEntry32 struct {
	Size           uint32
	Usage          uint32
	ThreadID       uint32
	OwnerProcessID uint32
	BasePri        int32
	DeltaPri       int32
	Flags          uint32
}

// sysState is the platform-specific state hanging off Cmd: the job object that
// owns the tree and, if ownership could not be taken, the reason why.
//
// The mutex is there because Close (releaseTree) and a caller's Signal can run
// concurrently on different goroutines.
type sysState struct {
	mu        sync.Mutex
	job       syscall.Handle
	ownership error
}

// jobHandle returns the owning job, 0 when the tree is unowned.
func (s *sysState) jobHandle() syscall.Handle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.job
}

// takeJob transfers ownership of the handle to the caller, leaving the state
// empty so a second release cannot double-close it.
func (s *sysState) takeJob() syscall.Handle {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.job
	s.job = 0
	return h
}

func (s *sysState) setOwnership(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ownership = err
}

// prepareStart creates and configures the job object BEFORE the child exists.
// The child is then started suspended (newSysProcAttr), so it cannot spawn a
// grandchild that escapes the job before attach assigns it.
//
// Failure to create the job is not fatal: the child still runs, unowned, and
// cancellation falls back to taskkill. OwnershipError reports why. This is the
// same "fail open, but never leave the child suspended" rule attach follows.
func prepareStart(c *Cmd) error {
	job, err := createKillOnCloseJob()
	if err != nil {
		c.sys.setOwnership(fmt.Errorf("exec: cannot create job object, tree is unowned: %w", err))
		return nil
	}
	c.sys.mu.Lock()
	c.sys.job = job
	c.sys.mu.Unlock()
	return nil
}

// newSysProcAttr starts the child suspended: CREATE_SUSPENDED keeps it from
// executing a single instruction until attach has it inside the job, which is
// what makes the job a guarantee instead of a race.
func newSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createSuspended}
}

// attach is the post-Start half: assign the suspended child to the job, then
// resume its primary thread.
//
// If the assignment fails the child is resumed anyway and the reason is kept in
// OwnershipError — a running unowned child is recoverable (taskkill), a
// suspended one is a hang. If the resume itself fails Start kills the child.
func attach(c *Cmd) error {
	if job := c.sys.jobHandle(); job != 0 {
		if err := assignToJob(job, c.pid); err != nil {
			c.sys.setOwnership(fmt.Errorf("exec: job object ownership not taken, tree is unowned: %w", err))
			// The job holds nothing, so it must not be consulted for liveness
			// (an empty job always looks "gone") or used for the kill. Dropping
			// it switches treeGone/terminateTree to the leader + taskkill path.
			releaseTree(c)
		}
	}
	if err := resumePrimaryThread(c.pid); err != nil {
		return fmt.Errorf("exec: resume suspended child %d: %w", c.pid, err)
	}
	return nil
}

// releaseTree closes the job handle, which — because the job was created with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE — also kills every process still in it.
// The handle must therefore outlive the leader: a .cmd wrapper exits while the
// real CLI keeps running, and dropping the handle early would kill it.
func releaseTree(c *Cmd) {
	if h := c.sys.takeJob(); h != 0 {
		_ = syscall.CloseHandle(h)
	}
}

// treeOwnershipError reports why the tree is unowned, nil when the job holds
// it. With a non-nil value Close still terminates the leader via taskkill; it
// just cannot promise the grandchildren.
func treeOwnershipError(c *Cmd) error {
	c.sys.mu.Lock()
	defer c.sys.mu.Unlock()
	return c.sys.ownership
}

// terminateSignal is the signal Close sends before waiting out KillGrace.
// Windows has no SIGTERM — there is no way to ask a process to exit — so
// nothing is sent and the grace period exists only to let the tree finish on
// its own after stdin reached EOF. The hard terminate is killTree.
func terminateSignal() os.Signal { return nil }

// signalTree terminates the tree. Windows has no signals: SIGTERM, SIGKILL and
// os.Kill all mean "terminate the tree", and the sentinel is ignored on
// purpose rather than silently no-op'd.
func signalTree(c *Cmd, sig os.Signal) error {
	return terminateTree(c)
}

// treeGone reports whether anything is still alive in the tree. With a job it
// is the job's own accounting (ActiveProcesses == 0); without one it is the
// leader's liveness, since an unowned tree cannot be enumerated.
func treeGone(c *Cmd) bool {
	if job := c.sys.jobHandle(); job != 0 {
		active, err := jobActiveProcesses(job)
		if err != nil {
			// The handle is no longer usable: nothing is owned any more.
			return true
		}
		return active == 0
	}
	return !leaderAlive(c)
}

// killTree is the last-resort kill used after KillGrace.
func killTree(c *Cmd) {
	_ = terminateTree(c)
}

// terminateTree kills the whole tree: TerminateJobObject when the job owns it,
// taskkill /T /F when it does not.
func terminateTree(c *Cmd) error {
	if job := c.sys.jobHandle(); job != 0 {
		if err := terminateJob(job); err == nil {
			return nil
		} else if tkErr := taskkillTree(c.pid); tkErr != nil {
			return errors.Join(err, tkErr)
		}
		return nil
	}
	return taskkillTree(c.pid)
}

// exitStatus reads the leader's exit code. Windows has no signals: a
// TerminateProcess/TerminateJobObject death surfaces as a non-zero exit code
// and Signal stays nil, which is what ExitStatus documents.
func exitStatus(cmd *osexec.Cmd) ExitStatus {
	if cmd == nil || cmd.ProcessState == nil {
		return ExitStatus{Code: -1}
	}
	return ExitStatus{Code: cmd.ProcessState.ExitCode()}
}

// createKillOnCloseJob creates an unnamed job object with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE: every process in the job dies when the
// last handle to the job closes, so cancellation cannot leak a grandchild even
// if we crash.
func createKillOnCloseJob() (syscall.Handle, error) {
	h, _, err := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return 0, callErr("CreateJobObjectW", err)
	}
	job := syscall.Handle(h)
	info := jobObjectExtendedLimitInformation{}
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	if err := setJobInformation(job, jobClassExtendedLimit, unsafe.Pointer(&info), unsafe.Sizeof(info)); err != nil {
		_ = syscall.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// setJobInformation is a typed wrapper around SetInformationJobObject. The
// struct is kept alive across the call by the caller's local variable; the
// uintptr(unsafe.Pointer(&x)) form is the documented way to pass it.
func setJobInformation(job syscall.Handle, class uint32, info unsafe.Pointer, size uintptr) error {
	r, _, err := procSetInformationJobObject.Call(uintptr(job), uintptr(class), uintptr(info), size)
	if r == 0 {
		return callErr("SetInformationJobObject", err)
	}
	return nil
}

// assignToJob puts the (suspended) child into the job. PROCESS_SET_QUOTA and
// PROCESS_TERMINATE are the rights AssignProcessToJobObject requires.
func assignToJob(job syscall.Handle, pid int) error {
	h, _, err := procOpenProcess.Call(processSetQuota|processTerminate, 0, uintptr(pid))
	if h == 0 {
		return callErr("OpenProcess", err)
	}
	defer func() { _ = syscall.CloseHandle(syscall.Handle(h)) }()
	r, _, err := procAssignProcessToJobObject.Call(uintptr(job), h)
	if r == 0 {
		return callErr("AssignProcessToJobObject", err)
	}
	return nil
}

// jobActiveProcesses reads the job's live process count.
func jobActiveProcesses(job syscall.Handle) (uint32, error) {
	info := jobObjectBasicAccountingInformation{}
	r, _, err := procQueryInformationJobObject.Call(
		uintptr(job),
		jobClassBasicAccounting,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
		0,
	)
	if r == 0 {
		return 0, callErr("QueryInformationJobObject", err)
	}
	return info.ActiveProcesses, nil
}

// terminateJob kills every process in the job.
func terminateJob(job syscall.Handle) error {
	r, _, err := procTerminateJobObject.Call(uintptr(job), 1)
	if r == 0 {
		return callErr("TerminateJobObject", err)
	}
	return nil
}

// resumePrimaryThread resumes the thread CREATE_SUSPENDED suspended. A process
// created suspended has exactly one thread, at the moment of creation, so the
// lowest thread id owned by the pid is the primary one.
func resumePrimaryThread(pid int) error {
	tid, err := primaryThreadID(pid)
	if err != nil {
		return err
	}
	h, _, err := procOpenThread.Call(threadSuspendResume, 0, uintptr(tid))
	if h == 0 {
		return callErr("OpenThread", err)
	}
	defer func() { _ = syscall.CloseHandle(syscall.Handle(h)) }()
	r, _, err := procResumeThread.Call(h)
	if r == uintptr(0xFFFFFFFF) { // (DWORD)-1 means failure
		return callErr("ResumeThread", err)
	}
	return nil
}

// primaryThreadID finds the pid's primary thread with a Toolhelp snapshot.
func primaryThreadID(pid int) (uint32, error) {
	snap, _, err := procCreateToolhelp32Snapshot.Call(toolhelpSnapThread, 0)
	if syscall.Handle(snap) == syscall.InvalidHandle {
		return 0, callErr("CreateToolhelp32Snapshot", err)
	}
	snapshot := syscall.Handle(snap)
	defer func() { _ = syscall.CloseHandle(snapshot) }()

	entry := threadEntry32{Size: uint32(unsafe.Sizeof(threadEntry32{}))}
	r, _, err := procThread32First.Call(uintptr(snapshot), uintptr(unsafe.Pointer(&entry)))
	if r == 0 {
		return 0, callErr("Thread32First", err)
	}
	var primary uint32
	for {
		if int(entry.OwnerProcessID) == pid {
			if primary == 0 || entry.ThreadID < primary {
				primary = entry.ThreadID
			}
		}
		entry.Size = uint32(unsafe.Sizeof(threadEntry32{}))
		r, _, _ = procThread32Next.Call(uintptr(snapshot), uintptr(unsafe.Pointer(&entry)))
		if r == 0 {
			break // ERROR_NO_MORE_FILES: end of enumeration
		}
	}
	if primary == 0 {
		return 0, fmt.Errorf("no thread found for process %d", pid)
	}
	return primary, nil
}

// leaderAlive reports whether the leader still exists, used only when the tree
// is unowned and the job cannot answer. A process that cannot be opened, or
// whose handle is already signalled, is gone.
//
// Only the OS is asked: osexec.Cmd.ProcessState is written by a concurrent
// Wait, so reading it here would race (Close is safe concurrently with Wait).
func leaderAlive(c *Cmd) bool {
	if c.cmd == nil || c.cmd.Process == nil {
		return false
	}
	h, _, _ := procOpenProcess.Call(processQueryInformation, 0, uintptr(c.pid))
	if h == 0 {
		return false
	}
	defer func() { _ = syscall.CloseHandle(syscall.Handle(h)) }()
	event, err := syscall.WaitForSingleObject(syscall.Handle(h), 0)
	if err != nil {
		return false
	}
	return event == syscall.WAIT_TIMEOUT
}

// taskkillTree is the documented fallback for a tree we do not own: taskkill /T
// walks the parent/child tree and /F forces it. The 10s bound (and
// CREATE_NO_WINDOW) keep Close from hanging on a wedged taskkill or flashing a
// console. A non-zero taskkill exit means "already gone" or "lost the race",
// neither of which Close should report as its own failure.
func taskkillTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), taskkillTimeout)
	defer cancel()
	cmd := osexec.CommandContext(ctx, "taskkill", "/pid", strconv.Itoa(pid), "/T", "/F")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	err := cmd.Run()
	if err == nil {
		return nil
	}
	var exitErr *osexec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}
	return fmt.Errorf("taskkill /pid %d /T /F: %w", pid, err)
}

// callErr turns a failed LazyProc call into an error. LazyProc.Call reports
// GetLastError, which is only meaningful when the call itself failed (r1 == 0
// or INVALID_HANDLE_VALUE), so it is only consulted there.
func callErr(name string, err error) error {
	if err == nil || errors.Is(err, syscall.Errno(0)) {
		return fmt.Errorf("%s failed", name)
	}
	return fmt.Errorf("%s: %w", name, err)
}
