# ADR-0003: Pi is the first adapter

**Status:** accepted · 2026-10-28

## Context

The team's current workloads run on Pi. Building Claude or Codex first would
prove the architecture on a runtime nobody uses, and delay the only integration
that has production value.

Pi also happens to be a good architectural probe: a JSONL request/response
protocol, a real session file, a thinking level, and an extension UI channel —
enough surface to exercise the kernel, the folds, resume and `toApp` requests.

## Decision

M1 is Pi-only. The order is spike → transport → controls → projection → kernel
folds, so the riskiest unknown (the RPC dialect and its exit semantics) is
resolved in week 2, not month 2.

Claude moves to M2 and becomes the proof that the contract generalises: it must
add zero kernel code.

## Consequences

- If the Pi spike fails, the project stops for a week, not a quarter.
- The kernel is designed against one real runtime, then stress-tested by a
  second — the same order OAR itself used.
- Codex (the largest protocol block) is deferred to M3, where it cannot block
  delivery.

## Rejected

- Claude-first — better documented, but no production user.
- Codex-first — largest risk on the critical path.
- All adapters in parallel — no shared contract to converge on.
