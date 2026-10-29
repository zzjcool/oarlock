package oarlock_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
)

type fakeWaitSession struct {
	mu      sync.Mutex
	records []oarlock.Record
	handled bool
	reject  *oarlock.ControlOutcome
	timeout bool
}

func (s *fakeWaitSession) append(rec oarlock.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec.Seq = uint64(len(s.records) + 1)
	rec.Kind = oarlock.KindFrame
	rec.SessionID = "s"
	s.records = append(s.records, rec)
}

func (s *fakeWaitSession) Prompt(_ context.Context, input string, _ oarlock.PromptOptions) (oarlock.ControlOutcome, error) {
	if s.reject != nil {
		return *s.reject, nil
	}
	s.mu.Lock()
	req := oarlock.Record{
		Seq:       uint64(len(s.records) + 1),
		Kind:      oarlock.KindRequest,
		SessionID: "s",
		ID:        "r1",
		Direction: oarlock.ToRuntime,
		Request:   &oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: input},
	}
	s.records = append(s.records, req)
	s.mu.Unlock()
	switch {
	case s.handled:
		s.append(oarlock.Record{Events: []oarlock.Event{{Kind: oarlock.EventInputHandled, RequestID: "r1"}}})
	case s.timeout:
		s.append(oarlock.Record{Events: []oarlock.Event{{Kind: oarlock.EventTurnActive}}})
	default:
		s.append(oarlock.Record{Events: []oarlock.Event{{Kind: oarlock.EventTextDelta, Text: "hello ", MessageID: "m"}}})
		s.append(oarlock.Record{Events: []oarlock.Event{{Kind: oarlock.EventTextDelta, Text: "world", MessageID: "m"}}})
		s.append(oarlock.Record{Events: []oarlock.Event{{Kind: oarlock.EventTurnEnded, Outcome: &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted}}}})
	}
	return oarlock.ControlOutcome{Kind: oarlock.ControlAccepted, Seq: req.Seq, RequestID: "r1", Request: req}, nil
}

func (s *fakeWaitSession) Queue(context.Context, string, oarlock.InputOptions) (oarlock.ControlOutcome, error) {
	return oarlock.ControlOutcome{}, nil
}

func (s *fakeWaitSession) Abort(context.Context) (oarlock.ControlOutcome, error) {
	s.append(oarlock.Record{Events: []oarlock.Event{{Kind: oarlock.EventTurnEnded, Outcome: &oarlock.TurnOutcome{Kind: oarlock.OutcomeAborted}}}})
	return oarlock.ControlOutcome{Kind: oarlock.ControlAccepted}, nil
}

func (s *fakeWaitSession) Dispose(context.Context) error { return nil }

func (s *fakeWaitSession) Records(c oarlock.Cursor) []oarlock.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []oarlock.Record
	for _, rec := range s.records {
		if rec.Seq > c.AfterSeq && (c.SessionID == "" || c.SessionID == rec.SessionID) {
			out = append(out, rec)
		}
	}
	return out
}

func (s *fakeWaitSession) Events(*oarlock.Cursor, func(oarlock.Record)) func() { return func() {} }
func (s *fakeWaitSession) Capabilities() oarlock.Capabilities                  { return oarlock.Capabilities{} }

func TestPromptAndWait_CompletedCollectsTextAndTheRuntimeOutcome(t *testing.T) {
	s := &fakeWaitSession{}
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != oarlock.RunCompleted || run.Text != "hello world" || run.Turn == nil || run.Turn.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("run = %+v", run)
	}
	if run.Seq != 4 {
		t.Fatalf("seq = %d, want the last frame (4)", run.Seq)
	}
}

func TestPromptAndWait_RejectedKeepsTheBranchWord(t *testing.T) {
	s := &fakeWaitSession{reject: &oarlock.ControlOutcome{Kind: oarlock.ControlRejected, Code: oarlock.Busy, Reason: "busy", Seq: 1}}
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != oarlock.RunRejected || run.Code != oarlock.Busy || run.Reason != "busy" {
		t.Fatalf("run = %+v", run)
	}
}

func TestPromptAndWait_HandledDoesNotInventATurn(t *testing.T) {
	s := &fakeWaitSession{handled: true}
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != oarlock.RunCompleted || run.Turn != nil {
		t.Fatalf("run = %+v, want completed with no turn", run)
	}
}

func TestPromptAndWait_TimeoutAbortsAndReportsInterrupted(t *testing.T) {
	s := &fakeWaitSession{timeout: true}
	started := time.Now()
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi", oarlock.WithTimeout(30*time.Millisecond))
	if err != nil {
		t.Fatalf("a WaitOption deadline is not a caller error: %v", err)
	}
	if run.Outcome != oarlock.RunInterrupted || run.Turn == nil || run.Turn.Kind != oarlock.OutcomeAborted {
		t.Fatalf("run = %+v", run)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatalf("timeout took %v", time.Since(started))
	}
}
