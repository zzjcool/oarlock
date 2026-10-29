# Cursor: `cursor-agent` over stream-json (issue #22)

Oarlock drives Cursor through the vendor CLI, not `@cursor/sdk`. The SDK is a
compiled in-process JavaScript package, so a Go adapter cannot use it; the CLI's
`--output-format stream-json` is Claude Code's dialect frame for frame, and
oarlock reuses the one parser it already has (`internal/streamjson`). No second
parser exists, on purpose.

Decision and rejected alternatives: [ADR-0004](../adr/0004-cursor-via-cli.md);
evidence: `testdata/cursor/` (recorded 2026-10-28 from `cursor-agent`
`2026.09.18-9a7762b`, a normal turn and a turn with a shell tool).

## 1. The invocation

```sh
printf '%s' "$prompt" | cursor-agent -p --output-format stream-json \
  --stream-partial-output --force --trust --sandbox disabled \
  [--model <model>] [--resume <chat id>]
```

- `-p --output-format stream-json --stream-partial-output` — print mode, the
  shared dialect, and incremental `assistant` snapshots.
- `--force` — headless: a permission prompt is a hang, not safety (the same
  reasoning as claude's `--dangerously-skip-permissions`). `--yolo` is the
  CLI's alias; oarlock uses `--force`, the flag the recordings came from.
- `--trust --sandbox disabled` — the workspace is trusted and the CLI's sandbox
  is off, so a headless run in a fresh directory cannot stop at a prompt.
- `--model` is `Options.Model` verbatim; `--resume` is `Options.Resume`.
- The **prompt goes on stdin**, never in argv: a 1 MB prompt is not an `E2BIG`
  and not a command line anyone can read. The child reads stdin to EOF, so the
  adapter writes the bytes and then closes the pipe (`WriteRaw` + `CloseStdin`).
  Pinned by `TestWriteRawThenCloseStdinIsEOF` (transport) and by the session's
  own 1 MiB prompt test.

`cursor-agent` is one-shot per turn: it prints `system/init`, the turn's frames,
`result`, then exits. A session therefore runs **one process per prompt**; the
second prompt of a session is a new process with `--resume <chat id>` (the id the
first process reported in `system/init`).

## 2. Frame mapping

Everything is `internal/streamjson`'s fold with this package's `Profile`
(`adapters/cursor/project.go`): the same function runs the live pump and an
offline replay, so the two cannot disagree. `Project(frames)` is the replay entry
point used by the fixture tests.

| native frame | events | notes |
|---|---|---|
| `system/init` | `model` (from `model`) | the fold states it; `session_id` is the **chat id**, read by the session and stamped on records once known; `permissionMode`, `apiKeySource`, `cwd` stay in `Native` |
| `user` (prompt echo) | none | the prompt is our own request record; the echo stays in `Native` |
| `assistant` | `text_delta` | snapshots without message ids: only the text the frame adds is emitted (see §2.1) |
| `thinking` / `delta` | `reasoning` | **top-level** frame, not an assistant content block (the MUL-5231 lesson) |
| `thinking` / `completed` | none | closes a reasoning block; carries no text |
| `tool_call` / `started` | `tool_call_started` | `CallID`, `Tool` (`shell` from `shellToolCall`), `Input` = compact JSON of `args` |
| `tool_call` / `completed` | `tool_call_ended` | `Content` = stdout then stderr text parts; `Result` = `ok` when `result.success` is present, `failed` when another result key is; `ExitCode` + `ExitCodeReported` from `result.success.exitCode` |
| `result` | `turn_ended` + `usage` | see §2.2 |
| anything else | none | kept in `Native` and tallied (§4) |

Native quirks that are data, not accidents:

- `tool_call.call_id` packs two ids into one newline-separated string; the
  adapter keeps the first line, so an id never breaks a log line.
- An `assistant` frame can repeat the routing preamble (`> Auto routed to …`)
  that a previous frame already carried. The projection emits deltas, never the
  frame text verbatim, so the transcript is not duplicated.

### 2.1 Assistant snapshots without ids

Claude's projector needs the API message id (`--include-partial-messages` sends
`stream_event` deltas and then one completed block). Cursor's `assistant` frames
carry no id and are snapshots of the message so far: the recorded turn sends
`> Auto routed…`, then the words of the answer, then the cumulative message
again. oarlock keeps a per-lane accumulator and emits:

- the suffix when the frame's text extends what was already emitted,
- nothing when the frame repeats what was already emitted (equal, or a prefix of
  it), and
- the whole text as a new delta when it neither extends nor repeats — the
  runtime said something new.

Both recorded fixtures produce exactly the text the runtime's own `result`
reports, with no repeated piece (`adapters/cursor/fixture_test.go`).

### 2.2 `result`, usage and the outcome

- `turn_ended.outcome` is `aborted` when an abort was requested while the turn
  ran, `failed` when `is_error` is true or the subtype is `error` (a small
  classifier maps auth, quota, rate limit, model, input-too-large and overloaded
  prose to `FailureClass`; anything else is `unknown`, and the runtime's own
  words are kept in `Reason`), and `completed` otherwise.
- `usage` follows `turn_ended` in the same record, from `result.usage`:
  `inputTokens`, `outputTokens`, `cacheReadTokens`, `cacheWriteTokens` →
  `Tokens{Input, Output, CacheRead, CacheWrite}`. A key the frame omits stays
  nil ("not reported"); a reported `0` stays `0`.
  This is the CLI's figure, not the SDK's structured `run.wait()` result, and the
  matrix says so.

## 3. Capabilities and declared degradations

`Runtime()` declares `ID: "cursor"`, `Session: Open`, `Refused: Refused()`. The
session declares:

| fact | value | why |
|---|---|---|
| `steer` | **absent** (no `Steerer` method) | the CLI has no mid-run input: stdin is the prompt and it is closed to start the turn |
| `withdraw` | absent (no `Withdrawer`) | queued input is held in the adapter; withdrawing it is not implemented |
| queue | held, not durable | held input lives in the adapter process |
| attribution | `opaque` | no `parent_tool_use_id` has been measured; sub-agents are not attributed rather than guessed |
| images | false | the adapter sends none; an input with images is rejected `unsupported` |
| `contextBreakdown` | absent | the CLI reports no itemization |
| `login` / `authStatus` | absent | sign in with `cursor-agent login`; oarlock never sees a token |
| installer / account usage / inventories | absent | the CLI's preconditions and use are the user's |

Refused options, each with the reason `Refused()` declares and `Open` checks
before any process starts (one table, so declaration and behaviour cannot
drift):

| option | reason |
|---|---|
| `mcpServers`, `mcpServerCredentials` | the CLI has no per-session MCP flag; the user's own Cursor configuration applies |
| `launchArgs` | oarlock owns the CLI's argv (`-p`, `--output-format`, `--stream-partial-output`, `--force`, `--trust`, `--sandbox`): the invocation the fixtures were recorded from |
| `systemPrompt`, `appendSystemPrompt` | there is no system-prompt flag; oarlock refuses instead of splicing policy text into the user's prompt |
| `disallowedTools` | no deny channel: approvals are bypassed with `--force` |
| `additionalDirectories` | the CLI has no extra-workspace-root flag; `Cwd` is the workspace |
| `outputSchema` | no schema flag, and oarlock never validates output |
| `name` | no display-name flag |
| `serviceTier` | no tier flag |
| `userConfiguration: exclude` | the CLI always reads the user's configuration |
| `effort` (not in the frozen set; a local option constant) | there is no effort flag |

Honoured: `Cwd`, `Env`, `Model`, `Resume`. Everything in
`oarlock.AllRefusableOptions` is decided above: a future option is refused by the
fallback entry rather than dropped.

Not implemented, and not pretended: `Steerer`, `Withdrawer`,
`ContextBreakdownReader`, `LoginProvider`, `Installer`, `AccountUsageReader`,
`Upgrader`, `InventoryReader`, `ModelLister`.

## 4. Unknown frames are counted, never absorbed

The CLI's stream is undocumented. Two signals stay visible:

- The fold's tally of **top-level types it does not know** (`Session.Unknown()`),
  with the frames retained verbatim in `Native`.
- The Profile's tally of **known-type frames with a subtype it does not handle**
  (`thinking/progress`, `tool_call/progress`, …), also in `Session.Unknown()`.

`thinking` and `tool_call` are declared as the vendor's own types, so they are
not "unknown"; an *unknown subtype* of them is. A silent upstream rename
therefore shows up as a non-zero tally and a failing fixture test, not as a run
that reports success with no reasoning and no tools (MUL-5231).

## 5. Resume

- `Options.Resume` is a **chat id**, the value `system/init` reports as
  `session_id` (a UUID). It is passed as `--resume <chat id>`.
- This is **not** the SDK's agent id (`agent-<uuid>`). Old SDK sessions, and ids
  of that shape, are not resumable here; the CLI rejects them and oarlock
  reports the CLI's failure.
- A resumed session is a **new stream instance**: a new kernel, `Seq` from 1, no
  replay of the earlier conversation, and the same chat id once `system/init`
  confirms it. oarlock does not pre-validate the id (the CLI is the only
  authority), so a bad id opens and fails at the first turn.
- `(*Session).ChatID()` answers the id the CLI reported, `""` before the first
  `system/init`.

## 6. Abort and dispose

The CLI takes no mid-run input, so there is nothing to interrupt politely: an
abort ends the **process group** — SIGTERM, the abort grace, then SIGKILL (the
shared transport's `Close`). The abort is answered `accepted`; no `turn_ended`
is invented for a turn whose process was killed (the same rule claude's
take-over follows). `no_active_turn` when nothing is running.

A turn that reached its `result` frame is over; the CLI's exit that follows is
that turn's lifecycle, **not** the session's outcome, so it is not recorded as an
`exited` response (that would tell a host a session that can still `--resume` is
dead — the kernel treats any `exited` response as terminal). The status stays
available to the adapter as `(*Session).LastExit()` (exit code, signal, stderr
tail).

An exit **without** a `result` for the running turn — an abort, a crash, a
startup failure such as a rejected `--resume` id — *is* the session's outcome:
it is recorded as the `exited` response (exit code, signal, stderr tail as
`Reason`, cause in `Native`), and the kernel then answers every later control
`runtime_exited`. Nothing that ends a turn is invented: a process that died
without `result` gets no `turn_ended`. Continue that chat in a new session with
`ChatID()` and `--resume`.

`Dispose` is idempotent: it records one `dispose` request, drops held input,
closes the current process (if a turn is running) and waits for its exit — which
is then recorded as the `exited` response, because a disposed turn never reached
a `result`. A dispose with no turn running records the request and no exit: there
is no process to end. Later calls return the same error. Prompt/queue after
dispose are rejected `disposed`; after the process is gone, `runtime_exited`.

## 7. What is verified, and what is not

Verified offline (fixtures) and with fake processes:

- both recorded streams replay frame for frame (`fixture_test.go`), including
  reasoning deltas, the shell tool call with its exit code and output, the
  cumulative-assistant deduplication, and usage;
- unknown top-level types and unknown subtypes are counted and kept;
- a 1 MiB multi-line prompt over stdin does not deadlock;
- abort on a hanging process, dispose idempotence, queue drain, chat-id learning
  and `--resume` argv;
- `Steer` is absent at compile time and in a type assertion.

Not verified (kept honest):

- **live**: a real `cursor-agent` run is behind `OAR_CURSOR_LIVE=1`
  (`live_test.go`); CI skips it. On 2026-10-29 it was run green on this machine
  against `cursor-agent` 2026.09.18-9a7762b: one turn completed, the chat id was
  reported, and a second real turn continued that chat with `--resume`
  (41.95 s, 26 records). A **bad** `--resume` id was not exercised against the
  live CLI; the "exit without result" branch is proven with the fake agent.
- `tool_call` result shapes other than `shellToolCall`'s `result.success`
  (read/update/edit are classified only by "success or not"); a payload key set
  that is neither success nor failure yields `Result: unknown`, never a guess.
- `--stream-partial-output` ordering in a long multi-block answer; the
  accumulator is written from the two recorded turns plus unit cases.
- sub-agent attribution (`parent_tool_use_id`), per-session MCP, plugins: the CLI
  gives none of them, so `opaque` stays declared.
- images: the CLI may accept them; this adapter sends none and refuses them.

## 8. Tests

| file | what it pins |
|---|---|
| `adapters/cursor/fixture_test.go` | the two real streams, event by event (counts included) |
| `adapters/cursor/project_test.go` | the snapshot accumulator, tool call projection, result/usage edge cases, subtype tally |
| `adapters/cursor/session_test.go` | prompt/queue/abort/dispose, chat id + `--resume`, 1 MiB stdin, unknown events, capabilities, steer absence |
| `adapters/cursor/live_test.go` | `OAR_CURSOR_LIVE=1` only: one real turn |
| `internal/streamjson/vendor_test.go` | the Profile extensions and the free-text stdin transport |
