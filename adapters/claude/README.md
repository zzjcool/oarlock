# adapters/claude

Claude Code `-p --input-format stream-json --output-format stream-json` (issue #19).

* `internal/streamjson` — the **stream-json family**: framing, the bidirectional
  transport (`Launch`/`Conn`) and the shared fold (`Folder` + `Profile`). **Issue #22
  (Cursor) must import this package** (`streamjson.Launch`, `streamjson.NewFolder`,
  `streamjson.Profile`, `streamjson.ParseRec`/`Rec`/`AsRec`/`AsStr`/`AsNum`/`AsList`
  helpers, `ToolResults`) and implement only a `Profile`; it must not add a parser.
* `adapters/claude` — what is Claude's alone: argv, refused options, usage, context,
  failure classification, tasks, steer echo, resume, `ContextBreakdownReader`,
  native images, the held queue's `Withdrawer`, `cancel_queued` and permission
  answers (issue #53).

## Decisions

* Steer writes a user message with a uuid and is `accepted` **only** when claude's
  replayed user frame with that uuid arrives; otherwise `rejected` and the input
  stays the caller's.
* A requested abort ends the turn `aborted` (claude reports it as an `is_error`
  result); otherwise `is_error` outranks `subtype`.
* Usage: `result.usage` (main loop, summed per turn) is the root's `Tokens`;
  `result.modelUsage` (cumulative, latest wins) is `Total`; the difference is the
  fold's `Unattributed` (sub-agents). Resume counts from a `get_usage` baseline.
* Private readbacks (`oar-` request ids) are consumed, never recorded.
* Images are native: `Capabilities().Images` is true and a prompt/steer/queue
  with `InputImage`s writes one
  `{"type":"image","source":{"type":"base64","media_type":…,"data":…}}` block
  per image **before** the text block (text block omitted for image-only input;
  png/jpeg/gif/webp only — other types `unsupported`, unreadable file `error`).
* `*Session` implements `oarlock.Withdrawer` over the queue it holds itself,
  keyed by the `inputId` the queue request carried: `accepted` only for an entry
  still held, `not_queued` for anything the drain may already have taken (or an
  id it never saw), `unsupported` for an empty id. The queue and withdraw
  records stay in the stream.
* `cancel_queued` is an option on abort, not a verb: the interrupt frame carries
  `"cancel_queued": true` only when the root `system/init` advertised
  `interrupt_cancel_queued_v1` and a written prompt-like input has no
  `command_lifecycle started` yet; otherwise the frame is exactly
  `{"subtype":"interrupt"}`. `(*Session).SupportsCancelQueued()` reports what
  claude declared.
* Permission requests are answerable but not assumed: the default
  `PermissionRecordOnly` records a runtime `control_request` as a `toApp`
  request and never answers it (headless claude runs with
  `--dangerously-skip-permissions` and cannot emit `can_use_tool`); a host
  answers explicitly through `Answer`, which writes claude's own
  `control_response` and records `answered` only once that write succeeded.
  Opt-in `SessionOptions.Permissions = PermissionAllow` answers the ladder
  allow_always > allow_once > cancelled, and `cancelled` means no answer is
  sent.

## Verification

Live on claude `2.1.312` against a third-party Anthropic-compatible endpoint
(no official account): `TestClaudePassesSuite` 9/9, `TestLiveSteerEcho` (real
matching-uuid `isReplay` echo), and `TestLivePromptAndWait`. The opt-in tests are
gated by `OAR_CLAUDE_LIVE=1`; when run as root, set `IS_SANDBOX=1`. Without that
environment they skip, never fake a pass.

The #53 capabilities, live on the same host and endpoint (2026-10-29, claude
`2.1.312`):

* **Images — PASS** (`TestLiveImages`, opt-in on `OAR_CLAUDE_LIVE_IMAGES=1` after
  `OAR_CLAUDE_LIVE=1`): an image-only prompt carrying a 1×1 PNG was accepted at
  seq 1, claude echoed its input lifecycle (`queued` → `started` → `completed`)
  and the turn ended with its own `result` (`completed`). The endpoint accepted
  the native block; the run does not prove the model *described* the picture —
  it proves the wire shape reached the runtime and a turn ran.
* **`cancel_queued` — PASS, with a caveat**: the live root `system/init`
  advertises `interrupt_cancel_queued_v1` (so `SupportsCancelQueued()` is true on
  2.1.312), an abort of a running turn is accepted and ends `aborted`, and the
  prompt's lifecycle closed `cancelled`. In that run the prompt had already
  `started` before the interrupt landed, so the interrupt was a plain one: the
  `"cancel_queued": true` frame itself is still fake-proven only.
* **Permission answers — record-only confirmed**: a live Bash turn produced zero
  runtime→app requests and zero `answered` records (`TestLiveNoPermissionRequest`)
  — headless claude under `--dangerously-skip-permissions` emits no
  `can_use_tool`, which is why `Answer` stays unmatched live
  (`TestLiveAnswerSkipped`).

The offline half (fake process + committed recordings) still carries the exact
shapes: `session_test.go`, `images_test.go`, `permission_test.go`.

## Gaps

* Refused until implemented: `mcpServers`, `mcpServerCredentials`, `serviceTier`,
  `userConfiguration`.
* The `allow_always` tier is unreachable on this dialect: claude's `can_use_tool`
  carries no options list, so the ladder degenerates to the one-shot allow, and
  nothing here has live evidence (`can_use_tool` cannot arrive under
  `--dangerously-skip-permissions`).
* `cancel_queued` is unsupported on a build that does not advertise
  `interrupt_cancel_queued_v1`: the abort sends a plain interrupt and queued
  input survives it.
* `Withdraw` only takes back input that never reached claude; once it is on the
  wire the answer is `not_queued`, and the native bulk cancellation is
  `cancel_queued` on abort.
* No open-time version gate: `Open` runs the CLI it is given; the ops probe reads
  `--version`, but a forced old binary is not refused at session open yet.
