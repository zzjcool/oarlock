package oarlock

import "fmt"

// Code is the typed branch word on a rejected control. A consumer branches on
// Code and never on vendor prose.
type Code string

const (
	// Busy: a prompt arrived while a turn was running. A session has one
	// control plane and at most one active turn; prompts are never queued
	// implicitly.
	Busy Code = "busy"
	// NoActiveTurn: steer or abort arrived while nothing was running. A late
	// abort is a normal race, not an error.
	NoActiveTurn Code = "no_active_turn"
	// NotQueued: withdraw named no held input. It was already sent, never
	// queued in this session, or already withdrawn. Never answered accepted
	// when the input may already have gone.
	NotQueued Code = "not_queued"
	// Unsupported: this control cannot be done with these inputs (images
	// where the runtime takes none, empty text without images). A control the
	// runtime cannot do at all is an absent interface, not this code.
	Unsupported Code = "unsupported"
	// RuntimeExited: the stream already holds the process exit.
	RuntimeExited Code = "runtime_exited"
	// Disposed: the stream already holds this session's dispose request.
	Disposed Code = "disposed"
	// RuntimeRefused: the runtime answered no (its typed refusal or RPC
	// error); Reason is its own message.
	RuntimeRefused Code = "runtime_refused"
	// ErrorCode: the adapter could not deliver the control (a transport
	// failure); Reason is the failure text.
	ErrorCode Code = "error"
)

// Rejected is the response body's typed refusal: "not taken over; the caller
// still owns the input". Code says why in one word; Reason keeps the prose;
// Seq is the request's position in the stream; RequestID points at the
// request record.
//
// Cause is diagnostic only. It is unwrappable so a transport failure such as
// E2BIG or a deadline stays inspectable with errors.Is/As, but the control
// answer is still the typed Code.
type Rejected struct {
	Code      Code
	Reason    string
	Seq       uint64
	RequestID string
	Cause     error
}

func (e *Rejected) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("rejected (%s): %s", e.Code, e.Reason)
	}
	return fmt.Sprintf("rejected (%s)", e.Code)
}

func (e *Rejected) Unwrap() error { return e.Cause }

// SessionNotFound says a resume target does not exist, with positive evidence
// from the runtime. Cause preserves the native error. It is not a network
// failure and must not be produced from one.
type SessionNotFound struct {
	SessionID string
	Cause     error
}

func (e *SessionNotFound) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("session %q not found: %v", e.SessionID, e.Cause)
	}
	return fmt.Sprintf("session %q not found", e.SessionID)
}

func (e *SessionNotFound) Unwrap() error { return e.Cause }

// Retryable reports whether resending the same control could succeed. A
// rejection's caller owns the input; this is advice, never an automatic
// retry.
func (e *Rejected) Retryable() bool {
	switch e.Code {
	case Busy, NoActiveTurn, NotQueued:
		return true
	default:
		return false
	}
}

// UnsupportedOperation says an operating capability this runtime normally has
// cannot be done by this adapter (a CLI with no check-only mode, a login flow
// that needs a protocol the adapter does not drive). It is the ops-level
// counterpart of UnsupportedOption: callers branch with errors.As, never on
// prose, and it is never used to claim success.
//
// A whole operation the runtime does not have at all stays an absent
// interface (nil Runtime field), not this error.
type UnsupportedOperation struct {
	Operation string
	Reason    string
}

func (e *UnsupportedOperation) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("unsupported operation %q", e.Operation)
	}
	return fmt.Sprintf("unsupported operation %q: %s", e.Operation, e.Reason)
}
