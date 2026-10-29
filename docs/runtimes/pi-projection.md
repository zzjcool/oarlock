# Pi projection (issue #11)

**Status:** implemented in [`adapters/pi/project.go`](../../adapters/pi/project.go), wired in
[`adapters/pi/session.go`](../../adapters/pi/session.go), tested in
[`adapters/pi/project_test.go`](../../adapters/pi/project_test.go). Evidence base:
[`pi.md`](pi.md) and [`testdata/pi`](../../testdata/pi/).

Tags: **evidenced (fixture frame N)** = a frame in `testdata/pi/*.frames.jsonl` (1-based, like `pi.md` §7);
**synthetic** = tested with a hand-written frame whose shape is read from the vendor source, no fixture exercises it;
**not exercised** = neither.

## 1. The function

```go
foldPiEvent(state piState, native json.RawMessage) (piState, []oarlock.Event)
```

- **Pure.** No I/O, no clock, no goroutine. The state it is given is never mutated (the unknown map is copy-on-write;
  token pointers are cloned). `TestFoldIsPure`.
- **One frame in, that frame's events out.** Nothing is read from a neighbouring frame and nothing is synthesized.
  The only turn end is the one `agent_settled` states.
- **Live and replay are the same function.** `Session.ingest` folds every frame with it; a replay folds the retained
  `Record.Native` bytes with it. `TestLiveEqualsReplay` pushes both fixtures through a real `Session` and asserts
  `replay == live` record by record, then checks the `internal/fold` answers.
- The one reading that cannot be pure is `response(prompt)` with `disposition: "handled"` → `input_handled`: it needs
  the prompt *request* it answers, which only the control plane knows (#10). It stays in `Session.projectResponse`; a
  replay of **retained records** already has it in `Events`, a replay of **bare native frames** does not and cannot.

## 2. Mapping

| native frame | events | evidence |
|---|---|---|
| `agent_start` | `turn_active` | evidenced (6) |
| `agent_settled` | `turn_ended{Outcome}`; per-run state reset. Outcome: `aborted` flag, else last assistant `stopReason == "aborted"`, → `aborted`; else the run's provider failure → `failed{classified}`; else `completed` | evidenced (normal 23, abort 24) |
| `turn_end` | none. Supplies `stopReason` and the provider error to the settlement; **the last assistant `turn_end` of a run wins**, so a retry that succeeded is `completed` | evidenced (normal 18, abort 19); error path synthetic |
| `message_start` role `user` | `user_message{Text, Evidence: conversation}` (string or text parts joined) | evidenced (10) |
| `message_start` role `system` / `assistant` | none | evidenced (8, 12) |
| `message_end` role `assistant` | `usage{Tokens}` (§3) | evidenced (normal 16, abort 17) |
| `message_update` `text_delta` | `text_delta{Text}` | evidenced (normal 14) |
| `message_update` `thinking_delta` | `reasoning{Text}`; an empty delta says nothing | evidenced (abort 14–16) |
| `message_update` `thinking_start` / `thinking_end` | none; a thinking block that streamed **no** text yields one `reasoning` with no text at its end | evidenced (abort 13); empty-block rule synthetic |
| `message_update` `error` | none; records the provider failure for the settlement | synthetic |
| `message_update` `start`, `done`, `text_start`, `text_end`, `toolcall_*` | none, **not** counted unknown | evidenced (`text_*`); the rest synthetic |
| `tool_execution_start` | `tool_call_started{CallID, Tool, Input}`; `Input` is the compacted `args` JSON, complete | synthetic |
| `tool_execution_update` | `tool_call_progress{CallID, Output}`; `Output` is the compacted `partialResult` JSON, a snapshot | synthetic |
| `tool_execution_end` | `tool_call_ended{CallID, Content, Result, ExitCode}`; `Content` = `result.content` blocks (text / image / other), `Result` **only** when `isError` is a boolean, `ExitCode` **only** from `result.structuredContent.exit_code` (a reported 0 stays 0, a signal is 128+n, a fraction or string is not reported) | synthetic; field shape from `core/tools/bash.js` |
| `compaction_start` | `compaction_started{Trigger}` | synthetic |
| `compaction_end` | `compaction_ended{CompactionResult, Trigger}`: `aborted` > `errorMessage` > `completed` | synthetic |
| `thinking_level_changed` | `effort{Effort}` | synthetic |
| `auto_retry_start`, `summarization_retry_scheduled` | `retry{Attempt, MaxAttempts, DelayMS, Reason}` | synthetic |
| `response(get_state)` | `model{provider/id}` + `effort{thinkingLevel}` (the runtime's word, not the request echoed) | evidenced (4; abort 26) |
| `response(get_session_stats)` | `usage{Context{Tokens, ContextWindow, Percent}}` (§4) | synthetic; response shape observed once on this host, frame not committed |
| `response(prompt)` | none here; `input_handled` only via the control plane | evidenced (5) for `started` |
| `response` of any other command, or `success:false` | none | evidenced (abort 25 `abort`) |
| `turn_start`, `agent_end`, `queue_update`, `entry_appended`, `session_info_changed`, `auto_retry_end`, `summarization_retry_attempt_start`, `summarization_retry_finished`, `bash_execution_update`, `extension_ui_request`, `extension_error` | none, **not** counted unknown | `turn_start`, `agent_end`, `extension_ui_request` evidenced; the rest not exercised |

Decisions worth stating:

- **Tool arguments are complete at `tool_execution_start`.** No `tool_call_input` or `tool_call_input_delta` is produced:
  the `toolcall_*` streaming frames precede the call, and their deltas carry no call id to attach to.
- **`turn_end` is not a turn end.** A run with tools has several; OAR and `pi.md` fact 2 agree that `agent_settled` is the end.
- **Divergence from OAR `projection.ts`:** OAR keeps `providerError` once set; here a later assistant `turn_end` with a
  non-error `stopReason` clears it, because Pi retries inside one run and the run that finally succeeded is not a failure.
  A `turn_end` with `stopReason: "error"` and **no** `errorMessage` keeps the more specific text an earlier
  `message_update` error carried.
- `extension_ui_request` stays a plain frame (it is a `toApp` request in OAR's TypeScript; here that is #10's scope).

## 3. Usage

Pi accounting → `UsageReport.Tokens` of the root agent:

| Pi `message.usage` | `TokenTotals` |
|---|---|
| `input` (excludes cache) | `Input += input + cacheRead + cacheWrite` |
| `output` (includes reasoning) | `Output += output` |
| `cacheRead` / `cacheWrite` | own parts, present only once a message reported them |
| `reasoning` | own part, present only once reported |
| `cacheWrite1h` | a subset of `cacheWrite`; adds nothing |

- The total is **running, counted from session open, never reset by a turn** (only per-run failure state resets).
- Only `message_end` of an `assistant` message counts. `message_update.usage` is a snapshot of the same message and is
  deliberately **not** summed (it would double count).
- **Nil, never zero.** No usage object, or a non-numeric / negative `input` or `output`, yields **no event**. A reported
  zero stays zero: normal-turn frame 16 reports `reasoning: 0`, `cacheWrite: 0` (kept as `0`); abort-turn frame 17 reports
  all-zero usage with **no** `reasoning` key (`Reasoning` stays `nil`). `TestUsageNilNeverZero`.
- **Attribution `none`:** the adapter fills `Tokens` and leaves `Total` nil; `internal/fold` derives `Total` from the one
  root figure, `ByAgent` is elided (only root), `Unattributed` is nil.
- Normal turn, exact: `Input 510 (254+256+0)`, `Output 2`, `CacheRead 256`, `CacheWrite 0`, `Reasoning 0`.

## 4. Context

After a recorded `agent_settled`, `Session.afterSettle` sends `get_session_stats` (own goroutine, bounded 10 s wait, the
Conn's own command timeout still applies; never kills the child; skipped once the session is unreachable). The reply is
an ordinary frame, so it is recorded and read by the same fold into `usage{Context}`. Only `contextUsage` is projected;
`stats.tokens` counts the whole session file and is not used.

- `tokens: null` / `percent: null` (Pi right after a compaction) stay `nil`; `contextWindow` is still reported.
- A transport failure, an exit, a timeout or a refusal produces **no** context event, so the fold's `ContextUsage()`
  stays `nil`; nothing is invented and the session is undisturbed.
- **Queue ordering:** the held-input drain waits (bounded) for that frame, so the stats frame precedes the next prompt in
  the stream and the figure describes the settled turn. `TestQueueDrainWaitsForTheStatsFrame`.
- **Evidence:** the committed fixtures contain **no** `get_session_stats` frame (`pi.md` §3). Shape observed once on this
  host, frame not committed:
  `{"type":"response","command":"get_session_stats","success":true,"data":{"sessionId":"…","userMessages":0,
  "assistantMessages":0,"toolCalls":0,"toolResults":0,"totalMessages":0,"tokens":{"input":0,"output":0,"cacheRead":0,
  "cacheWrite":0,"total":0},"cost":0,"contextUsage":{"tokens":0,"contextWindow":1000000,"percent":0}}}`.
  The settle → stats ordering is tested against the fake Conn only (`TestContextAfterSettleComesFromGetSessionStats`).

## 5. Failure classification

Evidence order, as OAR `failure.ts`: **(1)** pi-ai 1.4.0 `isContextOverflow` (`dist/utils/overflow.js`, pattern tables
ported verbatim) → **(2)** the provider error body's `code`, then `type` → **(3)** the HTTP status the message is
prefixed with (`<status> <body>`) → **(4)** `unknown`. Prose is never the branch word.

| issue #11 name | `FailureClass` | e.g. |
|---|---|---|
| `auth` | `auth` | `401 …authentication_error`, `invalid_api_key` |
| `rate_limit` | `rate_limited` | `429 …`, `rate_limit_exceeded` |
| `context_overflow` | `input_too_large` | `prompt is too long: …`, `exceeds the context window`, `413 request_too_large` |
| `unknown` | `unknown` | no status, no recognised body |
| (evidenced by OAR) | `quota`, `billing`, `invalid_request`, `overloaded`, `provider`, `model_unavailable` | `insufficient_quota`, `503`, `500`, … |

The issue's names map onto the **frozen** `FailureClass` vocabulary; no new class was added. `Status` carries the HTTP
status when the message had one; `Reason` is the runtime's own message. The non-overflow exclusions (`Throttling error:` /
`Service unavailable:` prefixes, `rate limit`, `too many requests`) win over overflow patterns; `too many tokens` alone is
an overflow pattern in pi-ai. `TestOutcomeAndFailureClasses` covers all four required classes and the edges.

## 6. Text coalescing is a reading

`coalesceText(records) []readEvent` returns a merged **view in a new slice**. The stream is untouched: one record per
native frame, `Native` verbatim, `Events` as projected, `Seq` dense.

| | stream | coalesced reading |
|---|---|---|
| abort-turn thinking deltas (frames 14, 15, 16) | 3 records, 3 `reasoning` events, `Seq` 14, 15, 16 | 1 event `We need answer`, `FirstSeq 14`, `Seq 16` |
| records | 26 | 26 (unchanged) |
| events | E | E − 2 (N pieces → 1 removes N−1) |

A run ends at any other event, another agent path or another `MessageID`; a frame that states **no** event (`text_end`,
`extension_ui_request`) does not end a run; a redacted or empty reasoning item never merges. Pi sets no `MessageID`, so the
merge key is (kind, agent path). `internal/fold`'s `Conversation()` merges only under a non-empty `MessageID`, so for Pi it
shows one message per delta; `coalesceText` is the merged reading. `TestCoalesceTextIsAReadingOnly`.

## 7. Unknown events

A native `type` outside the known set above produces **no event** and is tallied in the state: `message_update/<sub>` for
an unknown `assistantMessageEvent` sub-type, `""` for a frame with no string type or a non-object. The frame stays in
`Record.Native` verbatim and in order; nothing is coerced, renamed or dropped. `Session.Unknown()` exposes the tally; a
non-zero tally is a **test signal**, not a log line (`pi.md` §9). Both fixtures tally zero.
`adapters/pi/conn.go`'s `Stats.Unknown` is the transport's tally against its own **narrower** set (it has no
`tool_execution_*`, `compaction_*`, …), so the two differ by design; the projection's is authoritative for event reading.
`TestUnknownEventsAreCountedAndKept`.

## 8. Gaps

- Tool-using turns, compaction, auto-retry, `extension_error` and `thinking_level_changed` are in **no** committed fixture;
  their mappings are read from the vendor source and exercised with synthetic frames. Same for the provider-error path:
  the OAR vendor 400 test is not reproduced.
- No `get_session_stats` frame is committed (§4).
- `isContextOverflow`'s **silent** overflow cases (z.ai, Xiaomi MiMo) need the model's `contextWindow`, which a frame does
  not carry: not ported.
- No `Event` slot for `compaction_end.errorMessage`, `agent_end.willRetry`, `tool_execution_*.parentToolCallId`; they stay in `Native`.
- No model/effort *change* event beyond `get_state` and `thinking_level_changed`; there is no `set_model` path yet (#13).
- `session_info_changed`, `queue_update`, `entry_appended` are known and event-less; no `input_queued` is projected
  from `queue_update` (Pi's queue is not used, #10). A `steer` populates `steering`, and that `queue_update` is still
  projected as **no event** (the next `queue_update` empties `steering`); the steered text reaches the stream as the
  ordinary `role:"user"` `message_start` → `user_message` (issue #45).

## Verification

```
gofmt -l .                # empty
go vet ./...
go test -race ./...
GOOS=windows go build ./...
```

Tests: `TestGoldenNormalTurn`, `TestGoldenAbortTurn`, `TestLiveEqualsReplay`, `TestContextAfterSettleComesFromGetSessionStats`,
`TestQueueDrainWaitsForTheStatsFrame`, `TestUnknownEventsAreCountedAndKept`, `TestToolCallMapping`, `TestUsageNilNeverZero`,
`TestOutcomeAndFailureClasses`, `TestMessageUpdateAndLifecycleMapping`, `TestCoalesceTextIsAReadingOnly`, `TestFoldIsPure`.
