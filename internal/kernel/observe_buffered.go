package kernel

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Policy says what a full ObserveBuffered buffer discards. The zero value is
// not a policy: the caller must choose one, because "what is lost" is the
// decision this call exists to make explicit.
type Policy uint8

const (
	// DropOldest evicts the oldest waiting record and queues the arriving one.
	// The consumer is not told.
	DropOldest Policy = iota + 1
	// DropNewest discards the arriving record and keeps what is waiting. The
	// consumer is not told.
	DropNewest
	// SignalGap evicts like DropOldest and reports the lost Seq range through
	// the OnGap callback, merged into the largest contiguous Gap.
	SignalGap
)

// String implements fmt.Stringer.
func (p Policy) String() string {
	switch p {
	case DropOldest:
		return "DropOldest"
	case DropNewest:
		return "DropNewest"
	case SignalGap:
		return "SignalGap"
	default:
		return fmt.Sprintf("Policy(%d)", uint8(p))
	}
}

// Gap is the closed Seq interval [From, To] that the kernel offered to one
// observer and that observer did not receive. From <= To and From >= 1.
//
// A Gap is NOT a Record. It is delivered through its own callback (OnGap), is
// never appended to the stream and is invisible to every other observer, so
// the "no synthesized facts" invariant is untouched: the kernel reports one
// observer's own loss, it does not invent a record.
type Gap struct {
	From, To uint64
}

// Len is the number of records the gap covers.
func (g Gap) Len() uint64 {
	if g.To < g.From {
		return 0
	}
	return g.To - g.From + 1
}

// ErrBufferedConfig is wrapped by every ObserveBuffered configuration error,
// so errors.Is works. When it is returned nothing is registered and no
// goroutine is started.
var ErrBufferedConfig = errors.New("kernel: invalid buffered observer configuration")

// BufferedOption configures ObserveBuffered.
type BufferedOption func(*bufferedConfig)

type bufferedConfig struct {
	onGap    func(Gap)
	onGapSet bool
}

// OnGap sets the callback that receives lost Seq ranges. It is required by
// SignalGap and refused by the other policies. It runs on the observer's
// consumer goroutine, before the first record that follows the gap, and is
// panic-isolated like the record callback.
func OnGap(fn func(Gap)) BufferedOption {
	return func(c *bufferedConfig) {
		c.onGap = fn
		c.onGapSet = true
	}
}

// BufferedObserver is a bounded, non-blocking observer. See ObserveBuffered.
//
// Why there is no backpressure (full text: docs/spec/observers.md): the
// appending goroutine is the adapter's pump, the one that reads the runtime's
// stdout. If it waited for a slow observer it would stop reading the pipe, the
// agent would block on its next write, and a slow UI could freeze a running
// agent, or deadlock with it. The stream is lossless and retained (Replay,
// Records, ObserveFrom), so a dropped delivery costs one observer its live view
// and never a fact; Seq is dense, so what was missed is always exactly
// nameable as a Gap. OAR's deliver() likewise calls the observer synchronously,
// swallows its failure and never awaits it. Go's default for a full channel is
// the opposite (the sender blocks), which is why the policy is explicit here.
//
// size is the memory bound: the ring of size records is allocated once at
// registration and never grows. Plain Observe is unchanged: it stays inline on
// the appending goroutine, so a slow callback there still slows the run.
type BufferedObserver struct {
	fn     func(Record)
	onGap  func(Gap)
	policy Policy

	// cancelKernel unregisters the inline tap (Observe's cancel).
	cancelKernel func()

	// Lock-free metrics.
	dropped      atomic.Uint64
	lastSeq      atomic.Uint64
	deliveredSeq atomic.Uint64

	done chan struct{}
	once sync.Once

	// mu guards the ring, the pending gap and closed. It is never held while
	// user code runs and is never held together with a kernel lock.
	mu     sync.Mutex
	wake   *sync.Cond
	ring   []Record
	head   int
	n      int
	gap    Gap // pending gap; zero value (From == 0) means none
	closed bool
}

// ObserveBuffered registers an observer whose callback runs on its own
// goroutine behind a bounded buffer of size records. Like Observe it has no
// replay: only records appended after the call are offered.
//
// The append side only ever does O(1) work for this observer (one atomic
// store, one short critical section writing a pre-allocated ring slot, one
// wake-up). It never waits for fn, never allocates and never blocks, whatever
// the policy and however slow fn is. When the buffer is full the policy
// decides what is discarded (see Policy); every discard is counted in
// Dropped, and with SignalGap the consumer is also told the exact lost range
// through OnGap.
//
// size counts records waiting; the record currently inside fn does not count.
// fn and OnGap each run under their own recover, so a panic loses nothing but
// that one call. Cancel releases the consumer; Done reports that it exited.
//
// A configuration error wraps ErrBufferedConfig and registers nothing: nil fn,
// size < 1, an unknown policy, SignalGap without OnGap, or OnGap with a
// policy that never signals.
func (k *sessionKernel) ObserveBuffered(fn func(Record), size int, policy Policy, opts ...BufferedOption) (*BufferedObserver, error) {
	var cfg bufferedConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	switch {
	case fn == nil:
		return nil, fmt.Errorf("%w: nil callback", ErrBufferedConfig)
	case size < 1:
		return nil, fmt.Errorf("%w: size %d, want at least 1", ErrBufferedConfig, size)
	case policy != DropOldest && policy != DropNewest && policy != SignalGap:
		return nil, fmt.Errorf("%w: unknown policy %s", ErrBufferedConfig, policy)
	case policy == SignalGap && cfg.onGap == nil:
		return nil, fmt.Errorf("%w: SignalGap needs OnGap", ErrBufferedConfig)
	case policy != SignalGap && cfg.onGapSet:
		return nil, fmt.Errorf("%w: OnGap is only meaningful with SignalGap, not %s", ErrBufferedConfig, policy)
	}

	b := &BufferedObserver{
		fn:     fn,
		onGap:  cfg.onGap,
		policy: policy,
		ring:   make([]Record, size),
		done:   make(chan struct{}),
	}
	b.wake = sync.NewCond(&b.mu)

	go b.run()
	// Observe is unchanged; the buffered observer is just an inline tap whose
	// callback is the O(1) offer.
	b.cancelKernel = k.Observe(b.offer)
	return b, nil
}

// offer runs inline on the appending goroutine. It is the whole append-side
// cost of a buffered observer and must stay O(1) and non-blocking.
func (b *BufferedObserver) offer(record Record) {
	b.lastSeq.Store(record.Seq)

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	if b.n == len(b.ring) {
		if b.policy == DropNewest {
			b.dropped.Add(1)
			b.mu.Unlock()
			return
		}
		// DropOldest and SignalGap evict the head. Seq is dense and the queue
		// is in Seq order, so the evicted Seq is always adjacent to the pending
		// gap: merging is just extending To.
		evicted := b.ring[b.head].Seq
		b.ring[b.head] = Record{}
		b.head = (b.head + 1) % len(b.ring)
		b.n--
		b.dropped.Add(1)
		if b.policy == SignalGap {
			if b.gap.From == 0 {
				b.gap.From = evicted
			}
			b.gap.To = evicted
		}
	}
	b.ring[(b.head+b.n)%len(b.ring)] = record
	b.n++
	b.mu.Unlock()
	b.wake.Signal()
}

// take blocks until a record is waiting or the observer is closed. It returns
// the pending gap together with the head record atomically, so two gaps are
// never adjacent and a gap is always followed by the record after it.
func (b *BufferedObserver) take() (gap Gap, record Record, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.n == 0 && !b.closed {
		b.wake.Wait()
	}
	if b.closed {
		return Gap{}, Record{}, false
	}
	record = b.ring[b.head]
	b.ring[b.head] = Record{}
	b.head = (b.head + 1) % len(b.ring)
	b.n--
	gap, b.gap = b.gap, Gap{}
	return gap, record, true
}

func (b *BufferedObserver) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// run is the consumer goroutine. User code runs here, outside every lock.
func (b *BufferedObserver) run() {
	defer close(b.done)
	for {
		gap, record, ok := b.take()
		if !ok {
			return
		}
		if gap.From != 0 && b.onGap != nil && !b.isClosed() {
			callGap(b.onGap, gap)
		}
		if b.isClosed() {
			return
		}
		b.deliveredSeq.Store(record.Seq)
		callRecord(b.fn, record)
	}
}

// callRecord and callGap isolate a panicking callback: the consumer goroutine
// survives and the next record is still delivered.
func callRecord(fn func(Record), record Record) {
	defer func() { _ = recover() }()
	fn(record)
}

func callGap(fn func(Gap), gap Gap) {
	defer func() { _ = recover() }()
	fn(gap)
}

// Cancel stops the observer. It is idempotent, never blocks and is safe to call
// from inside fn or OnGap. After it returns nothing new is queued, every record
// still waiting (and any pending gap) is discarded undelivered, and at most the
// callback that was already running may finish. Cancel-time discards are not
// counted in Dropped.
func (b *BufferedObserver) Cancel() {
	b.once.Do(func() {
		// The kernel lock is released before b.mu is taken: never nested.
		b.cancelKernel()
		b.mu.Lock()
		b.closed = true
		b.ring = nil
		b.head, b.n = 0, 0
		b.gap = Gap{}
		b.mu.Unlock()
		b.wake.Broadcast()
	})
}

// Dropped is the number of records the policy discarded.
func (b *BufferedObserver) Dropped() uint64 { return b.dropped.Load() }

// LastSeq is the newest Seq the kernel offered to this observer, whether it was
// queued or dropped. It is 0 before the first record.
func (b *BufferedObserver) LastSeq() uint64 { return b.lastSeq.Load() }

// DeliveredSeq is the Seq of the last record handed to fn, set just before fn
// runs. It is 0 before the first record.
func (b *BufferedObserver) DeliveredSeq() uint64 { return b.deliveredSeq.Load() }

// Done is closed once the consumer goroutine has exited, which is after Cancel
// and after the callback that was in flight returned.
func (b *BufferedObserver) Done() <-chan struct{} { return b.done }
