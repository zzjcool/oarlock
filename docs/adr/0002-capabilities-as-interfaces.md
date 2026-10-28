# ADR-0002: Capabilities are interfaces, plus one data declaration

**Status:** accepted · 2026-10-28

## Context

Runtimes differ: some cannot steer, some take no system prompt, some have no
usage surface. A lowest-common-denominator API drags strong runtimes down; a
`SupportsXxx() bool` can disagree with the implementation; a silently ignored
option writes application logic against a lie.

## Decision

Two mechanisms, each used only where the previous one does not fit:

1. **A whole operation is an interface the runtime may not implement.**
   `Steerer`, `Withdrawer`, `ContextBreakdownReader`. The Go type assertion is
   the capability, and the compiler forces the caller to handle absence.
2. **An option nobody can drop is refused at open** with
   `*UnsupportedOption{Option, Reason}` — never dropped silently.
3. **A declaration exists only where a host must decide before acting**:
   `Runtime.Refused` (option → reason) and `Session.Capabilities()` (queue
   durability, attribution tier, images).

## Consequences

- No boolean flag sits beside an implementation, so they cannot drift.
- Callers can `try → catch UnsupportedOption → fall back` without naming a
  runtime.
- Adding a capability is additive; removing one is a breaking change for
  consumers, which is the correct incentive.

## Rejected

- `Capabilities struct` with booleans only — compiles fine while being wrong.
- Ignoring unsupported options (multica's approach) — convenient, dishonest.
- Throwing at call time for a whole missing operation — turns a compile-time
  fact into a runtime failure.
