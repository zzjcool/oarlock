# ADR-0005: The Go module has no Node; the runtimes may

**Status:** accepted · 2026-10-28

## Context

"No Node" is easy to promise and easy to get wrong. Pi's RPC entry is
`dist/bundle/rpc-entry.js`; its package declares `engines: node >= 22.19`.
Claude, Codex and the ACP agents are binaries or Node CLIs of their own. Cursor
is a CLI once we drop the SDK.

So the honest statement is narrower than "pure Go".

## Decision

State the boundary precisely, in three layers:

1. **Build:** `go build` only. No cgo, no npm, no code generation that needs
   Node.
2. **Library:** no JavaScript execution inside the process. Everything is a
   child process speaking JSONL or JSON-RPC.
3. **Runtime:** a driven runtime may itself need Node. For Pi that is the
   default; a single-file host (`createPiRuntime`-style `command`) or a
   pre-packaged binary removes it, and is the host's deployment choice.

The README states this, the compat matrix repeats it per runtime, and the Pi
adapter's refusal messages name it when the entry cannot be resolved.

## Consequences

- Deployment images for Pi include a Node runtime (or a single-file build).
- The Go API never leaks that fact: a caller sees `Session`, not a process.
- "Go-native" remains true where it matters: no cgo, no FFI, no embedded JS
  engine, no vendor SDK.

## Rejected

- Embedding a JS engine (goja/quickjs) to run `pi` in-process — version skew,
  memory, and a second runtime to debug.
- Pretending "pure Go" — repeats the exact dishonesty this project exists to
  avoid.
