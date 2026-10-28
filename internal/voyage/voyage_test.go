package voyage

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/redact"
)

// vtSecret is a known session credential handed to the redactor.
// vtBearer is assembled at run time so no scannable token literal sits in the
// source.
var vtBearer = "tok" + "-" + strings.Repeat("b", 16)

const vtSecret = "sk-test-0123456789abcdef"

func vtAt() time.Time { return time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC) }

func vtHeader() Header {
	return Header{
		Runtime:   "pi",
		Model:     "model-x",
		Cwd:       ".",
		SessionID: "s-1",
		StartedAt: 1700000000000,
		Recorder:  "voyage_test",
	}
}

// vtRecords is a small dense stream with a frame (events, native), a request,
// a frame with a tool result, and a response with an exit code.
func vtRecords() []oarlock.Record {
	base := time.Date(2026, 1, 2, 3, 4, 5, 123000000, time.UTC)
	code := 7
	reasoning := int64(3)
	return []oarlock.Record{
		{
			Seq: 1, ReceivedAt: base, Kind: oarlock.KindFrame, SessionID: "s-1",
			Native: json.RawMessage(`{"type":"agent_start","n":1.50,"big":12345678901234567890}`),
			Events: []oarlock.Event{{Kind: oarlock.EventTurnActive}},
		},
		{
			Seq: 2, ReceivedAt: base.Add(time.Millisecond), Kind: oarlock.KindRequest, SessionID: "s-1",
			ID: "r1", Direction: oarlock.ToRuntime,
			Request: &oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "hello <&> wörld"},
		},
		{
			Seq: 3, ReceivedAt: base.Add(2 * time.Millisecond), Kind: oarlock.KindFrame, SessionID: "s-1",
			AgentPath: []string{"root", "child"}, SpanID: "span-1",
			Native: json.RawMessage(`{"type":"message_update","delta":"hi"}`),
			Events: []oarlock.Event{
				{Kind: oarlock.EventTextDelta, Text: "hi"},
				{Kind: oarlock.EventToolCallEnded, CallID: "c1", Tool: "bash", Result: oarlock.ToolOK,
					ExitCode: &code, ExitCodeReported: true,
					Content: []oarlock.ToolOutputPart{{Type: "text", Text: "ok"}}},
				{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{
					Tokens: &oarlock.TokenTotals{Input: 10, Output: 5, Reasoning: &reasoning}}},
				{Kind: oarlock.EventTurnEnded, Outcome: &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted}},
			},
		},
		{
			Seq: 4, ReceivedAt: base.Add(3 * time.Millisecond), Kind: oarlock.KindResponse, SessionID: "s-1",
			RequestID: "r1",
			Response: &oarlock.ResponseBody{
				Kind: oarlock.ResponseExited, ExitCode: &code, ExitCodeReported: true,
				Reason: "exited", Native: json.RawMessage(`{"signal":null}`),
			},
		},
	}
}

func vtWrite(t *testing.T, path string, h Header, recs []oarlock.Record, red *redact.Redactor) {
	t.Helper()
	w, err := Create(path, h, red)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, r := range recs {
		if err := w.Append(r); err != nil {
			t.Fatalf("Append seq %d: %v", r.Seq, err)
		}
	}
	if err := w.EndAt("done", vtAt()); err != nil {
		t.Fatalf("EndAt: %v", err)
	}
}

// vtValidFile writes the standard valid log and returns its bytes.
func vtValidFile(t *testing.T) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "valid.jsonl")
	vtWrite(t, p, vtHeader(), vtRecords(), nil)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func vtCanon(t *testing.T, recs []oarlock.Record) string {
	t.Helper()
	if recs == nil {
		recs = []oarlock.Record{}
	}
	b, err := json.Marshal(recs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func vtDense(recs []oarlock.Record) bool {
	for i, r := range recs {
		if r.Seq != uint64(i+1) {
			return false
		}
	}
	return true
}

func vtReadFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// (1) byte-level layout + round trip.
func TestWriterLayoutHeaderRecordsEnd(t *testing.T) {
	p := filepath.Join(t.TempDir(), "v.jsonl")
	recs := vtRecords()
	w, err := Create(p, vtHeader(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if w.Path() != p {
		t.Fatalf("Path = %q, want %q", w.Path(), p)
	}
	for _, r := range recs {
		if err := w.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	if w.Count() != uint64(len(recs)) || w.Err() != nil {
		t.Fatalf("Count=%d Err=%v", w.Count(), w.Err())
	}
	if err := w.EndAt("settled", vtAt()); err != nil {
		t.Fatal(err)
	}

	raw := vtReadFile(t, p)
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatal("file does not end in LF")
	}
	if bytes.Contains(raw, []byte("\r")) {
		t.Fatal("file contains CR")
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != len(recs)+2 {
		t.Fatalf("got %d lines, want %d", len(lines), len(recs)+2)
	}
	for i, ln := range lines {
		if ln == "" {
			t.Fatalf("line %d is blank", i+1)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("line %d is not a JSON object: %v", i+1, err)
		}
		var kind string
		_ = json.Unmarshal(m["kind"], &kind)
		want := "record"
		switch i {
		case 0:
			want = "header"
		case len(lines) - 1:
			want = "end"
		}
		if kind != want {
			t.Fatalf("line %d kind = %q, want %q", i+1, kind, want)
		}
	}

	var hdr map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &hdr); err != nil {
		t.Fatal(err)
	}
	if hdr["format"] != Format || hdr["dialect"] != Dialect || Format != "oar-voyage/3" || Dialect != "oarlock" {
		t.Fatalf("header format/dialect wrong: %v", hdr)
	}
	for k, want := range map[string]any{"runtime": "pi", "model": "model-x", "cwd": ".", "sessionId": "s-1", "recorder": "voyage_test"} {
		if hdr[k] != want {
			t.Fatalf("header[%s] = %v, want %v", k, hdr[k], want)
		}
	}
	if hdr["startedAt"] != float64(1700000000000) {
		t.Fatalf("startedAt = %v", hdr["startedAt"])
	}
	for _, k := range []string{"effort", "serviceTier"} {
		if _, ok := hdr[k]; ok {
			t.Fatalf("empty %s must be omitted: %v", k, hdr)
		}
	}
	var end map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &end); err != nil {
		t.Fatal(err)
	}
	if end["reason"] != "settled" || end["at"] != float64(vtAt().UnixMilli()) {
		t.Fatalf("end line wrong: %v", end)
	}

	l, err := Read(p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !l.Complete() || l.End == nil || l.End.Reason != "settled" || l.End.At != vtAt().UnixMilli() {
		t.Fatalf("log not complete: %+v", l.End)
	}
	if l.Header != vtHeader() {
		t.Fatalf("header round trip: %+v", l.Header)
	}
	if len(l.Records) != len(recs) {
		t.Fatalf("got %d records", len(l.Records))
	}
	if got, want := vtCanon(t, l.Records), vtCanon(t, recs); got != want {
		t.Fatalf("round trip differs:\n got %s\nwant %s", got, want)
	}
	for i := range recs {
		if !l.Records[i].ReceivedAt.Equal(recs[i].ReceivedAt) {
			t.Fatalf("record %d time %v != %v", i, l.Records[i].ReceivedAt, recs[i].ReceivedAt)
		}
	}
	// Replay agrees with Read.
	rr, err := Replay(p)
	if err != nil || vtCanon(t, rr) != vtCanon(t, recs) {
		t.Fatalf("Replay: %v", err)
	}
	// Spot-check typed fields survived (not just the canonical string).
	ev := l.Records[2].Events[1]
	if code, ok := ev.ReportedExitCode(); !ok || code != 7 || ev.Result != oarlock.ToolOK {
		t.Fatalf("tool event lost fields: %+v", ev)
	}
	if got := l.Records[3].Response; got == nil || got.ExitCode == nil || *got.ExitCode != 7 || !got.ExitCodeReported {
		t.Fatalf("response lost exit code: %+v", got)
	}
	if string(l.Records[0].Native) != `{"type":"agent_start","n":1.50,"big":12345678901234567890}` {
		t.Fatalf("native not verbatim: %s", l.Records[0].Native)
	}
}

// (2) Create never clobbers.
func TestCreateRefusesToClobber(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "exists.jsonl")
	orig := []byte("precious evidence\n")
	if err := os.WriteFile(p, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := Create(p, vtHeader(), nil)
	if err == nil {
		_ = w.Close()
		t.Fatal("Create over an existing file succeeded")
	}
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("error %v does not wrap fs.ErrExist", err)
	}
	if got := vtReadFile(t, p); !bytes.Equal(got, orig) {
		t.Fatalf("existing file was modified: %q", got)
	}
	// A second Create over a finished recording also refuses.
	q := filepath.Join(dir, "second.jsonl")
	vtWrite(t, q, vtHeader(), vtRecords(), nil)
	before := vtReadFile(t, q)
	if _, err := Create(q, vtHeader(), nil); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second Create: %v", err)
	}
	if !bytes.Equal(before, vtReadFile(t, q)) {
		t.Fatal("finished recording was modified")
	}
	// A missing parent directory is an error, not a panic.
	if _, err := Create(filepath.Join(dir, "no", "such", "dir", "x.jsonl"), vtHeader(), nil); err == nil {
		t.Fatal("Create in a missing directory succeeded")
	}
}

// (3) Seq discipline: refused, nothing written, writer stays usable.
func TestSeqDiscipline(t *testing.T) {
	p := filepath.Join(t.TempDir(), "seq.jsonl")
	w, err := Create(p, vtHeader(), nil)
	if err != nil {
		t.Fatal(err)
	}
	recs := vtRecords()
	at := func(seq uint64) oarlock.Record {
		r := recs[0]
		r.Seq = seq
		return r
	}
	steps := []struct {
		name    string
		seq     uint64
		wantErr bool
	}{
		{"first is not 1", 2, true},
		{"first is zero", 0, true},
		{"first is 1", 1, false},
		{"duplicate", 1, true},
		{"gap", 3, true},
		{"far gap", 1 << 40, true},
		{"zero after start", 0, true},
		{"second is 2", 2, false},
		{"rewind", 1, true},
		{"third is 3", 3, false},
	}
	// want is how many records are in the log (the next Seq is want+1). A
	// refused Seq above the next one means a record is missing, and Err() says
	// so until a record with the highest such Seq has been appended; a repeated
	// or lower Seq loses nothing (see lost_test.go).
	var want, maxGap uint64
	for _, s := range steps {
		before := vtReadFile(t, p)
		err := w.Append(at(s.seq))
		after := vtReadFile(t, p)
		if s.wantErr {
			if !errors.Is(err, ErrSeq) {
				t.Fatalf("%s: err = %v, want ErrSeq", s.name, err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("%s: a refused record changed the file", s.name)
			}
			if s.seq > want+1 && s.seq > maxGap {
				maxGap = s.seq
			}
		} else {
			if err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			want++
		}
		if missing := maxGap > want; missing != (w.Err() != nil) {
			t.Fatalf("%s: Err() = %v, but %d records are written and the highest skipped Seq is %d", s.name, w.Err(), want, maxGap)
		}
		if w.Count() != want {
			t.Fatalf("%s: Count = %d, want %d", s.name, w.Count(), want)
		}
	}
	// The offers above skipped records (a gap, then 1<<40) that were never
	// made good, so the writer must not claim the log is complete.
	if err := w.Close(); !errors.Is(err, ErrSeq) {
		t.Fatalf("Close = %v, want the lost-record ErrSeq", err)
	}
	l, err := Read(p)
	if !errors.Is(err, ErrTruncated) || len(l.Records) != 3 || !vtDense(l.Records) || l.Complete() {
		t.Fatalf("Read: %v %+v", err, l)
	}
}

// A marshal failure writes nothing; the writer stays usable, and the record it
// could not write keeps the log from being marked complete until it is made
// good (see lost_test.go).
func TestAppendMarshalFailureWritesNothing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m.jsonl")
	w, err := Create(p, vtHeader(), redact.New([]string{vtSecret}))
	if err != nil {
		t.Fatal(err)
	}
	before := vtReadFile(t, p)
	bad := vtRecords()[0]
	bad.Native = json.RawMessage(`{not json ` + vtSecret)
	bad.Request = &oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "leak " + vtSecret}
	err = w.Append(bad)
	if err == nil {
		t.Fatal("invalid Native was accepted")
	}
	if errors.Is(err, ErrSeq) || errors.Is(err, ErrClosed) {
		t.Fatalf("wrong error class: %v", err)
	}
	after := vtReadFile(t, p)
	if !bytes.Equal(before, after) {
		t.Fatalf("a failed marshal wrote bytes: %q", after)
	}
	if bytes.Contains(after, []byte(vtSecret)) {
		t.Fatal("secret reached the file on a failed append")
	}
	if w.Err() == nil || w.Count() != 0 {
		t.Fatalf("a record that could not be written must show in Err: err=%v count=%d", w.Err(), w.Count())
	}
	if err := w.Append(vtRecords()[0]); err != nil {
		t.Fatalf("writer unusable after a marshal failure: %v", err)
	}
	if w.Count() != 1 || w.Err() != nil {
		t.Fatalf("made good by appending that Seq: Count = %d, Err = %v", w.Count(), w.Err())
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// Sticky I/O error: returned forever, secret never reaches the file, and End
// writes no end line.
func TestStickyErrorAndNoEndLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sticky.jsonl")
	w, err := Create(p, vtHeader(), redact.New([]string{vtSecret}))
	if err != nil {
		t.Fatal(err)
	}
	recs := vtRecords()
	if err := w.Append(recs[0]); err != nil {
		t.Fatal(err)
	}
	// Break the descriptor underneath the writer: the next write fails.
	if err := w.f.Close(); err != nil {
		t.Fatal(err)
	}
	r2 := recs[1]
	r2.Request = &oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "leak " + vtSecret}
	first := w.Append(r2)
	if first == nil {
		t.Fatal("append on a broken descriptor succeeded")
	}
	if w.Err() == nil {
		t.Fatal("I/O error is not sticky")
	}
	// Every later call returns the sticky error, even for a valid record.
	for i := 0; i < 3; i++ {
		if err := w.Append(recs[1]); err == nil || err.Error() != first.Error() {
			t.Fatalf("call %d: sticky error changed: %v vs %v", i, err, first)
		}
	}
	if w.Count() != 1 {
		t.Fatalf("Count = %d, want 1", w.Count())
	}
	endErr := w.End("should-not-be-written")
	if endErr == nil {
		t.Fatal("End after a sticky error reported success")
	}
	if again := w.Close(); again != endErr {
		t.Fatalf("second End returned a different result: %v vs %v", again, endErr)
	}
	raw := vtReadFile(t, p)
	if bytes.Contains(raw, []byte(`"kind":"end"`)) || bytes.Contains(raw, []byte(vtSecret)) {
		t.Fatalf("end line or secret present after a sticky error: %q", raw)
	}
	l, err := Read(p)
	if !errors.Is(err, ErrTruncated) || l == nil || len(l.Records) != 1 {
		t.Fatalf("Read: %v %+v", err, l)
	}
	if recs, err := Replay(p); recs != nil || !errors.Is(err, ErrTruncated) {
		t.Fatalf("Replay of a lossy log: %v %v", recs, err)
	}
	if err := w.Append(recs[1]); !errors.Is(err, ErrClosed) && err == nil {
		t.Fatal("Append after End succeeded")
	}
}

// (4) End / Close idempotence and ErrClosed.
func TestEndCloseIdempotentAndClosed(t *testing.T) {
	t.Run("EndAt then End then Close", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "e.jsonl")
		w, err := Create(p, vtHeader(), nil)
		if err != nil {
			t.Fatal(err)
		}
		recs := vtRecords()
		if err := w.Append(recs[0]); err != nil {
			t.Fatal(err)
		}
		first := vtAt()
		if err := w.EndAt("first", first); err != nil {
			t.Fatal(err)
		}
		before := vtReadFile(t, p)
		if err := w.EndAt("second", first.Add(time.Hour)); err != nil {
			t.Fatalf("second EndAt: %v", err)
		}
		if err := w.End("third"); err != nil {
			t.Fatalf("End: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if !bytes.Equal(before, vtReadFile(t, p)) {
			t.Fatal("a repeated End changed the file")
		}
		if n := strings.Count(string(before), `"kind":"end"`); n != 1 {
			t.Fatalf("%d end lines", n)
		}
		l, err := Read(p)
		if err != nil || l.End == nil || l.End.Reason != "first" || l.End.At != first.UnixMilli() {
			t.Fatalf("Read: %v %+v", err, l)
		}
		// Append after End: ErrClosed, nothing written, Count unchanged.
		err = w.Append(recs[1])
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Append after End = %v, want ErrClosed", err)
		}
		if !bytes.Equal(before, vtReadFile(t, p)) || w.Count() != 1 {
			t.Fatal("Append after End wrote or counted")
		}
	})
	t.Run("Close is End closed", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "c.jsonl")
		w, err := Create(p, vtHeader(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		if err := w.Append(vtRecords()[0]); !errors.Is(err, ErrClosed) {
			t.Fatalf("Append after Close = %v", err)
		}
		l, err := Read(p)
		if err != nil || !l.Complete() || l.End.Reason != "closed" || len(l.Records) != 0 {
			t.Fatalf("Read: %v %+v", err, l)
		}
		recs, err := Replay(p)
		if err != nil || len(recs) != 0 {
			t.Fatalf("Replay of an empty complete log: %v %v", recs, err)
		}
	})
}

// (5) Crash survival: every Append is durable and LF-terminated before End.
func TestDurabilityWithoutClose(t *testing.T) {
	p := filepath.Join(t.TempDir(), "d.jsonl")
	w, err := Create(p, vtHeader(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	recs := vtRecords()
	for i, r := range recs {
		if err := w.Append(r); err != nil {
			t.Fatal(err)
		}
		// A second descriptor sees the record right away.
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(b) == 0 || b[len(b)-1] != '\n' {
			t.Fatalf("after append %d the file ends mid-line", i+1)
		}
		if n := bytes.Count(b, []byte("\n")); n != i+2 {
			t.Fatalf("after append %d: %d lines, want %d", i+1, n, i+2)
		}
		l, err := Decode(bytes.NewReader(b))
		if !errors.Is(err, ErrTruncated) || l == nil {
			t.Fatalf("after append %d: %v", i+1, err)
		}
		if l.End != nil || len(l.Records) != i+1 {
			t.Fatalf("after append %d: %d records, end=%v", i+1, len(l.Records), l.End)
		}
		if vtCanon(t, l.Records) != vtCanon(t, recs[:i+1]) {
			t.Fatalf("after append %d: salvaged records differ", i+1)
		}
	}
	if recs, err := Replay(p); recs != nil || !errors.Is(err, ErrTruncated) {
		t.Fatalf("Replay of an unfinished log: %v %v", recs, err)
	}
}

// (6) Truncation shapes.
func TestTruncationShapes(t *testing.T) {
	full := vtValidFile(t)
	lines := bytes.SplitAfter(full, []byte("\n"))
	lines = lines[:len(lines)-1] // trailing empty element
	if len(lines) != 6 {
		t.Fatalf("setup: %d lines", len(lines))
	}
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	half := lines[3][:len(lines[3])/2]

	cases := []struct {
		name    string
		data    []byte
		records int
		header  bool
	}{
		{"empty file", nil, 0, false},
		{"header only", lines[0], 0, true},
		{"header + 1 record, no end", join(lines[0], lines[1]), 1, true},
		{"all records, no end", join(lines[:5]...), 4, true},
		{"end line without LF", full[:len(full)-1], 4, true},
		{"header without LF", lines[0][:len(lines[0])-1], 0, false},
		{"half-written last line", join(lines[0], lines[1], lines[2], half), 2, true},
		{"half-written first record", join(lines[0], lines[1][:5]), 0, true},
		{"kill-style: bytes of a header", lines[0][:10], 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, err := Decode(bytes.NewReader(c.data))
			if !errors.Is(err, ErrTruncated) {
				t.Fatalf("err = %v, want ErrTruncated", err)
			}
			if l == nil {
				t.Fatal("no salvaged log returned with ErrTruncated")
			}
			if l.Complete() || l.End != nil {
				t.Fatal("truncated log reported complete")
			}
			if len(l.Records) != c.records || !vtDense(l.Records) {
				t.Fatalf("salvaged %d records, want %d", len(l.Records), c.records)
			}
			if c.header && l.Header != vtHeader() {
				t.Fatalf("header not salvaged: %+v", l.Header)
			}
			p := filepath.Join(t.TempDir(), "cut.jsonl")
			if err := os.WriteFile(p, c.data, 0o644); err != nil {
				t.Fatal(err)
			}
			rl, rerr := Read(p)
			if !errors.Is(rerr, ErrTruncated) || rl == nil || len(rl.Records) != c.records {
				t.Fatalf("Read: %v %+v", rerr, rl)
			}
			recs, perr := Replay(p)
			if recs != nil || !errors.Is(perr, ErrTruncated) {
				t.Fatalf("Replay = %v, %v; want nil, ErrTruncated", recs, perr)
			}
		})
	}
}

// (6) Exhaustive: cut the file at every byte offset.
func TestTruncationAtEveryByteOffset(t *testing.T) {
	full := vtValidFile(t)
	want := vtRecords()
	if l, err := Decode(bytes.NewReader(full)); err != nil || !l.Complete() || len(l.Records) != len(want) {
		t.Fatalf("setup: full file does not decode: %v", err)
	}
	p := filepath.Join(t.TempDir(), "cut.jsonl")
	for n := 0; n < len(full); n++ {
		cut := full[:n]
		l, err := Decode(bytes.NewReader(cut))
		if !errors.Is(err, ErrTruncated) {
			t.Fatalf("cut at %d/%d: err = %v, want ErrTruncated", n, len(full), err)
		}
		if l == nil || l.Complete() || l.End != nil {
			t.Fatalf("cut at %d: log %+v is not a truncated partial", n, l)
		}
		if !vtDense(l.Records) || len(l.Records) > len(want) {
			t.Fatalf("cut at %d: salvaged records are not a dense prefix", n)
		}
		committed := bytes.Count(cut, []byte("\n")) // lines whose LF arrived
		wantRecs := committed - 1
		if wantRecs < 0 {
			wantRecs = 0
		}
		if len(l.Records) != wantRecs {
			t.Fatalf("cut at %d: salvaged %d records, want %d", n, len(l.Records), wantRecs)
		}
		if vtCanon(t, l.Records) != vtCanon(t, want[:wantRecs]) {
			t.Fatalf("cut at %d: salvaged records differ from the originals", n)
		}
		if committed >= 1 && l.Header != vtHeader() {
			t.Fatalf("cut at %d: header not salvaged", n)
		}
		if err := os.WriteFile(p, cut, 0o644); err != nil {
			t.Fatal(err)
		}
		recs, perr := Replay(p)
		if recs != nil || !errors.Is(perr, ErrTruncated) {
			t.Fatalf("cut at %d: Replay = %d records, %v", n, len(recs), perr)
		}
	}
	// The whole file is the only prefix that is complete.
	l, err := Decode(bytes.NewReader(full))
	if err != nil || l.End == nil {
		t.Fatalf("full file: %v", err)
	}
}

// (7) Corrupt / format table.
func TestCorruptAndFormatTable(t *testing.T) {
	const hdr = `{"kind":"header","format":"oar-voyage/3","dialect":"oarlock","runtime":"pi","cwd":".","sessionId":"s","startedAt":1,"recorder":"t"}` + "\n"
	rec := func(seq int, kind string) string {
		return `{"kind":"record","record":{"seq":` + itoa(seq) + `,"receivedAt":"2026-01-01T00:00:00Z","kind":"` + kind + `","sessionId":"s"}}` + "\n"
	}
	const end = `{"kind":"end","at":2,"reason":"done"}` + "\n"

	// A well-formed baseline decodes, so every case below fails for its own reason.
	if l, err := Decode(strings.NewReader(hdr + rec(1, "frame") + rec(2, "request") + end)); err != nil || !l.Complete() || len(l.Records) != 2 {
		t.Fatalf("baseline: %v", err)
	}

	cases := []struct {
		name    string
		data    string
		want    error
		alsoSeq bool
	}{
		// ErrFormat
		{"first line is a record", rec(1, "frame"), ErrFormat, false},
		{"first line is an end", end, ErrFormat, false},
		{"first line is garbage", "garbage\n", ErrFormat, false},
		{"first line is blank", "\n" + hdr, ErrFormat, false},
		{"first line is a JSON array", "[1,2,3]\n", ErrFormat, false},
		{"wrong format", strings.Replace(hdr, "oar-voyage/3", "oar-voyage/2", 1), ErrFormat, false},
		{"missing format", strings.Replace(hdr, `"format":"oar-voyage/3",`, "", 1), ErrFormat, false},
		{"wrong dialect", strings.Replace(hdr, `"oarlock"`, `"oar-ts"`, 1), ErrFormat, false},
		{"missing dialect", strings.Replace(hdr, `"dialect":"oarlock",`, "", 1), ErrFormat, false},
		{"OAR TypeScript log", `{"kind":"header","format":"oar-voyage/3","runtime":"claude","cwd":"/x","sessionId":"s","startedAt":1,"recorder":"oar"}` + "\n" +
			`{"kind":"record","record":{"type":"frame","seq":1,"frame":{"a":1}}}` + "\n" + end, ErrFormat, false},
		{"header kind misspelled", strings.Replace(hdr, `"kind":"header"`, `"kind":"Header"`, 1), ErrFormat, false},

		// ErrCorrupt
		{"line 2 is not JSON", hdr + "not json\n", ErrCorrupt, false},
		{"line 2 is truncated JSON but terminated", hdr + `{"kind":"record","record":{"seq":1` + "\n", ErrCorrupt, false},
		{"unknown kind", hdr + `{"kind":"mystery"}` + "\n", ErrCorrupt, false},
		{"missing kind", hdr + `{"record":{}}` + "\n", ErrCorrupt, false},
		{"blank line", hdr + "\n" + rec(1, "frame") + end, ErrCorrupt, false},
		{"blank line before end", hdr + rec(1, "frame") + "\n" + end, ErrCorrupt, false},
		{"second header", hdr + hdr + end, ErrCorrupt, false},
		{"second header after records", hdr + rec(1, "frame") + hdr + end, ErrCorrupt, false},
		{"record after end", hdr + rec(1, "frame") + end + rec(2, "frame"), ErrCorrupt, false},
		{"second end", hdr + end + end, ErrCorrupt, false},
		{"blank line after end", hdr + end + "\n", ErrCorrupt, false},
		{"header after end", hdr + end + hdr, ErrCorrupt, false},
		{"bytes after end without LF", hdr + end + "tail", ErrCorrupt, false},
		{"record with unknown Kind", hdr + rec(1, "mystery"), ErrCorrupt, false},
		{"record with empty Kind", hdr + rec(1, ""), ErrCorrupt, false},
		{"record line without record", hdr + `{"kind":"record"}` + "\n", ErrCorrupt, false},
		{"record is not an object", hdr + `{"kind":"record","record":5}` + "\n", ErrCorrupt, false},
		{"record is null", hdr + `{"kind":"record","record":null}` + "\n", ErrCorrupt, false},
		{"end with wrong types", hdr + `{"kind":"end","at":"soon","reason":1}` + "\n", ErrCorrupt, false},
		{"seq gap", hdr + rec(1, "frame") + rec(3, "frame") + end, ErrCorrupt, true},
		{"seq starts at 2", hdr + rec(2, "frame") + end, ErrCorrupt, true},
		{"seq starts at 0", hdr + rec(0, "frame") + end, ErrCorrupt, true},
		{"seq duplicate", hdr + rec(1, "frame") + rec(1, "frame") + end, ErrCorrupt, true},
		{"seq goes backwards", hdr + rec(1, "frame") + rec(2, "frame") + rec(1, "frame"), ErrCorrupt, true},
		{"seq gap without end", hdr + rec(1, "frame") + rec(5, "frame"), ErrCorrupt, true},
		{"seq overflows uint64", hdr + `{"kind":"record","record":{"seq":99999999999999999999999,"kind":"frame"}}` + "\n", ErrCorrupt, false},
		{"negative seq", hdr + `{"kind":"record","record":{"seq":-1,"kind":"frame"}}` + "\n", ErrCorrupt, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, err := Decode(strings.NewReader(c.data))
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if c.alsoSeq && !errors.Is(err, ErrSeq) {
				t.Fatalf("err = %v does not wrap ErrSeq", err)
			}
			if errors.Is(err, ErrTruncated) {
				t.Fatalf("err = %v must not be ErrTruncated", err)
			}
			if c.want == ErrFormat && errors.Is(err, ErrCorrupt) {
				t.Fatalf("err = %v is both format and corrupt", err)
			}
			if l != nil {
				t.Fatalf("a nil Log is required with %v, got %+v", c.want, l)
			}
			p := filepath.Join(t.TempDir(), "x.jsonl")
			if err := os.WriteFile(p, []byte(c.data), 0o644); err != nil {
				t.Fatal(err)
			}
			rl, rerr := Read(p)
			if !errors.Is(rerr, c.want) || rl != nil {
				t.Fatalf("Read = %+v, %v", rl, rerr)
			}
			recs, perr := Replay(p)
			if recs != nil || !errors.Is(perr, c.want) {
				t.Fatalf("Replay = %v, %v", recs, perr)
			}
		})
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// vtRepeat is an endless reader of one byte.
type vtRepeat byte

func (b vtRepeat) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

func TestLineLongerThanMaxIsCorrupt(t *testing.T) {
	const hdr = `{"kind":"header","format":"oar-voyage/3","dialect":"oarlock","runtime":"pi","cwd":".","sessionId":"s","startedAt":1,"recorder":"t"}` + "\n"
	l, err := Decode(io.MultiReader(
		strings.NewReader(hdr),
		io.LimitReader(vtRepeat('a'), MaxLineBytes+1),
		strings.NewReader("\n"),
	))
	if !errors.Is(err, ErrCorrupt) || l != nil {
		t.Fatalf("Decode = %+v, %v; want nil, ErrCorrupt", l, err)
	}
	// An over-long first line is rejected before the header is trusted.
	l, err = Decode(io.MultiReader(io.LimitReader(vtRepeat('b'), MaxLineBytes+1), strings.NewReader("\n")))
	if err == nil || l != nil {
		t.Fatalf("over-long first line: %+v, %v", l, err)
	}
	if errors.Is(err, ErrTruncated) {
		t.Fatalf("over-long first line is not a truncation: %v", err)
	}
}

// (8) Fuzz: no panics; a nil error means a complete dense log.
func FuzzDecode(f *testing.F) {
	const hdr = `{"kind":"header","format":"oar-voyage/3","dialect":"oarlock","runtime":"pi","cwd":".","sessionId":"s","startedAt":1,"recorder":"t"}` + "\n"
	valid := func() []byte {
		p := filepath.Join(f.TempDir(), "seed.jsonl")
		w, err := Create(p, vtHeader(), nil)
		if err != nil {
			f.Fatal(err)
		}
		for _, r := range vtRecords() {
			if err := w.Append(r); err != nil {
				f.Fatal(err)
			}
		}
		if err := w.EndAt("done", vtAt()); err != nil {
			f.Fatal(err)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		return b
	}()
	f.Add(valid)
	for n := 0; n < len(valid); n++ {
		f.Add(valid[:n])
	}
	f.Add([]byte(""))
	f.Add([]byte("\n"))
	f.Add([]byte("\x00\xff\xfe garbage \x01\n{{{{\n"))
	f.Add([]byte(hdr))
	f.Add([]byte(hdr + `{"kind":"record","record":{"seq":99999999999999999999999999,"kind":"frame"}}` + "\n"))
	f.Add([]byte(hdr + `{"kind":"record","record":{"seq":1,"receivedAt":"2026-01-01T00:00:00Z","kind":"frame","sessionId":"s","native":` +
		strings.Repeat("[", 20000) + strings.Repeat("]", 20000) + `}}` + "\n"))
	f.Add([]byte(hdr + strings.Repeat("{\"a\":", 5000)))
	f.Add([]byte(strings.Repeat("[", 100000)))
	f.Add([]byte(hdr + `{"kind":"record","record":{"seq":1,"kind":"frame","events":[{"kind":"usage","usage":{"tokens":{"input":1e400}}}]}}` + "\n"))
	f.Add([]byte(hdr + `{"kind":"end","at":1e999,"reason":"x"}` + "\n"))
	f.Add([]byte(hdr + `{"kind":"end","at":2,"reason":"x"}` + "\r\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		l, err := Decode(bytes.NewReader(data))
		switch {
		case err == nil:
			if l == nil || l.End == nil || !l.Complete() {
				t.Fatalf("nil error without an end line: %+v", l)
			}
			if !vtDense(l.Records) {
				t.Fatal("nil error with non-dense Seq")
			}
		case errors.Is(err, ErrTruncated):
			if l == nil || l.End != nil || l.Complete() {
				t.Fatalf("ErrTruncated with a complete or missing log: %+v", l)
			}
			if !vtDense(l.Records) {
				t.Fatal("salvaged records are not a dense prefix")
			}
		case errors.Is(err, ErrFormat), errors.Is(err, ErrCorrupt):
			if l != nil {
				t.Fatalf("%v returned a non-nil Log", err)
			}
		default:
			t.Fatalf("error outside the documented classes: %v", err)
		}
	})
}

// (9) Static guard: Replay needs no network and no process.
var vtForbiddenImport = func(p string) bool {
	switch {
	case p == "net", p == "os/exec", p == "plugin":
		return true
	case p == "github.com/zzjcool/oarlock/internal/exec":
		return true
	case strings.HasPrefix(p, "net/") && p != "net/url":
		return true
	}
	return false
}

func vtBadImports(filename string, src any) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), filename, src, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	var bad []string
	for _, imp := range f.Imports {
		if p := strings.Trim(imp.Path.Value, "\"`"); vtForbiddenImport(p) {
			bad = append(bad, p)
		}
	}
	return bad, nil
}

func TestNoNetworkOrProcessImports(t *testing.T) {
	// The checker itself must fire.
	bad, err := vtBadImports("x.go", "package x\nimport (\n\t\"net\"\n\t\"net/http\"\n\t\"net/url\"\n\t\"os/exec\"\n\t\"plugin\"\n\t\"net/rpc\"\n\t\"github.com/zzjcool/oarlock/internal/exec\"\n\t\"fmt\"\n)\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(bad, ","); got != "net,net/http,os/exec,plugin,net/rpc,github.com/zzjcool/oarlock/internal/exec" {
		t.Fatalf("checker flagged %q", got)
	}

	seen := map[string]bool{}
	err = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		seen[filepath.ToSlash(path)] = true
		bad, err := vtBadImports(path, nil)
		if err != nil {
			return err
		}
		if len(bad) > 0 {
			t.Errorf("%s imports %v: replay must be pure file I/O (no network, no process)", path, bad)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !seen["voyage.go"] {
		t.Fatalf("guard is vacuous: voyage.go not scanned (saw %v)", seen)
	}
}

func TestReplayReadOnlyFileAndDirectory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ro.jsonl")
	vtWrite(t, p, vtHeader(), vtRecords(), nil)
	if runtime.GOOS != "windows" {
		if err := os.Chmod(p, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := Replay(p)
	if err != nil || len(recs) != len(vtRecords()) {
		t.Fatalf("Replay of a read-only file: %d records, %v", len(recs), err)
	}
	// A read-only file must stay byte-identical (Replay never writes).
	before := vtReadFile(t, p)
	if _, err := Replay(p); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, vtReadFile(t, p)) {
		t.Fatal("Replay modified the file")
	}

	for name, target := range map[string]string{
		"directory": dir,
		"missing":   filepath.Join(dir, "missing.jsonl"),
	} {
		recs, err := Replay(target)
		if err == nil || recs != nil {
			t.Fatalf("Replay(%s) = %v, %v; want nil, error", name, recs, err)
		}
		if l, err := Read(target); err == nil || l != nil {
			t.Fatalf("Read(%s) = %+v, %v; want nil, error", name, l, err)
		}
	}
	if _, err := Replay(filepath.Join(dir, "missing.jsonl")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file error = %v, want fs.ErrNotExist", err)
	}
}

// (10) Redaction happens before a byte reaches the file descriptor.
func vtSecretRecords() []oarlock.Record {
	at := vtAt()
	native := `{"type":"x","note":"key ` + vtSecret + ` here","env":{"ANTHROPIC_API_KEY":"sk-live-aaaaaaaaaaaa","SSH_AUTH_SOCK":"/tmp/agent.sock","HOME":"/home/alice"},"headers":{"Authorization":"Bearer ` + vtBearer + `"}}`
	return []oarlock.Record{
		{
			Seq: 1, ReceivedAt: at, Kind: oarlock.KindFrame, SessionID: "s-1",
			Native: json.RawMessage(native),
			Events: []oarlock.Event{{Kind: oarlock.EventTextDelta, Text: "the key is " + vtSecret}},
		},
		{
			Seq: 2, ReceivedAt: at, Kind: oarlock.KindRequest, SessionID: "s-1", ID: "r1", Direction: oarlock.ToRuntime,
			Request: &oarlock.RequestBody{
				Action: oarlock.ActionPrompt, Input: "use " + vtSecret,
				Native: json.RawMessage(`{"k":"` + vtSecret + `"}`),
			},
		},
		{
			Seq: 3, ReceivedAt: at, Kind: oarlock.KindResponse, SessionID: "s-1", RequestID: "r1",
			Response: &oarlock.ResponseBody{
				Kind: oarlock.ResponseRejected, Reason: "bad " + vtSecret,
				Native: json.RawMessage(`{"e":"` + vtSecret + `"}`),
			},
		},
	}
}

func TestRedactionBeforeWrite(t *testing.T) {
	forbidden := []string{vtSecret, "sk-test-0123456789abcdef", "sk-live-aaaaaaaaaaaa", "tok-bbbbbbbbbbbb"}

	t.Run("known value and structural rules", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "r.jsonl")
		vtWrite(t, p, vtHeader(), vtSecretRecords(), redact.New([]string{vtSecret}))
		raw := vtReadFile(t, p) // raw bytes, not Decode
		for _, s := range forbidden {
			if bytes.Contains(raw, []byte(s)) {
				t.Fatalf("file contains %q:\n%s", s, raw)
			}
		}
		if !bytes.Contains(raw, []byte(redact.RedactedPlaceholder)) {
			t.Fatal("no [redacted] marker in the file")
		}
		// Names that must stay readable are not over-redacted.
		for _, keep := range []string{`"SSH_AUTH_SOCK":"/tmp/agent.sock"`, `"HOME":"/home/alice"`, `"ANTHROPIC_API_KEY":"[redacted]"`, `"Authorization":"[redacted]"`} {
			if !bytes.Contains(raw, []byte(keep)) {
				t.Fatalf("expected %s in the file:\n%s", keep, raw)
			}
		}
		// The redacted file is still a complete, dense log.
		recs, err := Replay(p)
		if err != nil || len(recs) != 3 || !vtDense(recs) {
			t.Fatalf("Replay: %d %v", len(recs), err)
		}
		if recs[1].Request == nil || recs[1].Request.Input != "use [redacted]" {
			t.Fatalf("request input = %+v", recs[1].Request)
		}
		if recs[2].Response == nil || recs[2].Response.Reason != "bad [redacted]" {
			t.Fatalf("response reason = %+v", recs[2].Response)
		}
		if recs[0].Events[0].Text != "the key is [redacted]" {
			t.Fatalf("event text = %q", recs[0].Events[0].Text)
		}
	})

	t.Run("nil redactor still applies structural rules", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "n.jsonl")
		vtWrite(t, p, vtHeader(), vtSecretRecords(), nil)
		raw := vtReadFile(t, p)
		for _, s := range []string{"sk-live-aaaaaaaaaaaa", "tok-bbbbbbbbbbbb"} {
			if bytes.Contains(raw, []byte(s)) {
				t.Fatalf("nil redactor leaked %q:\n%s", s, raw)
			}
		}
		if !bytes.Contains(raw, []byte(`"ANTHROPIC_API_KEY":"[redacted]"`)) || !bytes.Contains(raw, []byte(`"SSH_AUTH_SOCK":"/tmp/agent.sock"`)) {
			t.Fatalf("structural rules wrong:\n%s", raw)
		}
	})

	t.Run("header and end reason", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "h.jsonl")
		h := vtHeader()
		h.Cwd = "/work/" + vtSecret
		h.SessionID = "sess-" + vtSecret
		w, err := Create(p, h, redact.New([]string{vtSecret}))
		if err != nil {
			t.Fatal(err)
		}
		// The header is on disk and clean before anything else happens.
		if raw := vtReadFile(t, p); bytes.Contains(raw, []byte(vtSecret)) || !bytes.Contains(raw, []byte("[redacted]")) {
			t.Fatalf("header not redacted at Create: %s", raw)
		}
		if err := w.EndAt("crashed near "+vtSecret, vtAt()); err != nil {
			t.Fatal(err)
		}
		raw := vtReadFile(t, p)
		if bytes.Contains(raw, []byte(vtSecret)) {
			t.Fatalf("secret in header or end line:\n%s", raw)
		}
		l, err := Read(p)
		if err != nil || l.Header.Cwd != "/work/[redacted]" || l.Header.SessionID != "sess-[redacted]" || l.End.Reason != "crashed near [redacted]" {
			t.Fatalf("Read: %v %+v %+v", err, l.Header, l.End)
		}
	})

	t.Run("idempotent on already-redacted kernel output", func(t *testing.T) {
		red := redact.New([]string{vtSecret})
		var kernelOut []oarlock.Record
		for _, r := range vtSecretRecords() {
			rr, _ := red.Record(r) // what the real kernel retains
			kernelOut = append(kernelOut, rr)
		}
		dir := t.TempDir()
		a, b := filepath.Join(dir, "a.jsonl"), filepath.Join(dir, "b.jsonl")
		vtWrite(t, a, vtHeader(), kernelOut, red)
		l, err := Read(a)
		if err != nil {
			t.Fatal(err)
		}
		vtWrite(t, b, l.Header, l.Records, red) // redact it all again
		ab, bb := vtReadFile(t, a), vtReadFile(t, b)
		if !bytes.Equal(ab, bb) {
			t.Fatalf("second pass changed bytes:\n%s\n%s", ab, bb)
		}
		if vtCanon(t, l.Records) != vtCanon(t, kernelOut) {
			t.Fatal("writer changed already-redacted records")
		}
		for _, s := range forbidden {
			if bytes.Contains(ab, []byte(s)) {
				t.Fatalf("file contains %q", s)
			}
		}
	})
}

// -race: accessors may be called while another goroutine appends.
func TestConcurrentAccessorsWhileAppending(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.jsonl")
	w, err := Create(p, vtHeader(), nil)
	if err != nil {
		t.Fatal(err)
	}
	const n = 40
	var wg sync.WaitGroup
	done := make(chan struct{})
	appendErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		base := vtRecords()[0]
		for i := 1; i <= n; i++ {
			r := base
			r.Seq = uint64(i)
			if err := w.Append(r); err != nil {
				appendErr <- err
				return
			}
		}
	}()
	var last uint64
	deadline := time.Now().Add(20 * time.Second)
poll:
	for {
		select {
		case <-done:
			break poll
		default:
		}
		c := w.Count()
		if c < last || c > n {
			t.Errorf("Count went %d -> %d", last, c)
		}
		last = c
		if err := w.Err(); err != nil {
			t.Errorf("Err = %v", err)
		}
		if w.Path() != p {
			t.Errorf("Path = %q", w.Path())
		}
		if time.Now().After(deadline) {
			t.Fatal("appender did not finish")
		}
	}
	wg.Wait()
	select {
	case err := <-appendErr:
		t.Fatalf("Append: %v", err)
	default:
	}
	if w.Count() != n {
		t.Fatalf("Count = %d, want %d", w.Count(), n)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	l, err := Read(p)
	if err != nil || len(l.Records) != n || !vtDense(l.Records) {
		t.Fatalf("Read: %v", err)
	}
}
