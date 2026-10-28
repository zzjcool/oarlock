//go:build unix

package exec

// The argv-limit acceptance evidence: a real kernel refusal of a real
// over-long command line, produced by execve, not by a fabricated errno (the
// pure mapping tests live in argvlimit_test.go). Self-bounding like the tests
// in exec_unix_test.go, because Start can in principle wedge.

import (
	"context"
	"errors"
	"io/fs"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// argvLimitProbeLen is the single-argument size that reliably trips the
// platform's execve limit.
//
// Linux caps one argument at 32 pages — 128 KiB on 4 KiB pages — so 200 KiB is
// refused by the kernel itself. macOS has no per-string cap worth relying on;
// it enforces the total NCARGS/ARG_MAX budget (1 MiB), where a 200 KiB argument
// would start happily and the test would silently assert nothing, so the probe
// there has to exceed the total budget. Either way the failure Start sees is a
// real E2BIG out of execve.
func argvLimitProbeLen() int {
	if runtime.GOOS == "linux" {
		return 200 << 10
	}
	return 2 << 20
}

// TestStartMapsE2BIGToErrArgvLimit is the acceptance evidence for issue #16's
// "超长 argv → ErrArgvLimit，含可操作提示" criterion.
func TestStartMapsE2BIGToErrArgvLimit(t *testing.T) {
	argLen := argvLimitProbeLen()
	arg := strings.Repeat("a", argLen)

	truePath, lookErr := osexec.LookPath("true")
	if lookErr != nil {
		t.Skipf("no true executable on PATH: %v", lookErr)
	}
	cmd, err := Start(context.Background(), &Command{Path: truePath, Args: []string{arg}})
	if err == nil {
		if cmd != nil {
			_ = cmd.Close(context.Background())
			_, _ = cmd.Wait(context.Background())
		}
		t.Fatalf("Start() with one %d-byte argument = nil error, want E2BIG: the OS accepted a command line this test expects it to refuse", argLen)
	}
	if cmd != nil {
		t.Fatalf("Start() on failure = (%v, %v), want a nil *Cmd", cmd, err)
	}
	if !errors.Is(err, ErrArgvLimit) {
		t.Fatalf("errors.Is(err, ErrArgvLimit) = false, want true (err = %T: %v)", err, err)
	}
	var argvErr *ArgvLimitError
	if !errors.As(err, &argvErr) {
		t.Fatalf("errors.As(err, *ArgvLimitError) = false, want true (err = %T: %v)", err, err)
	}
	if argvErr.Err == nil {
		t.Fatal("*ArgvLimitError.Err = nil, want the original start error kept on the chain")
	}
	if !errors.Is(err, syscall.E2BIG) {
		t.Fatalf("errors.Is(err, syscall.E2BIG) = false, want the raw errno kept on the chain (err = %v)", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "stdin") {
		t.Fatalf("error message %q does not name the remedy (send the payload on stdin)", msg)
	}
}

// TestStartPassesOrdinaryStartErrorsThrough is the other half of the mapping:
// only argv-limit failures may be rewritten. A missing executable has to stay
// an ordinary start error so callers can still test errors.Is(err,
// fs.ErrNotExist).
//
// Asserted shape, and it is worth being explicit about it: the path here is
// absolute, so no LookPath happens and the failure comes from execve itself —
// an *fs.PathError (or a bare syscall.Errno) carrying ENOENT, which satisfies
// errors.Is(err, fs.ErrNotExist). exec.ErrNotFound is the LookPath shape, i.e. a
// bare name that is not on PATH; that case is covered by
// TestStartOfMissingBareNameFails in exec_test.go.
func TestStartPassesOrdinaryStartErrorsThrough(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-oarlock-binary")

	cmd, err := Start(context.Background(), &Command{Path: missing})
	if err == nil {
		if cmd != nil {
			_ = cmd.Close(context.Background())
			_, _ = cmd.Wait(context.Background())
		}
		t.Fatalf("Start(%q) = nil error, want ENOENT", missing)
	}
	if errors.Is(err, ErrArgvLimit) {
		t.Fatalf("errors.Is(err, ErrArgvLimit) = true for a missing executable, want false (err = %v)", err)
	}
	var argvErr *ArgvLimitError
	if errors.As(err, &argvErr) {
		t.Fatalf("errors.As(err, *ArgvLimitError) = true for a missing executable, want false (err = %v)", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("errors.Is(err, fs.ErrNotExist) = false for a missing executable, want true (err = %T: %v)", err, err)
	}
}
