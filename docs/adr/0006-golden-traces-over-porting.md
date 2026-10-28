# ADR-0006: Sync behaviour, not code

**Status:** accepted · 2026-10-28

## Context

OAR shipped 8 releases in 8 days during this research. A Go port that tracks
TypeScript diffs would be permanently behind and would re-learn every vendor
quirk by reading commit messages.

OAR's value is concentrated in `experiments/` (≈90 probes) and its fixture
suite, not in its type definitions.

## Decision

Treat OAR as a **reference implementation and oracle**, not a source tree:

1. Record real sessions with OAR into `oar-voyage/3` JSONL.
2. Replay those logs through the Go folds and assert identical
   `status` / `usage` / `turn outcome` / event sequences.
3. On an OAR upgrade, regenerate traces and run the differential suite. Never
   port a diff by hand.

The same mechanism tests our own adapters against vendor upgrades, with
fixtures recorded once and replayed forever.

## Consequences

- Upstream churn becomes a test failure (cheap) instead of a rewrite (expensive).
- The suite is executable documentation of vendor behaviour.
- We may legitimately diverge where Go needs it (capability interfaces, error
  values), as long as the *observable* folds agree.

## Rejected

- Vendoring OAR's TypeScript and transpiling — the whole point is to leave Node.
- Reading specs and reimplementing from memory — loses the evidence.
- Tracking upstream commits — unbounded maintenance with no test coverage.
