package kernel

import (
	"errors"
	"fmt"
)

// ErrCursorMismatch reports a cursor that names a different stream instance.
//
// Cursors are per instance: Seq restarts at 1 when an instance is recreated, so
// applying another instance's cursor would silently replay the wrong records.
// An empty Cursor.SessionID is the zero-value "this instance" and is not a
// mismatch. Errors returned by Replay and ObserveFrom wrap this sentinel, so
// errors.Is works.
var ErrCursorMismatch = errors.New("kernel: cursor belongs to a different session instance")

// Records implements Kernel.
//
// It replays the retained records with Seq > cursor.AfterSeq, in Seq order, in
// a fresh slice. A cursor whose SessionID names another instance is not
// silently applied: it yields nil. Use Replay to observe the typed error.
func (k *sessionKernel) Records(cursor Cursor) []Record {
	records, err := k.Replay(cursor)
	if err != nil {
		return nil
	}
	return records
}

// Replay implements Kernel.
//
// The zero cursor replays the whole instance: the first record has Seq 1 and
// only records with Seq > 0 exist. An empty cursor.SessionID is the zero-value
// "this instance" and matches; a non-empty one that differs from the kernel's
// session is refused with an error wrapping ErrCursorMismatch, and nothing is
// replayed.
func (k *sessionKernel) Replay(cursor Cursor) ([]Record, error) {
	if err := k.validateCursor(cursor); err != nil {
		return nil, err
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	// Seq is dense and starts at 1, so the retained log holds index == Seq-1
	// and everything after a cursor is one suffix. Copy it: the caller gets a
	// fresh slice and cannot shorten the log.
	if cursor.AfterSeq >= uint64(len(k.log)) {
		return []Record{}, nil
	}
	out := make([]Record, len(k.log)-int(cursor.AfterSeq))
	copy(out, k.log[cursor.AfterSeq:])
	return out, nil
}

// validateCursor refuses a cursor that names another stream instance.
//
// An empty SessionID is the zero-value "this instance" and always matches, so
// Cursor{} replays the whole instance from the start. Only a non-empty
// SessionID that differs from the kernel's session is refused: Seq restarts at
// 1 when an instance is recreated, so applying another instance's cursor would
// replay the wrong records.
func (k *sessionKernel) validateCursor(cursor Cursor) error {
	if cursor.SessionID == "" || cursor.SessionID == k.sessionID {
		return nil
	}
	return fmt.Errorf("kernel: cursor for session %q cannot be applied to session %q: %w",
		cursor.SessionID, k.sessionID, ErrCursorMismatch)
}
