# Voyage logs: record, replay and golden fixtures

A voyage log is a file that holds the record stream of one session, so the
session can be folded again later without the runtime. Package:
[`internal/voyage`](../internal/voyage). Specs it builds on:
[the record stream](spec/record-stream.md) and [redaction](spec/redaction.md).
Decision: [ADR-0006](adr/0006-golden-traces-over-porting.md).

## What a voyage log is

One JSON object per LF-terminated line, discriminated by `kind`. The framing is
the one OAR uses for `oar-voyage/3`.

```
{"kind":"header","format":"oar-voyage/3","dialect":"oarlock","runtime":"pi","cwd":".","sessionId":"...","startedAt":1700000000000,"recorder":"..."}
{"kind":"record","record":{...one oarlock.Record as encoding/json writes it...}}
{"kind":"end","at":1700000009000,"reason":"settled"}
```

* The header comes first and exactly once. `model`, `effort` and `serviceTier`
  are optional.
* Zero or more `record` lines follow. `Seq` is dense and starts at 1, the
  stream-instance rule of the record stream spec. The writer refuses a
  non-dense `Seq`; the reader reports a gap as `ErrCorrupt` (wrapping `ErrSeq`).
* The `end` line comes last. A log without it is truncated.

The `dialect` field is required and must be `oarlock`. OAR's own TypeScript
logs share the framing but carry a different record body, so they are **not
readable**: `Read` returns `ErrFormat` for them. There is no TypeScript-to-Go
converter; this is a documented gap, not a feature.

## Recording

Recording is a library call, not a command. Create a `Writer` with the same
redactor the session's kernel uses, then copy each retained kernel record into
it. The kernel has already redacted the record before it was retained or
published; the `Writer` redacts again before writing.

```go
red := redact.New(knownSecrets) // the session's known credential values
k := kernel.New(sessionID, red.Record)

w, err := voyage.Create("run.jsonl", voyage.Header{
	Runtime: "pi", Cwd: cwd, SessionID: sessionID,
	StartedAt: time.Now().UnixMilli(), Recorder: "myapp/1.0",
}, red)
if err != nil {
	return err // Create never overwrites an existing file
}

// Replays the retained prefix, then follows live records with no gap.
cancel, err := k.ObserveFrom(oarlock.Cursor{}, func(rec oarlock.Record) {
	_ = w.Append(rec) // sticky: the first failure stays in w.Err()
})
if err != nil {
	return err
}
// ... run the session ...
cancel()
return w.End("settled") // after a write error: that error, and no end line
```

Every retained record goes through `Append` in `Seq` order. `Append` redacts,
marshals, writes one line and fsyncs it, so a crash loses at most the line being
written. `End(reason)` writes the end line and closes the file; `Close` is
`End("closed")`.

## Truncation semantics

A log with no `end` line is truncated. This covers an empty file, a header
only, records without `end`, and a crash tail.

* A last line without its terminating LF is never committed: it is ignored and
  the log counts as truncated.
* `Read` and `Decode` return the salvaged prefix together with `ErrTruncated`,
  so a post-mortem can still look at what was recorded.
* `Replay` is strict: it returns no records and an error unless the log has a
  header, dense records and an `end` line.
* `ErrFormat` (first line not a header, wrong format, wrong or missing dialect)
  and `ErrCorrupt` (bad JSON, unknown kind, second header, data after `end`, bad
  record, line over `MaxLineBytes`, non-dense `Seq`) return no log.
* The reader is written never to panic on any input. The contract asks
  `voyage_test.go` to cut a valid log at every byte offset and to guard the
  imports; check that file for what is actually covered.

Replay is plain file I/O: the package uses no network and starts no process.

## Redaction timing

1. The kernel redaction hook runs before a record is retained or published.
2. `Writer.Append` runs the redactor over the marshalled record before a single
   byte reaches the file descriptor. If marshalling fails, nothing is written.
3. The header and the end reason are redacted the same way.
4. Redacting already-redacted kernel output changes nothing (idempotent).
5. A `nil` redactor still applies the structural rules (env, headers,
   Authorization, URL passwords).

Committed fixtures must never contain a real credential. `ScanCredentials`
checks the raw bytes of every fixture in CI, in `CheckFixture`, in
`WriteIndex` and in `TestCommittedFixturesPassTheCredentialScanAndSizeCap`.
It flags these shapes:

* OpenAI and Anthropic keys (`sk-...`, `sk-ant-...`)
* GitHub tokens (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_`)
* AWS access key ids (`AKIA...`), Google API keys (`AIza...`), Slack tokens
  (`xox[baprs]-...`)
* PEM private key headers
* `Bearer <token>` (but not `Bearer [redacted]`) and JWTs
* URLs with a password (`scheme://user:pw@`) unless the password is `[redacted]`
* `[redacted]` glued to credential characters, which means a partial
  redaction; the bare `"[redacted]"` is fine
* absolute home paths (`/root`, `/home/<user>`, `/Users/<user>`); fixtures use
  the normalised `$HOME/...` form

Bare words such as `password`, `secret` or `apiKey` are not flagged, because a
model transcript may legitimately say them. A finding reports the pattern, the
1-based line and a clipped excerpt, never the full value.

## Fixture layout and naming

```
testdata/voyage/<runtime>-<version>-<scenario>.jsonl
testdata/voyage/index.json
```

The directory is flat. Examples: `pi-1.4.0-normal-turn.jsonl`,
`pi-1.4.0-abort-turn.jsonl`, `pi-1.4.0-tool-turn.jsonl`,
`pi-1.4.0-multi-tool-turn.jsonl`, `pi-1.4.0-live-abort-turn.jsonl`,
`pi-1.4.0-compaction-turn.jsonl`, `pi-1.4.0-retry-turn.jsonl`.

Two kinds of fixture share the directory, and the index does not tell them
apart: the **stand-in** fixtures built from `testdata/pi` wire frames (`pi-1.4.0-abort-turn.jsonl`,
`pi-1.4.0-normal-turn.jsonl`, see below) and the **live recordings** of a real
Pi 1.4.0 session:

| fixture | issue | what it records |
| --- | --- | --- |
| `pi-1.4.0-tool-turn.jsonl` | [#71](../issues/0071-voyage-tool-compaction-live.md) | one bash call and the answer that follows it |
| `pi-1.4.0-multi-tool-turn.jsonl` | [#76](../issues/0076-voyage-breadth.md) | two bash calls in one turn |
| `pi-1.4.0-live-abort-turn.jsonl` | [#76](../issues/0076-voyage-breadth.md) | a turn aborted mid-call |
| `pi-1.4.0-compaction-turn.jsonl` | [#82](../issues/0082-pi-compaction-live.md) | a manual compaction via Pi's RPC `compact` command |
| `pi-1.4.0-retry-turn.jsonl` | [#88](../issues/0088-pi-retry-live.md) | two provider failures retried before the answer (`auto_retry_start`) |
| `codex-0.165.0-subagent-turn.jsonl` | [#83](../issues/0083-voyage-subagent-live.md) | a root turn that spawns a child thread (sub-agent session) |

A live fixture is committed bytes, so its pinned numbers are a regression check
on the fold, not a claim that a fresh run on another machine would report the
same tokens. The `live-abort-turn` name keeps a live recording from being
confused with the *stand-in* `pi-1.4.0-abort-turn.jsonl`, which is built from
`testdata/pi` and not recorded from a Session.

* `index.json` (`oar-voyage-index/1`) lists every fixture sorted by name with
  its runtime, session id, end reason, record counts per kind, byte size,
  SHA-256 and a `complete` flag. It is generated, never edited by hand.
* Size caps: `MaxFixtureBytes` (256 KiB) per file and `MaxTotalBytes` (2 MiB)
  for the directory. `CheckFixture` and `BuildIndex` enforce them.
* `.gitattributes` marks `testdata/voyage/*` as `text eol=lf`, so a Windows
  checkout keeps the bytes and the SHA-256 identical.

## Recording a live fixture

The live fixtures are produced by opt-in tests, never hand-written:

```sh
OAR_PI_LIVE=1 OAR_PI_RECORD=1 go test ./internal/voyage -run TestLiveToolTurnRecordsAndReplays -v
OAR_PI_LIVE=1 OAR_PI_RECORD=1 go test ./internal/voyage -run TestLiveMultiToolTurnRecordsAndReplays -v
OAR_PI_LIVE=1 OAR_PI_RECORD=1 go test ./internal/voyage -run TestLiveAbortTurnRecordsAndReplays -v
OAR_PI_LIVE=1 OAR_PI_RECORD=1 go test ./internal/voyage -run TestLiveCompactionRecordsOrReportsUnverified -v
go generate ./internal/voyage   # rewrites index.json; the stand-in fixtures are unchanged
```

Each test opens a real Pi session in a temp cwd, prompts, waits for the turn to
finish (and, after a normal turn, for the post-settlement stats frame),
disposes, and then, in the same run:

1. asserts the evidence in the live records — which tool call started, in what
   order, which one ended and with what exit code, what text the end report and
   the conversation carry, and how the turn ended — so a recording that lost
   the fact is a failure, not a fixture;
2. **normalises** the recording, because a live log carries the wall clock, the
   runtime's session id and the recording host's paths:
   `receivedAt` becomes `ltBase+Seq` (ms), the session id becomes
   `pi-live-<fixture>`, the home directory becomes `$HOME` and the cwd `.`;
3. proves the normalisation changed no fold answer (`live == fold(normalised)`);
4. writes the log through `voyage.Create`/`Append` (so the fixture is redacted
   and fsynced exactly like a production recording);
5. replays it with `PATH` pointing at a tripwire `pi` script that would record
   itself if it ran — the offline proof that replay starts no process — and
   asserts `Status`, `Usage`, `Context` and `Conversation` equal the live
   answers from step 2.

`OAR_PI_RECORD` is a recording switch, not a build step: without it the live
tests keep their log in a temp dir, and without `OAR_PI_LIVE=1` they skip.
`TestNoNetworkOrProcessImports` keeps `net`, `os/exec` and friends out of the
package, so "no network, no process" is enforced twice.

### The tool turn (#71)

`TestCommittedToolTurnPin` and `TestCommittedToolTurnCallAndTextOrder` in
`internal/voyage/tool_turn_test.go` replay the committed bytes, pin the live
session's numbers (input 21583 / output 75 / reasoning 23 / cacheRead 13312,
context 10843 of 1000000 = 1.0843 %), pin the call id, its input and output
text, and assert the order started → ended → answer in both the event stream and
the folded conversation.

### Two calls in one turn (#76)

The offline half is `TestCommittedMultiToolTurnPin` in
`internal/voyage/breadth_test.go`, which shares the `lbMultiTurn` helper with
the live test. The prompt asks for
the two commands one at a time, so the recording is a sequential pair:
`bash echo oarlock-tool-one` (started seq 105, ended 108, exit 0) and
`bash echo oarlock-tool-two` (started 130, ended 133, exit 0), with the single
`turn_ended completed` at seq 158. What is pinned:

* exactly two `tool_call_started` and two `tool_call_ended`, matched by call id,
  with no overlap between them (`ended[n] <= started[n+1]`);
* both end reports carry `result: ok` and a **reported** exit code 0 — the
  distinction from "no exit code reported" is part of the fixture;
* each end report's text is that command's output (`oarlock-tool-one\n`,
  `oarlock-tool-two\n`), and the answer's deltas are the two outputs in that
  order and nothing else;
* one `turn_active` and one `turn_ended`: a second turn end would mean the
  second call opened a new turn, which the recording proves it does not;
* the folded conversation is user → tool(call 1) → tool(call 2) → assistant, in
  that order, with each tool message carrying its own call id, exit code and
  output;
* usage input 32668 / output 172 / reasoning 72 / cacheRead 24192 and context
  10990 of 1000000 = 1.099 %, and `fold.Of` equals the incremental `Feed`.

### An aborted turn (#76)

`TestCommittedAbortTurnPin` pins the stream of a turn aborted while its tool was
running. The prompt asks for `sleep 30`; the test waits for a running turn *and*
for the bash call, sends `Session.Abort`, and only then settles and disposes.
The live session answered `ControlAccepted`. What is pinned:

* exactly one `turn_active` and one `turn_ended`, and the outcome is
  `aborted` with no `failure` and no `reason` of its own — the runtime's report,
  not a synthesis;
* the `abort` **request record** is in the log and precedes `turn_ended`, so the
  fixture shows who stopped the turn;
* the interrupted call started (seq 63) and ended (seq 67) before the end, with
  `result: failed`, the text `Command aborted`, and **no exit code at all**
  (`exitCodeReported` false) — Pi reported a signal, not a number, and the
  fixture keeps that difference;
* the conversation is user → tool and has no assistant text: this session
  planned the call in reasoning and never spoke, so any answer would be
  invention;
* usage input 10746 / output 84 / reasoning 28 / cacheRead 2688 and context
  10834 of 1000000 = 1.0834 %.

### A child thread: the codex sub-agent turn (#83)

The first committed fixture with a **derived session**, recorded from a real
codex 0.165.0 against a third-party OpenAI Responses endpoint (`spawn_agent` is
part of this build's own multi-agent tool surface; no vendor account is
involved):

```sh
OARLOCK_LIVE_CODEX=1 OAR_CODEX_RECORD=1 \
CODEX_HOME=<dir naming the provider> OPENAI_API_KEY=<token> \
  go test ./internal/voyage -run TestLiveSubagentRecordsAndReplays -v -count=1
go generate ./internal/voyage   # index.json
```

`TestLiveSubagentRecordsAndReplays` opens a codex session, prompts it to
delegate (`spawn_agent`, asking the child for one word, then `done`), waits for
**both** turns, normalises (clock, the two thread ids, home, cwd), proves the
fold answers did not move, writes the log and replays it with a tripwire
`codex` on `PATH`. `TestCommittedCodexSubagentTurn` then replays the committed
bytes offline and pins:

* 69 records: 50 on the root thread and 19 on the child thread; every record's
  `SessionID` is one of the two, so no record is unattributed.
* one `session_linked` at seq 19 — `codex-live-codex-0.165.0-subagent-turn` →
  `…-child` — from the `spawnAgent` item. The same codex build restates the
  edge in its `wait` (seq 43/44) and `closeAgent` (seq 51/53) items; the graph
  holds it once, which is what the single event proves.
* every child record carries the **child thread id** and an empty `AgentPath`:
  a nested codex child is a session root, not a path inside the root session
  (unlike claude's `Task`, which is in-session `AgentPath` attribution).
* the root's `turn/completed` is seq 66 and the child's is seq 40 — one each;
  the folded root status is `exited` (the recorder disposed the session) with
  `lastTurnOutcome completed`, so the child's turn end neither ended nor
  replaced the root's.
* usage, folded once per session: root `Total` 37822/212 (cacheRead 36992) and
  child 6977/3 (cacheRead 6656), so `Usage.WithChildren` is 44799/215 — the
  child's total added **exactly once** — while `Total`, `ByAgent` (nil) and
  `Unattributed` (nil) stay the root's own.

The delegation is a `collabAgentToolCall` item on this build, so the projection
states it as the graph edge and **not** as `task_started`/`task_ended`; codex
states those for its `subAgentActivity` items instead (`adapters/codex/testdata/
codex-subagent.raw.jsonl`, `TestReplaySubagentThroughTheSession`). The issue's
four criteria are the edge, the child's own id, `WithChildren` once and the
root's turn status — none of them needs a task event, and the fixture asserts
exactly those.

**Claude `Task` on the same host (probe, not committed).** claude 2.1.312
against the Anthropic-compatible endpoint does spawn a sub-agent when the prompt
requires it: `Agent` tool_use, `system/task_started` (`local_agent`),
`task_updated`, `task_notification completed`, and the child's own prompt echoed
with `parent_tool_use_id` set. But in this build's `--print` stream that echo is
the **only** frame carrying `parent_tool_use_id` (the child's assistant frames
are not forwarded), and claude attributes sub-agents **in-session** by
`AgentPath` with no `session_linked` edge, so it cannot produce the graph or
`WithChildren` evidence this fixture pins. Claude's `AgentPath` shape stays
covered by the adapter's real recording `adapters/claude/testdata/
usage-subagent.jsonl` (`TestReplayUsageSubagentCompact`); no claude voyage
fixture was recorded for #83.

## Compaction: recorded live (issue #82)

`pi-1.4.0-compaction-turn.jsonl` records a real Pi 1.4.0 **manual compaction**:
two short turns, Pi's own RPC command `{"type":"compact"}` on the session's
Conn, and the compaction Pi ran. It was made by
`TestLiveCompactionRecordsOrReportsUnverified`
(`internal/voyage/live_compaction_test.go`); `TestCommittedCompactionPin`
replays the committed bytes offline and asserts the same facts.

What the recording proves (all offline-checkable):

* Exactly one `compaction_started` and one `compaction_ended`, in that order,
  both with trigger `manual`, the end carrying `compaction_completed` — Pi's own
  frames `compaction_start{reason:"manual"}` / `compaction_end{reason:"manual",
  result:{…}}`, projected one-to-one.
* The command's answer is in the stream as a frame, not as an oarlock record:
  `{"type":"response","command":"compact","success":true,"data":{…}}` with
  Pi's `tokensBefore` and `estimatedTokensAfter`. An oarlock request record for
  it **does not exist** — `oarlock.Action` has no compact and the adapter sends
  Pi's RPC commands only for the actions it has (issue #82 excludes adding one),
  so the test speaks the command on the session's `Conn` and the fixture keeps
  the native answer. Do not read the fixture as "oarlock can compact".
* The fold's `Usage` ledger is untouched by the compaction: the fold before
  `compaction_started` and the fold just after `compaction_ended` report the
  same ledger. That is a property of the projection, **not** of Pi: the
  `compaction_end.result.usage` in the recording reports the summarization
  call's own usage (input 407 / output 474 / cacheRead 128 / reasoning 211,
  1009 tokens in total), and Pi's cumulative `get_session_stats` grows by
  exactly that (21478 → 22487). oarlock folds usage from assistant
  `message_end` frames only (`adapters/pi/project.go`), so a compaction's
  usage reaches no ledger — recorded here as a gap, not as "Pi reports none".
* Pi's post-compaction `get_session_stats` reports **no token count**
  (`contextUsage.tokens` null, the window still there). That is Pi's own rule
  (its `getContextUsage`: the last assistant usage is pre-compaction, so the
  count is unknown until a response follows the compaction), and the fold keeps
  it: a number here would be invented.

Two measurements that are **not** a claim that everything is well:

* **The folded status stays `running` after a manual compaction.** The fold
  turns `compaction_started` into a `running/compacting` span
  (`internal/fold/status.go`), and no `turn_ended` follows a compaction, so the
  status ends `running/waiting_model` and only the disposal moves it to
  `exited`. Consequence in the adapter: the next `Prompt` is refused locally as
  `busy` ("a turn is already running") — measured live, before the recording was
  narrowed: the compact command had answered and `compaction_end` had arrived,
  so Pi was not running a turn. The recording therefore reads the
  post-compaction context with a `get_session_stats` command rather than with a
  third `Prompt`; the fixture pins the measured status so a fold fix shows up as
  a test failure.
* **The context has to be made compactable.** Pi's manual compaction summarizes
  what a cut point keeps out, and it walks the branch back until
  `keepRecentTokens` is covered (per Pi's source, `DEFAULT_COMPACTION_SETTINGS`
  is 20000 tokens ≈ 80 kB of text; not measured here). A bounded live test
  cannot honestly produce such a conversation, so the recording's temp cwd
  carries a project setting `{"compaction": {"keepRecentTokens": 0}}`: the cut
  lands in the last turn and the turn before it is summarized. With Pi's default
  the same session would answer `Nothing to compact (session too small)` (Pi's
  source; not measured here) — a fact about the test's context size, not about
  the command. The setting lives in the session's cwd and is not part of the
  fixture.

So: the Pi RPC `compact` command is **available and recorded**, sent while the
session was idle (after the second turn settled; Pi also aborts a running
operation before a manual compaction, but that timing was not recorded). Pi's own
figures put the effect at `tokensBefore 10746` → `estimatedTokensAfter 9445`,
≈12 % — small, because the recording had to keep the context compactable. The
`/compact`-as-a-prompt path stays the refuted one: #71 showed Pi ran it as an
ordinary turn, which is why this fixture drives the RPC command instead. Not
covered: threshold/overflow compaction (needs a context beyond the model's
window), a compaction mid-turn, the fold's status rule above (recorded, not
fixed), and the compaction's own usage in the fold (recorded, not folded).

## Sub-agent: verified for codex, UNVERIFIED for Pi. Retry: recorded live

A child **session** is now recorded live (codex, #83, previous section). What
stays unverified is Pi's side: the recorded installation of Pi 1.4.0 offered no
delegating tool, and the pi adapter has no sub-agent fact at all. A sub-agent
task is therefore **UNVERIFIED for live recording**, and the evidence is in the
committed bytes rather than in this sentence:

* The recording's own system prompt lists the tool surface that installation
  offered:
  `read`, `bash`, `edit`, `write`, `codemode`, `sop_search`, `sop_save`,
  `tool_search` — note that `codemode` and the `sop_*` tools are host
  extensions, so this list is that machine's surface, not a claim about every
  Pi install. Nothing in it delegates to a sub-agent, so there is no tool call
  that could produce a task stream. A Pi with a sub-agent extension is the
  genuinely unverified case. `TestCommittedLiveFixturesHaveNoTaskOrRetryFacts`
  pins that list for all three live fixtures and fails if a delegating tool ever
  appears — at which point the task recording must be written, not deferred.
* `adapters/pi/project.go` has no `task_started`/`task_updated`/`task_ended`
  case at all: such a native frame would be counted as unknown, and Pi is
  declared `AttributionNone` because it has no child sessions. There is no
  projection to prove and no frame to project.
* The only retry frames the adapter maps are `auto_retry_start` and
  `summarization_retry_scheduled` (→ `retry{attempt,maxAttempts,delayMs,reason}`);
  `auto_retry_end`, `summarization_retry_attempt_start` and
  `summarization_retry_finished` are deliberately silent.
* `TestLiveSubagentAndRetryProbe` runs a real turn that asks for a sub-agent
  tool if one exists, reads the stream, and skips with `UNVERIFIED` and the
  tool surface it saw instead of inventing a fixture (the run that wrote this
  note: 0 task events, 0 retry events, the eight tools above).

The `EventTaskStarted`/`EventTaskUpdated`/`EventTaskEnded` kinds stay folded
only through synthetic frames (`adapters/pi/project_test.go` and the `fold`
cases). Do not read a green run as evidence for a live sub-agent.

## Retry: recorded live behind a fault proxy (issue #88)

A retry needs a provider that fails, so the recording injects the failure
locally instead of waiting for one. `TestLiveRetryRecordsAndReplays`
(`internal/voyage/live_retry_test.go`) starts an OpenAI-compatible proxy in the
test process: the first two requests answer `HTTP 500` with
`{"error":{"message":"upstream overloaded",...}}` — a body Pi classifies as
retriable, and deliberately not `quota`/`billing` wording, which Pi's
`isRetryableAssistantError` rejects before it looks at the status — and every
later request is forwarded verbatim to the machine's own configured endpoint,
same model, same account, only the base URL and the `Authorization` header
differ.

Isolation is the whole reason this is safe to run unattended:

* Pi is pointed at a throwaway agent directory through `PI_CODING_AGENT_DIR`
  (the variable `pi.AgentDir` already ports), which is a hard override: Pi reads
  `models.json`, `settings.json`, `auth.json`, extensions, skills and sessions
  from there and from nowhere else. The isolated directory declares a provider
  `oarlock-retry-proxy` whose `baseUrl` is the proxy, the real model id, and a
  `retry` policy of `baseDelayMs: 1, maxAgentDelayMs: 5` so the test does not
  sleep through Pi's 2 s/4 s backoff.
* The user's `~/.pi/agent` is **read only** (settings for the default provider,
  `models.json`/`models-store.json` for the model and base URL, `auth.json` for
  the credential). Nothing is written there; the credential is held in memory
  for the proxy's header and appears in no fixture, no log and no error.
* Without a discoverable default provider the test skips with `UNVERIFIED` and
  the reason, and without `OAR_PI_LIVE=1` it does not run at all. The recording
  is never a build step.

What a run produces (the committed bytes, `pi-1.4.0-retry-turn.jsonl`, 39
records / 34 frames / 46 KB): the prompt, a failed assistant attempt
(`turn_end` with `stopReason: "error"`, no projected outcome), then

```json
{"type":"auto_retry_start","attempt":1,"maxAttempts":3,"delayMs":1,
 "errorMessage":"oarlock-retry-proxy API error (500): {\"message\":\"upstream overloaded\",...}"}
```

a second failed attempt and `attempt: 2, delayMs: 2`, then the successful
answer, `{"type":"auto_retry_end","success":true,"attempt":2}` (deliberately
unprojected) and one `turn_ended{completed}` at settlement. The offline half
(`TestRetryFixtureReplays`) proves, with no process and no network, that the
retry events carry `attempt` 1 → 2, `maxAttempts 3`, the recorded backoff and
the provider's 500 as the reason, that each one arrives while the fold's status
is `running/waiting_model` (a retry never settles or restarts a turn), that the
failed attempts are not reported as the turn's outcome, and that the folded
Usage is the successful answer's tokens (the failed attempts report zeros and
add nothing) with the context report intact.

`summarization_retry_scheduled` stays synthetic: it fires only from a
compaction or branch-summary retry, i.e. a context overflow or a manual
compaction, and the retry shape it shares with `auto_retry_start` is the same
projection case. The remaining unverified retry case is exactly that one.

## How to upgrade a fixture

1. Re-record the session (or the raw runtime frames the fixture is built from).
2. Make sure it is redacted and contains no real credential.
3. Run `go generate ./internal/voyage`. It rebuilds the **stand-in** `pi-*`
   fixtures with `voyage-gen` and rewrites `index.json` with `voyage-index`,
   which refuses to write the index when a fixture is too large, truncated or
   fails the scan. The live fixture is not rebuilt by this step: re-record it
   with `OAR_PI_RECORD=1` (previous section), then run `go generate` for the
   index.
4. Review the diff of `testdata/voyage`, in particular `index.json`.
5. Run `go test ./internal/voyage/...`.

For the Pi stand-in fixtures the source of truth is `testdata/pi`: re-record
those frames with the live probe (`OAR_PI_LIVE=1`, see `testdata/pi` and
[the Pi runtime notes](runtimes/pi.md)), then run `go generate
./internal/voyage`. Before committing, `go generate ./internal/voyage && git diff
--exit-code -- testdata/voyage` must show no change beyond the one intended. The
GitHub workflow does not run that step yet (it runs `go test -race ./...`);
`TestCommittedIndexIsCurrent` fails on a stale index.

## Differential testing

`internal/voyage/golden_test.go` replays the committed fixtures and asserts that
independent paths agree on Status, Usage, Context and Conversation: the records
the live kernel retained against the records read back from the file, and the
incremental `Feed` fold against `fold.Of`, plus pinned expected values. This is
the flow of ADR-0006: record once, replay forever, and treat a vendor or
implementation change as a test failure rather than a rewrite.

## What is NOT verified

* **Live recording of a real Session is verified only for the shapes that have
  a live test**: one plain turn (`live_test.go`, issue #17), one bash tool turn
  (`live_tool_test.go`, issue #71), two bash calls in one turn and an aborted
  turn (`live_breadth_test.go`, issue #76), a codex child thread
  (`live_subagent_test.go`, issue #83), a provider retry behind the fault proxy
  (`live_retry_test.go`, issue #88), on the machine the test ran on. A Pi
  sub-agent session, a summarization retry and a context overflow are not
  recorded live; see "Sub-agent: verified for codex, UNVERIFIED for Pi" and
  "Retry: recorded live behind a fault proxy".
* **Manual compaction is verified; threshold and overflow compaction are not**
  (see "Compaction: recorded live"): the recording drives Pi's own RPC `compact`
  command and keeps its frames, and it pins the folded status staying `running`
  afterwards, which is a recorded gap rather than a working compaction
  control plane.
* The committed `pi-*` fixtures are built from the **real Pi 1.4.0 wire frames**
  in `testdata/pi`, but through a minimal stand-in projection
  (`internal/voyage/pifixture`), not through a live Session or the real
  adapter. Their `Native` payloads are the real frames; the `Events` are the
  stand-in's.
* There are no logs recorded by OAR and no TypeScript-to-Go converter, so the
  old-versus-new implementation differential against OAR is **unverified**.
* Windows is only cross-compiled locally (`GOOS=windows go build ./...`); the
  tests have not run there.
