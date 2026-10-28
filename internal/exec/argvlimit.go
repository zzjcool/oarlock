package exec

import (
	"errors"
	"syscall"
)

// ErrArgvLimit reports that the OS refused to create the child because the
// command line was too large: E2BIG (execve) on POSIX, error 206
// (ERROR_FILENAME_EXCED_RANGE) from CreateProcess on Windows. A raw errno
// gives the caller no hint at the cause or the fix, so Start maps it to
// *ArgvLimitError, which satisfies errors.Is(err, ErrArgvLimit) while keeping
// the original errno on the chain.
var ErrArgvLimit = errors.New("exec: command line exceeds the OS argument size limit")

// ArgvLimitError is the concrete argv-limit failure. Its message names the
// cause and the remedy, and its unwrap chain is {ErrArgvLimit, Err} so that
// errors.Is(err, ErrArgvLimit), errors.As(err, **ArgvLimitError) and
// errors.Is(err, syscall.E2BIG) all hold at once.
type ArgvLimitError struct{ Err error }

// Error reports the limit, the underlying OS error when present and the
// remedy: the payload belongs on stdin, not on the command line.
func (e *ArgvLimitError) Error() string {
	msg := ErrArgvLimit.Error()
	if e != nil && e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg + ": remedy: send large payloads on stdin instead of the command line, or shorten the command line"
}

// Unwrap returns both sentinels so errors.Is(ErrArgvLimit) and
// errors.Is(syscall.E2BIG) (or 206) succeed.
func (e *ArgvLimitError) Unwrap() []error {
	if e == nil {
		return []error{ErrArgvLimit}
	}
	if e.Err == nil {
		return []error{ErrArgvLimit}
	}
	return []error{ErrArgvLimit, e.Err}
}

// errnoFilenameExcedRange is Windows ERROR_FILENAME_EXCED_RANGE (206), the
// code CreateProcess returns for an over-long command line. syscall has no
// named constant for it and we must not pull in golang.org/x/sys.
const errnoFilenameExcedRange = syscall.Errno(206)

// isArgvLimitError reports whether err is the OS refusing an oversized command
// line. Both checks live in one portable file: syscall.E2BIG is defined on
// every GOOS (on Windows it is the C-runtime errno value), and 206 never
// collides with it.
func isArgvLimitError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.E2BIG) {
		return true
	}
	return errors.Is(err, errnoFilenameExcedRange)
}

// wrapStartError maps a failed child start to the frozen error shape: nil
// stays nil, an argv-limit failure becomes *ArgvLimitError (wrapping the
// original), and every other error is returned unchanged — same value, so
// callers comparing against their own error variables still match.
func wrapStartError(err error) error {
	if err == nil {
		return nil
	}
	if !isArgvLimitError(err) {
		return err
	}
	return &ArgvLimitError{Err: err}
}
