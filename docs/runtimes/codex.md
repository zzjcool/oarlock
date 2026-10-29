# Codex (app-server JSON-RPC) — adapter notes

Issue [#20](../../issues/0020-codex-app-server.md). Package `adapters/codex`.
Reference: OAR `runtimes/codex/*`. Most evidence in this repo is **offline**:
OAR's recorded raw notification streams
(`adapters/codex/testdata/codex-*.raw.jsonl`), a fake app-server (the test binary
re-executed), and — for the option surface of issue
[#54](../../issues/0054-codex-options.md) — the vendor's own schema bundle:
`codex app-server generate-json-schema` from the **codex-cli 0.165.0** installed
on this host, plus `codex doctor --json` for the CODEX_HOME layout. That host is
still **not signed in**, so nothing below is proved against a vendor account. One
exception: the option surface has a live run through a third-party OpenAI
Responses provider (issue [#63](../../issues/0063-codex-options-live.md), the
same shape as `TestLivePromptAndWait`, issue
[#20](../../issues/0020-codex-app-server.md)). The three items it measured are
marked **live** below; everything else stays unverified.

## Transport (`rpc.go`)

`codex app-server [-c sandbox_mode="…"] [launchArgs…] --listen stdio://`, one JSON
object per LF line. Three inbound shapes: notification (method), server request
(method + id), reply (id). Replies are delivered on the same ordered feed as
notifications, and `Call` returns only after the reply's hook ran on the
consumer, so a control's response record sits at the reply's wire position
(before any notification written after it). A late reply (after a take-over or
timeout) is a frame, never a second response.

## Handshake and gate (`open.go`, `gate.go`)

1. the option checks that need no process: an MCP list codex cannot key, a
   `disallowedTools` name codex cannot express, and the isolated home of
   `userConfiguration: exclude` — all before the probe;
2. `codex --version` inside `[0.149.0, 0.166.0)` or `*VersionError` /
   `*VersionProbeError` — before the session process starts;
3. `initialize {clientInfo, capabilities.experimentalApi}` → the `userAgent` is
   checked against the same window → `initialized`;
4. `config/read {cwd, includeLayers:false}` when `disallowedTools` is set: the
   denials are resolved against codex's own effective MCP config, and an open
   without that answer fails;
5. `thread/start|resume`, then (when the reply does not confirm a requested
   effort on a resume) `thread/settings/update` and the `thread/settings/updated`
   readback, then `thread/name/set` last — so a refused open leaves the user's
   thread unrenamed;
6. the thread reply must carry `thread.id` (`*SchemaError`), and model /
   workspace roots / service tier / effort are read back against the request.
   Every error codex reports while it opens goes through the session redactor;
   the refusals before the process starts carry only option names, names in the
   request and paths. The `--version` probe's stderr tail is the one error text
   outside the redactor: it runs before any session value exists.

`Refused()` is that same table and it is now **empty**: every frozen option is
honoured. Two option-level refusals remain, both combinations a per-option table
cannot express, and both a typed `*UnsupportedOption` from the open itself:
`appendSystemPrompt` on a resume (codex ignores it there) and
`Env[CODEX_HOME]` with a value together with `userConfiguration: exclude` (a nil
value removes the variable and is accepted).

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

## Session options (issue #54)

All four travel as native codex state. Nothing is filtered on the oarlock side,
and an option codex cannot be told about is refused before the probe, never
dropped.

| option | native channel | notes |
|---|---|---|
| `mcpServers`, `mcpServerCredentials` | `thread/start` **and** `thread/resume` `config.mcp_servers` | stdio `{command, args, env, enabled:true}`, http `{url, http_headers, enabled:true}` (`mcp.go`); server config **live** (issue #63), credential redaction offline-only |
| `disallowedTools` | per-server `disabled_tools` in the same override, resolved against codex's `config/read` | only `mcp__server__tool` names: codex has no builtin deny channel (`denials.go`); **not live**: the default approval policy refuses the MCP call first |
| `userConfiguration: exclude` | isolated persistent `CODEX_HOME` holding the login but no `config.toml` (`home.go`) | `Env[CODEX_HOME]` with a value together with `exclude` is a refusal; a nil value removes the variable and is accepted; **live**: a real turn on the isolated home (issue #63) |
| `effort` on a resumed thread | `thread/settings/update {threadId, effort}` + `thread/settings/updated` readback (`effort.go`) | unconfirmed or different level → the open fails; **live**: `high` on a thread last run at `low` (issue #63) |

`mcpServers`: codex merges a config override into the user's `config.toml` field
by field, so each entry writes `args` (an array is replaced whole) and
`enabled:true`: the user's own `args` or `enabled=false` cannot change or drop the
session's server. The user's other fields of a same-named entry (more `env`, `cwd`,
timeouts, tool filters) stay. Names are checked before anything starts: empty,
duplicated, reserved (`__proto__`, `__defineGetter__`, `__defineSetter__`,
`__lookupGetter__`, `__lookupSetter__` — dropped without a word by some
runtimes), or outside codex's `^[\w:@/.-]+$`. An http entry needs `url`, a stdio
entry needs `command`, and an entry carrying the other transport's fields
(`url`/`headers` on stdio, `command`/`args`/`env` on http) is refused rather than
half-sent: codex starts one entry as one transport.

MCP credentials: every `env` and `headers` value is a credential value of the
session and goes through the same redactor as `Options.Env`
(`mcp.go sessionSecrets`), so a value codex echoes back — or an error that quotes
one — records as `[redacted]`. They are sent to codex in the `config` override;
oarlock writes them to no file of its own.

`disallowedTools`: `config/read {cwd, includeLayers:false}` is sent after the
handshake, and each name is resolved to exactly one server of the effective
config (the user's servers and this session's alike). The override written back
is `disabled_tools` per server, unioned with what the server already disables.
A name that is not `mcp__server__tool`, a server that is unknown or ambiguous, a
reserved server name, and an existing `disabled_tools` that is not an array of
strings are all refusals: the open ends and the process is released.

`userConfiguration: exclude`: the session runs with `CODEX_HOME` set to
`<dir>/<name>.oarlock-exclude` — persistent, per user home, **no `config.toml`**
(never created, never copied) — and `<name>` is the user's own home (`$CODEX_HOME`,
else `~/.codex`) read only for its `auth.json`, which is copied when it changed so
the session stays signed in. Everything else (state and thread-history sqlite,
sessions, logs) is created in the isolated home, so a thread opened there is
resumable there and unknown to the user's own home: thread ids are home-scoped.
This is deliberately not a temporary directory — OAR records that a throwaway
home leaves its history paths in a shared index and breaks resume after cleanup.

`effort` on resume: the reply's `reasoningEffort` is the level the thread last ran
with. When it is not the requested one, `thread/settings/update` sets it on the
loaded thread and the open waits (10 s) for codex's `thread/settings/updated`. A
report with a different level, no report, or an RPC error fails the open: an
unconfirmed level is refused rather than silently accepted. The notification is
recorded as a frame in wire order, so the effort event it carries is on the
stream.

### Live evidence (issue #63)

`TestLiveCodexOptions` in `adapters/codex/live_test.go` shares the gate and the
preconditions of `TestLivePromptAndWait` — `codex` on `PATH` inside
`[0.149.0, 0.166.0)`, a `$CODEX_HOME/config.toml`, the token in the variable that
file names as `env_key` — and skips as `NOT VERIFIED` when one is missing:

```sh
export CODEX_HOME=<dir whose config.toml names an OpenAI-Responses provider> OPENAI_API_KEY=<token>
OARLOCK_LIVE_CODEX=1 go test ./adapters/codex -run TestLiveCodexOptions -v -count=1
```

The endpoint is a third-party OpenAI **Responses** provider: a
`[model_providers.<id>]` block with `wire_api = "responses"` and an `env_key`
naming the token variable, outside the repository. The host is not signed in; the
run authenticates through that variable, not an `auth.json`.

Measured on codex-cli 0.165.0, in three subtests:

1. `userConfigurationExclude` — the session opened on the isolated home and
   completed a real turn (`Reply with the single word: pong`). The user's own
   home held a poisoned config (loopback port 9, an unserved model) and was
   byte-for-byte unchanged afterwards; the isolated home received codex's own
   state (sessions, thread-history sqlite, logs, shell snapshots …). The user's
   configuration was not read, and the isolated home is a working home.
2. `resumeEffort` — a thread last run at `low`, `thread/resume` with
   `Effort: "high"`: codex's `thread/settings/updated` reported `effort: "high"`
   and `observe.EffortOf` agreed. The resume reply itself had named the previous
   level, so `thread/settings/update` is the channel that set it.
3. `mcpServers` — a stdio entry from `Options.MCPServers` was accepted:
   `mcpServerStatus/list {threadId}` reported it `connected` with serverInfo
   `oarlock-echo` and tool `shout`, and the local server process really started
   (it received `initialize`, `tools/list`).

Not verified: `disallowedTools`. The model tried to call
`mcp__echo__shout` and codex refused it — "MCP tool call requires approval, but
approval policy is never" — because the adapter's default approval policy is
`never`, so the call is refused before any denial matters. The live test logs
`NOT VERIFIED: disallowedTools …` and claims nothing. Also **not live**: the
`auth.json` copy (this host is not signed in), `mcpServers` on a resume,
credential redaction beyond the offline fake, `Options.Effort` on a new thread
beyond what the run showed, `Withdraw`, cloud/remote codex, `AccountUsage`.

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

- most of it **not live**: codex-cli 0.165.0 is installed on this host but not
  signed in, so only the items of "Live evidence (issue #63)" above have run
  against a real endpoint. No vendor-account login, approval flow or
  `AccountUsage` has been exercised. The wire names come from the vendor's own
  0.165.0 schema bundle;
- `mcpServers` on a **resume**: a config override on `thread/resume` is sent (codex
  remembers no server for a thread) although OAR measured that any resume override
  can rebuild the thread's settings from `config.toml` (their env run kept the
  thread's own model and effort). The readback still refuses a model or tier that
  came back different, but an unrequested model drifting on resume would only be
  visible on the stream — and there is no readback for the MCP servers themselves:
  a codex that ignored the resume override would run the thread without the
  session's servers, and only the tool inventory would show it;
- `userConfiguration: exclude` copies `auth.json` at open: a token refresh inside
  the session writes to the isolated home, and the next open copies the user's
  copy over it only when that one is newer. The other direction is covered: a
  missing `auth.json` in the user's home removes the isolated copy, so a logout
  reaches an exclude session on its next open. Neither direction is live: the
  host that ran issue #63 is not signed in;
- `disallowedTools` covers MCP tools only (codex has no builtin deny channel) and
  needs `config/read`; a codex that does not answer it fails the open. It is not
  live-verified either: the adapter's default approval policy is `never`, and
  codex refuses the model's MCP tool call itself ("MCP tool call requires
  approval, but approval policy is never") before any denial matters;
- `Options.Effort` on a new thread is passed as `config.model_reasoning_effort`
  and read back; not live — issue #63 measured only the resumed-thread path;
- `Withdraw` (see above); cloud/remote codex (non-goal).
