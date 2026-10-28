# Pi runtime (pi 1.4.0)

**Status:** evidence report for issue #5 — a spike, not an adapter. Code: [`experiments/pi-rpc`](../../experiments/pi-rpc/),
raw wire: [`testdata/pi`](../../testdata/pi/). Consumers: #9, #10, #11, #15.

Tags: **已实测** = executed on this host with the argv in §1, traceable to a frame in `testdata/pi/*` or a named
probe proof (`launch`, `framing`, `id_correlation`, `settled`, `turn_end`, `abort_response`, `alive_after_settle`,
`get_state_after_abort`, `exit`, `group_gone`) printed by `OAR_PI_LIVE=1 go run ./experiments/pi-rpc/cmd/pi-rpc-probe`;
**已实测†** = a one-off driver run on this host, frames not committed; **未验证** = source-read only or not exercised.
Vendor paths are relative to `$HOME/.pi/agent/install/releases/1.4.0/node_modules/@earendil-works/pi-coding-agent/`.

## 1. Evidence baseline

| item | value | tag |
|---|---|---|
| pi / Node | `1.4.0` / `v22.23.1` | 已实测 |
| probe | `experiments/pi-rpc/cmd/pi-rpc-probe`, gated on `OAR_PI_LIVE=1` | 已实测 |
| fixtures | `normal-turn` (23 frames, 2 requests), `abort-turn` (26 frames, 4 requests) | 已实测 |

```
pi --mode rpc --offline --no-session --no-tools --no-mcp --no-skills --no-prompt-templates \
   --no-context-files --model any-endpoint/workbuddy/deepseek-v4.1-flash
```

This argv is frozen for every measurement in this document.

- **`--no-extensions` must not be added.** With it the `any-endpoint` provider disappears and the child exits `1`
  with `Error: Model "any-endpoint/workbuddy/deepseek-v4.1-flash" not found.` (已实测); `-e npm:pi-any-endpoint@0.1.3`
  does not restore it either (已实测). The provider is registered by a **host extension**, not by pi, so a host that
  owns the provider differently must not read that failure as "pi cannot resolve the model". Consequence: host-extension
  frames (`extension_ui_request`, 7 per fixture) are stream traffic and are **kept** (已实测).
- `--no-session` keeps state ephemeral: `get_state.data` carries `sessionId` but no `sessionFile`/`sessionName`
  (`undefined`, dropped by `JSON.stringify`; 0 occurrences in either fixture, 已实测) — no session file, no absolute
  home path. The system `message_start` in both fixtures carries an empty `content` string, and `turn_end.toolResults`
  is `[]` — nothing ran a tool (已实测).

## 2. Transport and framing

| property | rule | evidence |
|---|---|---|
| record, separator | one JSON object per line, **LF only**, split on `\n` and nothing else | `dist/modes/rpc/jsonl.js` `serializeJsonLine`; `attachJsonlLineReader` uses `buffer.indexOf("\n")` and avoids Node `readline`, which splits on extra Unicode separators |
| `\r` | one trailing `\r` stripped by the vendor reader | `jsonl.js` `emitLine` |
| U+2028 / U+2029 | legal inside strings, must not split a record | `jsonl.js` doc comment; `TestReadFramesIsLFOOnly` |
| stdout purity | `takeOverStdout()` rewrites `process.stdout.write` to stderr, so stdout carries only framed records; stderr is diagnostics, never parsed | `dist/core/output-guard.js`; `rpc-mode.js:24`; `client.go` keeps the bytes in an unbounded buffer, read after exit |
| buffer | 64 KiB start, 16 MiB cap | `client.go`: `Buffer(make([]byte, 64<<10), 16<<20)` |
| 5 MiB line / over cap | read intact / `bufio.ErrTooLong`, never truncation | `TestReadFramesReadsFiveMegabyteLine`, `TestReadFramesRejectsOverCapLine` |

已实测: both fixtures are LF-only (no `\r` byte, trailing LF) and `bufio.Scanner`+`ScanLines` reproduces every frame
byte-for-byte. The 5 MiB case is proved by unit test, **not** by a live frame — no real turn emitted such a line.

## 3. Command set

| command | status | evidence |
|---|---|---|
| `get_state` | 已实测 | request `probe-1`; `data` = model, thinkingLevel, isStreaming, isCompacting, steeringMode, followUpMode, sessionId, autoCompactionEnabled, messageCount, pendingMessageCount |
| `prompt` | 已实测 | request `probe-2`; `data.disposition = "started"`; enum `started \| queued \| handled` (`PromptDisposition`, `dist/core/agent-session.d.ts:174`) |
| `abort` | 已实测 | request `probe-3`; `{"success":true}` with **no** `data` key (`rpc-types.d.ts:188` declares none); a second idle abort also returned `success:true`, no `data` (已实测†) |
| `get_session_stats` | 已实测 (earlier run, 已实测†) | it appears as `response(get_session_stats)` in the live normal order the contract records. The **committed probe does not call it**: the requests hold only `get_state`/`prompt`/`abort`, and no such frame exists in a fixture (0 occurrences) |

Unexercised, all 未验证 and all declared in `rpc-types.d.ts`: `steer`, `follow_up`, `clear_queue`, `set_model`,
`cycle_model`, `get_available_models`, `set_thinking_level`, `compact`, `bash`, `new_session`, `switch_session`, `fork`,
`clone`, `get_entries`, `get_tree`, `get_messages`, `get_commands`, `set_session_name`, `export_html`, `abort_retry`,
`set_auto_compaction`, `set_auto_retry`, `set_steering_mode`, `set_follow_up_mode` — plus five the spike's own list omits
(`cycle_thinking_level`, `get_available_thinking_levels`, `abort_bash`, `get_fork_messages`, `get_last_assistant_text`,
also 未验证). The bundle declares 33 command types; the spike exercised 4.

**`dispose` is not a command.** The vendor's `switch (command.type)` has no such case; unknown types return
`{success:false, error:"Unknown command: …"}` (`rpc-mode.js`, `default` branch). Disposal is process-level: close stdin
(EOF), then SIGTERM, then SIGKILL of the **group** (已实测: no `dispose` frame exists, 0 occurrences).

## 4. Event types

Normal, 23 frames in order (`testdata/pi/normal-turn.frames.jsonl`):

```
ext_ui_request setStatus, notify, setWidget      ×3, before the handshake
response(get_state) probe-1; response(prompt) probe-2, disposition=started
agent_start; turn_start
message_start/message_end system; … user; message_start assistant
message_update text_start, text_delta, text_end  ×3
message_end assistant; ext_ui_request setStatus
turn_end            stop / rawStopReason=completed / toolResults=[]
ext_ui_request setStatus; agent_end willRetry=false; ext_ui_request notify, setStatus ×2
agent_settled       aborted=false
```

Abort, 26 frames, deviates at: `message_update ×4` (`thinking_start`, `thinking_delta` ×3); `message_end`/`turn_end`
say `stopReason:"aborted"` with `errorMessage` present and `rawStopReason` absent; `agent_settled{aborted:true}`; then
`response(abort)` (no `data`) and `response(get_state)` (`isStreaming:false`, `messageCount:3`).

| field | observed | tag / source |
|---|---|---|
| `turn_end.message.role`, `toolResults` | `assistant`, `[]` (no tools available) | 已实测 |
| `turn_end.message.stopReason` | `stop` (normal), `aborted` (abort); enum `pending \| stop \| length \| toolUse \| error \| aborted \| deferred` | 已实测; enum in `pi-ai/dist/types.d.ts:307` |
| `turn_end.message.rawStopReason` | `"completed"` on `stop`; key **absent** on `aborted` | 已实测 |
| `agent_end.willRetry` / `agent_settled.aborted` | `false` / `false` (normal), `false` / `true` (abort) | 已实测; `willRetry` added by `agent-session.js:753` |
| `message_update.assistantMessageEvent.type` | `text_start \| text_delta \| text_end` (normal), `thinking_start \| thinking_delta` (abort); delta-only, **no `partial`** | 已实测 (0 `partial` in either fixture); stripped by `toJsonAssistantMessageEvent`, `dist/modes/json-event.js` |
| `message_start`/`message_end` roles | `system`, `user`, `assistant` | 已实测 |
| `extension_ui_request` methods | `setStatus` ×4, `notify` ×2, `setWidget` ×1 per fixture | 已实测 |
| dialog `select \| confirm \| input \| editor` | need a matching `extension_ui_response` with the same `id` (the vendor parks the call in `pendingExtensionRequests`) | 未验证; source `createDialogPromise`/`editor` in `rpc-mode.js` |
| fire-and-forget methods | `notify`, `setStatus`, `setWidget`, `setTitle`, `set_editor_text` emit with no pending entry | the three seen are 已实测; `setTitle`/`set_editor_text` 未验证 |
| `extension_error` | `{type, extensionPath, event, error}` from the extension `onError` hook | 已实测† at EOF once; **absent** from both fixtures (0 occurrences) |

Four completion facts, all measured:

| # | fact | evidence |
|---|---|---|
| 1 | prompt response arrives **before** `agent_start` | normal 5 then 6 (all indices here are 1-based, like §7); emitted from the prompt preflight callback (`preflightResult`), so structural |
| 2 | `turn_end` precedes `agent_settled` | normal 18 < 23; abort 18 < 23 (1-based) |
| 3 | abort response arrives **after** `agent_settled` | abort 24 > 23 (same-millisecond detail seen live, 已实测†; frames carry no timestamps) |
| 4 | frames can follow `agent_settled` | abort 24–25; the normal fixture stops at `agent_settled` because the probe snapshots before dispose (an earlier run saw two frames after EOF, 已实测†) |

## 5. Timeouts

- Adopted bound: **30 s per command** (`client.go` `CommandTimeout`), from OAR's `control.ts` behaviour (quoted in
  `issues/0009-pi-transport.md`). The vendor has **no** per-command timeout: `handleInputLine` awaits `handleCommand`
  with no timer; its only timers are extension dialog `opts.timeout` (未验证 as wire behaviour — source-read).
- On expiry the client `SIGKILL`s the **group** and the in-flight request fails; nothing stays pending (`client.go`
  `Request`). **Policy decided, firing 未验证**: no test drives a silent child to 30 s (30 s of wall clock in `go test`),
  so that path is code-reviewed only.

## 6. Exit semantics

| trigger | exit | stdout | descendants | tag |
|---|---|---|---|---|
| stdin EOF, idle | **0**, ~63 ms | tail flushed (`shutdown()` awaits `flushRawStdout()` when no signal is passed) | survivors remain, §8 | 已实测 (proof `exit`: `code=0 signal=0`) |
| stdin EOF **mid-turn** | **0**, ~63 ms | tail flushed | survivors remain | 已实测†, **no `turn_end`, no `agent_settled`** |
| SIGTERM to the leader only | **143** — the vendor calls `process.exit(143)`: a real exit code, not death by signal | flush **skipped** (`if (signal !== "SIGTERM") await flushRawStdout()`), so a truncated last line is expected | **4 pids still in the group** ~600 ms later | 已实测†; flush-skip 未验证 but source-cited (`rpc-mode.js` `shutdown`) |
| SIGKILL to the group | killed by signal, no exit code | in-flight stdout may end mid-line | killed | 未验证 (nothing traps SIGKILL; no handler) |

**Exit 0 is not a turn boundary.** EOF during a turn yields code 0 with no `turn_end` and no `agent_settled`
(已实测†), so a mid-turn dispose produces no turn boundary and the adapter must **never synthesize one** — it records
the exit, not a fabricated end. There is also **no SIGINT handler** (the vendor installs `SIGTERM`, plus `SIGHUP` off
win32, only), so an interrupt must be the `abort` command or a signal the host sends itself (未验证).

## 7. Abort, grace and takeover

Measured timeline (abort fixture; the abort went out after `agent_start` + the first `message_update`):

| index | frame | fact |
|---|---|---|
| 18 | `turn_end` | `stopReason:"aborted"`, `rawStopReason` absent, `toolResults:[]` |
| 20 | `agent_end` | `willRetry:false` |
| 23 | `agent_settled` | `aborted:true` |
| 24 | `response(abort)` | `success:true`, no `data`; lands **after** `agent_settled` |
| 25 | `response(get_state)` | `isStreaming:false`, `messageCount:3` — alive, and answered a later command |
| — | second abort while idle | `success:true`, no `data` (已实测†) |

Policy:

a. A turn ends at `turn_end` / `agent_settled`, **never at the abort ack** — the ack answers a command; here it arrives after settlement, and a mid-turn EOF has no boundary at all.
b. The ack may never arrive if the process dies, so **every pending command must fail on exit** (`client.go`: `ErrExited`; `Request` selects on `c.exited`).
c. Grace ladder: accept the ack → `SIGTERM` the group → `SIGKILL` → prove the group empty with `kill(-pgid, 0) == ESRCH` (POSIX; `pollGone`). The proof is the ESRCH, not the exit code.
d. **Always `Setpgid`**: without it the child inherits the parent's group (measured `pid 653717 / pgid 653710`, 已实测†), so `-pgid` would signal the Go process itself; `signalGroup` refuses when `Pgid != Pid`.

## 8. Dispose cleanup

Measured sequence (probe proof `group_gone`): close stdin → EOF; the leader exits **0** in ~63 ms with the tail
flushed; **the group is still non-empty** — pi-lens LSP helpers outlive the leader by ~600 ms (4 pids, 已实测†);
`SIGTERM` the group; `SIGKILL` if it still lives; `kill(-pgid, 0)` → `ESRCH` → `group_gone`.

Exit code 0 is **not** evidence of a clean tree, and neither is a quiet stderr; the only proof is
`kill(-pgid, 0) == ESRCH`, which is why the probe fails without `group_gone`. The vendor does call
`killTrackedDetachedChildren()` on its signal path (`rpc-mode.js`) but that did not reach the pi-lens children here
(4 survivors, 已实测†) — trust the group sweep, not the vendor's cleanup.

## 9. Unknown events

Keep the raw JSON, count the `type`, never coerce, rename or drop: `ReadFrames` stores `json.RawMessage` verbatim and
`dispatch` forwards every frame even when it also resolves a request. Known set: `response`, `agent_start`, `turn_start`,
`message_start`, `message_update`, `message_end`, `turn_end`, `agent_end`, `agent_settled`, `extension_ui_request`,
`extension_error`; both fixtures tally **zero** unknown types (已实测). A non-zero tally is a **test signal, not a log
line** — `extension_ui_request` frames are exactly the noise a host extension injects, so new types are how a vendor
upgrade or new plugin announces itself, and a growing tally must fail a test.

## 10. What this spike did not verify

All 未验证: tool-using turns (`--no-tools` was on), images, steering (`steer`/`follow_up`), queueing (the
`queued`/`handled` dispositions), resume (`switch_session`/`fork`/`clone`), Windows (a Job Object instead of `Setpgid`),
the 30 s timeout firing, `get_commands`/`get_available_models`, context and usage derivation (#11), the version gate and
refused options (#15), the `extension_ui_response` round trip, the full `extension_error` shape,
`agent_end.willRetry:true` (auto-retry), compaction, and `messageCount` accounting after a real tool turn.

## 11. 结论

**可做.** Pi 1.4.0's RPC mode is a sufficient substrate for #9's transport: one real child, framed, commands
correlated, a full turn streamed, aborted, recovered with `get_state`, and its process group proved gone.

| issue #5 criterion | probe proof | fixture | replay test |
|---|---|---|---|
| launch + newline framing | `launch`, `framing` (+ `TestReadFramesIsLFOOnly`, `TestReadFramesReadsFiveMegabyteLine`, `TestReadFramesRejectsOverCapLine`) | both | `TestReplayFramesContainOnlyKnownTypesAndTallyUnknown` |
| `prompt` → `response` by `id` | `id_correlation` | both | `TestReplayNormalTurnCorrelatesEveryResponseToARequestID` |
| stream through `agent_settled`, `turn_end.stopReason` | `settled`, `turn_end` | `normal-turn` | `TestReplayNormalTurnEndsWithAgentSettledAfterTurnEndStop`, `TestReplayNormalTurnNarratesTheNativeEventOrder` |
| abort, recovery, dispose cleanup | `abort_response`, `alive_after_settle`, `get_state_after_abort`, `exit`, `group_gone` | `abort-turn` | `TestReplayAbortTurnReportsStopReasonAbortedAndSettledAborted`, `TestReplayAbortTurnAnswersAbortAfterAgentSettled`, `TestReplayAbortTurnRecoversWithGetStateAfterSettlement` |

Conditions #9/#10 must honour, each traced to the evidence above:

1. **Frozen argv, provider caveat** — `--no-extensions` breaks the provider, so supply it the same way or accept a different argv (§1).
2. **Exact version pin `1.4.0`** — every measurement is one version; claim no other (§1). The gate is #15.
3. **LF-only framing, 5 MiB-tolerant buffer** — the vendor reader is LF-only, and a 5 MB line must survive (§2).
4. **30 s command timeout that kills the group** — inherited policy, firing 未验证 (§5); the one condition never observed in action.
5. **`agent_settled` is the only turn end** — a mid-turn EOF has none (§6).
6. **The abort ack arrives after settle and is never a gate** — the turn ends at `turn_end`/`agent_settled`; the ack may never arrive (§7).
7. **No `dispose` command** — EOF, then SIGTERM, then SIGKILL of the group (§3, §8).
8. **`Setpgid` always** — otherwise `-pgid` kills the Go process (§7d).
9. **Unknown-type tally** — zero today, but host extensions inject frames, so an untallied new type must fail rather than pass silently (§9).

No condition is invented. Eight rest on a fixture frame or a measured exit; number 4 is policy whose firing has never
been observed, and number 9 is precautionary — the fixtures tally zero unknown types, so it guards a future change, not
a measured present one. Both are labelled as such instead of being presented as measurements.

## 12. Resume (issue #12)

**Status:** evidence + decisions for `Options.Resume`. Code: [`adapters/pi/resume.go`](../../adapters/pi/resume.go) (resolver),
[`adapters/pi/open.go`](../../adapters/pi/open.go) (the minimal `Open`). Tags as above; every 已实测 row below is a
2026-10-28 run of `pi 1.4.0` on this host with the §1 argv minus `--no-session`, in a throw-away cwd (drivers and raw frames were
not committed: **已实测†**). The committed live proof is `OAR_PI_LIVE=1 go test ./adapters/pi -run TestResumeLive`
(prompt → dispose → resume by id → follow-up → the answer contained the earlier codeword; a bogus id gave `SessionNotFound`;
it passed on this host on 2026-10-28, 12 s, pi 1.4.0, `any-endpoint/workbuddy/deepseek-v4.1-flash`, and is **off by default**:
without `OAR_PI_LIVE=1`, a login and the model it is skipped, i.e. 未验证 for whoever runs the suite).

### 12.1 What Pi does (measured)

| # | question | observation | tag |
|---|---|---|---|
| E1 | `--session <abs file>`, same cwd, then `prompt` "which codeword?" | `get_state.sessionId`/`sessionFile` equal the file's, `messageCount` restored (3), the answer was the codeword taught before the exit | 已实测† |
| E2 | `--session <file recorded under cwd A>` launched in cwd B | accepted without a word; a `bash` command printed **A**. Pi runs where the header says, not where it was started | 已实测† |
| E3 | `--session <path that does not exist>` | **no error**: Pi opens a brand-new session (new id) bound to that path | 已实测† |
| E4 | `--session <bare id>` of a session recorded under another cwd | stderr `Session found in different project: …`, then `Fork this session into current directory? [y/N]` on stdin; EOF → `Aborted.`, exit 0, **zero frames** (`main.js` `resolveSessionPath`: a value with `/`, `\` or `.jsonl` is a path, anything else is an id) | 已实测† |
| E5 | `--session-id <unknown>` | stderr `Warning: No project session found … creating a new session with that id.`; `get_state` reports that id; **no error** | 已实测† |
| E6 | EOF in the middle of a turn, then `--session <file>` and a prompt | the file holds the user message and nothing else; the resumed Pi is idle (`prompt` → `disposition:"started"`, `agent_start` … `agent_settled`); the model saw the dangling user message as history | 已实测† |
| E7 | a session file exists before any message? | no: written when the session first holds a user or assistant message (`session-manager.js` `_hasConversation`, `_persist`) | source-read; the E1 fixture shows the file appearing with the turn |
| E8 | a lock on the session file | none in `session-manager.js`; two writers are not refused | source-read (grep for lock/flock) — **未验证** live |
| E9 | the session directory | `<agentDir>/sessions/--<cwd, leading sep dropped, / \ : → ->--`; `agentDir` = `PI_CODING_AGENT_DIR` (non-empty) else `~/.pi/agent` | 已实测 (the directories on disk) + `config.js getAgentDir`, `session-manager.js getDefaultSessionDirPath` |

`switch_session` (RPC command, §3) is still 未验证: resume here is a launch argument, not an in-process switch.

### 12.2 Decisions

1. **Always `--session <absolute file>`**, never an id (E4) and never `--session-id` (E5). The file is resolved by oarlock before
   anything starts: `ResolveResume(resume, cwd, agentDir)`.
2. **Accepted input:** a Pi session id (exact match on the header id of the `*.jsonl` files in the directory of **this** cwd; no
   prefix match, no other project) **or** a session file (a value with a separator or ending in `.jsonl`; relative → against `Options.Cwd`).
3. **`*oarlock.SessionNotFound{SessionID, Cause}`** only on positive evidence: the path does not exist (or a parent is not a
   directory), the session directory does not exist, or a **complete** scan of the directory read every header and none matched.
   `Cause` is a `*pi.LookupError` that names the cwd and directory searched and unwraps to the native `*PathError`, so
   `errors.Is(err, fs.ErrNotExist)` holds when that is the reason. A scan that could not finish (permission, I/O, a `.jsonl` whose
   header is not a session, a directory where a file should be, two files with one id) is a **plain error**: absence was not shown.
   There is no network call in resume at all, so a network error cannot be read as not-found; model/provider failures surface
   later as failed turns (§4).
4. **Cwd:** the session file's recorded cwd must be `Options.Cwd` (same path, or the same directory through a symlink — Pi stores the
   physical `process.cwd()`). Otherwise `*oarlock.UnsupportedOption{Option: "cwd"}` before any process starts (E2 shows Pi would
   silently run in the recorded directory). By id from another cwd is `SessionNotFound` whose cause says which directory was searched.
5. **Identity check after launch:** `get_state.sessionId` must equal the resolved id (E3). A different id is a plain error and the
   child is closed; it is never `SessionNotFound`.
6. **A resumed session is a new stream instance** (record-stream.md "Cursor and stream instances"): a new kernel, `Seq` starts at **1**
   (the issue text says 0; the frozen spec and `Record` say the first record is 1, and a zero `AfterSeq` is "from the start"), the
   same native session id. Nothing is read back from the file and nothing is replayed. Pi's own startup frames (the three
   `extension_ui_request`s, the `get_state` response) are ordinary frames; the system `message_*` frames Pi emits on a prompt are frames
   with no events. Resume writes no `turn_active`, `turn_ended` or `user_message` (invariant 1).
7. **Cursor:** a cursor of the previous instance carries the same `SessionID` (the native id did not change), so the new kernel
   **cannot** tell it apart and would apply it to the wrong records. This is the spec's rule, not a bug to paper over: the host stores the
   instance identity (e.g. its own counter) beside the cursor. A cursor naming another session id is refused.
8. **Dispose interaction:** E6 shows that a previous instance ended by `dispose` (stdin EOF) mid-turn leaves nothing running in Pi.
   The new stream says idle (no `turn_active` without a following `turn_ended` exists in it), so the first `Prompt` is accepted and
   starts a normal turn; `Dispose` stays idempotent (issue #10). Held queue input does not survive (`Queue.Durable=false`).
9. **Options not implemented are refused**, not dropped: `Open` honours `Cwd`, `Env`, `Model`, `Effort` and `Resume` and returns
   `*oarlock.UnsupportedOption` for the rest until issue #15 supplies the table. `Model`/`Effort` become `--model`/`--thinking` and are
   not read back (#13).

### 12.3 Different cwd — conclusion

| request | result | evidence |
|---|---|---|
| same cwd, by id or by file | **supported** | E1 live, and the Go live test |
| other cwd, by id | **refused** — `SessionNotFound` (scope: that cwd + this agent dir; the cause names the directory) | resolver + live smoke; OAR's `resume-cwd.md` reaches the same result for Pi |
| other cwd, by file of the original cwd | **refused** — `UnsupportedOption{cwd}` | E2 (Pi would run in the old cwd) |
| symlinked cwd | **supported** when it is the same directory as the header's | unit test (`os.SameFile`); macOS `/var`→`/private/var` 未验证 on a Mac |
| cwd deleted since | **未验证** beyond "the launch itself fails": Pi's own `MissingSessionCwdError` path is not exercised |
| `--session-dir` pointing elsewhere / `sessionDir` in Pi settings | **未验证**: oarlock passes `--session-dir` for the cwd it computed and does not read Pi's `settings.json` `sessionDir` |

### 12.4 Not done, on purpose

- No history read-back and no replay (README non-goals; OAR decision "Session history readback").
- No "opened elsewhere" / archived verdict: Pi has no lock or archive notion to give positive evidence (E8), so neither is ever emitted.
- No `fork`/`clone`/`switch_session` (transcript branches, not resume).
- `Options.Name`, `SystemPrompt` and the other options stay with #15.
