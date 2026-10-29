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

// freshChatSession is a fresh Cursor chat (adapters/cursor): the chat id is not
// known when the prompt goes in, so the prompt request is recorded under the
// kernel's empty session id and every later frame is stamped with the id the
// runtime reported. A child session's frames carry their own id.
type freshChatSession struct {
	mu      sync.Mutex
	records []oarlock.Record
	// quiet suppresses the turn's frames until Abort, so the chat id is only
	// learned while the abort is already in flight.
	quiet bool
}

const freshChatID = "chat-1"

func (s *freshChatSession) append(sessionID string, agentPath []string, events ...oarlock.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, oarlock.Record{
		Seq:       uint64(len(s.records) + 1),
		Kind:      oarlock.KindFrame,
		SessionID: sessionID,
		AgentPath: agentPath,
		Events:    events,
	})
}

func (s *freshChatSession) Prompt(_ context.Context, input string, _ oarlock.PromptOptions) (oarlock.ControlOutcome, error) {
	s.mu.Lock()
	req := oarlock.Record{
		Seq:       uint64(len(s.records) + 1),
		Kind:      oarlock.KindRequest,
		SessionID: "", // a fresh chat: no chat id yet
		ID:        "r1",
		Direction: oarlock.ToRuntime,
		Request:   &oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: input},
	}
	s.records = append(s.records, req)
	quiet := s.quiet
	s.mu.Unlock()
	if !quiet {
		s.append(freshChatID, nil, oarlock.Event{Kind: oarlock.EventTurnActive})
		s.append(freshChatID, nil, oarlock.Event{Kind: oarlock.EventTextDelta, Text: "hello ", MessageID: "m"})
		// A child session's frame and a sub-agent's frame carry text the root
		// run must never see.
		s.append("child", nil, oarlock.Event{Kind: oarlock.EventTextDelta, Text: "child ", MessageID: "c"})
		s.append(freshChatID, []string{"sub"}, oarlock.Event{Kind: oarlock.EventTextDelta, Text: "sub ", MessageID: "s"})
		s.append(freshChatID, nil, oarlock.Event{Kind: oarlock.EventTextDelta, Text: "world", MessageID: "m"})
		s.append(freshChatID, nil, oarlock.Event{Kind: oarlock.EventTurnEnded, Outcome: &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted}})
	}
	return oarlock.ControlOutcome{Kind: oarlock.ControlAccepted, Seq: req.Seq, RequestID: "r1", Request: req}, nil
}

func (s *freshChatSession) Queue(context.Context, string, oarlock.InputOptions) (oarlock.ControlOutcome, error) {
	return oarlock.ControlOutcome{}, nil
}

// Abort ends the turn the way the runtime does: with its own frames, stamped
// with the chat id.
func (s *freshChatSession) Abort(context.Context) (oarlock.ControlOutcome, error) {
	s.append(freshChatID, nil, oarlock.Event{Kind: oarlock.EventTextDelta, Text: "late ", MessageID: "m"})
	s.append(freshChatID, nil, oarlock.Event{Kind: oarlock.EventTurnEnded, Outcome: &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted}})
	return oarlock.ControlOutcome{Kind: oarlock.ControlAccepted}, nil
}

func (s *freshChatSession) Dispose(context.Context) error { return nil }

func (s *freshChatSession) Records(c oarlock.Cursor) []oarlock.Record {
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

func (s *freshChatSession) Events(*oarlock.Cursor, func(oarlock.Record)) func() { return func() {} }
func (s *freshChatSession) Capabilities() oarlock.Capabilities                  { return oarlock.Capabilities{} }

func (s *freshChatSession) lastSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return uint64(len(s.records))
}

// TestPromptAndWait_FreshChatLearnsTheRootSessionID pins the bug the Cursor live
// run exposed: a fresh chat's prompt request has no session id, every later
// frame is stamped with the chat id learned from system/init, and filtering by
// the prompt-time root alone dropped the text and the runtime's turn_ended.
func TestPromptAndWait_FreshChatLearnsTheRootSessionID(t *testing.T) {
	s := &freshChatSession{}
	// The deadline only bounds a regression: the runtime ends the turn at once.
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi", oarlock.WithTimeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != oarlock.RunCompleted {
		t.Fatalf("run = %+v, want completed", run)
	}
	if run.Text != "hello world" {
		t.Fatalf("run.Text = %q, want the root agent's text only", run.Text)
	}
	if run.Turn == nil || run.Turn.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("run.Turn = %+v, want one completed turn", run.Turn)
	}
	// Seq still tracks the last record read, child frames included.
	if run.Seq != s.lastSeq() {
		t.Fatalf("run.Seq = %d, want the last frame (%d)", run.Seq, s.lastSeq())
	}
}

// TestPromptAndWait_FreshChatAbortUsesTheLearnedRoot: the abort wait reads the
// same root, so a runtime that ends the turn with the chat id while the abort
// is in flight still reports its own completion.
func TestPromptAndWait_FreshChatAbortUsesTheLearnedRoot(t *testing.T) {
	s := &freshChatSession{quiet: true}
	run, err := oarlock.PromptAndWait(context.Background(), s, "hi", oarlock.WithTimeout(30*time.Millisecond))
	if err != nil {
		t.Fatalf("a WaitOption deadline is not a caller error: %v", err)
	}
	if run.Outcome != oarlock.RunCompleted || run.Text != "late " {
		t.Fatalf("run = %+v, want the runtime's own completion after the abort", run)
	}
	if run.Turn == nil || run.Turn.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("run.Turn = %+v, want one completed turn", run.Turn)
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
