# Observers: fan-out and backpressure

**Status:** contract for [#8](../../issues/0008-observers-backpressure.md).
Implementation: [`internal/kernel`](../../internal/kernel) (`Observe`,
`ObserveFrom`, `ObserveBuffered`). Reference behaviour: OAR
`packages/oar/src/shared/session-kernel.ts`, `deliver()`. Stream semantics:
[record-stream.md](record-stream.md).

An observer is a **side tap** on the record stream. It sees every retained
record the kernel publishes, in `Seq` order, and it can never change, delay or
block the run that produces them.

## Three ways to observe

| call | delivery | a slow callback | loss |
|---|---|---|---|
| `Observe(fn)` | inline on the appending goroutine, no replay | **slows the run** (documented, see below) | none |
| `ObserveFrom(cursor, fn)` | replay then inline, no gap and no duplicate | **slows the run** | none |
| `ObserveBuffered(fn, size, policy, ...)` | `fn` runs on its own goroutine behind a bounded buffer | **never** slows the run | by policy, always counted |

`Observe` and `ObserveFrom` are unchanged by this document. They are the right
tool for a cheap, trusted callback (a counter, a fold, a channel send that
cannot block). Anything that can be slow (a UI, a socket, a disk, another
process) must use `ObserveBuffered`.

All three isolate panics: every callback runs under its own `recover`, per
observer per record, so a failing observer cannot affect a later observer or
the run. This is the Go form of OAR `deliver()`, which swallows the error and
carries on.

## `ObserveBuffered`

```go
func (k Kernel) ObserveBuffered(fn func(Record), size int, policy Policy, opts ...BufferedOption) (*BufferedObserver, error)
```

Like `Observe` it has no replay: only records appended after the call are
offered. `size` is the number of records that may be **waiting**; the record
currently inside `fn` does not count. It is also the memory bound: the ring is
allocated once, at registration, and never grows.

Misconfiguration is refused with an error wrapping `ErrBufferedConfig`
(`errors.Is` works), and nothing is registered and no goroutine is started:
nil `fn`; `size < 1`; an unknown policy (the zero `Policy` is invalid on
purpose, the caller must choose); `SignalGap` without `OnGap`; `OnGap` with a
policy that never signals.

### The three policies

When the buffer already holds `size` waiting records and another arrives:

| policy | what is discarded | what the consumer is told |
|---|---|---|
| `DropNewest` | the arriving record | nothing; `Dropped()` counts it |
| `DropOldest` | the oldest waiting record; the arriving one is queued | nothing; `Dropped()` counts it |
| `SignalGap` | the oldest waiting record, as `DropOldest` | **exactly which `Seq` range was lost**, through `OnGap` |

`DropOldest` is for a live view that only cares about "now" (a progress
display). `DropNewest` is for a consumer that must see a contiguous prefix and
would rather lose the tail than the head. `SignalGap` is for a consumer that
must *know* it missed something and may want to repair it from the log.

### `Gap` is not a record

```go
type Gap struct{ From, To uint64 } // closed interval, From <= To, From >= 1
```

A `Gap` is delivered through its own callback, `OnGap(func(Gap))`. It is never
a `Record` and never a `Frame`, it is never appended to the stream, never
retained, and never visible to any other observer. Record invariant 1
(*no synthesized facts*) is therefore untouched: the kernel invents no record
to say that records were lost, it tells **one observer** about **its own**
loss, on a separate channel of information.

For one observer, once the buffer has drained, the delivered records and the
reported gaps **partition** the offered `Seq` range exactly once, in increasing
order, with no overlap and no hole, and `Dropped()` equals the summed length of
the gaps. Because `Seq` is dense, a loss is always one contiguous run, and
consecutive losses are merged into a single maximal `Gap`. A gap is always
delivered *before* the first record that follows it, and never trails the last
record: eviction happens together with the arrival of a newer record.

Recovering is the consumer's choice: ignore the gap, or fetch it with
`Replay(Cursor{AfterSeq: g.From - 1})` and stop at `g.To`. The retained log is
complete, so a gap never means a fact is lost, only that this observer's live
view skipped it.

### Never blocks the run

The append path calls the observer's `offer` inline. It does one atomic store,
one short critical section that writes a slot of a pre-allocated ring, and one
wake-up signal. It allocates nothing, sends on no channel, waits for no
consumer and holds no lock while user code runs. Its cost is therefore bounded
by a constant, **independent of how slow `fn` is, of `size`, and of the
policy**. `fn` and `OnGap` run on one dedicated goroutine per observer.

### Cancel

`BufferedObserver.Cancel()` is idempotent, never blocks, and is safe to call
from inside `fn` or `OnGap`. After it returns:

- nothing new is queued and the kernel no longer calls the observer;
- every record still waiting is discarded **undelivered**, together with any
  pending gap. These are not drops: `Dropped()` stays what it was;
- at most the callback that was already running may finish.

`Done()` is closed once the consumer goroutine has exited, which is after
`Cancel` and after the in-flight callback returned. A host that must not leak
a goroutine waits on `Done()`; a callback that never returns is the host's bug
and is reported by `Done()` never closing, not hidden.

### Metrics

| method | meaning |
|---|---|
| `Dropped()` | records the policy discarded; cancel-time discards are not counted |
| `LastSeq()` | newest `Seq` the kernel *offered* to this observer (queued or dropped); `0` before the first |
| `DeliveredSeq()` | `Seq` of the last record handed to `fn`, set just before `fn` runs; `0` before the first |

All three are lock-free reads. The observer's backlog at any moment is
`LastSeq() - DeliveredSeq() - Dropped()` once the consumer is idle, which is
enough to alert on without touching the buffer.

## Why there is no backpressure

Backpressure would mean the appending side waits for a slow observer. In oarlock
the appending side is the adapter's pump: the goroutine that reads the runtime's
stdout and turns each frame into a record. Making it wait is the one thing it
must not do.

1. **Waiting stalls the runtime.** A pump that waits stops reading the pipe.
   The agent blocks when it next writes to stdout, so a slow UI can freeze a
   running agent, and a UI that is waiting for the agent's next output while the
   agent is waiting for the UI to drain is a deadlock, not a slowdown. The
   observer exists to *watch* the run; it must not be able to *stop* it.
2. **Nothing is lost that cannot be recovered.** The record stream is lossless
   and retained: `Replay`, `Records` and `ObserveFrom` hand back every record by
   `Seq`. Backpressure is the right answer when the data would otherwise be
   gone. Here it is not: a dropped delivery costs one observer its live view,
   never a fact, and the observer can read the log whenever it is ready.
3. **A loss can be named exactly.** `Seq` is dense, so what a consumer missed is
   always the closed interval `[From, To]`, which is cheap to report and cheap
   to repair. A policy that tells the consumer is strictly more useful than a
   policy that stalls the producer to avoid telling it.
4. **The reference behaves the same.** OAR's `deliver()` calls the observer
   synchronously, swallows whatever it throws, and never awaits it. Go makes the
   opposite mistake easy: a plain `ch <- record` blocks the sender once the
   channel is full. A bounded buffer with an explicit, named policy is how the
   same guarantee is stated in Go instead of being left to the channel's default.

What this does **not** do: it does not make a slow *inline* observer fast.
`Observe(fn)` runs `fn` on the appending goroutine by design, so a slow `fn`
there does slow the run. That is why the slow path has its own call.

## Non-goals

- Network transport, SSE or any wire format for observers (the host).
- Cross-process fan-out.
- Buffering for `ObserveFrom` replay: replay is synchronous and exact by design.
- Priority, batching or rate limiting between observers.

## Verification

```sh
gofmt -l .            # empty
go vet ./...
go test -race ./...
go test -run '^$' -bench 'Append_' ./internal/kernel
```

Acceptance criteria of [#8](../../issues/0008-observers-backpressure.md) and the
test that pins each:

| criterion | evidence |
|---|---|
| a 100ms consumer + 10k records does not drag the run's p99 | `TestObserveBuffered_AppendP99BoundedWithSlowConsumer`, `BenchmarkAppend_SlowBufferedObserver` |
| the append never blocks, whatever the consumer does | `TestObserveBuffered_AppendNeverBlocksOnAStuckConsumer` |
| `SignalGap` reports the exact missed `[a,b]` | `TestObserveBuffered_SignalGapReportsExactRanges`, `TestObserveBuffered_SignalGapPartitionProperty`, `TestObserveBuffered_SignalGapNeverFabricatesRecords` |
| concurrent observe + cancel leaves no goroutine behind | `TestObserveBuffered_ConcurrentObserveAndCancelLeaveNoGoroutines` |
| `DropOldest` / `DropNewest` keep exactly the documented set | `TestObserveBuffered_DropOldestKeepsExactlyTheNewest`, `TestObserveBuffered_DropNewestKeepsExactlyTheOldest` |
| panics are isolated | `TestObserveBuffered_PanicInCallbackIsIsolated` |
| why there is no backpressure | this document |
