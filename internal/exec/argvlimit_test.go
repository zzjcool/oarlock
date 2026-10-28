package exec

import (
	"errors"
	"io/fs"
	"strings"
	"syscall"
	"testing"
)

// TestArgvLimitErrorIsAndAs pins the frozen error shape for a POSIX E2BIG:
// errors.Is(ErrArgvLimit) and errors.As(*ArgvLimitError) both hold, the raw
// errno stays on the chain, and the message carries the remedy.
func TestArgvLimitErrorIsAndAs(t *testing.T) {
	cause := &fs.PathError{Op: "fork/exec", Path: "/usr/local/bin/cursor-agent", Err: syscall.E2BIG}

	err := wrapStartError(cause)
	if err == nil {
		t.Fatal("wrapStartError(E2BIG) = nil, want *ArgvLimitError")
	}
	if !errors.Is(err, ErrArgvLimit) {
		t.Fatalf("errors.Is(err, ErrArgvLimit) = false, want true (err = %v)", err)
	}
	var limitErr *ArgvLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("errors.As(err, *ArgvLimitError) = false, want true (err = %T)", err)
	}
	if limitErr.Err == nil {
		t.Fatal("*ArgvLimitError.Err = nil, want the original start error")
	}
	if !errors.Is(err, syscall.E2BIG) {
		t.Fatalf("errors.Is(err, syscall.E2BIG) = false, want true (raw errno must stay on the chain)")
	}
	msg := err.Error()
	for _, want := range []string{"stdin", "shorten the command line", "argument size limit"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q does not mention %q", msg, want)
		}
	}
	if !strings.Contains(msg, cause.Path) {
		t.Fatalf("message %q does not name the original failure (path %q)", msg, cause.Path)
	}
}

// TestArgvLimitErrorWindowsExcedRange covers Windows error 206, which has no
// named syscall constant and must map to the same frozen shape.
func TestArgvLimitErrorWindowsExcedRange(t *testing.T) {
	cause := &fs.PathError{Op: "fork/exec", Path: `C:\Users\user\AppData\Roaming\npm\cursor-agent.cmd`, Err: syscall.Errno(206)}

	err := wrapStartError(cause)
	if err == nil {
		t.Fatal("wrapStartError(errno 206) = nil, want *ArgvLimitError")
	}
	if !errors.Is(err, ErrArgvLimit) {
		t.Fatalf("errors.Is(err, ErrArgvLimit) = false, want true (err = %v)", err)
	}
	var limitErr *ArgvLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("errors.As(err, *ArgvLimitError) = false, want true (err = %T)", err)
	}
	if !errors.Is(err, syscall.Errno(206)) {
		t.Fatalf("errors.Is(err, syscall.Errno(206)) = false, want true (raw errno must stay on the chain)")
	}
	msg := err.Error()
	for _, want := range []string{"stdin", "shorten the command line"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q does not mention %q", msg, want)
		}
	}
}

// TestArgvLimitPassesUnrelatedErrorThrough checks the identity half of
// wrapStartError: anything that is not an argv-limit failure is returned as-is,
// same value, so callers can still compare against their own error variables.
func TestArgvLimitPassesUnrelatedErrorThrough(t *testing.T) {
	enoent := &fs.PathError{Op: "fork/exec", Path: "/nonexistent/cursor-agent", Err: syscall.ENOENT}
	if got := wrapStartError(enoent); got != error(enoent) {
		t.Fatalf("wrapStartError(ENOENT) = %#v (%T), want the identical value", got, got)
	}
	if got := wrapStartError(fs.ErrNotExist); got != error(fs.ErrNotExist) {
		t.Fatalf("wrapStartError(fs.ErrNotExist) = %#v (%T), want the identical sentinel", got, got)
	}
	if isArgvLimitError(enoent) {
		t.Fatalf("isArgvLimitError(ENOENT) = true, want false")
	}
	if isArgvLimitError(nil) {
		t.Fatalf("isArgvLimitError(nil) = true, want false")
	}
}

// TestArgvLimitWrapNilIsNil keeps Start's "no start error" path free of
// sentinel noise.
func TestArgvLimitWrapNilIsNil(t *testing.T) {
	if err := wrapStartError(nil); err != nil {
		t.Fatalf("wrapStartError(nil) = %v, want nil", err)
	}
}
