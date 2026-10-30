package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/voyage"
)

// checkRecordPath refuses an existing --record target before a session opens.
// voyage.Create still uses O_EXCL as the authoritative guard; this check makes
// the common case fail early (and, unlike a race, without starting a runtime).
func checkRecordPath(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("record file already exists: %s (refusing to overwrite)", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("checking record path %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("record directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("record directory %s is not a directory", dir)
	}
	return nil
}

// recordLog writes a voyage log while the run proceeds instead of after it:
// every record the session's event tap hands over is appended (and fsynced)
// inline, so a killed process leaves a durable prefix rather than nothing.
//
// The file is created lazily, on the first root record that names a session,
// because the header carries that session id; records seen before then are
// buffered and flushed in arrival order. voyage.Create keeps O_EXCL, so a
// --record target is never overwritten: a recording is evidence.
//
// One mutex guards the whole state, writer included. Add runs inline on the
// kernel's appending goroutine, so Finish and Abandon (on the main goroutine)
// must be able to wait for an in-flight append. Nothing here calls back into
// the session, which is what keeps that wait from deadlocking.
type recordLog struct {
	mu     sync.Mutex
	path   string
	header func(sessionID string) voyage.Header
	w      *voyage.Writer

	lastSeq   uint64
	sessionID string
	pending   []oarlock.Record

	// err is the first failure; it is sticky, because a log that lost a record
	// must never later look complete.
	err error
	// abandoned is the second-SIGINT path: no end line is written at all.
	abandoned bool
}

// newRecordLog returns a log that creates path on its first root record.
// header is called with the root stream's session id ("" when the stream never
// named one) and must be cheap: it runs under the log's mutex.
func newRecordLog(path string, header func(sessionID string) voyage.Header) *recordLog {
	return &recordLog{path: path, header: header}
}

// Add is the Session.Events callback. It never panics and never blocks a
// control call beyond the append it performs inline: one write plus fsync per
// record, which is the durability the file exists for.
//
// A nil log is a no-op, so a caller without --record needs no branch. After the
// first error every call is a no-op too: the log is already as good as it gets.
func (l *recordLog) Add(rec oarlock.Record) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.abandoned || l.err != nil {
		return
	}
	if l.w == nil {
		if rec.SessionID == "" || len(rec.AgentPath) != 0 {
			// The header needs the root stream's session id: hold the record
			// until a root record names one.
			l.pending = append(l.pending, rec)
			return
		}
		if err := l.openLocked(rec.SessionID); err != nil {
			l.failLocked(err)
			return
		}
		if !l.flushPendingLocked() {
			return
		}
	}
	if err := l.appendLocked(rec); err != nil {
		l.failLocked(err)
	}
}

// Finish appends whatever the tap has not seen yet and writes the end line.
// all is the session's retained stream, dispose included; a record the tap
// already wrote is skipped by Seq, so the file stays dense.
//
// A log whose writer never opened (the stream never named a root session) is
// still created here with an empty session id, so --record keeps producing a
// file for a runtime that reports none. A sticky error leaves the file
// truncated and is returned unchanged.
func (l *recordLog) Finish(all []oarlock.Record, reason string) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.abandoned {
		return nil
	}
	if l.err != nil {
		return l.err
	}
	if l.w == nil {
		if err := l.openLocked(""); err != nil {
			l.failLocked(err)
			return l.err
		}
		if !l.flushPendingLocked() {
			return l.err
		}
	}
	for _, rec := range all {
		if rec.Seq <= l.lastSeq {
			continue
		}
		if err := l.appendLocked(rec); err != nil {
			l.failLocked(err)
			return l.err
		}
	}
	if err := l.w.End(reason); err != nil {
		l.failLocked(err)
		return l.err
	}
	return nil
}

// Abandon freezes the written prefix without an end line: the process is on
// its way out (second SIGINT) and a truncated log is the honest shape of a run
// that did not finish. It takes the mutex, so an append already running
// completes first. The descriptor is left to the OS, which is reclaiming the
// process anyway.
func (l *recordLog) Abandon() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.abandoned = true
	l.pending = nil
}

// openLocked creates the log with the given session id. The caller holds the
// mutex.
func (l *recordLog) openLocked(sessionID string) error {
	w, err := voyage.Create(l.path, l.header(sessionID), nil)
	if err != nil {
		return err
	}
	l.w = w
	l.sessionID = sessionID
	return nil
}

// appendLocked writes one record unless the log already has it. Seq is dense
// and the tap replays from zero, so "already written" is exactly "Seq is not
// above the last one written". The caller holds the mutex.
func (l *recordLog) appendLocked(rec oarlock.Record) error {
	if rec.Seq <= l.lastSeq {
		return nil
	}
	if err := l.w.Append(rec); err != nil {
		return err
	}
	l.lastSeq = rec.Seq
	return nil
}

// flushPendingLocked writes the records buffered before the log existed, in
// arrival order, and reports whether the log is still usable. The caller holds
// the mutex.
func (l *recordLog) flushPendingLocked() bool {
	pending := l.pending
	l.pending = nil
	for _, rec := range pending {
		if err := l.appendLocked(rec); err != nil {
			l.failLocked(err)
			return false
		}
	}
	return true
}

// failLocked records the first failure. The caller holds the mutex.
func (l *recordLog) failLocked(err error) {
	if l.err == nil {
		l.err = err
	}
}

// recordHeader is the voyage header for one --record file. It names what was
// requested (model/effort/serviceTier), not what the runtime later reported;
// the stream itself carries the reported values.
func (inv *runInvocation) recordHeader(sessionID string) voyage.Header {
	return voyage.Header{
		Runtime:     inv.runtimeID,
		Model:       inv.model,
		Effort:      inv.effort,
		ServiceTier: inv.serviceTier,
		Cwd:         inv.cwd,
		SessionID:   sessionID,
		StartedAt:   inv.startedAt.UnixMilli(),
		Recorder:    "oarlock-cli/" + version(),
	}
}
