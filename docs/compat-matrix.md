# Compatibility matrix

What each runtime actually gives, and what oarlock declares instead of faking.
Every row is a **claim with a test to back it**; "unverified" is a valid value
and must stay visible.

Legend: ✅ native · 🟡 emulated by oarlock · ❌ absent (member does not exist) ·
⚠️ degraded vs the vendor's richer interface · `?` not yet measured

| | pi | claude | codex | grok | kimi | opencode | qwen | qoder | antigravity | cursor |
|---|---|---|---|---|---|---|---|---|---|---|
| transport | RPC | stream-json | app-server | ACP | ACP | ACP | ACP | ACP | ACP | CLI stream-json |
| `steer` | ❌ | ✅ | ✅ | ✅ | ❌ | ✅ v1 / ❌ v2 | ❌ | ✅ | ❌ | ❌ ⚠️ |
| `queue` | 🟡 held | 🟡 held | ✅ native | 🟡 held | 🟡 held | 🟡 held | 🟡 held | 🟡 held | 🟡 held | 🟡 held |
| `withdraw` | ❌ (held queue, no `Withdrawer`) | ✅ | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| images | ❌ (native; Go adapter sends none yet) | ✅ | ✅ | ✅ | ? | ? | ❌ | ❌ | ? | ? |
| `contextBreakdown` | ❌ | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| attribution tier | none | attributed | nested | nested | opaque | opaque / nested (v2) | attributed | attributed | opaque | opaque ⚠️ |
| resume | session file | session id | threadId | ACP id | ACP id | ACP id | ACP id | ACP id | ACP id | chat id ⚠️ |
| usage total | ✅ | ✅ (+unattributed) | ✅ per thread | ⚠️ views not summable | ❌ context only | ❌ | ✅ | ✅ | ❌ | 🟡 parse |
| install | ❌ bundled | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ | ✅ | ❌ |
| login | ❌ | ✅ | ✅ | ❌ | ❌ | ❌ | ❌ | ✅ | ❌ | ❌ |
| account usage | ❌ | ✅ | ✅ | ✅ | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ |
| inventories | 🟡 (models/skills/commands; tools unsupported) | ✅ | ✅ | 🟡 | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |

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
- Refused options are one table, `adapters/pi/gate.go` (`pi.Refused()` declares it, `Open` checks the same map, before any
  process starts). Honoured: `Cwd`, `Env`, `Model`, `Effort`, `Resume`. Refused, each with its own reason: `launchArgs` (oarlock owns
  the RPC startup arguments), `systemPrompt`, `appendSystemPrompt`, `mcpServers`, `mcpServerCredentials`, `disallowedTools`,
  `additionalDirectories`, `outputSchema`, `name`, `userConfiguration` (`exclude` only), `serviceTier`. Reasons:
  [pi.md §13.2](runtimes/pi.md#132-refused-options-table); issue [#15](../issues/0015-pi-version-gate.md).
- The Pi version is verified (exact `1.4.0`) but is **not** in the record stream: Pi has no init frame that reports it and oarlock
  never synthesizes a frame. `(*pi.Session).PiVersion()` hands it to the host ([pi.md §13.3](runtimes/pi.md#133-the-version-in-the-record--a-gap-not-faked)).
- Project packages must be pre-installed (`--offline` by default) unless the
  host opts in.
- No `Withdrawer`: the adapter holds queued input, but withdrawing a held item
  is not implemented; `Session.Queue` accepts, `Withdraw` is absent.
- No images: Pi's RPC accepts native image parts, but the Go adapter does not
  send them yet; `Capabilities.Images` is false and an input with images is
  rejected `unsupported`.
- Inventories cover models, skills and prompt commands. Tools are
  `unsupported`: Pi 1.4.0 does not expose its built-in/MCP tool inventory over
  RPC without a session, and the adapter does not guess from `--help`.
- Resume is a **session file**, resolved by oarlock before launch (`--session <abs file>`, never an id): by id in the directory of
  `Options.Cwd` only, or by file. A session recorded under another cwd is refused (`UnsupportedOption{cwd}`), not run in the
  recorded directory; a missing target is `SessionNotFound` only on positive evidence; a resumed stream is a new instance with
  `Seq` from 1 and no replay. Evidence, decisions and what is unverified: [pi.md §12](runtimes/pi.md#12-resume-issue-12) (issue #12).
- No `steer`: issue #10 decided the Go Pi session does not implement `Steerer`
  (compile-time absence). OAR TS declares a `steer` command in its Pi RPC
  session; the Go side has not verified it (docs/runtimes/pi.md §3 lists
  `steer` as 未验证). Revisit with evidence, not by editing the matrix.

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
| pi | `@earendil-works/pi-coding-agent` 1.4.0 | **exact match**: `<entry> --version` (stdout, trimmed) must equal `1.4.0`, checked before any session process starts; otherwise a typed `*pi.VersionError{Expected, Actual}` (`errors.Is(err, pi.ErrVersionMismatch)`), never a warning; `--version` gets 15 s and a hung probe is killed with its whole process group (`*pi.VersionProbeError`). [issue #15](../issues/0015-pi-version-gate.md), [pi.md §13](runtimes/pi.md#13-version-gate-and-refused-options-issue-15) |
| claude | 2.1.292+ | warn-and-run inside the window; refuse outside |
| codex | 0.149.0+ | pinned app-server schema |
| cursor | `cursor-agent` 2026.07.20+ | fixture test on every upgrade |
| ACP | protocol v1 | negotiate capabilities, refuse unadvertised extensions |
