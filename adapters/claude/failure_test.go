package claude

import (
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

func TestClassifyTable(t *testing.T) {
	reset := time.Unix(1_800_000_000, 0).UTC()
	cases := []struct {
		name    string
		reason  string
		f       failureFacts
		want    oarlock.FailureClass
		cred    string
		hasTime bool
	}{
		{"auth, no request sent", "Not logged in", failureFacts{category: "authentication_failed"}, oarlock.FailureAuth, "missing", false},
		{"auth 401", "Invalid API key", failureFacts{category: "authentication_failed", status: 401}, oarlock.FailureAuth, "rejected", false},
		{"auth by bare 401 status", "x", failureFacts{status: 401}, oarlock.FailureAuth, "", false},
		{"rate limit", "slow down", failureFacts{category: "rate_limit", status: 429}, oarlock.FailureRateLimited, "", false},
		{"rate limit with a named reset is quota", "limit", failureFacts{category: "rate_limit", limitResetsAt: &reset}, oarlock.FailureQuota, "", true},
		{"context overflow by terminal_reason", "Prompt is too long", failureFacts{terminalReason: "prompt_too_long"}, oarlock.FailureInputTooLarge, "", false},
		{"context overflow by words", "Prompt is too long", failureFacts{status: 400}, oarlock.FailureInputTooLarge, "", false},
		{"unknown without any evidence", "boom", failureFacts{}, oarlock.FailureUnknown, "", false},
		{"max turns", "error_max_turns", failureFacts{subtype: "error_max_turns"}, oarlock.FailureLimitReached, "", false},
		{"structured output", "x", failureFacts{subtype: "error_max_structured_output_retries"}, oarlock.FailureOutputSchema, "", false},
		{"billing words on 400", "Your credit balance is too low", failureFacts{status: 400}, oarlock.FailureBilling, "", false},
		{"overloaded 529", "x", failureFacts{status: 529}, oarlock.FailureOverloaded, "", false},
		{"server 500", "x", failureFacts{status: 500}, oarlock.FailureProvider, "", false},
		{"plain 400", "x", failureFacts{status: 400}, oarlock.FailureInvalidRequest, "", false},
	}
	for _, c := range cases {
		o := classify(c.reason, c.f)
		if o.Kind != oarlock.OutcomeFailed || o.Failure != c.want || o.Credential != c.cred || (o.ResetsAt != nil) != c.hasTime {
			t.Errorf("%s: %+v", c.name, o)
		}
		if c.hasTime && !o.ResetsAt.Equal(reset) {
			t.Errorf("%s: resetsAt %v", c.name, o.ResetsAt)
		}
	}
}

func TestLimitResetOnlyWhenTheLimitRefuses(t *testing.T) {
	mk := func(s string) sj.Rec { return sj.ParseRec([]byte(s)) }
	if r := limitReset(mk(`{"status":"rejected","resetsAt":1800000000}`)); r == nil || r.Unix() != 1800000000 {
		t.Fatalf("rejected = %v", r)
	}
	for _, s := range []string{
		`{"status":"allowed"}`, `{"status":"allowed_warning","resetsAt":1800000000}`,
		`{"status":"rejected"}`, `{"status":"rejected","resetsAt":1,"overageStatus":"allowed"}`,
	} {
		if r := limitReset(mk(s)); r != nil {
			t.Errorf("%s -> %v, want nil", s, r)
		}
	}
}

// The category lives on the turn's error assistant frame and the reset on the
// preceding rate_limit_event; both are read from frames.
func TestRateLimitEventAndErrorFrameMakeQuota(t *testing.T) {
	got := Project([][]byte{
		[]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1800000000}}`),
		[]byte(`{"type":"assistant","error":"rate_limit","message":{"content":[{"type":"text","text":"limit"}]}}`),
		[]byte(`{"type":"result","subtype":"success","is_error":true,"result":"You've hit your limit","api_error_status":429}`),
	}, false)
	o := got[2].Events[0].Outcome
	if o.Failure != oarlock.FailureQuota || o.ResetsAt == nil || o.ResetsAt.Unix() != 1800000000 || o.Status != 429 || o.Reason != "You've hit your limit" {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestAuthFailureFromFrames(t *testing.T) {
	got := Project([][]byte{
		[]byte(`{"type":"assistant","error":"authentication_failed","message":{"content":[{"type":"text","text":"Not logged in"}]}}`),
		[]byte(`{"type":"result","subtype":"success","is_error":true,"result":"Not logged in · Please run /login"}`),
	}, false)
	o := got[1].Events[0].Outcome
	if o.Failure != oarlock.FailureAuth || o.Credential != "missing" {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestContextOverflowFromFrames(t *testing.T) {
	got := Project([][]byte{[]byte(`{"type":"result","subtype":"success","is_error":true,"result":"Prompt is too long","terminal_reason":"prompt_too_long"}`)}, false)
	if o := got[0].Events[0].Outcome; o.Failure != oarlock.FailureInputTooLarge {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestErrorSubtypeWithoutResultTextUsesSubtypeAndErrors(t *testing.T) {
	got := Project([][]byte{[]byte(`{"type":"result","subtype":"error_max_turns","is_error":true,"errors":["too many","really"]}`)}, false)
	o := got[0].Events[0].Outcome
	if o.Reason != "error_max_turns: too many; really" || o.Failure != oarlock.FailureLimitReached {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestStructuredOutputRidesTheCompletedOutcome(t *testing.T) {
	got := Project([][]byte{[]byte(`{"type":"result","subtype":"success","is_error":false,"structured_output":{"a":1}}`)}, false)
	if o := got[0].Events[0].Outcome; o.Kind != oarlock.OutcomeCompleted || string(o.StructuredOutput) != `{"a":1}` {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestResultFailureDoesNotLeakIntoTheNextTurn(t *testing.T) {
	got := Project([][]byte{
		[]byte(`{"type":"assistant","error":"rate_limit","message":{"content":[]}}`),
		[]byte(`{"type":"result","subtype":"success","is_error":true,"result":"x"}`),
		[]byte(`{"type":"result","subtype":"success","is_error":true,"result":"y"}`),
	}, false)
	if got[1].Events[0].Outcome.Failure != oarlock.FailureRateLimited || got[2].Events[0].Outcome.Failure != oarlock.FailureUnknown {
		t.Fatalf("category leaked across turns: %+v / %+v", got[1].Events[0].Outcome, got[2].Events[0].Outcome)
	}
}
