package kernel

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
)

// sink collects what one buffered observer was handed, in order. A record is
// shown as its Seq; a gap as the closed range it names. completed is the Seq of
// the last record whose fn call has fully returned, which is what a test waits
// on instead of sleeping.
type sink struct {
	mu        sync.Mutex
	events    []any // uint64 (record Seq) or Gap
	recs      []Record
	completed atomic.Uint64
}

func (s *sink) record(r Record) {
	s.mu.Lock()
	s.events = append(s.events, r.Seq)
	s.recs = append(s.recs, r)
	s.mu.Unlock()
}

func (s *sink) finish(r Record) { s.completed.Store(r.Seq) }

func (s *sink) gap(g Gap) {
	s.mu.Lock()
	s.events = append(s.events, g)
	s.mu.Unlock()
}

func (s *sink) snapshot() []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]any(nil), s.events...)
}

func (s *sink) seqs() []uint64 {
	var out []uint64
	for _, e := range s.snapshot() {
		if seq, ok := e.(uint64); ok {
			out = append(out, seq)
		}
	}
	return out
}

// gate parks the consumer inside fn until the test opens it, so a scenario
// never depends on timing for its outcome.
type gate struct {
	entered chan struct{}
	release chan struct{}
	enter   sync.Once
	open    sync.Once
}

func newGate() *gate {
	return &gate{entered: make(chan struct{}), release: make(chan struct{})}
}

// park is called from inside fn: it announces arrival, then blocks until opened.
func (g *gate) park() {
	g.enter.Do(func() { close(g.entered) })
	<-g.release
}

func (g *gate) waitParked(t *testing.T) {
	t.Helper()
	waitClosed(t, "the consumer to park inside fn", g.entered)
}

func (g *gate) openUp() { g.open.Do(func() { close(g.release) }) }

func waitClosed(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not happen within 10s", what)
	}
}

// await polls cond up to a deadline. The poll interval is not a correctness
// assumption: it only bounds how long a failing test takes to fail.
func await(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func appendFrames(k *sessionKernel, n int) {
	for i := 0; i < n; i++ {
		k.Frame(frameBody("f"), RecordAt{})
	}
}

func gapOpts(policy Policy, onGap func(Gap)) []BufferedOption {
	if policy == SignalGap {
		return []BufferedOption{OnGap(onGap)}
	}
	return nil
}

func promptBody() oarlock.RequestBody {
	return oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "hi"}
}

// parked runs the reference scenario: a buffered observer whose consumer parks
// INSIDE fn on Seq 1, then Seq 2..last are appended, then the consumer is
// released and runs until its final delivery (Seq final) has returned.
func parked(t *testing.T, policy Policy, size int, last, final uint64) (*sink, *BufferedObserver, *sessionKernel) {
	t.Helper()
	k := newKernel("sess-1", nil)
	s := &sink{}
	g := newGate()
	b, err := k.ObserveBuffered(func(r Record) {
		s.record(r)
		if r.Seq == 1 {
			g.park()
		}
		s.finish(r)
	}, size, policy, gapOpts(policy, s.gap)...)
	if err != nil {
		t.Fatalf("ObserveBuffered: %v", err)
	}
	t.Cleanup(func() { g.openUp(); b.Cancel(); <-b.Done() })

	appendFrames(k, 1) // Seq 1: the consumer takes it and parks
	g.waitParked(t)
	appendFrames(k, int(last)-1)
	g.openUp()
	await(t, "the consumer to finish its last delivery", func() bool { return s.completed.Load() == final })
	return s, b, k
}

// S3: DropOldest keeps the record in flight and exactly the newest `size`.
func TestObserveBuffered_DropOldestKeepsExactlyTheNewest(t *testing.T) {
	s, b, _ := parked(t, DropOldest, 3, 10, 10)
	if got, want := s.seqs(), []uint64{1, 8, 9, 10}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DropOldest delivered %v, want %v", got, want)
	}
	if b.Dropped() != 6 {
		t.Fatalf("Dropped() = %d, want 6", b.Dropped())
	}
	if b.LastSeq() != 10 || b.DeliveredSeq() != 10 {
		t.Fatalf("LastSeq/DeliveredSeq = %d/%d, want 10/10", b.LastSeq(), b.DeliveredSeq())
	}
}

// S4: DropNewest keeps the record in flight and exactly the oldest `size`.
func TestObserveBuffered_DropNewestKeepsExactlyTheOldest(t *testing.T) {
	s, b, _ := parked(t, DropNewest, 3, 10, 4)
	if got, want := s.seqs(), []uint64{1, 2, 3, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DropNewest delivered %v, want %v", got, want)
	}
	if b.Dropped() != 6 {
		t.Fatalf("Dropped() = %d, want 6", b.Dropped())
	}
	if b.LastSeq() != 10 {
		t.Fatalf("LastSeq() = %d, want 10 (offered, even though dropped)", b.LastSeq())
	}
	if b.DeliveredSeq() != 4 {
		t.Fatalf("DeliveredSeq() = %d, want 4", b.DeliveredSeq())
	}
}

// S1 + S2: SignalGap names the exact lost range, before the first record after
// it, and a second gap on the same observer is independent of the first.
func TestObserveBuffered_SignalGapReportsExactRanges(t *testing.T) {
	k := newKernel("sess-1", nil)
	s := &sink{}
	g1, g2 := newGate(), newGate()
	b, err := k.ObserveBuffered(func(r Record) {
		s.record(r)
		switch r.Seq {
		case 1:
			g1.park()
		case 11:
			g2.park()
		}
		s.finish(r)
	}, 3, SignalGap, OnGap(s.gap))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g1.openUp(); g2.openUp(); b.Cancel(); <-b.Done() })

	// First burst: park on 1, append 2..10 -> ring ends as 8,9,10, lost [2,7].
	appendFrames(k, 1)
	g1.waitParked(t)
	appendFrames(k, 9)
	g1.openUp()
	await(t, "the first burst to drain", func() bool { return s.completed.Load() == 10 })

	want1 := []any{uint64(1), Gap{From: 2, To: 7}, uint64(8), uint64(9), uint64(10)}
	if got := s.snapshot(); !reflect.DeepEqual(got, want1) {
		t.Fatalf("first burst: consumer saw %v, want %v", got, want1)
	}
	if b.Dropped() != 6 || b.LastSeq() != 10 || b.DeliveredSeq() != 10 {
		t.Fatalf("Dropped/LastSeq/DeliveredSeq = %d/%d/%d, want 6/10/10",
			b.Dropped(), b.LastSeq(), b.DeliveredSeq())
	}

	// Second burst: park on 11, append 12..16 -> ring ends as 14,15,16, lost [12,13].
	appendFrames(k, 1)
	g2.waitParked(t)
	appendFrames(k, 5)
	g2.openUp()
	await(t, "the second burst to drain", func() bool { return s.completed.Load() == 16 })

	want := []any{
		uint64(1), Gap{From: 2, To: 7}, uint64(8), uint64(9), uint64(10),
		uint64(11), Gap{From: 12, To: 13}, uint64(14), uint64(15), uint64(16),
	}
	if got := s.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("whole run: consumer saw %v, want %v", got, want)
	}
	if b.Dropped() != 8 {
		t.Fatalf("Dropped() = %d, want 8", b.Dropped())
	}
	if (Gap{From: 2, To: 7}).Len() != 6 || (Gap{From: 12, To: 13}).Len() != 2 || (Gap{From: 5, To: 5}).Len() != 1 {
		t.Fatal("Gap.Len is wrong")
	}
}

// With an intermittent consumer, whatever interleaving happens, delivered
// records and gaps partition the offered range exactly once, in order.
func TestObserveBuffered_SignalGapPartitionProperty(t *testing.T) {
	k := newKernel("sess-1", nil)
	s := &sink{}
	b, err := k.ObserveBuffered(func(r Record) {
		s.record(r)
		if r.Seq%7 == 0 {
			runtime.Gosched()
			time.Sleep(50 * time.Microsecond)
		}
		s.finish(r)
	}, 4, SignalGap, OnGap(s.gap))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Cancel(); <-b.Done() })

	const total = 5000
	appendFrames(k, total)
	// The newest record is never evicted (an eviction accompanies a newer
	// arrival), so the consumer's final delivery is exactly Seq total.
	await(t, "the consumer to deliver the newest record", func() bool { return s.completed.Load() == total })

	next := uint64(1) // the next Seq that must be accounted for
	var gapSum uint64
	for _, e := range s.snapshot() {
		switch v := e.(type) {
		case uint64:
			if v != next {
				t.Fatalf("record %d where %d was expected: hole or overlap", v, next)
			}
			next++
		case Gap:
			if v.From == 0 || v.From > v.To {
				t.Fatalf("malformed gap %+v", v)
			}
			if v.From != next {
				t.Fatalf("gap %+v where %d was expected: hole or overlap", v, next)
			}
			gapSum += v.Len()
			next = v.To + 1
		}
	}
	if next != total+1 {
		t.Fatalf("events cover [1,%d], want [1,%d]", next-1, total)
	}
	if b.Dropped() != gapSum {
		t.Fatalf("Dropped() = %d, summed gaps = %d", b.Dropped(), gapSum)
	}
	if b.Dropped() == 0 {
		t.Fatal("the consumer never fell behind; the property was not exercised")
	}
}

// A Gap is not a Record: the stream holds only what was appended, and every
// record fn receives is the retained one.
func TestObserveBuffered_SignalGapNeverFabricatesRecords(t *testing.T) {
	s, b, k := parked(t, SignalGap, 2, 12, 12)
	if b.Dropped() == 0 {
		t.Fatal("scenario produced no gap; test is vacuous")
	}
	retained := k.Records(Cursor{})
	if len(retained) != 12 {
		t.Fatalf("the stream holds %d records, want exactly the 12 appended", len(retained))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.recs {
		if !reflect.DeepEqual(r, retained[r.Seq-1]) {
			t.Fatalf("delivered record %d differs from the retained one", r.Seq)
		}
	}
}

// A consumer parked forever must not stall the appender, for any policy. No
// timing threshold: the appends either finish or the watchdog fails the test.
func TestObserveBuffered_AppendNeverBlocksOnAStuckConsumer(t *testing.T) {
	for _, policy := range []Policy{DropOldest, DropNewest, SignalGap} {
		t.Run(policy.String(), func(t *testing.T) {
			k := newKernel("sess-1", nil)
			g := newGate()
			b, err := k.ObserveBuffered(func(Record) { g.park() }, 8, policy, gapOpts(policy, func(Gap) {})...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { g.openUp(); b.Cancel(); <-b.Done() })

			finished := make(chan struct{})
			go func() {
				defer close(finished)
				appendFrames(k, 10000)
				k.Control(promptBody(), acceptAll, RecordAt{}) // 2 more records
			}()
			waitClosed(t, "10k appends past a stuck consumer", finished)
			g.waitParked(t)
			if b.LastSeq() != 10002 {
				t.Fatalf("LastSeq() = %d, want 10002", b.LastSeq())
			}
			if b.Dropped() == 0 {
				t.Fatal("a stuck consumer behind an 8-slot buffer must have caused drops")
			}
		})
	}
}

// The run's append p99 stays far below the consumer's 100ms per-record sleep.
// The bound is absolute and generous (20ms); the structural no-blocking
// guarantee is AppendNeverBlocksOnAStuckConsumer.
func TestObserveBuffered_AppendP99BoundedWithSlowConsumer(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	for _, policy := range []Policy{DropOldest, DropNewest, SignalGap} {
		t.Run(policy.String(), func(t *testing.T) {
			k := newKernel("sess-1", nil)
			b, err := k.ObserveBuffered(func(Record) { time.Sleep(100 * time.Millisecond) }, 16, policy,
				gapOpts(policy, func(Gap) {})...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { b.Cancel(); <-b.Done() })

			const n = 10000
			body := frameBody("f")
			lat := make([]time.Duration, n)
			for i := 0; i < n; i++ {
				start := time.Now()
				k.Frame(body, RecordAt{})
				lat[i] = time.Since(start)
			}
			sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
			p50, p99, max := lat[n/2], lat[n*99/100], lat[n-1]
			t.Logf("%s: append p50=%v p99=%v max=%v dropped=%d", policy, p50, p99, max, b.Dropped())
			if p99 > 20*time.Millisecond {
				t.Fatalf("append p99 = %v with a 100ms consumer, want < 20ms", p99)
			}
			if b.Dropped() == 0 {
				t.Fatal("a 100ms consumer over 10k records must have caused drops")
			}
		})
	}
}

// fn and OnGap each run under their own recover: the consumer survives, later
// records still arrive, other observers and the run are untouched.
func TestObserveBuffered_PanicInCallbackIsIsolated(t *testing.T) {
	k := newKernel("sess-1", nil)

	var plainMu sync.Mutex
	var plain []uint64
	k.Observe(func(r Record) {
		plainMu.Lock()
		plain = append(plain, r.Seq)
		plainMu.Unlock()
	})

	// A panics on every odd Seq (after noting it); B is a healthy neighbour.
	a, healthy := &sink{}, &sink{}
	ba, err := k.ObserveBuffered(func(r Record) {
		a.record(r)
		a.finish(r)
		if r.Seq%2 == 1 {
			panic("record callback boom")
		}
	}, 64, DropOldest)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := k.ObserveBuffered(func(r Record) { healthy.record(r); healthy.finish(r) }, 64, DropOldest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ba.Cancel(); bb.Cancel(); <-ba.Done(); <-bb.Done() })

	appendFrames(k, 20)
	await(t, "the panicking observer to see Seq 20", func() bool { return a.completed.Load() == 20 })
	await(t, "the healthy observer to see Seq 20", func() bool { return healthy.completed.Load() == 20 })

	want := make([]uint64, 20)
	for i := range want {
		want[i] = uint64(i + 1)
	}
	if got := a.seqs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the panicking observer saw %v, want every record", got)
	}
	if got := healthy.seqs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the healthy buffered observer saw %v, want every record", got)
	}
	plainMu.Lock()
	gotPlain := append([]uint64(nil), plain...)
	plainMu.Unlock()
	if !reflect.DeepEqual(gotPlain, want) {
		t.Fatalf("the inline observer saw %v, want every record", gotPlain)
	}
	select {
	case <-ba.Done():
		t.Fatal("the consumer goroutine exited after a callback panic")
	default:
	}
	if out := k.Control(promptBody(), acceptAll, RecordAt{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("the run was disturbed by an observer panic: %+v", out)
	}

	// A panicking OnGap loses nothing but that one call: the record that
	// follows the gap is still delivered.
	k2 := newKernel("sess-2", nil)
	c := &sink{}
	g := newGate()
	var gapCalls atomic.Int32
	bc, err := k2.ObserveBuffered(func(r Record) {
		c.record(r)
		if r.Seq == 1 {
			g.park()
		}
		c.finish(r)
	}, 2, SignalGap, OnGap(func(Gap) {
		gapCalls.Add(1)
		panic("gap callback boom")
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.openUp(); bc.Cancel(); <-bc.Done() })
	appendFrames(k2, 1)
	g.waitParked(t)
	appendFrames(k2, 7) // 2..8 -> ring 7,8; lost [2,6]
	g.openUp()
	await(t, "Seq 8 after the panicking OnGap", func() bool { return c.completed.Load() == 8 })
	if got, wantSeqs := c.seqs(), []uint64{1, 7, 8}; !reflect.DeepEqual(got, wantSeqs) {
		t.Fatalf("after a panicking OnGap the consumer saw %v, want %v", got, wantSeqs)
	}
	if gapCalls.Load() != 1 {
		t.Fatalf("OnGap ran %d times, want 1", gapCalls.Load())
	}
}

// Cancel is idempotent, stops delivery, unregisters the kernel tap and closes
// Done.
func TestObserveBuffered_CancelIsIdempotentAndStopsDelivery(t *testing.T) {
	k := newKernel("sess-1", nil)
	s := &sink{}
	b, err := k.ObserveBuffered(func(r Record) { s.record(r); s.finish(r) }, 8, DropOldest)
	if err != nil {
		t.Fatal(err)
	}

	appendFrames(k, 3)
	await(t, "Seq 3", func() bool { return s.completed.Load() == 3 })
	b.Cancel()
	b.Cancel() // idempotent: no panic, no double unregister
	waitClosed(t, "Done after Cancel", b.Done())

	appendFrames(k, 3)
	if got, want := s.seqs(), []uint64{1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after Cancel the consumer saw %v, want only %v", got, want)
	}
	if b.LastSeq() != 3 {
		t.Fatalf("LastSeq() = %d: the kernel offered a record after Cancel", b.LastSeq())
	}
	if b.Dropped() != 0 {
		t.Fatalf("Dropped() = %d, want 0", b.Dropped())
	}
	k.mu.Lock()
	registered := len(k.observers)
	k.mu.Unlock()
	if registered != 0 {
		t.Fatalf("%d observers left after Cancel", registered)
	}
}

// Cancel is safe from inside the record callback: no deadlock, nothing after
// the cancelling record is delivered, and the consumer exits.
func TestObserveBuffered_CancelFromInsideTheCallback(t *testing.T) {
	k := newKernel("sess-1", nil)
	s := &sink{}
	var b *BufferedObserver
	var err error
	b, err = k.ObserveBuffered(func(r Record) {
		s.record(r)
		if r.Seq == 2 {
			b.Cancel()
			b.Cancel()
		}
		s.finish(r)
	}, 8, DropOldest)
	if err != nil {
		t.Fatal(err)
	}

	appendFrames(k, 5)
	waitClosed(t, "the consumer to exit after Cancel from inside fn", b.Done())
	if got, want := s.seqs(), []uint64{1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("consumer saw %v, want %v", got, want)
	}
	if b.Dropped() != 0 {
		t.Fatalf("Dropped() = %d: a cancel-time discard must not count as a drop", b.Dropped())
	}
}

// Cancelling a full buffer discards the waiting records undelivered, together
// with any pending gap, and those discards are not drops.
func TestObserveBuffered_CancelWhileFullNeverDeliversWaitingRecords(t *testing.T) {
	for _, policy := range []Policy{DropOldest, DropNewest, SignalGap} {
		t.Run(policy.String(), func(t *testing.T) {
			k := newKernel("sess-1", nil)
			s := &sink{}
			g := newGate()
			b, err := k.ObserveBuffered(func(r Record) {
				s.record(r)
				if r.Seq == 1 {
					g.park()
				}
				s.finish(r)
			}, 3, policy, gapOpts(policy, s.gap)...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { g.openUp(); b.Cancel(); <-b.Done() })

			appendFrames(k, 1)
			g.waitParked(t)
			appendFrames(k, 7) // 2..8: three waiting, four dropped
			b.Cancel()
			g.openUp()
			waitClosed(t, "Done", b.Done())

			if got, want := s.snapshot(), []any{uint64(1)}; !reflect.DeepEqual(got, want) {
				t.Fatalf("after Cancel the consumer saw %v, want only %v", got, want)
			}
			if b.Dropped() != 4 {
				t.Fatalf("Dropped() = %d, want 4 (the three discarded by Cancel are not drops)", b.Dropped())
			}
		})
	}
}

// The three accessors are zero before the first record, monotonic, and
// consistent with the scenario; reads are lock-free and race-clean.
func TestObserveBuffered_MetricsDroppedLastSeqDeliveredSeq(t *testing.T) {
	k := newKernel("sess-1", nil)
	s := &sink{}
	g := newGate()
	b, err := k.ObserveBuffered(func(r Record) {
		s.record(r)
		if r.Seq == 1 {
			g.park()
		}
		s.finish(r)
	}, 2, DropOldest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.openUp(); b.Cancel(); <-b.Done() })

	if b.Dropped() != 0 || b.LastSeq() != 0 || b.DeliveredSeq() != 0 {
		t.Fatalf("metrics before the first record = %d/%d/%d, want 0/0/0", b.Dropped(), b.LastSeq(), b.DeliveredSeq())
	}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			var dropped, last, delivered uint64
			for {
				select {
				case <-stop:
					return
				default:
				}
				d, l, v := b.Dropped(), b.LastSeq(), b.DeliveredSeq()
				if d < dropped || l < last || v < delivered {
					t.Errorf("a metric went backwards: %d/%d/%d after %d/%d/%d", d, l, v, dropped, last, delivered)
					return
				}
				dropped, last, delivered = d, l, v
			}
		}()
	}

	appendFrames(k, 1)
	g.waitParked(t)
	appendFrames(k, 5) // 2..6 with a 2-slot ring: 4,5,6 evict 2,3,4; ring 5,6
	if b.Dropped() != 3 || b.LastSeq() != 6 || b.DeliveredSeq() != 1 {
		t.Fatalf("parked: Dropped/LastSeq/DeliveredSeq = %d/%d/%d, want 3/6/1", b.Dropped(), b.LastSeq(), b.DeliveredSeq())
	}
	g.openUp()
	await(t, "Seq 6", func() bool { return s.completed.Load() == 6 })
	if b.Dropped() != 3 || b.LastSeq() != 6 || b.DeliveredSeq() != 6 {
		t.Fatalf("drained: Dropped/LastSeq/DeliveredSeq = %d/%d/%d, want 3/6/6", b.Dropped(), b.LastSeq(), b.DeliveredSeq())
	}
	close(stop)
	readers.Wait()
}

func TestObserveBuffered_RejectsInvalidConfiguration(t *testing.T) {
	k := newKernel("sess-1", nil)
	noop := func(Record) {}
	onGap := OnGap(func(Gap) {})

	invalid := []struct {
		name   string
		fn     func(Record)
		size   int
		policy Policy
		opts   []BufferedOption
	}{
		{"nil callback", nil, 4, DropOldest, nil},
		{"zero size", noop, 0, DropOldest, nil},
		{"negative size", noop, -1, DropNewest, nil},
		{"zero policy", noop, 4, 0, nil},
		{"unknown policy", noop, 4, Policy(99), nil},
		{"SignalGap without OnGap", noop, 4, SignalGap, nil},
		{"SignalGap with a nil OnGap", noop, 4, SignalGap, []BufferedOption{OnGap(nil)}},
		{"OnGap with DropOldest", noop, 4, DropOldest, []BufferedOption{onGap}},
		{"OnGap with DropNewest", noop, 4, DropNewest, []BufferedOption{onGap}},
	}
	for _, tc := range invalid {
		b, err := k.ObserveBuffered(tc.fn, tc.size, tc.policy, tc.opts...)
		if !errors.Is(err, ErrBufferedConfig) {
			t.Errorf("%s: err = %v, want one wrapping ErrBufferedConfig", tc.name, err)
		}
		if b != nil {
			t.Errorf("%s: got a handle with an error", tc.name)
		}
	}
	k.mu.Lock()
	registered := len(k.observers)
	k.mu.Unlock()
	if registered != 0 {
		t.Fatalf("%d observers registered by invalid configurations", registered)
	}

	for _, ok := range []struct {
		policy Policy
		opts   []BufferedOption
	}{
		{DropOldest, nil}, {DropNewest, []BufferedOption{nil}}, {SignalGap, []BufferedOption{onGap}},
	} {
		b, err := k.ObserveBuffered(noop, 1, ok.policy, ok.opts...)
		if err != nil || b == nil {
			t.Fatalf("%s with size 1 was refused: %v", ok.policy, err)
		}
		b.Cancel()
		waitClosed(t, "Done", b.Done())
	}

	for policy, want := range map[Policy]string{
		DropOldest: "DropOldest", DropNewest: "DropNewest", SignalGap: "SignalGap", 0: "Policy(0)", 99: "Policy(99)",
	} {
		if got := policy.String(); got != want {
			t.Errorf("Policy(%d).String() = %q, want %q", uint8(policy), got, want)
		}
	}
}

// A buffered observer is just another inline tap: plain observers around it
// stay synchronous, in registration order, in Seq order.
func TestObserveBuffered_DoesNotChangeInlineObserveSemantics(t *testing.T) {
	k := newKernel("sess-1", nil)
	var mu sync.Mutex
	var order []string
	note := func(tag string) func(Record) {
		return func(r Record) {
			mu.Lock()
			order = append(order, fmt.Sprintf("%s%d", tag, r.Seq))
			mu.Unlock()
		}
	}
	snapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), order...)
	}

	k.Observe(note("a"))
	b, err := k.ObserveBuffered(func(Record) {}, 4, DropOldest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Cancel(); <-b.Done() })
	k.Observe(note("c"))

	appendFrames(k, 1)
	// Inline: both plain observers have run by the time Frame returned.
	if got, want := snapshot(), []string{"a1", "c1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after one append plain observers saw %v, want %v", got, want)
	}
	appendFrames(k, 1)
	if got, want := snapshot(), []string{"a1", "c1", "a2", "c2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("plain observers saw %v, want %v", got, want)
	}
}

// Observe and cancel racing an appender terminate, every consumer goroutine
// exits, the registry empties, and the goroutine count returns to baseline.
func TestObserveBuffered_ConcurrentObserveAndCancelLeaveNoGoroutines(t *testing.T) {
	baseline := runtime.NumGoroutine()
	k := newKernel("sess-1", nil)

	var appender sync.WaitGroup
	appender.Add(1)
	go func() {
		defer appender.Done()
		appendFrames(k, 3000)
	}()

	const workers, rounds = 8, 50
	handles := make(chan *BufferedObserver, workers*rounds)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			policy := []Policy{DropOldest, DropNewest, SignalGap}[w%3]
			for r := 0; r < rounds; r++ {
				b, err := k.ObserveBuffered(func(Record) {}, 1+r%4, policy, gapOpts(policy, func(Gap) {})...)
				if err != nil {
					t.Errorf("ObserveBuffered: %v", err)
					return
				}
				handles <- b
				if r%2 == 0 {
					runtime.Gosched()
				}
				if r%2 == 1 { // two cancels racing each other
					var both sync.WaitGroup
					both.Add(2)
					for i := 0; i < 2; i++ {
						go func() { defer both.Done(); b.Cancel() }()
					}
					both.Wait()
				} else {
					b.Cancel()
					b.Cancel()
				}
			}
		}(w)
	}
	wg.Wait()
	appender.Wait()
	close(handles)

	for b := range handles {
		waitClosed(t, "a consumer goroutine to exit", b.Done())
	}
	k.mu.Lock()
	remaining := len(k.observers)
	k.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("%d observers leaked after every Cancel", remaining)
	}
	await(t, fmt.Sprintf("goroutine count to return to baseline %d", baseline), func() bool {
		return runtime.NumGoroutine() <= baseline
	})
	if records := k.Records(Cursor{}); len(records) != 3000 {
		t.Fatalf("the log holds %d records, want 3000", len(records))
	}
}
