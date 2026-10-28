package voyage

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
)

// A log must never be marked complete when the recorder offered a record that
// did not make it in. Replay returning a dense prefix as a finished session
// would fold to a plausible but wrong Status/Usage, which is the one failure
// this format exists to prevent.

func lostRecord(seq uint64) oarlock.Record {
	return oarlock.Record{
		Seq: seq, Kind: oarlock.KindFrame, SessionID: "s-1",
		Native: json.RawMessage(`{"type":"x"}`),
	}
}

func lostOpen(t *testing.T) (*Writer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lost.jsonl")
	w, err := Create(path, vtHeader(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return w, path
}

// wantRefused ends w and requires: no end line, the lost-record reason, Replay
// refusing the file, and Read salvaging exactly wantRecords records.
func wantRefused(t *testing.T, w *Writer, path string, wantRecords int) {
	t.Helper()
	if w.Err() == nil {
		t.Fatal("Err() is nil although a record is missing from the log")
	}
	err := w.End("settled")
	if err == nil || !strings.Contains(err.Error(), "missing a record") {
		t.Fatalf("End = %v, want the missing-record reason", err)
	}
	if again := w.End("settled"); again != err {
		t.Fatalf("End is not idempotent: %v then %v", err, again)
	}
	recs, rerr := Replay(path)
	if !errors.Is(rerr, ErrTruncated) || recs != nil {
		t.Fatalf("Replay = %d records, %v; want nil and ErrTruncated", len(recs), rerr)
	}
	l, rerr := Read(path)
	if !errors.Is(rerr, ErrTruncated) || l == nil || l.Complete() || len(l.Records) != wantRecords || !vtDense(l.Records) {
		t.Fatalf("Read = %+v, %v; want a dense partial log of %d", l, rerr, wantRecords)
	}
}

func TestSkippedSeqKeepsTheLogFromBeingComplete(t *testing.T) {
	w, path := lostOpen(t)
	for _, seq := range []uint64{1, 2} {
		if err := w.Append(lostRecord(seq)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Append(lostRecord(4)); !errors.Is(err, ErrSeq) { // 3 was dropped upstream
		t.Fatalf("Append(4) = %v, want ErrSeq", err)
	}
	if err := w.Append(lostRecord(5)); !errors.Is(err, ErrSeq) { // the log stays dense: 5 is refused too
		t.Fatalf("Append(5) = %v, want ErrSeq", err)
	}
	wantRefused(t, w, path, 2)
}

func TestMarshalFailureKeepsTheLogFromBeingComplete(t *testing.T) {
	w, path := lostOpen(t)
	if err := w.Append(lostRecord(1)); err != nil {
		t.Fatal(err)
	}
	bad := lostRecord(2)
	bad.Native = json.RawMessage(`{not json`)
	if err := w.Append(bad); err == nil {
		t.Fatal("invalid Native was accepted")
	}
	wantRefused(t, w, path, 1)
}

func TestOversizeRecordKeepsTheLogFromBeingComplete(t *testing.T) {
	w, path := lostOpen(t)
	if err := w.Append(lostRecord(1)); err != nil {
		t.Fatal(err)
	}
	big := lostRecord(2)
	big.Native = json.RawMessage(`{"pad":"` + strings.Repeat("a", MaxLineBytes) + `"}`)
	err := w.Append(big)
	if err == nil || !strings.Contains(err.Error(), "line limit") {
		t.Fatalf("Append(oversize) = %v, want the line-limit error (the reader would reject it)", err)
	}
	wantRefused(t, w, path, 1)
}

// Making the missing record good lifts the hold: the writer was never broken,
// it was told about a record it could not take yet.
func TestAppendingTheMissingSeqLiftsTheHold(t *testing.T) {
	w, path := lostOpen(t)
	if err := w.Append(lostRecord(1)); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(lostRecord(3)); !errors.Is(err, ErrSeq) {
		t.Fatalf("Append(3) = %v, want ErrSeq", err)
	}
	if w.Err() == nil {
		t.Fatal("Err() is nil after a skipped record")
	}
	for _, seq := range []uint64{2, 3} {
		if err := w.Append(lostRecord(seq)); err != nil {
			t.Fatalf("Append(%d): %v", seq, err)
		}
	}
	if err := w.Err(); err != nil {
		t.Fatalf("Err() = %v after the missing records were appended", err)
	}
	if err := w.End("settled"); err != nil {
		t.Fatal(err)
	}
	recs, err := Replay(path)
	if err != nil || len(recs) != 3 || !vtDense(recs) {
		t.Fatalf("Replay = %d records, %v", len(recs), err)
	}
}

// A repeated Seq loses nothing: it is refused, and the log still closes
// complete.
func TestDuplicateSeqLosesNothing(t *testing.T) {
	w, path := lostOpen(t)
	steps := []struct {
		seq     uint64
		wantErr bool
	}{
		{1, false}, {2, false}, {2, true}, {1, true}, {3, false}, {3, true},
	}
	for _, s := range steps {
		err := w.Append(lostRecord(s.seq))
		if s.wantErr != errors.Is(err, ErrSeq) || (!s.wantErr && err != nil) {
			t.Fatalf("Append(%d) = %v, wantErr=%v", s.seq, err, s.wantErr)
		}
		if err := w.Err(); err != nil {
			t.Fatalf("after Append(%d): a repeated Seq must lose nothing, Err() = %v", s.seq, err)
		}
	}
	if err := w.End("settled"); err != nil {
		t.Fatal(err)
	}
	recs, err := Replay(path)
	if err != nil || len(recs) != 3 || !vtDense(recs) {
		t.Fatalf("Replay = %d records, %v", len(recs), err)
	}
}

// What the writer accepts, the reader must accept: a line at exactly
// MaxLineBytes is the largest the format carries.
func TestLargestAcceptedRecordReadsBack(t *testing.T) {
	w, path := lostOpen(t)
	rec := lostRecord(1)
	const wrap = len(`{"kind":"record","record":`) + len(`}`)
	probe := rec
	probe.Native = json.RawMessage(`{"p":""}`)
	base, err := marshal(probe)
	if err != nil {
		t.Fatal(err)
	}
	room := MaxLineBytes - wrap - len(base)
	rec.Native = json.RawMessage(`{"p":"` + strings.Repeat("a", room) + `"}`)
	if err := w.Append(rec); err != nil {
		t.Fatalf("a line of exactly MaxLineBytes was refused: %v", err)
	}
	if err := w.End("settled"); err != nil {
		t.Fatal(err)
	}
	if recs, err := Replay(path); err != nil || len(recs) != 1 {
		t.Fatalf("writer accepted what the reader rejects: %d records, %v", len(recs), err)
	}
}
