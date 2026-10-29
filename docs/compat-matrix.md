# Compatibility matrix

What each runtime actually gives, and what oarlock declares instead of faking.
Every row is a **claim with a test to back it**; "unverified" is a valid value
and must stay visible.

Legend: ✅ native · 🟡 emulated by oarlock · ❌ absent (member does not exist) ·
⚠️ degraded vs the vendor's richer interface · `?` not yet measured

| | pi | claude | codex | grok | kimi | opencode | qwen | qoder | antigravity | cursor |
|---|---|---|---|---|---|---|---|---|---|---|
| transport | RPC | stream-json | app-server | ACP | ACP | ACP | ACP | ACP | ACP | CLI stream-json |
| `steer` | ✅ ² | ✅ ¹ | ✅ | ✅ | ❌ | ✅ v1 ³ / ❌ v2 | ❌ | ✅ | ❌ | ❌ ⚠️ |
| `queue` | 🟡 held | 🟡 held | ✅ native | 🟡 held | 🟡 held | 🟡 held | 🟡 held | 🟡 held | 🟡 held | 🟡 held |
| `withdraw` | ❌ (held queue, no `Withdrawer`) | ❌ (held queue, no `Withdrawer`) | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ (held queue, no `Withdrawer`) |
| images | ❌ (native; Go adapter sends none yet) | ❌ (native; Go adapter sends none yet) | ✅ | ✅ | ? | ✅ ³ | ❌ | ❌ | ? | ❌ (adapter sends none) |
| `contextBreakdown` | ❌ | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| attribution tier | none | attributed | nested | nested | opaque | opaque / nested (v2) | attributed | attributed | opaque | opaque ⚠️ |
| resume | session file | session id | threadId | ACP id | ACP id | ACP id ³ | ACP id | ACP id | ACP id | chat id ⚠️ |
| usage total | ✅ | ✅ (+unattributed) | ✅ per thread | ⚠️ views not summable | ❌ context only | ✅ ³ | ✅ | ✅ | ❌ | 🟡 parse (`result.usage`) |
| install | ❌ bundled | ✅ official script (unverified) | ✅ official script (unverified) | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ host installs the CLI |
| login | ❌ | ✅ URL relay, no stdin (unverified) | 🟡 typed refusal: device code needs the app-server; logout and auth status work | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ✅ URL relay, helper stopped (unverified) |
| account usage | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| inventories | 🟡 (models/skills/commands; tools unsupported) | ⚠️ 4 sections explicitly unsupported | ⚠️ 4 sections explicitly unsupported | ⚠️ explicit unsupported | ⚠️ explicit unsupported | ⚠️ explicit unsupported | ⚠️ explicit unsupported | ⚠️ explicit unsupported | ⚠️ explicit unsupported | 🟡 (models only; skills/commands/tools unsupported) |

¹ claude `steer` is live-verified with a real echo (issue #42, 2026-10-29:
raw probes and Go live tests on `claude` 2.1.312 against a third-party
Anthropic-compatible endpoint, model `workbuddy/deepseek-v4.1-flash`; no
official account; the committed fixture is a 2026-10-10 recording of 2.1.292).
The echo arrives only at a model-request boundary: inside a tool-boundary turn
it lands mid-turn and the steer is accepted; in a single-streaming-message turn
claude natively queues the input and applies it after the turn's `result`, i.e.
as a follow-up turn, so that steer is rejected — an accepted response is never
recorded without an echo. Details:
[runtimes/claude.md](runtimes/claude.md#4-steer-the-echo-protocol-issue-42).

³ opencode is live-verified (v1 line, opencode 1.19.5, 2026-10-29, third-party
OpenAI-compatible endpoint; issue #55, record and caveats in
[runtimes/opencode.md](runtimes/opencode.md#51-the-depth-run-issue-55-2026-10-29)):
the prompt answer's own per-turn `usage` is billed as `Usage/Tokens` (no
session-wide `Total`); a mid-turn `session/prompt` steers and the original
answer stays the only turn end; `session/resume` restores the context in a new
process with the stream's `Seq` from 1. Images: the block goes out natively as
an ACP `ContentBlock` and the turn completes, but the configured model answered
that it cannot view images — the transport is verified, model vision is the
provider's. v2 keeps `nested` on the recorded fixture and has **no** steer
(compile-time unsupported).

² pi `steer` is live-verified (2026-10-29: pi 1.4.0 on this host, `--mode rpc`,
the in-turn exchange is quoted in [runtimes/pi.md §3.1](runtimes/pi.md#31-steer-issue-45)
and the idle run is named there; the one-off run logs are not committed).
`{"type":"steer","message":…}` is a native Pi command: it answers
`{"type":"response","command":"steer","success":true,"data":{"disposition":"queued"}}`
right after `{"type":"queue_update","steering":["<text>"],"followUp":[]}`, and
it lands in the **current** turn — one `agent_start` and one `agent_settled`
span the exchange, a `role:"user"` `message_start`/`message_end` pair carrying
the steered text arrives inside the same run before that run's final `turn_end`,
and the answer incorporated it: not a new turn, not a follow-up turn.
`disposition:"queued"` is an acknowledgment ("accepted into this turn's steering
buffer"), **not** a landing; delivery is those later native frames, and oarlock
synthesizes no turn end for a steer. A steer with **no** turn running also
answers `success:true`/`queued` and then sits in `steering` (observed for 6 s,
no turn started, `agent_start` never arrived); Pi would then inject it into
whatever turn comes next. The adapter therefore rejects `no_active_turn`
locally, without sending the command. The gate reads the status at the moment
the request was recorded, so a run that settles before the command reaches the
wire can still take the input into `steering`; the adapter reads the runtime's
own answers and invents nothing to close that window.

Ops (install/login/account usage/update/inventory) details and verification status: [ops.md](ops.md).

An ACP-profile cell describes what the shared profile declares: a vendor's own
install/login surface, where it exists, is not reachable through oarlock, and
`AccountUsage` is absent for every runtime in Go today.

One ACP member has a live session: **opencode 1.19.5** (v1 line) on a
third-party OpenAI-compatible endpoint, 2026-10-29 — initialize, session/new,
prompt → session/update stream → the runtime's own `end_turn`, `session/cancel`
→ `cancelled`, one `session/request_permission` answered `allow_always`, and
(issue #55) a per-turn usage ledger, a mid-turn steer, a native image prompt
and a `session/resume` that restores the context. Its attribution tier is not
upgraded: no `task` subagent ran, so no child-session frame was observed. The
run record, provider configuration and the list of what it does **not** prove
are in [runtimes/opencode.md](runtimes/opencode.md). **grok, kimi, qwen, qoder
and antigravity remain UNVERIFIED live** (no binary and no account on that
host); their attribution and capability cells above are unchanged by it.

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

### codex — per-session option surface (issue #54)

`mcpServers`/`mcpServerCredentials`, `disallowedTools` and
`userConfiguration: exclude` are sent as codex's own state, not emulated:
`config.mcp_servers` on `thread/start`/`thread/resume`, per-server
`disabled_tools` resolved against `config/read`, and an isolated persistent
`CODEX_HOME` with the login but no `config.toml`. `effort` on a resumed thread is
set with `thread/settings/update` and only accepted when codex reports the level
back (`thread/settings/updated`); an unconfirmed level fails the open. Refused
therefore, each with its own reason, only what codex has no native channel for: a
`disallowedTools` name that is not `mcp__server__tool` (no builtin deny channel),
a reserved or ambiguous server name, and `Env[CODEX_HOME]` together with
`userConfiguration: exclude`. All of it is offline-verified only: the installed
codex-cli 0.165.0 is not signed in on this host, so none of it is live-verified.
[codex.md](runtimes/codex.md#session-options-issue-54).

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
- `steer` is implemented on Pi's native `{"type":"steer","message":…}` and is
  **running-gated**: with no turn running the adapter rejects `no_active_turn`
  locally and sends nothing, because idle Pi acknowledges a steer
  (`success:true`, `disposition:"queued"`) and then holds it in `steering` until
  whatever turn comes next — which would then carry it unasked. `disposition:"queued"`
  acknowledges the input into the current turn's steering buffer; it is not a
  landing — the steered user message arrives later as Pi's own frames inside the
  same run, and oarlock synthesizes no turn end for a steer. A rejected steer
  leaves the input with the caller: nothing is held, nothing is retried.
  Evidence: [pi.md §3](runtimes/pi.md#31-steer-issue-45); the issue #10 conclusion
  (no `Steerer`, `steer` 未验证) is superseded by that measurement.

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
| claude | 2.1.292+ | warn-and-run; the adapter has no open-time version gate yet (ops probe reads `--version`). Live 2.1.312 via a third-party Anthropic-compatible endpoint |
| codex | 0.149.0 … <0.166.0 | **window**: `codex --version` and the initialize `userAgent` must be inside it, checked before the session process starts (`*codex.VersionError`, `errors.Is(err, codex.ErrVersionOutOfWindow)`); thread reply schema checked (`*codex.SchemaError`). Offline evidence only, live unverified. [issue #20](../issues/0020-codex-app-server.md), [codex.md](runtimes/codex.md) |
| cursor | `cursor-agent` 2026.09.18-9a7762b (recorded) | fixture test on every upgrade; 2026.07.20+ is the accepted floor, and a live run is opt-in (`OAR_CURSOR_LIVE=1`) |
| ACP | protocol v1 | negotiate capabilities, refuse unadvertised extensions |
