# ADR-0001: One stream, three record kinds

**Status:** accepted · 2026-10-28

## Context

An application that embeds agent runtimes needs two things that look different:
*what the runtime said* (facts) and *what the application tried to do* (control).
The tempting design is two channels, or control objects that own their span of
events.

OAR rejected both, with evidence: a control-object model loses facts five ways
(synthesized turn boundaries, a settled-gate that swallows late events, a
mandatory turnId with no turn to attach to, one answer split across a promise
and an event, a query masquerading as a snapshot). Two channels lose total
order: "did the abort land before or after that tool_result" becomes
unanswerable.

## Decision

One append-only stream per session, three kinds by obligation:

- `frame` — the runtime's own words. Never synthesized, never gated.
- `request` — an action expecting an outcome, with a direction.
- `response` — points at a request; exists only when oarlock observed an outcome
  the runtime would not state itself.

`Seq` is dense and monotonic per stream instance. Attribution (`SessionID`,
`AgentPath`, `SpanID`) is a field on every record, not a separate channel.

## Consequences

- Consumers get one total order and one replay cursor.
- Liveness is derivable: an `exited` response ends every wait without inventing
  an outcome.
- Adapters must not keep private "is alive" flags; reachability is read from the
  stream.
- A record with no response is an honest fact ("initiated, outcome unobserved")
  and must never be backfilled.

## Rejected

- Control objects owning spans — loses facts in the five ways above.
- Two channels — no cross-channel ordering.
- A `state` snapshot API — no `Seq`, cannot be aligned or replayed.
