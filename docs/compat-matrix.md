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
| `withdraw` | 🟡 held ⁵ | ✅ ⁴ (held queue, by inputId) | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ (held queue, no `Withdrawer`) |
| images | ✅ ⁵ | ✅ ⁴ (native base64 blocks) | ✅ | ✅ | ? | ✅ v1 ³ | ❌ | ❌ | ? | ❌ (adapter sends none) |
| `contextBreakdown` | ❌ | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| attribution tier | none | attributed | nested | nested | opaque | opaque (v1) / nested (v2, live) | attributed | attributed | opaque | opaque ⚠️ |
| resume | session file | session id | threadId | ACP id | ACP id | ACP id ³ (v1) | ACP id | ACP id | ACP id | chat id ⚠️ ⁶ |
| usage total | ✅ | ✅ (+unattributed) | ✅ per thread | ⚠️ views not summable | ❌ context only | v1 ✅ ³ / v2 reports but is not billed | ✅ | ✅ | ❌ | 🟡 parse (`result.usage`) |
| install | ❌ bundled | ✅ official script (unverified) | ✅ official script (unverified) | ✅ official script (unverified) | ✅ official script (unverified) | ✅ official npm, two lines: v1 `opencode-ai` / v2 `@opencode/cli` — both lines were really installed on this host with **separate npm prefixes** (v1 `1.19.5` in the global prefix, v2 `@opencode/cli@2.0.26` into `/tmp/opencode-v2`, a one-off scratch prefix on that host rather than an oarlock default, 2026-10-29), but the `Install` op itself was never run, so the cell stays unverified | ✅ official script (unverified) | ✅ official script (unverified) | ❌ no installer (OAR `requires_gui`: from an editor's agent registry) | ❌ host installs the CLI |
| login | ❌ | ✅ URL relay, no stdin (unverified) | 🟡 typed refusal: device code needs the app-server; logout and auth status work | ❌ | ❌ | 🟡 typed refusals on login and logout (both are interactive prompts); `opencode auth list` answers auth status, live | ❌ | ❌ | ❌ | ✅ URL relay, helper stopped (unverified) |
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
provider's. All of it is the **v1** line. The **v2** line (opencode 2.0.26) has
its own live run (issue #77, §5.2): `nested` is live-verified — the model called
the `subagent` tool and the vendor's own `opencode/child-session` marker stated
six marker-carrying child frames, each restating the same root→child edge for one child session, with the child's records owned by that child session — and
the prompt answer **does** carry a per-turn `usage` object, but the v2 profile
has no `PromptTokens` hook, so v2 reports usage and bills none (no
token-bearing `EventUsage`, `fold.UsageOf(...).Total` nil: a known gap, never a runtime
that reports nothing). v2 has no steer: the open returns `*Session` and the run
pins it as not an `oarlock.Steerer`. Not measured on v2: steer, images beyond
the `image:true` advertisement, `session/resume`, `session/load`,
`close`/`delete`/`fork`.

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

⁴ claude `withdraw` and `images` are implemented (issue #53). `images` is
**live-verified** on claude 2.1.312 against a third-party Anthropic-compatible
endpoint (2026-10-29): an image-only prompt carrying a 1×1 PNG was accepted and
the turn ended with claude's own `result` — the wire shape reached the runtime;
the run does not prove the model described the picture. `withdraw` is proven
**offline** only (the fake `claude` process + this repo's committed recordings):
the queue is adapter-owned, so nothing about it is visible on the wire to a live
run. `withdraw` takes back an entry of the adapter-held queue
by the `inputId` the queue request carried: `accepted` only while the entry is
still held, `not_queued` once the drain may have taken it (already sent, never
queued here, or already withdrawn), `unsupported` for an empty id; the queue
request, its response and the withdraw request/response stay in the stream.
`images` are native base64 blocks
(`{"type":"image","source":{"type":"base64","media_type":…,"data":…}}`)
ahead of the text block (the text block is omitted for image-only input),
png/jpeg/gif/webp only — any other type `unsupported`, an unreadable file
`error`, no size limit of oarlock's own. Permission requests are
**recorded, not answered** by default (`PermissionRecordOnly`). Headless claude
runs `--dangerously-skip-permissions` and oarlock refuses a host
`--permission-mode`, so the runtime itself declares the consequence: the live
root `system/init` reports `permissionMode: "bypassPermissions"`, and a live Bash
turn (2026-10-29, issue #65) produced 0 runtime→app requests and 0 `answered`
records. A `can_use_tool` therefore **cannot arise** in a session this adapter
starts (issue #65): `Answer` and the opt-in `PermissionAllow` ladder stay
offline-only (the fake process), unverified live by construction — never
"verified" by a fabricated answer. `cancel_queued` (issue #65) is an option on
the abort's interrupt frame, sent only when the root `system/init` advertises
`interrupt_cancel_queued_v1` — the 2.1.292 recordings and live 2.1.312 do — and
a prompt oarlock wrote has no start receipt yet; otherwise the frame is exactly
`{"subtype":"interrupt"}`. **Live on 2.1.312 (2026-10-29)**: when the interrupt
won that window, claude dropped the queued input — `command_lifecycle queued →
cancelled` with no `started`, no replayed user frame, no turn and no turn end —
and the next prompt ran the only turn. The window is a race inside the CLI (4 of
7 live runs hit it, 3 never did; a miss carries the plain interrupt and ends the
turn `aborted`), so the exact wire line is pinned offline by the fake's stdin
recorder. Details: [runtimes/claude.md §3](runtimes/claude.md#3-controls).

⁵ pi `images` and `withdraw` are issue #52, re-run for issue #64 on 2026-10-29 on
this host (pi 1.4.0, the adapter's `--mode rpc` argv, model
`any-endpoint/workbuddy/deepseek-v4.1-flash`, whose `get_state` reports
`"input":["text","image"]`; opt-in
`OAR_PI_LIVE=1 go test ./adapters/pi -run 'TestLiveImagePrompt|TestLiveQueueAndWithdraw'`,
run frames in `/tmp/issue52-evidence/live.md` and the #64 run log, not committed —
without the variable both tests skip, so the claims are 未验证 for whoever runs the
plain suite). **images ✅ native**: an 8×8 82-byte PNG prompted with "What colour is
this image? Answer with one word." went out as Pi's own
`{"type":"image","data":<base64>,"mimeType":"image/png"}` part on `prompt`,
Pi echoed the user content with that part, a real turn ran and the model
answered "Red". The record keeps the paths only, the `images` member is absent
(never null) when there is none, and `Steer` carries the same parts —
image+steer and image+queue are fake-Conn tests, not live. **withdraw 🟡 held**:
Pi has no withdraw command, so the withdrawal is the adapter's own list edit and
**nothing goes on the wire**; `accepted` removes every held entry for that
inputID, `not_queued` (`no held input with this inputId is waiting`) when none
is waiting — already sent, never queued, already withdrawn — and the queue's own
request/response records are never rewritten. All three states were measured in
one live run (issue #64, 2026-10-29, 22.5 s, 96 records): `accepted` `seq 36/37`
inside a `bash sleep 8` tool call, `not_queued` for the already-withdrawn id
`seq 53/54`, and `not_queued` for an id that was never queued at all `seq 55/56`
— each with exactly one
request/response pair and no prompt request, no `user_message` event and no
native frame carrying the withdrawn text. **Dispose leaves no residue** (live,
same run): one `dispose` request and one accepted response, an idempotent second
call, the runtime's own `exited` record, a held input dropped (a later `Withdraw`
answers `rejected{runtime_exited}`, never `accepted`), the child pid gone and no
record appended for 2 s. The gates are the kernel's
(`runtime_exited`/`disposed` — the `disposed` code specifically is fake-proven
only) and the queue is still not durable across exit and dispose. Details:
[runtimes/pi.md §3.2](runtimes/pi.md#32-images-issue-52),
[§3.3](runtimes/pi.md#33-withdraw-issue-52).

⁶ cursor `resume` is live-verified as an **explicit** reopen (issue #64,
2026-10-29, `cursor-agent` 2026.09.18-9a7762b on this host, logged in): a chat
opened from `Options.Resume = <chat id>` (`--resume <chat id>` in both the built
argv and the live child's `/proc/<pid>/cmdline`) is a new stream instance with
`Seq 1..20`, nothing replayed, and its follow-up answer carried the code the
first session had planted — the earlier context survived the dispose. The ⚠️
stays for what it does **not** give: no replay of the earlier conversation, only
the CLI's chat id (not the SDK's `agent-<uuid>`), and a bad `--resume` id is
still unexercised live. Test: `OAR_CURSOR_LIVE=1 go test ./adapters/cursor -run
TestLiveExplicitResume`; without the variable it skips, so the claim is 未验证 for
whoever runs the plain suite. Details:
[runtimes/cursor.md §5.1](runtimes/cursor.md#51-explicit-resume-measured-live-issue-64).

Ops (install/login/account usage/update/inventory) details and verification status: [ops.md](ops.md).

Update checks differ per runtime (issue [#69](../issues/0069-ops-breadth.md)):
codex is the one check that runs a command, `codex doctor --json` reading its
`updates.status` row (live shape on codex-cli 0.165.0, which exits 1 even with
valid JSON; the newer/not-older verdict is unverified), while claude and
cursor are typed refusals — `claude update` is check and install in one step
with no `--check`, and `cursor-agent update` has no check-only sibling.

An ACP-profile cell describes what the shared profile declares. Since issue
[#69](../issues/0069-ops-breadth.md) the vendor's own install line is reachable:
grok, kimi, qwen and qoder plan and — only on an explicit `Install` — run their
official script, and opencode plans one of its two npm lines, where
`InstallRequest.Line` names `v1` or `v2` and an empty or unknown line is a typed
refusal. **No ACP installer was ever run**, so every install cell is unverified:
opencode's two lines were installed by hand outside oarlock (see its install
cell), which shows the packages exist and run, not that the `Install` path does.
Antigravity has no installer at all (OAR's `requires_gui`: the runtime comes
from an editor's agent registry) and no login. Authentication is there for
opencode only: `opencode auth list` answers auth status (live on 1.19.5, 0
credentials) while login and logout are typed refusals because both are
interactive prompts. `AccountUsage` is absent for every runtime in Go today.

The ACP family has one live member: **opencode**, on a third-party
OpenAI-compatible endpoint, 2026-10-29 — **1.19.5** (v1 line): initialize,
session/new, prompt → session/update stream → the runtime's own `end_turn`,
`session/cancel` → `cancelled`, one `session/request_permission` answered
`allow_always`, and (issue #55) a per-turn usage ledger, a mid-turn steer, a
native image prompt and a `session/resume` that restores the context. v1's
attribution tier is not upgraded: no `task` subagent ran, so no child-session
frame was observed. The v2 line (`@opencode/cli` 2.0.26, installed 2026-10-29
into a one-off temporary npm prefix beside the untouched v1) has its own live
run in
[runtimes/opencode.md §5.2](runtimes/opencode.md#52-the-v2-run-issue-77-2026-10-29):
all three v2 tests pass — a turn, a `nested` child session the vendor's own
marker named (one child session; 6 marker-carrying frames, records owned by that child), and a `usage` object on the
wire that the v2 profile does not bill. So v2's `nested` cell now rests on the
live run as well as the fixture, its `usage total` cell reads reported but
unbilled, and steer, images beyond the advertisement, `session/resume`,
`session/load` and `close`/`delete`/`fork` on v2 stay unverified. The run
record, provider configuration and the list of what it does **not** prove are in
[runtimes/opencode.md](runtimes/opencode.md). **grok, kimi, qwen, qoder
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
back (`thread/settings/updated`); an unconfirmed level fails the open. The
per-option refusal table is therefore empty; what the open still refuses are the
shapes codex has no native channel for — a `disallowedTools` name that is not
`mcp__server__tool` (no builtin deny channel), a reserved, unknown or ambiguous
server name, an MCP entry carrying both transports' fields, `Env[CODEX_HOME]`
(with a value) together with `userConfiguration: exclude`, and
`appendSystemPrompt` on a resume (codex ignores it there) — each a typed
`*UnsupportedOption` returned by the open itself. All four options are now live
on this host — codex-cli 0.165.0 behind a third-party OpenAI-Responses provider
(issues #63 and #70, the same endpoint shape as the cross-runtime live test):
the `userConfiguration: exclude` turn, the resumed thread's `effort`, the MCP
server config (accepted, the server really started and its tool really ran under
`on-request` approvals), and `disallowedTools` (the denied tool is absent from
codex's own `mcpServerStatus/list` inventory, no approval is asked for it, and
the call never reaches the server). What stays unverified live is the rest: the
host is still not signed in, so no vendor account, no `auth.json` copy and no
`AccountUsage` has run. Native denial is not approval refusal: under the
adapter's default approval policy (`never`) codex refuses an MCP tool call itself
and asks nothing, so the live test drives the approval channel explicitly
(`ApprovalPolicy: "on-request"` + `Session.Answer`).
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
- **Not a degradation any more (issue #52):** `withdraw` is 🟡 — emulated over the adapter's own held queue. Pi has no
  withdraw command, so a withdrawal is one record pair plus one edit of that list and **nothing on the wire**:
  `accepted` removes every held entry for that inputID, `not_queued` when none is waiting, and the queue's own
  request/response records are never rewritten. `*pi.Session` is a real `oarlock.Withdrawer`; the queue is still not
  durable, so an exit or a dispose drops the held list and leaves nothing to withdraw.
  [pi.md §3.3](runtimes/pi.md#33-withdraw-issue-52).
- **Not a degradation any more (issue #52):** images are native. `prompt` and `steer` carry Pi's own
  `{"type":"image","data":<base64>,"mimeType":…}` parts, read from the caller's files (`png`/`jpeg`/`gif`/`webp`);
  the record keeps the paths, never the bytes, and the `images` member is absent (never null) when there is none.
  `Capabilities.Images` is true. [pi.md §3.2](runtimes/pi.md#32-images-issue-52).
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
| codex | 0.149.0 … <0.166.0 | **window**: `codex --version` and the initialize `userAgent` must be inside it, checked before the session process starts (`*codex.VersionError`, `errors.Is(err, codex.ErrVersionOutOfWindow)`); thread reply schema checked (`*codex.SchemaError`). The in-window path is live-exercised by codex-cli 0.165.0 (issue #63); the out-of-window refusals are offline-only. [issue #20](../issues/0020-codex-app-server.md), [codex.md](runtimes/codex.md) |
| cursor | `cursor-agent` 2026.09.18-9a7762b (recorded) | fixture test on every upgrade; 2026.07.20+ is the accepted floor, and a live run is opt-in (`OAR_CURSOR_LIVE=1`) |
| ACP | protocol v1 | negotiate capabilities, refuse unadvertised extensions |
