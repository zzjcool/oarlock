# Contributing

## Before code

The M0 specifications (#1–#3) are frozen decisions. Changing them later breaks
every consumer's stored data, so open an issue before editing them.

## Conventions

- Every claim about a vendor needs a fixture or a probe script in
  `testdata/` or `experiments/`. "I think the CLI does X" is not evidence.
- Unknown native events are **counted, never coerced**.
- A capability the runtime lacks is an interface it does not implement — not a
  boolean, not a silent no-op.
- Tests: `go test -race ./...` must pass on linux, macOS and windows.

## Commits

`area: imperative summary` — e.g. `pi: frame oversized JSONL lines with a size error`.
