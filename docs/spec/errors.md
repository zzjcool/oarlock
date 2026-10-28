# Errors, rejections and refusals

**Status:** frozen for M0. Types: [`errors.go`](../../errors.go),
[`capabilities.go`](../../capabilities.go). Contract:
[record-stream](record-stream.md).

A host must be able to react without parsing vendor prose. There are three
typed errors and eight rejection codes, and each answers a different question.

| type | question | where it appears |
|---|---|---|
| `*Rejected` | was this control taken over? | the response record of a `toRuntime` control |
| `*UnsupportedOption` | can this runtime open with this option? | the return of `Session` opening, before anything starts |
| `*SessionNotFound` | does this resume target exist? | the return of `Session` opening |

`error` is reserved for operational failure (a broken transport, a cancelled
context), not for a normal "no". A rejection is returned as a `ControlOutcome`
with `Kind == ControlRejected`; it is not an `error` value. This keeps the
common race (abort after the turn ended) out of error handling.

## The eight codes

| `Code` | trigger | caller's action |
|---|---|---|
| `busy` | a prompt arrived while a turn was running | wait for the runtime's own turn end, or abort; never resend blindly |
| `no_active_turn` | `steer`/`abort` arrived while nothing was running | a normal race; the input (if any) is still the caller's |
| `not_queued` | `withdraw` named no held input | do not assume the input was unsent; it may already have gone |
| `unsupported` | this control cannot be done with these inputs (images where the runtime takes none, empty text without images) | drop the input or change its shape; a control the runtime cannot do at all is an absent interface, not this code |
| `runtime_exited` | the stream already holds the process exit | stop controlling; resume or start a new session |
| `disposed` | the stream already holds this session's `dispose` request | stop; the session is being released |
| `runtime_refused` | the runtime answered no (typed refusal or RPC error) | read `Reason`; do not take over work the runtime refused to stop |
| `error` | the adapter could not deliver (transport failure, deadline) | inspect the wrapped cause; the caller still owns the input |

`Retryable()` is advice (`busy`, `no_active_turn`, `not_queued`), never an
automatic retry. Only the caller knows whether the input is still wanted.

The four rules:

1. **A control answer is only `accepted` or `rejected`.** Final states and
   landing points are always events. A completion event is never turned into
   a "success" return.
2. **`unsupported` is input-level, not capability-level.** A session that
   cannot steer has no `Steer` method ([capabilities](capabilities.md)); it
   never records a steer request just to reject it.
3. **A rejection preserves the caller's ownership of the input.** The adapter
   does not swallow the input, does not resend it, and does not send a
   substitute.
4. **Wrapping stays inspectable.** `Rejected.Cause` and
   `SessionNotFound.Cause` unwrap, so `errors.Is`/`errors.As` reach an
   `E2BIG`, a deadline, or the native missing-session error.

## `Busy` must agree with `Status`

The one cross-check that is a test, not a comment:

- if `Status()` says `running` when a prompt is recorded, the response is
  `busy`;
- if `Status()` says `idle`, the response is never `busy`.

The kernel's status fold and its prompt decision read the same record stream,
so they cannot drift. A prompt while running is never queued implicitly.

## Process exit

The process exit is the one outcome a runtime can never report itself, so it
is an `exited` response:

```go
ResponseBody{
    Kind:             ResponseExited,
    ExitCode:         &code,  // nil when the exit had no numeric code
    ExitCodeReported: true,
}
```

- The `exited` response answers a `dispose` request when oarlock caused the
  exit, and stands alone (`RequestID == ""`) when the runtime died on its own.
- All later `toRuntime` controls are rejected `runtime_exited` by the kernel
  without consulting the adapter.
- `dispose` is the exception: after an observed exit it is still recorded and
  answered `accepted` immediately (nothing is left to release), so a session
  whose runtime died still ends with an answered dispose.
- A turn running at exit ends `failed{runtime_exited}` unless an accepted
  abort (by the runtime or by oarlock) or a `dispose` request precedes the
  exit, in which case it ends `aborted`. The exit code itself stays on the
  `exited` response; no synthetic `turn_ended` is written.

## Abort: rejected, accepted, taken over

The three outcomes look similar and must not be conflated:

1. **Turn already over.** The abort is recorded and rejected
   `no_active_turn`. The turn keeps its own `completed` outcome. There is no
   "aborted" event after a completion.
2. **Accepted, runtime stops.** The abort is `accepted`; the runtime reports
   its own `turn_ended{aborted}`. The response does not claim the process
   exited.
3. **Accepted, runtime does not stop.** After the grace period (ten seconds
   in OAR; the Go kernel uses a configurable equivalent), oarlock takes over:
   the response is still `accepted` ("we took the stop"), it sends SIGTERM to
   the process group, then SIGKILL after the kill grace, and records `exited`.
   A turn that never reported an end becomes `aborted` because the accepted
   stop precedes the exit.

A native refusal cancels only that abort attempt (`runtime_refused`); it must
not kill work the runtime refused to interrupt. If the process dies before it
replies to a still-pending abort, the abort is rejected `runtime_exited`. If
oarlock had already accepted and taken over, the late native reply remains a
frame, never a second response.

## Why these are not a `map[string]any` of vendor errors

- Vendor prose changes; the branch word does not.
- A host that has to regex `"context window"` will break on the next release
  and will treat an auth failure as retryable.
- The classification of a failed *turn* is `TurnOutcome.Failure`
  ([#7](../../issues/0007-folds.md)), a different axis from a rejected
  *control*. A turn failure is a fact in the stream; a rejection is an answer
  to an action.

## Verification

```
gofmt -l .            # empty
go vet ./...
go test -race ./...
```

`errors_test.go` asserts every code's branch word survives `errors.As`, that a
wrapped transport cause stays reachable, that `SessionNotFound` keeps its
native cause, and that `Retryable` is exactly the three advice codes. The
kernel ([#6](../../issues/0006-kernel.md)) adds the `Status`↔`Busy`
agreement test and the exit mapping.

## Non-goals

- Vendor failure taxonomy (auth / quota / rate_limit / context overflow) as a
  control code. That belongs to `TurnOutcome.Failure`.
- Retrying on the caller's behalf.
- Treating a network error as proof that a resume target is missing.
