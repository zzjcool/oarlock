// Package kernel is the one record-stream kernel every OAR adapter builds on.
//
// It owns the parts that must never be re-implemented per runtime: dense Seq
// allocation, the retained log behind the cursor, the observer side-tap,
// control recording (request → decide → response), reachability read off the
// stream, the input-level refusals, credential-redaction timing, and the
// session graph. An adapter only pumps runtime frames in (Kernel.Frame) and
// decides what a control should answer (the decide callback of
// Kernel.Control). Semantics: docs/spec/record-stream.md, docs/spec/errors.md
// and docs/spec/redaction.md.
//
// # The five invariants
//
//  1. No synthesized facts. The kernel never invents a frame, an event or a
//     turn end. Frame retains exactly the Native bytes and Events it was
//     given; one native frame is one record. A prompt that the runtime handled
//     without opening a turn therefore leaves no turn event at all.
//
//  2. Control never prunes facts. There is no settled gate. A frame that
//     arrives after its span ended, after an accepted abort, after a dispose
//     request or after an exited response is still retained and observed.
//     Control records appends; it never drops anything.
//
//  3. Reachability is read off the stream. Unreachable derives from the
//     retained records alone: an exited response, then a toRuntime dispose
//     request. No adapter flag participates: the kernel holds no adapter flags,
//     only derived booleans folded from the retained records (exited,
//     disposing) beside the publisher flag. exited has precedence over
//     disposed.
//
//  4. Empty input without images is refused before decide runs. A prompt,
//     steer or queue with Input == "" and no Images is answered
//     rejected{unsupported, "empty input: give text or images"} without
//     consulting the adapter, and the request stays in the stream. Whitespace
//     is never trimmed: only the empty string is refused.
//
//  5. Credential values never enter records. The Redactor hook runs once per
//     record, on a deep clone, before the record enters the log and before any
//     observer sees it. The retained record and the observed record are the
//     same value, and the unredacted original is what decide receives — the
//     runtime still gets the real value.
//
// # Control order and gates
//
// Control snapshots reachability and the empty-input refusal first (reads
// only), then records the request, then runs decide, then records the
// response, then returns the ControlOutcome. Precedence is reachability, then
// empty input, then decide:
//
//   - unreachable: decide is not called. The response is
//     rejected{runtime_exited} or rejected{disposed}, except dispose after an
//     observed exit, which is recorded and answered accepted by the kernel
//     itself (nothing is left to release), and dispose after a dispose, which
//     is rejected disposed;
//   - empty input: decide is not called; the response is rejected{unsupported};
//   - otherwise decide runs with the original (unredacted) request record. A
//     panic is recovered into rejected{error, <panic text>}: the process does
//     not crash and the request stays in the stream. A decide returning
//     ResponseExited maps the outcome to rejected{runtime_exited}, exactly like
//     the reference controlOutcomeOf.
//
// Frame, Request and Respond are never gated: a late frame after exit or
// dispose is still retained and observed (invariant 2).
//
// # Request ids
//
// Request uses RecordAt.ID when non-empty (runtime-issued toApp ids keep their
// own spelling), and "req-<seq>" otherwise. Control always generates its own
// "req-<seq>" id and ignores RecordAt.ID.
//
// # Observers
//
// Dispatch is inline on the appending goroutine; there is no dispatcher
// goroutine and no buffering policy. Every observer call is wrapped in a panic
// recovery, per observer per record, so a failing observer cannot affect a
// later observer or the run. Observers are called in registration order, so a
// host can rely on which observer runs first. A record appended from inside an
// observer is queued and dispatched by the active drain in Seq order before the
// outer append returns, so observers always see Seq order.
//
// Observe registers with no replay: only records appended after the call are
// delivered. ObserveFrom is the atomic replay-then-live composition: it
// validates the cursor, replays every retained record with Seq > AfterSeq
// synchronously, registers the observer and drains, so the seam has no gap and
// no duplicate; a mismatched cursor registers nothing. cancel is idempotent and
// removes the observer, and it takes effect for the rest of the current
// delivery batch too: at most the callback already running when cancel is
// called may finish. ObserveFrom must not be called from inside an observer
// callback: the callback runs while the publisher role is held, and ObserveFrom
// needs that role.
//
// # Cursor and ErrCursorMismatch
//
// Cursor{SessionID, AfterSeq} means "every retained record with Seq >
// AfterSeq, then live". The zero cursor replays the whole instance: AfterSeq 0
// is the start of the stream, because the first record has Seq 1. An empty
// Cursor.SessionID is the zero-value "this instance" and matches, so
// Cursor{AfterSeq: n} addresses this instance at a position.
//
// A non-empty SessionID that differs from the kernel's session names another
// stream instance and is never silently applied: Replay and ObserveFrom return
// an error wrapping ErrCursorMismatch (errors.Is works) and ObserveFrom
// registers no observer, while Records returns nil. Seq restarts at 1 when an
// instance is recreated, so another instance's records are never replayed. Use
// Replay when the mismatch matters.
//
// # Redaction changes content only
//
// The redactor runs once per record, on a deep clone, before the record enters
// the log and before any observer sees it; content it changes (Native, Events,
// Request.Input, Response.Reason, native payloads) is retained as returned.
// Structure is not credentials and is not rewritable: after the hook the kernel
// restores Seq, Kind, SessionID, AgentPath, SpanID, ID, Direction and
// RequestID, plus Request.Action and Response.Kind, from the pre-redaction
// record. A hook that drops the Request or Response body entirely gets the
// original body back, so a dispose request or an exit response cannot be
// hidden. A redactor therefore cannot renumber the stream, re-Kind a record,
// hide a dispose request or fabricate an exit, and reachability keeps agreeing
// with the stream it is read from.
//
// A panicking redactor is contained: the kernel recovers, retains the record as
// it was built (unredacted) and carries on, so a broken hook can neither
// deadlock the kernel nor lose the record. The clone the hook was handed is
// thrown away on that path rather than kept: a hook can mutate that clone in
// place before it panics, and a half-applied edit must not be able to hide a
// dispose or fabricate an exit.
//
// # ControlOutcome
//
// ControlOutcome.Request and .Response are the retained (redacted) records; Seq
// and RequestID are the request's. The response record's RequestID is the
// request's pre-redaction id, so redaction can never break the
// response→request link.
//
// # Go-izations relative to the TypeScript reference
//
//   - RequestRecord, ResponseRecord and Frame are aliases of oarlock.Record:
//     one Go type, discriminated by Kind.
//   - Request takes its id from RecordAt.ID; Control generates "req-<seq>",
//     which is a total-order-stable id instead of a random UUID.
//   - Records(Cursor) cannot return an error, so Replay carries the typed
//     ErrCursorMismatch and Records returns nil on a mismatch.
//   - ObserveFrom is added to give the atomic replay→live seam the spec asks
//     for; bare Observe has no cursor.
//   - FrameBody has no type field because the frozen Go record keeps the frame
//     discriminator inside Native ("every native frame is retained whole in
//     Native").
//   - Where the TypeScript reference rejects dispose after exited, the frozen
//     Go specification (record-stream.md invariant 3, errors.md "Process
//     exit") requires accepted, so dispose after an observed exit is accepted
//     without decide; every such dispose is accepted, because exited is checked
//     before disposed.
//   - Cursor zero value: Cursor{} is "this instance from the start" (the first
//     record has Seq 1), so an empty SessionID is the zero-value wildcard for
//     this instance. Only a non-empty SessionID naming another instance is an
//     ErrCursorMismatch. Records cannot return that error, so it returns nil on
//     a mismatch while Replay and ObserveFrom return the wrapped sentinel.
//   - The redactor is restricted to content: the stream-structural fields are
//     restored from the original record after the hook (see "Redaction changes
//     content only").
//   - Deferred, not part of the Go kernel: the reference's carriesRefusal gate.
//     No Go document requires it, and the input-ownership fold belongs to #7.
package kernel
