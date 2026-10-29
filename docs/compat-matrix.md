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
| `withdraw` | ❌ (held queue, no `Withdrawer`) | ✅ | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ (held queue, no `Withdrawer`) |
| images | ❌ (native; Go adapter sends none yet) | ✅ | ✅ | ✅ | ? | ? | ❌ | ❌ | ? | ❌ (adapter sends none) |
| `contextBreakdown` | ❌ | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| attribution tier | none | attributed | nested | nested | opaque | opaque / nested (v2) | attributed | attributed | opaque | opaque ⚠️ |
| resume | session file | session id | threadId | ACP id | ACP id | ACP id | ACP id | ACP id | ACP id | chat id ⚠️ |
| usage total | ✅ | ✅ (+unattributed) | ✅ per thread | ⚠️ views not summable | ❌ context only | ❌ | ✅ | ✅ | ❌ | 🟡 parse (`result.usage`) |
| install | ❌ bundled | ✅ official script (unverified) | ✅ official script (unverified) | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ host installs the CLI |
| login | ❌ | ✅ URL relay, no stdin (unverified) | 🟡 typed refusal: device code needs the app-server; logout and auth status work | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ✅ URL relay, helper stopped (unverified) |
| account usage | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| inventories | 🟡 (models/skills/commands; tools unsupported) | ⚠️ 4 sections explicitly unsupported | ⚠️ 4 sections explicitly unsupported | ⚠️ explicit unsupported | ⚠️ explicit unsupported | ⚠️ explicit unsupported | ⚠️ explicit unsupported | ⚠️ explicit unsupported | ⚠️ explicit unsupported | 🟡 (models only; skills/commands/tools unsupported) |

Ops (install/login/account usage/update/inventory) details and verification status: [ops.md](ops.md).

An ACP-profile cell describes what the shared profile declares: a vendor's own
install/login surface, where it exists, is not reachable through oarlock, and
`AccountUsage` is absent for every runtime in Go today.

## Declared degradations

These are **product decisions**, recorded so no future reader mistakes them for
oversights.

### cursor — CLI instead of the vendor SDK

- No `steer` (the CLI has no mid-run input).
- No per-session MCP servers, no plugin directories.
- Login/logout/status go through the CLI (`cursor-agent login|logout|status
  --format json`), not the vendor SDK: login relays the URL with
  `NO_OPEN_BROWSER=1` and stops the helper, so oarlock holds no token and
  `AuthStatus` is the only source that says the sign-in completed. Login was
  never run live; see [ops.md](ops.md).
- Resume uses the CLI's chat id, not the SDK's `agent-<uuid>`; old SDK sessions
  are not resumable.
- Sub-agent attribution is best-effort at most; declare `opaque` until measured.
- Usage comes from `result`/`step_finish` frames, not a structured `run.wait()`.
- No `Withdrawer`: queued input is held in the adapter and cannot be taken back.
- No images: the adapter sends none and refuses an image input `unsupported`.
- No `ContextBreakdown`; the CLI reports no itemization.
- The CLI is one-shot per prompt, so a session is one process per turn. A turn
  that reached `result` ends with `turn_ended` and its process exit is not an
  `exited` response (the chat is still resumable); an exit without `result` is.
  A resumed turn is a new stream instance with `Seq` from 1.
- Refused at open, each with its reason: `mcpServers`, `mcpServerCredentials`,
  `launchArgs`, `systemPrompt`, `appendSystemPrompt`, `disallowedTools`,
  `additionalDirectories`, `outputSchema`, `name`, `serviceTier`,
  `userConfiguration` (`exclude` only), and `effort` (no flag).
- Fixtures are recorded from `cursor-agent` 2026.09.18; unknown top-level frame
  types and unknown subtypes of `thinking`/`tool_call` are counted, never
  absorbed.

Details, mapping and what stays unverified: [runtimes/cursor.md](runtimes/cursor.md).
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
| codex | 0.149.0 … <0.166.0 | **window**: `codex --version` and the initialize `userAgent` must be inside it, checked before the session process starts (`*codex.VersionError`, `errors.Is(err, codex.ErrVersionOutOfWindow)`); thread reply schema checked (`*codex.SchemaError`). Offline evidence only, live unverified. [issue #20](../issues/0020-codex-app-server.md), [codex.md](runtimes/codex.md) |
| cursor | `cursor-agent` 2026.09.18-9a7762b (recorded) | fixture test on every upgrade; 2026.07.20+ is the accepted floor, and a live run is opt-in (`OAR_CURSOR_LIVE=1`) |
| ACP | protocol v1 | negotiate capabilities, refuse unadvertised extensions |
