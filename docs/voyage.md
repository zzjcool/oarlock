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
`pi-1.4.0-multi-tool-turn.jsonl`, `pi-1.4.0-live-abort-turn.jsonl`.

Two kinds of fixture share the directory, and the index does not tell them
apart: the **stand-in** fixtures built from `testdata/pi` wire frames (`pi-1.4.0-abort-turn.jsonl`,
`pi-1.4.0-normal-turn.jsonl`, see below) and the **live recordings** of a real
Pi 1.4.0 session:

| fixture | issue | what it records |
| --- | --- | --- |
| `pi-1.4.0-tool-turn.jsonl` | [#71](../issues/0071-voyage-tool-compaction-live.md) | one bash call and the answer that follows it |
| `pi-1.4.0-multi-tool-turn.jsonl` | [#76](../issues/0076-voyage-breadth.md) | two bash calls in one turn |
| `pi-1.4.0-live-abort-turn.jsonl` | [#76](../issues/0076-voyage-breadth.md) | a turn aborted mid-call |

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

## Compaction: UNVERIFIED

No committed fixture contains a compaction, and this is a measurement, not a
convenience: the attempt is `TestLiveCompactionRecordsOrReportsUnverified`.

* Pi 1.4.0 in `--mode rpc` did **not** handle `/compact` as a command. The
  prompt was accepted (`ControlAccepted`), the projection recorded no
  `input_handled`, and the frames that followed were an ordinary turn —
  `agent_start`, `message_start`/`message_end`/`message_update`, and (in the
  run that produced this note) three `tool_execution_start`/`tool_execution_end`
  pairs, then `agent_settled` — that is, the text `/compact` reached the model. No `compaction_start` or
  `compaction_end` was ever sent, and the recorded session's status ended
  `idle`/`exited` exactly like a normal turn.
* This repository's control surface has no compact action either: `Action` has
  `prompt`, `steer`, `queue`, `withdraw`, `abort`, `dispose` and `native`, and
  the Pi adapter sends Pi's RPC commands only for those. Adding one is adapter
  code, which issue #71 explicitly excludes.
* Threshold compaction would need a turn whose context crosses the model's
  window (1 000 000 tokens for the model the recording used); that is not a
  bounded live test.

So compaction stays **UNVERIFIED for live recording**, and the fold's
compaction rules are only covered by the synthetic `project_test.go` and
`fold` cases. Do not read a green run as evidence for it.

## Sub-agent and retry: UNVERIFIED

The recorded installation of Pi 1.4.0 offered no delegating tool, the pi adapter
has no sub-agent fact at all, and neither was triggerable safely for a retry. Both are therefore **UNVERIFIED for
live recording**, and the evidence is in the committed bytes rather than in
this sentence:

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
  `summarization_retry_finished` are deliberately silent. Firing one needs a
  failing provider or an overflowing context, neither of which is a bounded
  live test — a forced provider failure would be a different experiment, and a
  context overflow needs a turn beyond the model's 1 000 000-token window.
* `TestLiveSubagentAndRetryProbe` runs a real turn that asks for a sub-agent
  tool if one exists, reads the stream, and skips with `UNVERIFIED` and the
  tool surface it saw instead of inventing a fixture (the run that wrote this
  note: 0 task events, 0 retry events, the eight tools above).

The `EventTaskStarted`/`EventTaskUpdated`/`EventTaskEnded` and `EventRetry`
kinds stay folded only through synthetic frames (`adapters/pi/project_test.go`
and the `fold` cases). Do not read a green run as evidence for a live
sub-agent or a live retry.

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
  turn (`live_breadth_test.go`, issue #76), on the machine the test ran on. A
  sub-agent session, a retry and a context overflow are not recorded live; see
  "Sub-agent and retry: UNVERIFIED".
* **Live compaction is unverified** (see "Compaction: UNVERIFIED"): no fixture
  contains a compaction frame.
* The committed `pi-*` fixtures are built from the **real Pi 1.4.0 wire frames**
  in `testdata/pi`, but through a minimal stand-in projection
  (`internal/voyage/pifixture`), not through a live Session or the real
  adapter. Their `Native` payloads are the real frames; the `Events` are the
  stand-in's.
* There are no logs recorded by OAR and no TypeScript-to-Go converter, so the
  old-versus-new implementation differential against OAR is **unverified**.
* Windows is only cross-compiled locally (`GOOS=windows go build ./...`); the
  tests have not run there.
