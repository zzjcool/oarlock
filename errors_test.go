package oarlock

import (
	"errors"
	"os"
	"testing"
)

func TestRejected_EveryCodeCarriesItsBranchWord(t *testing.T) {
	cases := []struct {
		code Code
		want string
	}{
		{Busy, "busy"},
		{NoActiveTurn, "no_active_turn"},
		{NotQueued, "not_queued"},
		{Unsupported, "unsupported"},
		{RuntimeExited, "runtime_exited"},
		{Disposed, "disposed"},
		{RuntimeRefused, "runtime_refused"},
		{ErrorCode, "error"},
	}
	for _, tc := range cases {
		err := &Rejected{Code: tc.code, Reason: "why", Seq: 12, RequestID: "r1"}
		var rejected *Rejected
		if !errors.As(err, &rejected) {
			t.Fatalf("code %q does not survive errors.As", tc.code)
		}
		if string(rejected.Code) != tc.want {
			t.Fatalf("code = %q, want %q", rejected.Code, tc.want)
		}
		if rejected.Seq != 12 || rejected.RequestID != "r1" {
			t.Fatalf("rejection lost its position: %+v", rejected)
		}
	}
}

func TestRejected_WrapsATransportCauseWithoutLosingTheCode(t *testing.T) {
	err := &Rejected{Code: ErrorCode, Reason: "too many arguments", Cause: os.ErrInvalid}

	if !errors.Is(err, os.ErrInvalid) {
		t.Fatal("the transport cause is not unwrappable")
	}
	var rejected *Rejected
	if !errors.As(err, &rejected) || rejected.Code != ErrorCode {
		t.Fatalf("the typed code was lost: %v", err)
	}
}

func TestRejected_RetryableIsAdviceNotAutomaticRetry(t *testing.T) {
	yes := []Code{Busy, NoActiveTurn, NotQueued}
	for _, code := range yes {
		if !(&Rejected{Code: code}).Retryable() {
			t.Fatalf("%q should be retryable advice", code)
		}
	}
	no := []Code{Unsupported, RuntimeExited, Disposed, RuntimeRefused, ErrorCode}
	for _, code := range no {
		if (&Rejected{Code: code}).Retryable() {
			t.Fatalf("%q must not be retried automatically", code)
		}
	}
}

func TestSessionNotFound_PreservesNativeEvidence(t *testing.T) {
	cause := errors.New("no session file at /tmp/x")
	err := &SessionNotFound{SessionID: "abc", Cause: cause}

	if !errors.Is(err, cause) {
		t.Fatal("SessionNotFound lost its native cause")
	}
	var notFound *SessionNotFound
	if !errors.As(err, &notFound) || notFound.SessionID != "abc" {
		t.Fatalf("SessionNotFound lost its id: %v", err)
	}
}
