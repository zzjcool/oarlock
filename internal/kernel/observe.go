package kernel

import (
	"sort"
	"sync"
)

// Observe implements Kernel.
//
// Dispatch is inline on the appending goroutine and observers are called in
// registration order. There is no replay: only records appended after this call
// are delivered. The returned cancel is idempotent and removes the observer; it
// takes effect for the rest of the current delivery batch too, so after it
// returns at most the callback that is already running may finish.
//
// A nil fn is accepted and observes nothing.
func (k *sessionKernel) Observe(fn func(Record)) (cancel func()) {
	if fn == nil {
		return func() {}
	}
	// after starts at the current Seq: a bare Observe never replays, not even
	// a record that a concurrent drain has queued but not yet delivered.
	k.mu.Lock()
	entry := k.addObserverLocked(fn, k.seq)
	k.mu.Unlock()
	return k.cancelFunc(entry.id)
}

// ObserveFrom implements Kernel.
//
// It replays every retained record with Seq > cursor.AfterSeq synchronously,
// registers the observer, then drains, so a record appended while the replay
// runs is neither lost nor delivered twice. The cursor is validated first: an
// empty SessionID means this instance, and only a non-empty SessionID naming
// another instance is reported as an error wrapping ErrCursorMismatch, with
// nothing registered.
//
// ObserveFrom must not be called from inside an observer callback: the
// callback runs while the publisher role is held, and ObserveFrom needs that
// role, so calling it there blocks forever. Appending from a callback is safe
// (the record is queued for the active drain).
func (k *sessionKernel) ObserveFrom(cursor Cursor, fn func(Record)) (cancel func(), err error) {
	if err := k.validateCursor(cursor); err != nil {
		return nil, err
	}
	if fn == nil {
		return func() {}, nil
	}

	// The publisher role is held across replay and drain, which is what makes
	// the seam atomic: no other goroutine can deliver while we replay, and
	// anything appended meanwhile is queued for the drain below.
	k.publishMu.Lock()
	defer k.publishMu.Unlock()

	k.mu.Lock()
	entry := k.addObserverLocked(fn, cursor.AfterSeq)
	replay := make([]Record, 0, len(k.log))
	for _, record := range k.log {
		if record.Seq > cursor.AfterSeq {
			replay = append(replay, record)
		}
	}
	k.publishing = true
	k.mu.Unlock()

	for _, record := range replay {
		k.deliver(entry, record)
	}
	k.drain()

	return k.cancelFunc(entry.id), nil
}

// addObserverLocked appends one entry to the registry. The caller holds mu.
// after is the last Seq this observer is considered to have seen, so replay and
// live delivery can never hand it the same record twice.
func (k *sessionKernel) addObserverLocked(fn func(Record), after uint64) *observerEntry {
	k.nextObsID++
	entry := &observerEntry{id: k.nextObsID, fn: fn, after: after}
	k.observers[entry.id] = entry
	return entry
}

// cancelFunc returns an idempotent unsubscribe for one observer id.
//
// Cancelling flags the entry under mu and removes it from the registry: the
// flag is what makes the rest of an already-snapshotted delivery batch skip
// this observer, so only a callback that is running right now can still finish.
func (k *sessionKernel) cancelFunc(id uint64) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			k.mu.Lock()
			if entry, registered := k.observers[id]; registered {
				entry.cancelled.Store(true)
				delete(k.observers, id)
			}
			k.mu.Unlock()
		})
	}
}

// publish becomes the publisher role when nobody holds it. The role is what
// makes dispatch single-threaded and Seq-ordered; it is released by drain once
// the notify queue is empty.
func (k *sessionKernel) publish() {
	k.publishMu.Lock()
	defer k.publishMu.Unlock()

	k.mu.Lock()
	if k.publishing {
		k.mu.Unlock()
		return
	}
	k.publishing = true
	k.mu.Unlock()

	k.drain()
}

// drain dispatches the queued retained records to the observers that are
// registered when each batch is taken, in Seq order, calling the observers in
// registration order. The caller holds publishMu and has set publishing; drain
// clears publishing before it returns, so queued work is never stranded and
// there is exactly one publisher at a time.
//
// A record appended from inside an observer is enqueued and picked up by this
// loop, which is why an observer never sees a later record before an earlier
// one.
func (k *sessionKernel) drain() {
	for {
		k.mu.Lock()
		batch := k.notified
		k.notified = nil
		if len(batch) == 0 {
			k.publishing = false
			k.mu.Unlock()
			return
		}
		taps := make([]*observerEntry, 0, len(k.observers))
		for _, entry := range k.observers {
			taps = append(taps, entry)
		}
		k.mu.Unlock()

		// Registration order, not map order: a host (and a test) can rely on
		// which observer runs first for a record.
		sort.Slice(taps, func(i, j int) bool { return taps[i].id < taps[j].id })

		for _, record := range batch {
			for _, entry := range taps {
				k.deliver(entry, record)
			}
		}
	}
}

// deliver hands one record to one observer. It is called without either mutex
// held, so an observer may append (or cancel another observer) from inside the
// callback.
//
// A cancelled observer is skipped, which is what makes cancel bite for the rest
// of the current batch. A record at or before this observer's after position was
// already delivered to it, so it is skipped too; that is what removes the
// duplicate across the ObserveFrom seam.
// A panicking observer is recovered here so later observers and the run are
// unaffected: observers are a side-tap, never part of the run.
func (k *sessionKernel) deliver(entry *observerEntry, record Record) {
	if entry.cancelled.Load() {
		return
	}
	if record.Seq <= entry.after {
		return
	}
	entry.after = record.Seq
	defer func() { _ = recover() }()
	entry.fn(record)
}
