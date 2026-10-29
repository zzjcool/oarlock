# Codex (app-server JSON-RPC) — adapter notes

Issue [#20](../../issues/0020-codex-app-server.md). Package `adapters/codex`.
Reference: OAR `runtimes/codex/*`. Evidence in this repo is **offline**: OAR's
recorded raw notification streams (`adapters/codex/testdata/codex-*.raw.jsonl`)
and a fake app-server (the test binary re-executed). **No real codex binary was
available; every live behaviour below is unverified** (`TestLiveCodex` is
opt-in and skipped).

## Transport (`rpc.go`)

`codex app-server [-c sandbox_mode="…"] [launchArgs…] --listen stdio://`, one JSON
object per LF line. Three inbound shapes: notification (method), server request
(method + id), reply (id). Replies are delivered on the same ordered feed as
notifications, and `Call` returns only after the reply's hook ran on the
consumer, so a control's response record sits at the reply's wire position
(before any notification written after it). A late reply (after a take-over or
timeout) is a frame, never a second response.

## Handshake and gate (`open.go`, `gate.go`)

1. refused options (same table `Refused()` declares) — before anything starts;
2. `codex --version` inside `[0.149.0, 0.166.0)` or `*VersionError` /
   `*VersionProbeError` — before the session process starts;
3. `initialize {clientInfo, capabilities.experimentalApi}` → the `userAgent` is
   checked against the same window → `initialized` → `thread/start|resume`;
4. the thread reply must carry `thread.id` (`*SchemaError`), and model /
   workspace roots / service tier / effort are read back against the request.

Upper bound `0.166.0` is this adapter's decision (fixtures were recorded up to
0.165.x), not a vendor fact; move it when a newer app-server is evidenced.

## Controls

| control | RPC | notes |
|---|---|---|
| Prompt | `turn/start` | busy from the stream's status fold; `outputSchema` passed through |
| Steer | `turn/steer {expectedTurnId}` | `SteerTurn` pins the id: a mismatch is rejected `no_active_turn` before sending; codex's own refusal is `runtime_refused` (`not_steerable: …`) |
| Queue | `thread/queue/add` | codex's own, durable queue; codex starts the queued turn by itself and the stream adopts it |
| Abort | `turn/interrupt` | take-over after `AbortGrace` kills the tree; no `turn_ended` is invented |
| Withdraw | — | **not implemented** (`thread/queue/delete` is experimental, unverified): `*Session` is not an `oarlock.Withdrawer` |
| Answer | JSON-RPC response | host answers a `toApp` request by id; recorded as `answered` |

## Projection (`project.go`)

One notification → one frame (Native = the whole line). `SpanID` = codex turn id.
Another thread's notification is a frame of that child **session** (`SessionID` =
thread id, `AgentPath` empty: `nested` children are sessions, not agent paths).
`session_linked` comes from collaboration items (`collabAgentToolCall`,
`subAgentActivity`) once per edge. `contextCompaction` item →
`compaction_started/ended`; the deprecated `thread/compacted` closes an open
compaction at most once. `thread/tokenUsage/updated`: `total` less the baseline
(a resume counts from codex's re-report) is `Tokens`, `last.totalTokens` against
`modelContextWindow` is `Context`; parent and each child report their own, and
`fold.Usage` adds children once under `WithChildren`.

## Gaps (unverified or not done)

- everything live (no codex binary here);
- `mcpServers`, `mcpServerCredentials`, `disallowedTools`, `userConfiguration=exclude`
  are refused (config override / `config/read` round trip not ported);
- effort on a **resumed** thread (`thread/settings/update`) is refused, not set;
- `Options.Effort` on a new thread is passed as `config.model_reasoning_effort`
  and read back, unverified live;
- `Withdraw` (see above); cloud/remote codex (non-goal).
