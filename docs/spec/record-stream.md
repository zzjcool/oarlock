# The record stream

**Status:** frozen for M0. Shape: [`record.go`](../../record.go),
[`event.go`](../../event.go). Decision: [ADR-0001](../adr/0001-three-kind-record-stream.md).
Reference behaviour: OAR `docs/spec/record-stream.md` and
`docs/design/decisions.md`.

One session owns one append-only stream. Control and facts travel on that one
stream, because two channels have no shared order: "did the abort land before
or after that `tool_result`" must stay answerable.

```
                 runtime frames                 controls / exits
                        │                              │
                        ▼                              ▼
  ┌──────────────────────────────────────────────────────────────┐
  │ Record{ Seq, ReceivedAt, Kind, SessionID, AgentPath, SpanID, │
  │         Native, Events | ID, Direction, Request |            │
  │         RequestID, Response }                                │
  └──────────────────────────────────────────────────────────────┘
        │                    │                         │
        ▼                    ▼                         ▼
     frame                request                   response
  (runtime words)     (action + outcome)      (points at a request)
```

`Seq` is the total order and the cursor anchor. `ReceivedAt` is observation
time, outside the determinism guarantee. `Native` is the runtime's frame
verbatim (except documented credential redaction, [#4](redaction.md)).
`Events` is oarlock's typed reading of that frame, in frame order.

## Three kinds, discriminated by obligation

| kind | meaning | invariants |
|---|---|---|
| `frame` | the runtime's own words | append-only; one native frame is exactly one record; oarlock never synthesizes one |
| `request` | an action that expects an outcome | `Direction` says `toRuntime` (prompt/steer/queue/withdraw/abort/dispose) or `toApp` (permission/question/external tool) |
| `response` | the outcome of a request | always carries `RequestID`; exists **only** when oarlock observed an outcome the runtime would not state itself |

A response does not have to exist. A request without a response is an honest
record: the action was initiated and its outcome was not observed. Backfilling
a guessed response is forbidden.

`ResponseBody.Kind` has four values:

- `accepted` — the runtime or adapter took the action over. Weak promise:
  delivery obligation ends. It says nothing about where the input landed.
- `rejected` — not taken over; the caller still owns the input. `Code` is the
  branch word ([#3](errors.md)).
- `answered` — oarlock's own reply to a `toApp` request.
- `exited` — the process exited. `ExitCodeReported` distinguishes "reported a
  numeric-less exit" from "no exit reported". This is the one outcome a
  runtime can never report itself.

## Five invariants, and the accident each prevents

### 1. No synthesized facts

A turn starts at the prompt request and ends at the runtime's own completion
event (pi `agent_settled`, claude `result`, codex `turn/completed`). If the
runtime reports neither, the stream says so. When a prompt does not start a
turn (a slash command handled it, `disposition: "handled"`), there is no turn
event at all.

*Prevents:* a fabricated `turn_ended`. OAR names this as the first failure of
a control-object model. A host that waits on a synthetic turn end reports
success for work the runtime never did.

*Test:* a scripted runtime whose prompt is handled must produce `Status=idle`
with no `turn_ended` event and a dense, monotonic `Seq`.

### 2. Control never prunes facts

There is no settled gate. A frame that arrives after its span ended still
enters the stream. A late native reply after an accepted abort remains a
frame; it never becomes a second response.

*Prevents:* the settled-gate accident, where an adapter drops a `tool_result`
that races a turn end, leaving a tool call open forever and a replay that
disagrees with the live view.

*Test:* inject a frame after `turn_ended`; the record must still be retained
and observable.

### 3. Reachability is read off the stream

Once the stream contains an `exited` response or a `dispose` request, every
later `toRuntime` control is rejected by the kernel as `runtime_exited` or
`disposed` **without consulting the adapter**. No private `isAlive` flag
exists. `dispose` itself is still recorded and answered `accepted` after an
observed exit (nothing is left to release).

*Prevents:* two sources of truth for liveness. An adapter flag that disagrees
with the stream either sends a command into a dead pipe or refuses a live one.

*Test:* after an `exited` response, `Abort` returns `Rejected{Code:
RuntimeExited}` and the adapter's decide function is not called.

### 4. Empty input without images is refused before the adapter runs

The request is still recorded (for audit and replay), and the response is
`rejected{code: unsupported}`. Whitespace is not trimmed; only the empty
string is refused.

*Prevents:* a vendor-specific empty-prompt path, and an adapter that opens a
turn with no input and leaves the session `running`.

*Test:* `Prompt(ctx, "", InputOptions{})` records a request and rejects
`unsupported`; `Prompt(ctx, "", InputOptions{Images: ...})` reaches the
adapter.

### 5. Credential values never enter records

Adapters redact documented credential fields and known session credential
values **before** the kernel retains or publishes a frame. Live observers and
replay therefore see the same redacted record; the runtime's own
configuration is unchanged. Rules: [#4](redaction.md).

*Prevents:* a secret that exists only in the live stream and not in the replay
log, or the reverse, making incident analysis untrustworthy.

*Test:* the redaction table in `internal/redact`, plus a record-level test
that a native `env` credential arrives as `[redacted]` while a harmless
variable stays readable.

## Attribution

Every record carries:

- `SessionID` — runtime-native; a derived child session carries its own id.
- `AgentPath` — `nil`/`[]` is the root. A tool call or span is identified by
  `(AgentPath, id)`, never by a bare id.
- `SpanID` — only when the runtime has a native turn/span id. oarlock never
  generates one.

Folds scope to the root session. A child's `turn_ended` must not end the
root's turn. Usage reads a child only to add its total once under
`WithChildren`.

## Cursor and stream instances

`Cursor{SessionID, AfterSeq}` means: replay every retained record with
`Seq > AfterSeq`, then continue live. `AfterSeq == 0` is the start of the
stream; the first record has `Seq == 1`. Sequence numbers are dense: no gaps,
no reordering, no reuse.

A **stream instance** is one opened process/session. `resume` opens a new
instance and restarts `Seq` at 1, even when the runtime-native session id is
the same. The cursor's `SessionID` names the instance's session, not a
persistent log. The host must store the instance identity it used with the
cursor and must not apply a cursor from one instance to another. A cursor
whose `SessionID` does not match the kernel's session is rejected.

The cursor replays retained records and then continues live with no gap and no
duplicate. This is one of the five properties the kernel's tests exercise.

## `toApp` direction

`Direction` is the only test for "does the application need to answer this?".
A `toApp` body is the runtime's method/subtype plus payload, verbatim, with an
open vocabulary. Consumers must not parse the body to decide direction; a new
vendor approval shape must not require a parsing change to stay answerable.

The adapter records its own automatic answer (where it gives one) as an
`answered` response pointing at the request. A `toApp` request cancelled by
the runtime produces an `app_request_cancelled` event; the request record
stays, unanswered.

## Event vocabulary

Every `EventKind` in [`event.go`](../../event.go). An event is a fact the runtime
stated in a frame; an adapter that does not understand a native event leaves
it in `Native` and counts it (next section).

| kind | meaning | produced from |
|---|---|---|
| `user_message` | a user message observed in the runtime's output; `Evidence` says how | frame |
| `turn_active` | the runtime says a turn is now running | frame (pi `agent_start`) |
| `text_delta` | one streamed assistant text chunk | frame |
| `reasoning` | a reasoning item; `Redacted` when the runtime withheld the content | frame |
| `turn_ended` | the runtime's own end of a turn, with its `Outcome`; the only turn end | frame (pi `agent_settled`, claude `result`, codex `turn/completed`) |
| `session_linked` | runtime-reported lineage between two sessions (`Parent`, `Child`) | frame |
| `input_queued` | the runtime queued an input (`InputID`) | frame |
| `input_dropped` | the runtime discarded an accepted input (`InputID`, `Dropped` says why) | frame |
| `input_handled` | the runtime reported a prompt as handled without opening a turn (pi: prompt response `disposition=handled`). Status returns to idle; no `turn_active`, no `turn_ended`. | frame (pi `response` to `prompt`) |
| `tool_call_started` | a tool call began (`CallID`, `Tool`) | frame |
| `tool_call_input_delta` | a fragment of the call's arguments: append-only, possibly invalid JSON | frame |
| `tool_call_input` | the complete arguments; replaces the whole input | frame |
| `tool_call_progress` | streamed output preview of a running call | frame |
| `tool_call_ended` | the runtime's own end report for the call (`Content`, `Result`, `ExitCode` only when reported) | frame |
| `usage` | a running token total the runtime reported (`Usage`) | frame |
| `model` | the model the runtime reported using | frame |
| `effort` | the reasoning effort the runtime reported using | frame |
| `service_tier` | the service tier the runtime reported using | frame |
| `retry` | the runtime is retrying a model call (`Attempt`, `MaxAttempts`, `DelayMS`, `Reason`) | frame |
| `compaction_started` | context compaction began (`Trigger`) | frame |
| `compaction_ended` | context compaction ended (`CompactionResult`) | frame |
| `task_started` | a native task (sub-agent, background work) began | frame |
| `task_updated` | a native task reported progress | frame |
| `task_ended` | a native task ended | frame |
| `app_request_cancelled` | the runtime cancelled a `toApp` request (`RequestID`); the request record stays, unanswered | frame |

`input_handled` is the runtime's own report, carried by the frame that stated
it (for pi, the `prompt` response frame). oarlock never synthesizes it, and a
handled prompt never gets a `turn_active` or `turn_ended` ([invariant
1](#1-no-synthesized-facts)). The event names the prompt by `RequestID` (always
the prompt request's id) and by `InputID` (the host's optional id, so it may be
empty); the status fold matches either, so a prompt without an `InputID` still
returns to idle, and it sets no `LastTurnOutcome` because no turn ended.

## Native, events, and unknown input

- Every native frame is retained whole in `Native`, even when `Events` is
  empty.
- `Events` is a **list** because one frame can say several things (a claude
  assistant message with thinking + text + tool_use is one frame with three
  events). Splitting the frame would duplicate `Native`; merging frames would
  lose the runtime's own framing.
- An unknown native event type is kept in `Native` and counted by the
  adapter's unknown-event tally. It is never coerced into a plausible
  `Event`, never dropped, and never turned into a log-only warning. A
  non-zero tally is a test signal.
- Tool input distinguishes fragments (`tool_call_input_delta`, append-only,
  possibly invalid JSON) from complete arguments (`tool_call_input`, replaces
  the whole input). oarlock never parses partial JSON.
- A tool outcome (`ok`/`failed`) and an exit code are present only when the
  runtime reported them. Neither is inferred from output, timing, or the
  other field.

## Three rejected designs

1. **Synthesized `turn_end`.** A timer or an adapter's `finally` block emits
   the end of a turn. The stream then claims a completion the runtime never
   reported; a host cannot tell a real finish from a killed run.
2. **Settled gate.** Control owns a span and drops facts that arrive after it
   settles. A late `tool_result` vanishes, the live view and the replay
   disagree, and the loss is silent because there is no record of the drop.
3. **Two channels.** Control and facts each get a stream. Cross-channel order
   is undefined, so "abort vs tool_result" cannot be answered and cursors
   cannot align the two histories.

## Verification

```
gofmt -l .            # empty
go vet ./...
go test -race ./...
```

The type-level assertions live in `record_test.go`. The five invariants are
enforced and tested by the kernel ([#6](../../issues/0006-kernel.md)); this
document is the contract those tests cite.

## Non-goals

- The persistence format (voyage, [#17](../../issues/0017-voyage-fixtures.md)).
- Cross-process cursor recovery (the host stores and supplies the cursor).
- Reading native session history back through the library.
