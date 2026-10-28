package kernel

import (
	"errors"
	"reflect"
	"testing"

	"github.com/zzjcool/oarlock"
)

// TestReplay_ReplaysOnlyRecordsAfterAfterSeq pins the suffix semantics of the
// cursor and that the returned slice is fresh.
func TestReplay_ReplaysOnlyRecordsAfterAfterSeq(t *testing.T) {
	k := newKernel("sess-1", nil)
	for index := 0; index < 5; index++ {
		k.Frame(frameBody("f"), RecordAt{})
	}

	records, err := k.Replay(Cursor{SessionID: "sess-1", AfterSeq: 2})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := seqsOf(records); !reflect.DeepEqual(got, []uint64{3, 4, 5}) {
		t.Fatalf("replay after 2 gave %v, want the records with Seq > 2", got)
	}

	// An empty SessionID is the zero-value "this instance".
	records, err = k.Replay(Cursor{AfterSeq: 4})
	if err != nil {
		t.Fatalf("replay after 4: %v", err)
	}
	if got := seqsOf(records); !reflect.DeepEqual(got, []uint64{5}) {
		t.Fatalf("replay after 4 gave %v, want [5]", got)
	}

	// Past the end replays nothing, and that is not an error.
	records, err = k.Replay(Cursor{AfterSeq: 99})
	if err != nil {
		t.Fatalf("replay past the end: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("replay past the end gave %d records, want 0", len(records))
	}

	// The slice is the caller's: shortening it must not touch the log.
	whole, err := k.Replay(Cursor{})
	if err != nil {
		t.Fatalf("replay everything: %v", err)
	}
	whole = whole[:0]
	again, err := k.Replay(Cursor{})
	if err != nil {
		t.Fatalf("replay everything again: %v", err)
	}
	if len(again) != 5 {
		t.Fatalf("the log shrank to %d records through a returned slice", len(again))
	}
}

// TestRecords_ZeroCursorReplaysTheWholeInstance pins that AfterSeq 0 means "the
// start of the stream", because the first record has Seq 1.
func TestRecords_ZeroCursorReplaysTheWholeInstance(t *testing.T) {
	k := newKernel("sess-1", nil)
	if records := k.Records(Cursor{}); len(records) != 0 {
		t.Fatalf("a fresh instance replayed %d records, want 0", len(records))
	}

	for index := 0; index < 3; index++ {
		k.Frame(frameBody("f"), RecordAt{})
	}

	records := k.Records(Cursor{})
	if got := seqsOf(records); !reflect.DeepEqual(got, []uint64{1, 2, 3}) {
		t.Fatalf("the zero cursor replayed %v, want the whole instance", got)
	}
	// The same records the live tap saw, in the same order.
	if records[0].Kind != oarlock.KindFrame || len(records[0].Events) != 0 {
		t.Fatalf("replay changed the record shape: %+v", records[0])
	}
}

// TestReplay_RejectsCursorFromAnotherSession pins that a cursor naming another
// instance is never applied and that the error is inspectable with errors.Is.
func TestReplay_RejectsCursorFromAnotherSession(t *testing.T) {
	k := newKernel("sess-1", nil)
	k.Frame(frameBody("f"), RecordAt{})

	for _, testCase := range []struct {
		name   string
		cursor Cursor
	}{
		{name: "another session id", cursor: Cursor{SessionID: "sess-2"}},
		{name: "another session id at a position", cursor: Cursor{SessionID: "sess-2", AfterSeq: 1}},
	} {
		records, err := k.Replay(testCase.cursor)
		if err == nil {
			t.Fatalf("%s: replay applied the cursor and returned %d records", testCase.name, len(records))
		}
		if !errors.Is(err, ErrCursorMismatch) {
			t.Fatalf("%s: error %v does not wrap ErrCursorMismatch", testCase.name, err)
		}
		if records != nil {
			t.Fatalf("%s: a rejected replay must return no records, got %d", testCase.name, len(records))
		}
		if records := k.Records(testCase.cursor); records != nil {
			t.Fatalf("%s: Records applied a mismatched cursor: %d records", testCase.name, len(records))
		}
	}

	// The empty SessionID is the zero-value "this instance" and is accepted.
	records, err := k.Replay(Cursor{})
	if err != nil {
		t.Fatalf("the zero cursor must be accepted by this instance: %v", err)
	}
	if got := seqsOf(records); !reflect.DeepEqual(got, []uint64{1}) {
		t.Fatalf("the zero cursor replayed %v, want [1]", got)
	}
}

// TestRecords_MismatchedCursorIsNotSilentlyApplied pins the documented nil:
// Records has no error, so the caller that cares uses Replay.
func TestRecords_MismatchedCursorIsNotSilentlyApplied(t *testing.T) {
	k := newKernel("sess-1", nil)
	k.Frame(frameBody("f"), RecordAt{})
	k.Frame(frameBody("f"), RecordAt{})

	if records := k.Records(Cursor{SessionID: "sess-2", AfterSeq: 0}); records != nil {
		t.Fatalf("a foreign cursor was silently applied: %d records", len(records))
	}
	if records := k.Records(Cursor{SessionID: "sess-1", AfterSeq: 0}); len(records) != 2 {
		t.Fatalf("this instance's own cursor replayed %d records, want 2", len(records))
	}
	// The zero cursor is this instance from the start, not a mismatch.
	if records := k.Records(Cursor{}); len(records) != 2 {
		t.Fatalf("the zero cursor replayed %d records, want 2", len(records))
	}
}

// TestObserveFrom_RejectsMismatchedCursorBeforeRegistering pins that a
// mismatched cursor is refused before an observer exists, so nothing is
// replayed and nothing leaks into the registry. The zero cursor is not a
// mismatch: it replays this instance from the start.
func TestObserveFrom_RejectsMismatchedCursorBeforeRegistering(t *testing.T) {
	k := newKernel("sess-1", nil)
	for index := 0; index < 2; index++ {
		k.Frame(frameBody("f"), RecordAt{})
	}

	called := false
	cancel, err := k.ObserveFrom(Cursor{SessionID: "sess-2"}, func(Record) { called = true })
	if cancel != nil {
		t.Fatal("a mismatched cursor was refused but returned a cancel")
	}
	if !errors.Is(err, ErrCursorMismatch) {
		t.Fatalf("error %v does not wrap ErrCursorMismatch", err)
	}
	k.mu.Lock()
	registered := len(k.observers)
	k.mu.Unlock()
	if registered != 0 {
		t.Fatalf("a refused ObserveFrom registered %d observers", registered)
	}
	k.Frame(frameBody("f"), RecordAt{})
	if called {
		t.Fatal("a refused ObserveFrom still delivered records")
	}

	// The zero cursor registers and replays from the start, then continues live.
	var seen []uint64
	cancel, err = k.ObserveFrom(Cursor{}, func(record Record) {
		seen = append(seen, record.Seq)
	})
	if err != nil {
		t.Fatalf("the zero cursor must be accepted: %v", err)
	}
	defer cancel()
	if !reflect.DeepEqual(seen, []uint64{1, 2, 3}) {
		t.Fatalf("the zero cursor replayed %v, want every retained record once", seen)
	}
	k.Frame(frameBody("f"), RecordAt{})
	if !reflect.DeepEqual(seen, []uint64{1, 2, 3, 4}) {
		t.Fatalf("live delivery gave %v, want the seam to continue at [1 2 3 4]", seen)
	}
}

// TestObserveFrom_ReplayThenLiveHasNoGapAndNoDuplicate pins the atomic
// replay→live seam: the replay is complete before the call returns, then live
// records continue with no duplicate.
func TestObserveFrom_ReplayThenLiveHasNoGapAndNoDuplicate(t *testing.T) {
	k := newKernel("sess-1", nil)
	for index := 0; index < 3; index++ {
		k.Frame(frameBody("f"), RecordAt{})
	}

	var seen []uint64
	cancel, err := k.ObserveFrom(Cursor{SessionID: "sess-1", AfterSeq: 1}, func(record Record) {
		seen = append(seen, record.Seq)
	})
	if err != nil {
		t.Fatalf("ObserveFrom: %v", err)
	}
	if !reflect.DeepEqual(seen, []uint64{2, 3}) {
		t.Fatalf("replay delivered %v before returning, want [2 3]", seen)
	}

	k.Frame(frameBody("f"), RecordAt{})
	if !reflect.DeepEqual(seen, []uint64{2, 3, 4}) {
		t.Fatalf("live delivery gave %v, want the seam to continue at [2 3 4]", seen)
	}

	cancel()
	cancel() // idempotent
	k.Frame(frameBody("f"), RecordAt{})
	if !reflect.DeepEqual(seen, []uint64{2, 3, 4}) {
		t.Fatalf("canceled ObserveFrom still delivered: %v", seen)
	}

	// Replaying from the zero cursor reaches every record once, in order.
	full := newKernel("sess-1", nil)
	for index := 0; index < 3; index++ {
		full.Frame(frameBody("f"), RecordAt{})
	}
	var everything []Record
	wholeCancel, err := full.ObserveFrom(Cursor{}, func(record Record) {
		everything = append(everything, record)
	})
	if err != nil {
		t.Fatalf("ObserveFrom from the zero cursor: %v", err)
	}
	defer wholeCancel()
	if got := seqsOf(everything); !reflect.DeepEqual(got, []uint64{1, 2, 3}) {
		t.Fatalf("the zero cursor replayed %v, want every record once", got)
	}
}

// TestObserveFrom_DeliversARecordQueuedDuringReplayExactlyOnce is the white-box
// seam test: a record that is both in the replay snapshot and still queued for
// delivery must reach the observer once. It is the reason deliver skips records
// at or before the observer's after position.
func TestObserveFrom_DeliversARecordQueuedDuringReplayExactlyOnce(t *testing.T) {
	k := newKernel("sess-1", nil)
	for index := 0; index < 3; index++ {
		k.Frame(frameBody("f"), RecordAt{})
	}

	// Simulate an append whose notification is still pending: Seq 3 is retained
	// and queued, and the replay below will also see it.
	k.mu.Lock()
	k.notified = append(k.notified, k.log[2])
	k.mu.Unlock()

	var received []uint64
	cancel, err := k.ObserveFrom(Cursor{AfterSeq: 2}, func(record Record) {
		received = append(received, record.Seq)
	})
	if err != nil {
		t.Fatalf("ObserveFrom: %v", err)
	}
	defer cancel()

	if !reflect.DeepEqual(received, []uint64{3}) {
		t.Fatalf("the queued record was delivered %v, want exactly [3]", received)
	}

	// The pending notification was consumed by the drain, not left behind.
	k.mu.Lock()
	queued := len(k.notified)
	k.mu.Unlock()
	if queued != 0 {
		t.Fatalf("%d notifications were left queued after the drain", queued)
	}
}
