package voyage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/redact"
)

const (
	// Format is the OAR format tag this package writes and reads. The tag is
	// shared with OAR's TypeScript recorder; the record body is not (see
	// Dialect).
	Format = "oar-voyage/3"
	// Dialect marks the record body as oarlock's Go shape (oarlock.Record).
	// OAR's TypeScript logs carry the same line envelope but a different record
	// body, and are refused with ErrFormat rather than half-decoded.
	Dialect = "oarlock"
	// MaxLineBytes bounds one line (LF excluded). A longer line is corrupt, not
	// silently truncated; the bound keeps a hostile or damaged file from
	// exhausting memory.
	MaxLineBytes = 16 << 20
)

const (
	kindHeader = "header"
	kindRecord = "record"
	kindEnd    = "end"
)

var (
	// ErrTruncated means the log has no end line: the recorder was killed, the
	// disk filled, or the file is still being written. Everything before the
	// cut is intact and is returned by Read; Replay refuses it.
	ErrTruncated = errors.New("voyage: log is truncated (no end line)")
	// ErrFormat means the file is not a voyage log this package reads: the first
	// line is not a header, or its format or dialect differs.
	ErrFormat = errors.New("voyage: unsupported log format")
	// ErrCorrupt means a committed line is damaged: not JSON, an unknown kind,
	// a second header, a line after end, or a record out of order.
	ErrCorrupt = errors.New("voyage: corrupt log")
	// ErrSeq is a record whose Seq is not the next dense value. The stream is
	// dense from 1 (docs/spec/record-stream.md); a recorder that cannot keep
	// that promise must not write a log a replay would silently mis-fold.
	ErrSeq = errors.New("voyage: record sequence is not dense")
	// ErrClosed is an operation on a writer that has ended.
	ErrClosed = errors.New("voyage: writer is closed")
)

// Header is the first line of a log: what was recorded and by whom. StartedAt
// and End.At are Unix epoch milliseconds.
type Header struct {
	Runtime     string `json:"runtime"`
	Model       string `json:"model,omitempty"`
	Effort      string `json:"effort,omitempty"`
	ServiceTier string `json:"serviceTier,omitempty"`
	Cwd         string `json:"cwd"`
	SessionID   string `json:"sessionId"`
	StartedAt   int64  `json:"startedAt"`
	Recorder    string `json:"recorder"`
}

// End is the last line of a complete log.
type End struct {
	At     int64  `json:"at"`
	Reason string `json:"reason"`
}

// headerLine and endLine are the on-disk envelopes. Field order is the order
// OAR's recorder writes, so a log diffs cleanly against one OAR wrote.
type headerLine struct {
	Kind    string `json:"kind"`
	Format  string `json:"format"`
	Dialect string `json:"dialect"`
	Header
}

type endLine struct {
	Kind string `json:"kind"`
	End
}

type recordLine struct {
	Kind   string          `json:"kind"`
	Record json.RawMessage `json:"record"`
}

// Writer appends one session's records to a log. Every line is written with
// one write call and then fsynced, so a crashed or killed process loses at
// most the line it was writing; Decode ignores such a tail and reports the log
// as truncated.
//
// Records are redacted before a byte reaches the file descriptor. A Writer is
// safe for concurrent use; appends are serialized in call order.
type Writer struct {
	mu     sync.Mutex
	path   string
	f      *os.File
	red    *redact.Redactor
	next   uint64 // Seq the next record must carry
	count  uint64
	err    error  // first sticky I/O error
	lost   error  // first record that was offered but not written, until made good
	lostAt uint64 // highest Seq among such records
	ended  bool
	endErr error // result of the first End
}

// Create opens path for a new log and durably writes the header. It never
// replaces an existing file (the error wraps fs.ErrExist): a recording is
// evidence, and clobbering one by mistake is not recoverable.
//
// red is applied to every line before it is written; nil means the structural
// rules only (env/header names, password URLs), with no known session values.
func Create(path string, h Header, red *redact.Redactor) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("voyage: create %s: %w", path, err)
	}
	w := &Writer{path: path, f: f, red: red, next: 1}
	line, err := w.encode(headerLine{Kind: kindHeader, Format: Format, Dialect: Dialect, Header: h})
	if err == nil {
		err = w.writeLine(line)
	}
	if err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("voyage: write header %s: %w", path, err)
	}
	syncDir(filepath.Dir(path))
	return w, nil
}

// syncDir makes the new file's directory entry durable, so a crash right after
// Create cannot lose the file itself. It is best effort: some platforms
// (Windows) cannot fsync a directory, and the file's own fsync already covers
// its contents.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// encode marshals v and applies the redactor, in that order and before
// anything is written. A marshal failure writes nothing.
func (w *Writer) encode(v any) ([]byte, error) {
	raw, err := marshal(v)
	if err != nil {
		return nil, err
	}
	redacted, _ := w.red.Bytes(raw)
	return redacted, nil
}

// marshal is json.Marshal without HTML escaping. encoding/json rewrites <, >
// and & as \u003c, \u003e and \u0026 by default, which is the same JSON value
// but not the runtime's own bytes; Record.Native is documented as the frame
// verbatim, so an unredacted line must not change it. (A line the redactor does
// change is re-encoded by redact.Bytes: values and numbers are preserved, key
// order and HTML escaping are not.) The log is split on LF only; U+2028 and
// U+2029 are legal inside JSON strings and never split a line.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// writeLine writes line plus LF with a single Write and fsyncs. The caller
// holds the lock (or owns the writer, as Create does).
func (w *Writer) writeLine(line []byte) error {
	buf := make([]byte, 0, len(line)+1)
	buf = append(buf, line...)
	buf = append(buf, '\n')
	if _, err := w.f.Write(buf); err != nil {
		return err
	}
	return w.f.Sync()
}

// Append redacts, writes and fsyncs one record. The record's Seq must be the
// next dense value (the first is 1).
//
// A refused record writes nothing and the writer stays usable, but the writer
// remembers any record the log is now missing: a Seq above the next one (a
// record was skipped), a record that cannot be marshalled, or a line longer
// than MaxLineBytes (the reader would refuse it). Until a record with that Seq
// has been appended, End writes no end line, so a log that lost a record is
// never marked complete. A repeated Seq (below the next one) is refused with
// ErrSeq but loses nothing.
//
// An I/O error is sticky: it is returned by every later call, and End then
// writes no end line either.
func (w *Writer) Append(rec oarlock.Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended {
		return ErrClosed
	}
	if w.err != nil {
		return w.err
	}
	if rec.Seq != w.next {
		err := fmt.Errorf("%w: got seq %d, want %d", ErrSeq, rec.Seq, w.next)
		if rec.Seq > w.next {
			w.lose(rec.Seq, err)
		}
		return err
	}
	line, err := w.encodeRecord(rec)
	if err != nil {
		w.lose(rec.Seq, err)
		return err
	}
	if err := w.writeLine(line); err != nil {
		w.err = fmt.Errorf("voyage: write record %d: %w", rec.Seq, err)
		return w.err
	}
	w.next++
	w.count++
	if w.lost != nil && w.next > w.lostAt {
		w.lost, w.lostAt = nil, 0 // every Seq offered so far is in the log
	}
	return nil
}

// lose remembers a record the log is missing. The caller holds the lock.
func (w *Writer) lose(seq uint64, err error) {
	if w.lost == nil {
		w.lost = err
	}
	if seq > w.lostAt {
		w.lostAt = seq
	}
}

// encodeRecord marshals and redacts one record into a line, in that order, and
// refuses a line the reader would not accept.
func (w *Writer) encodeRecord(rec oarlock.Record) ([]byte, error) {
	body, err := marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("voyage: marshal record %d: %w", rec.Seq, err)
	}
	line, err := w.encode(recordLine{Kind: kindRecord, Record: body})
	if err != nil {
		return nil, fmt.Errorf("voyage: encode record %d: %w", rec.Seq, err)
	}
	if len(line) > MaxLineBytes {
		return nil, fmt.Errorf("voyage: record %d is %d bytes, over the %d byte line limit", rec.Seq, len(line), MaxLineBytes)
	}
	return line, nil
}

// Err is why the log cannot be marked complete right now: the first sticky I/O
// error, else the first record that was offered but is still missing from the
// log (see Append). It is nil while every offered record has been written, so a
// recorder checks it before End; End itself reports the same reason.
func (w *Writer) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	return w.lost
}

// Count is the number of records appended successfully.
func (w *Writer) Count() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.count
}

// Path is the file the writer owns.
func (w *Writer) Path() string { return w.path }

// End is EndAt(reason, time.Now()).
func (w *Writer) End(reason string) error { return w.EndAt(reason, time.Now()) }

// Close is End("closed").
func (w *Writer) Close() error { return w.End("closed") }

// EndAt writes the end line, fsyncs and closes the file. It is idempotent:
// later calls return the first call's result. After a sticky write error, or
// while a record that was offered is still missing from the log (see Append),
// no end line is written: the log stays truncated, Replay refuses it, and the
// returned error says why.
func (w *Writer) EndAt(reason string, at time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended {
		return w.endErr
	}
	w.ended = true
	var err error
	switch {
	case w.err != nil:
		err = w.err
	case w.lost != nil:
		err = fmt.Errorf("voyage: no end line written, the log is missing a record: %w", w.lost)
	default:
		var line []byte
		line, err = w.encode(endLine{Kind: kindEnd, End: End{At: at.UnixMilli(), Reason: reason}})
		if err == nil {
			err = w.writeLine(line)
		}
	}
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	w.endErr = err
	return err
}

// Log is a decoded voyage file. End is nil for a truncated log.
type Log struct {
	Header  Header
	Records []oarlock.Record
	End     *End
}

// Complete reports whether the log ended with an end line.
func (l *Log) Complete() bool { return l != nil && l.End != nil }

// Replay reads path and returns its records, in Seq order. It is strict: it
// returns nil records and an error unless the file has a header, a dense run
// of records and an end line. A truncated log is ErrTruncated, never a
// shorter-looking complete one; use Read to salvage the prefix.
//
// Replay touches nothing but the file: no network, no process, no clock.
func Replay(path string) ([]oarlock.Record, error) {
	l, err := Read(path)
	if err != nil {
		return nil, err
	}
	return l.Records, nil
}

// Read decodes the file at path. See Decode.
func Read(path string) (*Log, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("voyage: open %s: %w", path, err)
	}
	defer f.Close()
	l, err := Decode(f)
	if err != nil {
		return l, fmt.Errorf("%s: %w", path, err)
	}
	return l, nil
}

// Decode reads a log. It never panics on any input.
//
// A line counts only once its terminating LF has arrived, so the half line a
// crash leaves behind is never decoded. Input that ends before the end line
// (empty, header only, records only, half line) returns the salvaged Log
// together with ErrTruncated; the salvaged records are a dense prefix. A
// header problem is ErrFormat and a damaged committed line is ErrCorrupt; both
// return a nil Log, because nothing in such a file can be trusted.
func Decode(r io.Reader) (*Log, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	log := &Log{}
	var next uint64 = 1
	for lineNo := 1; ; lineNo++ {
		line, st, err := readLine(br)
		if err != nil {
			if errors.Is(err, errLineTooLong) {
				return nil, fmt.Errorf("%w: line %d is longer than %d bytes", ErrCorrupt, lineNo, MaxLineBytes)
			}
			return nil, fmt.Errorf("voyage: read line %d: %w", lineNo, err)
		}
		if st != lineOK {
			switch {
			case log.End == nil:
				// EOF before the end line: the recorder stopped early. A
				// partial tail (st == eofPartial) was never committed.
				return log, fmt.Errorf("%w: %d record(s) before the cut", ErrTruncated, len(log.Records))
			case st == eofPartial:
				return nil, fmt.Errorf("%w: bytes follow the end line", ErrCorrupt)
			}
			return log, nil // the end line was the last thing in the file
		}
		if log.End != nil {
			return nil, fmt.Errorf("%w: line %d follows the end line", ErrCorrupt, lineNo)
		}
		if lineNo == 1 {
			h, err := decodeHeader(line)
			if err != nil {
				return nil, err
			}
			log.Header = h
			continue
		}
		var head struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			return nil, fmt.Errorf("%w: line %d: %v", ErrCorrupt, lineNo, err)
		}
		switch head.Kind {
		case kindRecord:
			rec, err := decodeRecord(line)
			if err != nil {
				return nil, fmt.Errorf("%w: line %d: %v", ErrCorrupt, lineNo, err)
			}
			if rec.Seq != next {
				return nil, fmt.Errorf("%w: %w: line %d has seq %d, want %d", ErrCorrupt, ErrSeq, lineNo, rec.Seq, next)
			}
			next++
			log.Records = append(log.Records, rec)
		case kindEnd:
			var e struct {
				End
			}
			if err := json.Unmarshal(line, &e); err != nil {
				return nil, fmt.Errorf("%w: line %d: %v", ErrCorrupt, lineNo, err)
			}
			end := e.End
			log.End = &end
		case kindHeader:
			return nil, fmt.Errorf("%w: line %d is a second header", ErrCorrupt, lineNo)
		default:
			return nil, fmt.Errorf("%w: line %d has unknown kind %q", ErrCorrupt, lineNo, head.Kind)
		}
	}
}

// decodeHeader checks line 1: kind, format, dialect. A first line that is not
// a header, or names another format or dialect, is ErrFormat: this is not a
// file we can read, which differs from a damaged one.
func decodeHeader(line []byte) (Header, error) {
	var h struct {
		Kind    string `json:"kind"`
		Format  string `json:"format"`
		Dialect string `json:"dialect"`
		Header
	}
	if err := json.Unmarshal(line, &h); err != nil {
		return Header{}, fmt.Errorf("%w: first line is not a JSON header: %v", ErrFormat, err)
	}
	switch {
	case h.Kind != kindHeader:
		return Header{}, fmt.Errorf("%w: first line has kind %q, want %q", ErrFormat, h.Kind, kindHeader)
	case h.Format != Format:
		return Header{}, fmt.Errorf("%w: format %q, want %q", ErrFormat, h.Format, Format)
	case h.Dialect != Dialect:
		return Header{}, fmt.Errorf("%w: dialect %q, want %q (OAR TypeScript logs use a different record body)", ErrFormat, h.Dialect, Dialect)
	}
	return h.Header, nil
}

func decodeRecord(line []byte) (oarlock.Record, error) {
	var env recordLine
	if err := json.Unmarshal(line, &env); err != nil {
		return oarlock.Record{}, err
	}
	if len(bytes.TrimSpace(env.Record)) == 0 {
		return oarlock.Record{}, errors.New("record line has no record")
	}
	var rec oarlock.Record
	if err := json.Unmarshal(env.Record, &rec); err != nil {
		return oarlock.Record{}, err
	}
	switch rec.Kind {
	case oarlock.KindFrame, oarlock.KindRequest, oarlock.KindResponse:
	default:
		return oarlock.Record{}, fmt.Errorf("record has unknown kind %q", rec.Kind)
	}
	return rec, nil
}

var errLineTooLong = errors.New("voyage: line too long")

// lineState is what readLine found.
type lineState int

const (
	// lineOK is a complete, LF-terminated line.
	lineOK lineState = iota
	// eofClean is end of input exactly at a line boundary.
	eofClean
	// eofPartial is end of input inside a line: bytes with no LF. Such a tail
	// was never committed and is dropped.
	eofPartial
)

// readLine returns the next LF-terminated line without its LF (and without one
// trailing CR, tolerated for files that crossed a Windows editor). At EOF it
// reports whether the input ended on a line boundary or inside a line. A line
// longer than MaxLineBytes is errLineTooLong. bufio.Scanner is avoided on
// purpose: it hands an unterminated tail over as if it were a line.
func readLine(br *bufio.Reader) (line []byte, st lineState, err error) {
	var buf []byte
	for {
		chunk, rerr := br.ReadSlice('\n')
		if len(buf)+len(chunk) > MaxLineBytes+1 {
			return nil, lineOK, errLineTooLong
		}
		buf = append(buf, chunk...)
		switch {
		case rerr == nil:
			buf = buf[:len(buf)-1] // drop LF
			if n := len(buf); n > 0 && buf[n-1] == '\r' {
				buf = buf[:n-1]
			}
			if len(buf) > MaxLineBytes {
				return nil, lineOK, errLineTooLong
			}
			return buf, lineOK, nil
		case errors.Is(rerr, bufio.ErrBufferFull):
			continue
		case errors.Is(rerr, io.EOF):
			if len(buf) == 0 {
				return nil, eofClean, nil
			}
			return nil, eofPartial, nil
		default:
			return nil, lineOK, rerr
		}
	}
}
