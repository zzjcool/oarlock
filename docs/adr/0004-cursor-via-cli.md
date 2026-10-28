# ADR-0004: Cursor through its CLI, with declared degradations

**Status:** accepted · 2026-10-28

## Context

Cursor's only rich interface is `@cursor/sdk`: a compiled, in-process JavaScript
package with no public source. OAR uses it and accepts the consequences
(no process of its own, pinned peer version, no `resources` capability).

For a Go implementation that package is a hard stop. The alternative is
`cursor-agent`, whose `--output-format stream-json` is, frame for frame, Claude
Code's dialect — both agentexec (79 lines) and multica (7k lines, production)
confirm this.

The CLI lacks: mid-run steering, per-session MCP servers, plugin directories, a
login contract, and the SDK's structured `run.wait()` result.

## Decision

Ship a Cursor adapter over the CLI, and declare the missing surface instead of
imitating it:

- no `Steerer` interface on a Cursor session,
- `Refused{MCP: ...}`, `Refused{Plugins: ...}` at open,
- `login` absent; document `cursor-agent login`,
- attribution declared `opaque` until measured,
- resume documented as the CLI's chat id, not the SDK's agent id.

## Consequences

- Cursor remains usable in the one scenario the team needs: headless task runs.
- A host that needs SDK-grade control must keep a JavaScript sidecar for Cursor
  alone. That is an explicit, priced option, not an accident.
- We inherit CLI fragility: the stream is undocumented. Mitigation is a golden
  fixture recorded from a real run, plus an unknown-event tally.

## Rejected

- Sidecar for Cursor only — breaks the "no Node in the build" property and adds
  a second protocol to maintain.
- Dropping Cursor — loses a top-three runtime for many teams.
- Reverse-engineering the SDK bundle — unmaintainable and legally murky.
