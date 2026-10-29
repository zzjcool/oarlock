package oarlock

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// waitSession is the minimal session of the internal tests: one retained
// record stream, one set of live observers, and the ONE mutex that guards both,
// so Events replays and registers in the same critical section like the
// kernel's ObserveFrom. The callback is invoked under that mutex, which is
// exactly how the kernel dispatches inline on the appending goroutine: the
// watcher's tap must never block here.
type waitSession struct {
	mu      sync.Mutex
	records []Record
	subs    []func(Record)
	aborts  int

	// active, deltas and turnEnd shape what Prompt retains, all before Events is
	// called: the replay burst the tap has to survive.
	active  bool
	deltas  int
	turnEnd bool

	// beforeRecords, when set, runs once at the start of the next Records call,
	// before the snapshot is taken. It lets a test append a frame while the
	// watcher is reading: the record then lands in both the snapshot and the tap,
	// which is the overlap the read position's dedup exists for.
	beforeRecords func()
}

func (s *waitSession) emitLocked(rec Record) {
	rec.Seq = uint64(len(s.records) + 1)
	s.records = append(s.records, rec)
	for _, fn := range s.subs {
		if fn != nil {
			fn(rec)
		}
	}
}

func (s *waitSession) emit(rec Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emitLocked(rec)
}

func (s *waitSession) Prompt(_ context.Context, input string, _ PromptOptions) (ControlOutcome, error) {
	s.mu.Lock()
	req := Record{
		// emitLocked stamps its own copy, so the caller's request carries the
		// same position.
		Seq:       uint64(len(s.records) + 1),
		Kind:      KindRequest,
		SessionID: "s",
		ID:        "r1",
		Direction: ToRuntime,
		Request:   &RequestBody{Action: ActionPrompt, Input: input},
	}
	s.emitLocked(req)
	s.mu.Unlock()
	if s.active {
		s.emit(Record{Kind: KindFrame, SessionID: "s", Events: []Event{{Kind: EventTurnActive}}})
	}
	for i := 0; i < s.deltas; i++ {
		s.emit(Record{Kind: KindFrame, SessionID: "s", Events: []Event{{Kind: EventTextDelta, Text: fmt.Sprintf("d%d", i), MessageID: "m"}}})
	}
	if s.turnEnd {
		s.emit(Record{Kind: KindFrame, SessionID: "s", Events: []Event{{Kind: EventTurnEnded, Outcome: &TurnOutcome{Kind: OutcomeCompleted}}}})
	}
	return ControlOutcome{Kind: ControlAccepted, Seq: req.Seq, RequestID: "r1", Request: req}, nil
}

func (s *waitSession) Queue(context.Context, string, InputOptions) (ControlOutcome, error) {
	return ControlOutcome{}, nil
}

// Abort accepts and appends nothing: the runtime that never reports its own end.
func (s *waitSession) Abort(context.Context) (ControlOutcome, error) {
	s.aborts++
	return ControlOutcome{Kind: ControlAccepted}, nil
}

func (s *waitSession) Dispose(context.Context) error { return nil }

func (s *waitSession) Records(c Cursor) []Record {
	if hook := s.beforeRecords; hook != nil {
		s.beforeRecords = nil // one shot: the re-entrancy is exercised once
		hook()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for _, rec := range s.records {
		if rec.Seq > c.AfterSeq && (c.SessionID == "" || c.SessionID == rec.SessionID) {
			out = append(out, rec)
		}
	}
	return out
}

// lastSeq is the highest retained Seq: what Seq tracks once every record is
// read.
func (s *waitSession) lastSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return uint64(len(s.records))
}

func (s *waitSession) Events(cursor *Cursor, fn func(Record)) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cursor != nil {
		for _, rec := range s.records {
			if rec.Seq > cursor.AfterSeq && (cursor.SessionID == "" || cursor.SessionID == rec.SessionID) {
				fn(rec)
			}
		}
	}
	s.subs = append(s.subs, fn)
	id := len(s.subs) - 1
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.subs[id] = nil
	}
}

func (s *waitSession) Capabilities() Capabilities { return Capabilities{} }

// emitLater retains a frame after d, from its own goroutine: the late record a
// wait that already started still has to see.
func emitLater(s *waitSession, d time.Duration, events ...Event) {
	go func() {
		time.Sleep(d)
		s.emit(Record{Kind: KindFrame, SessionID: "s", Events: events})
	}()
}

// TestPromptAndWait_OverflowingTapKeepsEveryDeltaOnce forces the tap's drop
// path: a one-record buffer and a replay burst bigger than it. The catch-up
// Records read has to restore the dropped deltas exactly once, in order, and the
// turn end still has to be seen.
func TestPromptAndWait_OverflowingTapKeepsEveryDeltaOnce(t *testing.T) {
	prev := eventTapBuffer
	eventTapBuffer = 1
	defer func() { eventTapBuffer = prev }()

	const deltas = 12 // far more than the one-record tap
	s := &waitSession{deltas: deltas, turnEnd: true}
	started := time.Now()
	run, err := PromptAndWait(context.Background(), s, "hi", WithTimeout(5*time.Second))
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	var want string
	for i := 0; i < deltas; i++ {
		want += fmt.Sprintf("d%d", i)
	}
	if run.Text != want {
		t.Fatalf("run.Text = %q, want %q (no lost, duplicated or reordered delta)", run.Text, want)
	}
	if run.Outcome != RunCompleted || run.Turn == nil || run.Turn.Kind != OutcomeCompleted {
		t.Fatalf("run = %+v, want the runtime's own completion", run)
	}
	if last := uint64(deltas + 2); run.Seq != last {
		t.Fatalf("run.Seq = %d, want the last record (%d)", run.Seq, last)
	}
	// The drop path's own catch-up read has to resolve it: the safety sweep is
	// the fallback, not the way an overflowing tap is recovered.
	if elapsed >= waitSweep {
		t.Fatalf("wait took %v: want the catch-up read, not the %v safety sweep", elapsed, waitSweep)
	}
}

// TestPromptAndWait_CatchUpAndTapOverlapIsDeduped puts the same record in the
// tap and in the catch-up Records snapshot. A one-record tap forces the drop
// path, and a re-entrancy hook emits one more frame while that catch-up read is
// in flight, so the snapshot carries it and so does the tap. The read position
// must apply it exactly once: every text delta once and in order, the turn end
// still seen, and Seq the last record read.
func TestPromptAndWait_CatchUpAndTapOverlapIsDeduped(t *testing.T) {
	prev := eventTapBuffer
	eventTapBuffer = 1 // a package-level seam: this test must never be parallel
	defer func() { eventTapBuffer = prev }()

	s := &waitSession{deltas: 3} // d0 d1 d2: more than the one-record tap
	hooked := false
	s.beforeRecords = func() {
		hooked = true
		// Appended before the snapshot is taken: the record lands in the snapshot
		// below and, through the live observer, in the tap as well.
		s.emit(Record{Kind: KindFrame, SessionID: "s", Events: []Event{{Kind: EventTextDelta, Text: "x", MessageID: "m"}}})
	}
	// The turn ends only after the overlap was applied, so the wait cannot decide
	// before the duplicate has been offered to consume.
	emitLater(s, 20*time.Millisecond, Event{Kind: EventTurnEnded, Outcome: &TurnOutcome{Kind: OutcomeCompleted}})

	run, err := PromptAndWait(context.Background(), s, "hi", WithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !hooked {
		t.Fatal("the catch-up Records read never happened: the overlap was not exercised")
	}
	if want := "d0d1d2x"; run.Text != want {
		t.Fatalf("run.Text = %q, want %q (every delta exactly once, in order)", run.Text, want)
	}
	if run.Outcome != RunCompleted || run.Turn == nil || run.Turn.Kind != OutcomeCompleted {
		t.Fatalf("run = %+v, want the runtime's own completion", run)
	}
	if last := s.lastSeq(); run.Seq != last {
		t.Fatalf("run.Seq = %d, want the last record read (%d)", run.Seq, last)
	}
}

// TestPromptAndWait_AbortGraceExpiryReportsInterrupted: a runtime that accepts
// the abort but never reports its own end leaves PromptAndWait to report its own
// interruption once the bounded grace expires.
func TestPromptAndWait_AbortGraceExpiryReportsInterrupted(t *testing.T) {
	prev := abortWaitBound
	abortWaitBound = 20 * time.Millisecond
	defer func() { abortWaitBound = prev }()

	s := &waitSession{active: true} // a turn that never ends by itself
	started := time.Now()
	run, err := PromptAndWait(context.Background(), s, "hi", WithTimeout(20*time.Millisecond))
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("a WaitOption deadline is not a caller error: %v", err)
	}
	if run.Outcome != RunInterrupted {
		t.Fatalf("run = %+v, want interrupted", run)
	}
	if run.Turn == nil || run.Turn.Kind != OutcomeAborted {
		t.Fatalf("run.Turn = %+v, want the summary's aborted turn", run.Turn)
	}
	if s.aborts != 1 {
		t.Fatalf("Abort called %d times, want 1", s.aborts)
	}
	// The stream keeps exactly what the runtime said: no turn end was invented.
	for _, rec := range s.Records(Cursor{}) {
		for _, ev := range rec.Events {
			if ev.Kind == EventTurnEnded {
				t.Fatalf("PromptAndWait appended a turn_ended: %+v", rec)
			}
		}
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("wait took %v, want the shrunk grace", elapsed)
	}
}
