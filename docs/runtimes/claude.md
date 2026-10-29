# Claude Code: `claude -p` over stream-json (issues #19, #42, #53)

Oarlock drives Claude Code in print mode over Claude Code's own stream-json
dialect. Unlike Cursor's CLI (one process per turn, the prompt is stdin and then
closed), claude's stdin stays **open for the life of the session**: every prompt
and every steer is one `user` line written into the same process, and the answer
is frames until the turn's `result`. That open stdin is why claude has a real
`steer` and Cursor has none.

The parser, framing, transport and fold are `internal/streamjson` (the
stream-json family claude and Cursor share, issue #22). `adapters/claude` owns
only what is Claude's alone: argv, refused options, usage, context, failure
classification, tasks, the steer echo protocol, resume, the context breakdown
and (issue #53) the native image content, a `Withdrawer` over the held queue,
the `cancel_queued` abort option and permission answers.
There is one parser in the module, on purpose.

Evidence tags below, the same idea as [pi.md](pi.md):

- **已实测** — executed live on this host with `claude` 2.1.312, all on
  2026-10-29: the #42 raw probes and the opt-in live Go tests, against a
  third-party Anthropic-compatible endpoint (`ANTHROPIC_BASE_URL` pointing at a
  magpie-style gateway, model `workbuddy/deepseek-v4.1-flash`). **No official
  Claude account was involved.** The probe frames were captured under `/tmp`
  and are not committed.
- **offline** — a committed recording made on 2026-10-10 from claude **2.1.292**
  (`adapters/claude/testdata/*.jsonl`; every `system/init` there that carries
  `claude_code_version` says `2.1.292`) or a fake `claude` process (the test
  binary re-executed, `helper_test.go`), or a unit test of a pure function.
- **未验证 / unverified** — source-read only or not exercised, and not claimed.

## 1. The invocation

```sh
claude -p --input-format stream-json --output-format stream-json \
  --verbose --replay-user-messages --include-partial-messages \
  --dangerously-skip-permissions --session-id <uuid> \
  [--resume <id>] [--name <n>] [--model <m>] [--effort <e>] \
  [--system-prompt <p>] [--append-system-prompt <p>] [--json-schema <j>] \
  [launchArgs…] [--add-dir <dirs…>] [--disallowed-tools <tools…>]
```

`BaseArgs()` is the fixed part, in this order (`args.go`):

- `-p` — print mode, the only mode that speaks stream-json.
- `--input-format stream-json --output-format stream-json` — the shared dialect.
- `--verbose` — claude refuses print-mode stream-json without it ("When using
  --print, --output-format=stream-json requires --verbose", 已实测), and it is
  what makes every frame show up rather than only the final result.
- `--replay-user-messages` — **re-emits every `user` line from stdin as a `user`
  frame with `isReplay: true` and the same `uuid`.** This flag is the entire
  steer protocol (see §4); it still exists in 2.1.312 (`claude --help`, 已实测).
- `--include-partial-messages` — `stream_event` deltas before the completed
  `assistant` block (text deltas, thinking deltas, tool-input deltas).
- `--dangerously-skip-permissions` — headless: a permission prompt is a hang, not
  safety (the same reasoning as cursor's `--force`).

`--session-id` is the id oarlock assigns (a fresh UUID); `--resume <id>` is
`Options.Resume` and replaces it. `--name` is only passed for a fresh session
(on a resume it would rename the user's conversation). The rest of the options
are `Options` verbatim, and `launchArgs` come before the variadic ones
(`--add-dir`, `--disallowed-tools`) so none of them swallows a host flag.

A host `LaunchArgs` entry that contradicts a flag oarlock manages is refused
before anything starts (`checkLaunchArgs`): `--continue`/`-c`, `--resume`/`-r`,
`--session-id`, `--fork-session`, `--permission-mode`,
`--dangerously-skip-permissions`, `--print`/`-p`, `--input-format`,
`--output-format`, `--json-schema`, `--verbose`, `--replay-user-messages`,
`--include-partial-messages` — each with the reason (use `Options.X`, or "oarlock
already sets it").

Environment: a claude started inside another claude inherits markers of that
parent session (`CLAUDECODE`, `CLAUDE_CODE_SESSION_ID`, `CLAUDE_CODE_ENTRYPOINT`,
…); `sessionEnv` drops all of them and then applies `Options.Env` (a nil value
removes a variable). `CLAUDECODE` is never settable — claude refuses to start
nested under it. The child runs in its own process group (`internal/exec`), so
abort and dispose can end the whole tree.

The prompt/steer body is one LF-terminated line on stdin, never argv
(`args_test.go: TestUserMessageMatchesTheRecordedStdinLine` against
`testdata/steer-stdin.jsonl`):

```json
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"…"}]},"uuid":"<input id>"}
```

That recording is text-only; an input carrying images puts native `image`
blocks **before** that text block and drops the text block when there is no
text (§3, Images row).

No `session_id` and no `parent_tool_use_id` are needed in that line: 2.1.312
echoes the message back as a `user` frame with `isReplay: true` and the same
`uuid` (its own `session_id`, `parent_tool_use_id` and `timestamp` added) —
已实测, §4.

Resume (`open.go`): a resumed session is a new stream instance — a new kernel,
`Seq` from 1, no replay of the earlier conversation — opened with
`--resume <id>` and confirmed by a private `initialize` control_request, then a
private `get_usage` readback for the token baseline. A missing conversation is
`*oarlock.SessionNotFound`, and only on positive evidence: claude's
`result/error_during_execution` whose first error begins "No conversation found
with session ID" (claude gives no code; prose is the last resort). A
`systemPrompt`/`appendSystemPrompt` on a resume is refused `unsupported`: claude
ignores it and oarlock will not pretend it was applied.

## 2. Layout and the shared fold

| file | what lives there |
|---|---|
| `args.go` | `BaseArgs`, managed-flag refusals, `Refused()` (over `refusedTable`, declared in `open.go`), `launchArgs`, env hygiene |
| `open.go` | `Open`/`open`, refused options before launch, session id, resume confirmation, usage baseline |
| `session.go` | the control plane: `Prompt`, `Steer`, `Queue`, `Abort`, `Dispose`, the pump, exit/watch |
| `project.go` | the `Profile`: `result` classification, usage, context, tasks, service tier, plus `Project()` |
| `failure.go` | the failure classifier (auth / rate limit / quota / input too large / …) |
| `usage.go` | `result.usage` vs `result.modelUsage`, resume baseline, context reading |
| `breakdown.go` | `ContextBreakdownReader` (`get_context_usage`, detail `full`) |
| `images.go` | the native image content builder: one base64 `image` block per `InputImage` ahead of the text block (png / jpeg / gif / webp), and its refusals |
| `permission.go` | `Answer` (the host's reply to a runtime request, recorded `answered` only after a successful write) and the allow policy ladder (`choosePermission`, `PermissionPolicy`) |
| `ops.go` | install / login / logout / auth status / self-update / inventory (issue #23) |

`internal/streamjson` owns everything the dialect shares: framing (one LF
line = one frame), the bidirectional `Launch`/`Conn` transport (writer
goroutine, read/write decoupled), `Folder`, and the `Profile` interface.
`adapters/claude/project.go` implements that interface and nothing else;
`Project(frames, resumed)` replays a recording through the **same** folder the
live pump uses, so the two cannot disagree. The fold also declares
`command_lifecycle` a known top-level type (it is claude's own input receipt);
it stays in `Native` and states no event — see §4 on the one place the session
reads it.

Unknown top-level types are counted in `Session.Unknown()` and kept verbatim in
`Native`; a non-zero tally is a test signal (an upstream rename), never silently
absorbed.

Attribution is `attributed`: sub-agent frames carry `parent_tool_use_id` and the
fold keeps them in a lane (a nested `Task` chain is the `AgentPath`). Task frames
(`system/task_started`, `task_updated`, `task_progress`, `task_notification`)
become `task_*` events with claude's own task id, type and description.

Version policy: evidence baseline **2.1.292+** (the [version policy
table](../compat-matrix.md#version-policy)). The version is read by the ops
probe (`claude --version`) for install/update, not by `Open`; when claude
reports `claude_code_version` in `system/init` it stays in `Native`, and
oarlock never synthesizes a version frame.

## 3. Controls

| control | implementation | what the caller gets |
|---|---|---|
| **Prompt** | one `user` line on stdin (image blocks ahead of the text block — §3, Images row) | accepted once written; `busy` while a turn runs (`no_active_turn` is steer's answer, not prompt's); `runtime_exited` after the process is gone; `unsupported` for an image type outside png/jpeg/gif/webp; `error` when an image file cannot be read |
| **Steer** | the same `user` line, carrying the caller's `InputID` as `uuid` | **accepted only when claude echoes that uuid back** (`user` frame, `isReplay: true`); otherwise rejected and the reason owns the input — see §4 |
| **Queue** | held in the adapter (`held []heldInput`), sent as an ordinary prompt on the next idle | accepted immediately, drained on turn end (and only while the stream says idle); a held entry keeps its `InputID` and its `[]oarlock.InputImage` and is re-prompted with them; **not durable** — lost on exit or dispose; no claude-side queue is used; `Withdraw` takes it back (next row) |
| **Abort** | `control_request {subtype: "interrupt"}`, carrying `"cancel_queued": true` only when the root `system/init` advertised `interrupt_cancel_queued_v1` **and** a written prompt-like input has no `command_lifecycle started` yet (computed under the session lock at write time; otherwise the request is exactly `{"subtype":"interrupt"}`), plus the take-over timer (`AbortGrace`, default 10 s); `(*Session).SupportsCancelQueued()` reports claude's own declaration | accepted; the turn ends `aborted` from claude's own result (an `is_error: true` frame, read as aborted because an abort was requested); if no `result` arrives in the grace the process group gets SIGTERM then SIGKILL, the exit is recorded, and **no `turn_ended` is invented**; claude's refusal → rejected `runtime_refused`; a build that never advertised the capability leaves queued input untouched, abort keeps its `no_active_turn` gate, and it is never widened into "cancel only" |
| **Withdraw** | `Withdraw(ctx, inputID)` — `*Session` implements `oarlock.Withdrawer`; the entry is spliced out of the adapter-held queue under the same lock the drain pops the head under (withdraw and drain cannot interleave), and the request is recorded through `k.Control` (`oarlock.ActionWithdraw`) so the queue request, its response and the withdraw request/response all stay in the stream | `accepted` when at least one held entry carried that `inputId` (removed head-first, order kept); `not_queued` when none does — already sent, never queued in this session, or already withdrawn — so an entry the drain may have taken is **never** answered `accepted`; an empty id names no held input and is refused `unsupported` |
| **Images** | native, `Capabilities.Images=true`: one `{"type":"image","source":{"type":"base64","media_type":…,"data":…}}` block per image, all of them before the single text block (the text block is omitted for image-only input); bytes are inlined base64 std and never logged; prompt, steer and queue alike | accepted for exactly png / jpeg / gif / webp (explicit `InputImage.MediaType` first — a type outside the set is refused, not re-derived from the extension — else the file extension, case-insensitive); any other type refused `unsupported`; an unreadable file refused `error`; no size limit of oarlock's own (OAR has none either) |
| **ContextBreakdown** | `control_request {subtype: "get_context_usage", detail: "full"}` under a private id | categories in claude's order and words with tokens, kinds (`used`, `deferred`, `free`, and `reserved` for claude's `buffer`) and per-entry items; answered now and **not recorded** (it carries local paths) |
| **Resume** | `--resume <id>`, `SessionNotFound` on positive evidence only | §1 |
| **Answer** | `Answer(ctx, requestID, result)` writes claude's own `{"type":"control_response","response":{"subtype":"success","request_id":…,"response":…}}` and records an `answered` response **only when the write reached the wire** (never a fabricated answer); `ErrUnknownRequest` / `ErrAlreadyAnswered` are checked under the session lock before the write; `SessionOptions.Permissions` picks the policy | a host can answer a pending runtime `control_request` (e.g. `can_use_tool`) with the runtime's own response object; **default `PermissionRecordOnly`**: the request is recorded as a `toApp` request and never answered — headless claude runs with `--dangerously-skip-permissions` and cannot emit `can_use_tool`, so an invented allow would be a claim; opt-in `PermissionAllow` answers the ladder `allow_always` > `allow_once` > `cancelled`, where `cancelled` means **no answer is sent** and the request stays unanswered |
| **Dispose** | close stdin, SIGTERM/SIGKILL the group, record the `exited` response | idempotent; later controls answer `disposed` then `runtime_exited` |

Not implemented, and not pretended: `LoginProvider` answers beyond the
challenge, `AccountUsageReader`, `ModelLister`.

## 4. Steer: the echo protocol (issue #42)

**The rule.** `Steer` writes a `user` line with the caller's uuid and waits. The
only accepted evidence is claude's replayed `user` frame with `isReplay: true`
and the same uuid — the fold reads it as a `user_message` event with
`Evidence: acknowledged`, `InputID == NativeMessageID == uuid` and the text. A
`user` frame that is a tool result is a tool result, not an echo. **An accepted
response is never recorded without that echo**; on rejection the input stays the
caller's (or, per the lifecycle ownership below, claude's).

**What was measured live (已实测, claude 2.1.312, third-party endpoint,
2026-10-29).** The wire format is correct and `--replay-user-messages` still
echoes in 2.1.312. The CLI's own receipt,
`command_lifecycle {command_uuid, state: queued|started|completed}`, is emitted
for both the prompt and the steer; the adapter records it as a native frame and
does **not** drive status from it — it is used only to state who owns a steer
whose echo never came (see below). `command_uuid` is the uuid of the input that
was written. What the echo does next depends on the turn's shape:

*Tool-boundary turn* — prompt "run Bash `sleep 8`, then answer"; the steer is
written ~2.5 s in, while the tool runs:

| t | frame |
|---|---|
| 2.5 s | host writes the steer `user` line; `command_lifecycle … queued` |
| when `sleep 8` ends | tool result (`user` / tool_result frame) |
| after it | `user` `isReplay: true`, **the steer's uuid** — about 8.5 s after the write, *inside* the active turn |
| then | `command_lifecycle … started`, then `completed`; the turn's `result` is success |

The adapter accepts this shape: the echo arrives while the turn is still
running, which is what `TestLiveSteerEcho` exercises (§6).

*Single-streaming-message turn* — a pure text answer, steer written 2 s in:

| t | frame |
|---|---|
| 2.00 s | steer written; `command_lifecycle … queued` |
| 7.88 s | the turn's `result` (success) |
| 7.89 s | `command_lifecycle … started` for the steer |
| 10.96 s | `user` `isReplay: true`, the steer's uuid — **after the `result`**, i.e. the first model request of a **follow-up turn** |
| 12.35 s | that follow-up turn's `result` |

The adapter rejected the steer: the turn ended before the echo, so there was no
turn for the input to land in. This is not an adapter format bug and not a
missing replay: claude 2.1.312 **queues a mid-turn user message natively and
applies it at the next model-request boundary** (its own queue, not oarlock's
held queue of §3), and a turn with no further request boundary has none until
after its `result`. The same call in a tool-boundary turn is applied mid-turn
and accepted.

**Ownership in the rejection reason.** Since #42 the session tracks claude's
`command_lifecycle` states for the pending steer uuid. If claude showed it
received the input (`queued`/`started`/`completed`) and the echo never arrived
inside the turn (turn end, exit, `EchoTimeout` or an abandoned context), the
rejection reason says so and adds *claude received the steer
(`command_lifecycle <state>`) and owns it from here; do not resend it* — a
second write would duplicate the input. On an exit the sentence is "claude
received the steer (`command_lifecycle <state>`) before the exit; it may not
have been applied": the process is gone, so a resend into a new session can
be the right move. After a `cancelled`
receipt (claude dropped the input; the state is handled but 未验证 — only
`queued`/`started`/`completed` were observed live), or when no lifecycle frame
was seen at all, the older reason stands: *the input is still yours* (and, on a
timeout, "a late echo would appear as a `user_message` frame"). Either way the
outcome is `rejected`: acceptance requires an echo, and a lifecycle frame is
never evidence of one.

**`EchoTimeout`** (default 10 s, `DefaultEchoTimeout`) bounds the wait for the
echo after the write (the next model-request boundary is what makes it
possible, not something the timer resets at). A steer queued behind a tool call longer than the
timeout can be rejected even though claude applies it later; the caller sees the
ownership note and must not resend. `SessionOptions.EchoTimeout` is only
reachable inside the package (`NewSession` and the tests); the public `Open`
does not take a timeout, so a host never silently changes the bound.

**What the fixture pins.** `testdata/steer-lifecycle.jsonl` (recorded on
2026-10-10 from claude 2.1.292) is a tool-boundary turn: prompt, task
notification, the tool result and, after it, the steer's `user` `isReplay: true`
echo with the steer's uuid (its `timestamp` is the moment the host wrote the
line, not the moment claude echoed it), lifecycle `queued` → `started` →
`completed` for the steer uuid, and a success
`result`. `TestReplaySteerEchoAndPartialStreaming` asserts the echo event's text,
uuid and `acknowledged` evidence, and that the streamed text is not repeated by
the completed `assistant` frame.

## 5. Usage, context and breakdown

- `result.usage` is the **main agent loop only** (claude's schema excludes Task
  sub-agents and auxiliary calls), per turn: summed over the session's results
  it is the root agent's `Tokens`.
- `result.modelUsage` is every model call of the session (sub-agents,
  sidechains, compaction), cumulative: the latest frame wins and is the
  session `Total`. The fold reports `Total` minus what agents account for as
  `Unattributed` — a sub-agent gets no per-agent entry because only its first
  frame carries `parent_tool_use_id` and that frame's `usage` is the
  stream-start value.
- A resumed session's running total continues the previous process's, so
  `open` reads `get_usage` (`skip_behaviors`) and counts from that baseline; a
  baseline that cannot be read leaves `Total` unreported, never guessed.
- Context is what the **latest root model call sent** (`assistant.message.usage`
  input + cache read + cache write; `output_tokens` is the stream's start), and
  the window is `result.modelUsage[model].contextWindow`. A compaction after the
  latest call leaves the tokens unknown but keeps the window.
- `ContextBreakdown` asks claude's own `get_context_usage`; the answer carries
  memory files, skills, custom agents and MCP tools, which is why it is read
  and not recorded. Refusals are errors carrying claude's words; after dispose
  it is `(nil, nil)` — nobody to ask.

## 6. Live tests

Opt-in only (`adapters/claude/live_test.go`). Without the env the tests
`t.Skip` and the rest of the suite stays offline:

```sh
OAR_CLAUDE_LIVE=1 ANTHROPIC_BASE_URL=… ANTHROPIC_AUTH_TOKEN=… \
  ANTHROPIC_MODEL=workbuddy/deepseek-v4.1-flash IS_SANDBOX=1 \
  go test ./adapters/claude -run 'TestLive' -v -count=1 -timeout 300s
```

- `TestLiveSteerEcho` — a turn with a request boundary (the prompt asks for a
  Bash round; the steer is written once the stream shows a `task_started` or
  `tool_call_started` frame) and **must** see the echo and be accepted. On
  failure it prints redacted diagnostics: every `user` frame's
  `isReplay`/`uuid`, the `command_lifecycle` command/state, the `result`
  subtypes and the `system` subtypes. A reachable runtime that produces no
  request boundary, or no echo inside one, is a **failure**, not a skip.
- `TestLivePromptAndWait` — the public `oarlock.PromptAndWait`: one prompt, one
  completed turn, the expected word in `RunResult.Text`.

`claude` must be on `PATH`; credentials come from the caller's environment
(values from `/root/.pi/agent/auth.json` are exported into the run, never
printed and never committed). `IS_SANDBOX=1` lets the CLI run as root.

**Status on this machine (2026-10-29, `claude` 2.1.312, third-party endpoint,
model `workbuddy/deepseek-v4.1-flash`): PASS** — an excerpt of the run (the
session uuid elided):

```text
$ OAR_CLAUDE_LIVE=1 … go test ./adapters/claude -run 'TestLive' -v -count=1 -timeout 300s
=== RUN   TestLiveSteerEcho
    live_test.go:69: request boundary reached: 0 task_started, 1 tool_call_started frames
    live_test.go:95: seq 37 +7.31s command_lifecycle state=queued for steer <uuid>
    live_test.go:95: seq 46 +13.58s user echo for steer <uuid> (evidence acknowledged)
--- PASS: TestLiveSteerEcho (13.98s)
=== RUN   TestLivePromptAndWait
    live_test.go:118: PromptAndWait completed the turn; prompt answered at seq 14; 14 records
--- PASS: TestLivePromptAndWait (5.87s)
PASS
ok  	github.com/zzjcool/oarlock/adapters/claude	19.853s
```

The steer leg steered a live `Bash sleep 4` turn; the echo arrived inside the
turn rather than after its `result`, and the adapter accepted it **on that
echo**. The wire facts in §4 are the earlier raw probes (`sleep 8`, 2026-10-29);
the offline suite still skips both tests without the env.

## 7. Fixtures and tests

| file | what it pins |
|---|---|
| `args_test.go` | `BaseArgs` flags in order; option → argv; managed `launchArgs` refused; `Refused()` is the table `Open` checks; parent-session env markers dropped; the stdin `user` line vs `testdata/steer-stdin.jsonl` |
| `testdata/steer-stdin.jsonl` | the two lines the host writes for the recorded turn: the prompt `user` line and the interrupt `control_request` (`args_test.go` compares `userMessage` with the first) |
| `testdata/tool-round.jsonl`, `multi-turn.jsonl`, `error.jsonl` | a real tool round, an unprompted-init turn, `is_error:true` + `subtype:"success"` |
| `testdata/usage-subagent.jsonl` | sub-agents, compaction, token totals, service tier |
| `testdata/steer-lifecycle.jsonl` | a tool-boundary turn with the steer echo (see §4) |
| `testdata/interrupt.jsonl` | the interrupt `control_response` and the aborted turn's `is_error` result |
| `replay_test.go` | `Project` over every recording, event by event: `TestReplayToolRound`, `TestReplayMultiTurnTurnActiveOnlyForUnpromptedInit`, `TestReplayErrorResultIsFailedDespiteSubtypeSuccess`, `TestReplayUsageSubagentCompact`, `TestReplaySteerEchoAndPartialStreaming`, `TestReplayInterruptResult`, `TestReplayUnknownTopLevelTypesAreCounted` |
| `session_test.go` | the fake claude (the test binary re-executed): the shared behaviour suite, prompt/usage, `TestSteerAcceptedOnlyWithEcho`, `TestSteerRejectedWithoutEchoKeepsTheInput` (no lifecycle frame → "still yours"), `TestSteerDeferredLifecycleIsRejectedAsRuntimeOwned` (queued receipt → "do not resend", still rejected), `TestSteerEchoTimeoutWithLifecycleIsRuntimeOwned` (the same over `EchoTimeout`), `TestSteerAcceptedWithLifecycleFramesOnTheEcho`, `TestSteerIdleIsNoActiveTurn`, abort (honoured, refused, take-over, SIGTERM group), `cancel_queued` on the interrupt only after the root init advertises it **and** a written prompt is still unstarted (and never for a steered input, never without the capability), withdraw (`accepted` by `inputId`, `not_queued` once the drain has taken the entry, `unsupported` for an empty id), dispose, queue-after-turn, `toApp` request recorded, open argv |
| `helper_test.go` | the fake `claude` process and its switches (`noecho`, `ignoreabort`, `refuseabort`, `nobaseline`, `nobreakdown`, `lifecycle`, `defersteer`, `pushperm`, `noinitcaps`), the stdin recorder (`CLAUDE_HELPER_STDIN`: the lines the fake actually received, so a test asserts the wire and not the intent), and its default `system/init` capabilities (`interrupt_cancel_queued_v1` unless `noinitcaps`) |
| `images_test.go` | the native content of a prompt, a steer and a drained queue entry: `image` blocks before the text block, no text block for image-only input, every accepted media type plus the extension table (case-insensitive), `unsupported` for a foreign type, `error` for an unreadable file, `Capabilities.Images`, and that no image bytes land in the record |
| `permission_test.go` | `choosePermission`'s order and ties, `Answer`'s wire line and the `answered` record written only after the write, `ErrUnknownRequest` / `ErrAlreadyAnswered`, the `PermissionRecordOnly` default, `PermissionAllow`'s one-shot allow on a `can_use_tool` without an options list, `control_cancel_request` dropping a pending request |
| `resume_test.go` | `--resume` argv + `Seq` from 1, baseline usage, no-baseline total stays unreported, `SessionNotFound`, prompts refused before anything starts |
| `failure_test.go` | the classification table, limit reset only when a limit refuses, auth/context-overflow frames, `is_error` outranks `subtype`, structured output on a completed outcome |
| `breakdown_test.go` | `get_context_usage` parsing (order, kinds, items, MCP split), shared in-flight reads, refusal carries claude's words, `(nil, nil)` after dispose, ctx honoured |
| `ops_test.go` | install plan/probe, login challenge (no stdin, URL only), auth status, upgrade, inventory's four explicit `unsupported` sections |
| `fixtures_gen_test.go` | `OAR_REGEN_FIXTURES` regeneration and `TestFixturesCarryNoCredentials` |
| `live_test.go` | `OAR_CLAUDE_LIVE=1` only: `TestLiveSteerEcho`, `TestLivePromptAndWait` |
| `internal/streamjson/*_test.go` | the family's own framing, transport and fold (claude's profile adds none of the vendor extensions) |

## 8. What stays unverified (or deliberately absent)

- **Official Claude account.** Every live fact here is a third-party
  Anthropic-compatible endpoint with `workbuddy/deepseek-v4.1-flash`; the model
  answered `unrecognized_model` on stderr while working. The vendor's own
  models may differ in timing (they should not differ in frames).
- **Live resume, abort, dispose, breakdown and ops.** Proven offline
  (recordings and the fake process); no live run of resume, abort, dispose or
  `get_context_usage` on this host, and the ops surface (install, login, auth
  status, update, inventory) has unit tests only. A live *follow-up* turn did
  happen (the CLI applying a queued steer after `result`, §4), but not a live
  resume.
- **Images** — proven offline only: the fake process receives the image blocks
  (`images_test.go`) and the shape is OAR's `streamJsonUserMessage`. **No live
  image run exists**: whether the endpoint behind the third-party gateway
  accepts image input at all is unverified, so a live image turn may still fail
  at the model rather than at oarlock.
- **Withdraw** — proven offline only (`session_test.go` against the fake):
  `accepted` for an entry still in the held queue, `not_queued` once the drain
  has taken it, `unsupported` for an empty id. The queue is adapter-owned, so
  what stays unverified is the drain's timing against a live turn, not the
  withdraw itself.
- **`cancel_queued`** — the negotiation is proven offline only: the committed
  2.1.292 recordings' root `system/init` carries `interrupt_cancel_queued_v1`
  (`interrupt.jsonl`, `steer-lifecycle.jsonl`) and the fake negotiates it on
  demand. **UNVERIFIED live**: no live run has aborted a turn with queued input,
  so whether claude honours `"cancel_queued": true` is unnamed; until a live run
  says so, `SupportsCancelQueued()` reports the init frame's declaration and
  nothing more. A build that does not advertise the capability cannot cancel
  queued input at all, and queued input survives the abort.
- **Permission answers** — the host path (`Answer`, the `answered` record,
  `ErrUnknownRequest` / `ErrAlreadyAnswered`) is proven offline against the
  fake; the automatic `PermissionAllow` shape is **unverified on claude**:
  headless runs with `--dangerously-skip-permissions` never send
  `can_use_tool`, so no `allow_always` or `allow_once` reply has ever been
  observed on this dialect, and the ladder degenerates to the one-shot allow.
- **Account usage** — claude reports it only over its control channel and
  oarlock does not ask; `AccountUsage` is absent.
- **Inventories** — four explicit `unsupported` sections (models, skills,
  commands, tools), never an empty `ok` list.
- **MCP servers / credentials, `serviceTier`, `userConfiguration: exclude`** —
  refused with a reason before anything starts (the `--mcp-config` /
  `--settings fastMode` hand-over is not implemented).
- **Permission auto-answers** — off by default: `PermissionRecordOnly` leaves a
  runtime `can_use_tool` as a `toApp` request with no reply, and only an
  explicit host `Answer` (or the opt-in `PermissionAllow`) writes one.
- **Structured-output schema** — `--json-schema` is passed and
  `structured_output` is read on a completed result, but not verified live.
- **`--effort`, `--name`, `--add-dir`, `--disallowed-tools`** — argv-mapped and
  unit-tested; not verified against a live claude.

### Two outcomes that are never fabricated

- **An `accepted` withdraw** — `Withdraw` splices under the same session lock the
  drain pops the head under. An entry the drain has already taken may already
  be on the wire, so it is `not_queued`: no timing heuristic, no "best effort"
  path, and no test asserts `accepted` for an entry the fake did not still
  hold.
- **An `answered` record** — `Answer` (and the `PermissionAllow` hook that goes
  through it) writes first and records `ResponseAnswered` only once that write
  succeeded. A failed write leaves the request pending and its `toApp` record
  alone; no record ever says oarlock answered something it did not send.
