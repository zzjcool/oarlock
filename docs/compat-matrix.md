# Compatibility matrix

What each runtime actually gives, and what oarlock declares instead of faking.
Every row is a **claim with a test to back it**; "unverified" is a valid value
and must stay visible.

Legend: ✅ native · 🟡 emulated by oarlock · ❌ absent (member does not exist) ·
⚠️ degraded vs the vendor's richer interface · `?` not yet measured

| | pi | claude | codex | grok | kimi | opencode | qwen | qoder | antigravity | cursor |
|---|---|---|---|---|---|---|---|---|---|---|
| transport | RPC | stream-json | app-server | ACP | ACP | ACP | ACP | ACP | ACP | CLI stream-json |
| `steer` | ✅ | ✅ | ✅ | ✅ | ❌ | ✅ v1 / ❌ v2 | ❌ | ✅ | ❌ | ❌ ⚠️ |
| `queue` | 🟡 held | 🟡 held | ✅ native | 🟡 held | 🟡 held | 🟡 held | 🟡 held | 🟡 held | 🟡 held | 🟡 held |
| `withdraw` | ✅ | ✅ | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| images | ✅ | ✅ | ✅ | ✅ | ? | ? | ❌ | ❌ | ? | ? |
| `contextBreakdown` | ❌ | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| attribution tier | none | attributed | nested | nested | opaque | opaque / nested (v2) | attributed | attributed | opaque | opaque ⚠️ |
| resume | session file | session id | threadId | ACP id | ACP id | ACP id | ACP id | ACP id | ACP id | chat id ⚠️ |
| usage total | ✅ | ✅ (+unattributed) | ✅ per thread | ⚠️ views not summable | ❌ context only | ❌ | ✅ | ✅ | ❌ | 🟡 parse |
| install | ❌ bundled | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ | ✅ | ❌ |
| login | ❌ | ✅ | ✅ | ❌ | ❌ | ❌ | ❌ | ✅ | ❌ | ❌ |
| account usage | ❌ | ✅ | ✅ | ✅ | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ |
| inventories | ✅ | ✅ | ✅ | 🟡 | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |

## Declared degradations

These are **product decisions**, recorded so no future reader mistakes them for
oversights.

### cursor — CLI instead of the vendor SDK

- No `steer` (the CLI has no mid-run input).
- No per-session MCP servers, no plugin directories.
- No `login`/`authStatus` contract; the user signs in with `cursor-agent login`.
- Resume uses the CLI's chat id, not the SDK's `agent-<uuid>`; old SDK sessions
  are not resumable.
- Sub-agent attribution is best-effort at most; declare `opaque` until measured.
- Usage comes from `result`/`step_finish` frames, not a structured `run.wait()`.

Rationale and rejected alternatives: [ADR-0004](adr/0004-cursor-via-cli.md).

### pi — RPC child, not in-process SDK

- The RPC entry is JavaScript (`rpc-entry.js`, `node >= 22.19`); the Go module
  stays dependency-free, but running Pi requires Node or a single-file host.
- `launchArgs` is refused: oarlock owns the RPC startup arguments.
- Project packages must be pre-installed (`--offline` by default) unless the
  host opts in.

### Everyone — the shape of "absent"

- A control a runtime cannot perform is an **interface it does not implement**,
  so the Go compiler forces the caller to handle it.
- An option a runtime cannot honour is `*UnsupportedOption{Option, Reason}`,
  returned before anything opens. Never a session that runs without it.

## Version policy

Each adapter pins an evidence baseline (the vendor version its fixtures were
recorded from) and a support window. Outside the window: a typed error before
the session opens, never silent wrongness.

| adapter | evidence baseline | policy |
|---|---|---|
| pi | `@earendil-works/pi-coding-agent` 1.4.0 | exact match on `--version`, refuse otherwise |
| claude | 2.1.292+ | warn-and-run inside the window; refuse outside |
| codex | 0.149.0+ | pinned app-server schema |
| cursor | `cursor-agent` 2026.07.20+ | fixture test on every upgrade |
| ACP | protocol v1 | negotiate capabilities, refuse unadvertised extensions |
