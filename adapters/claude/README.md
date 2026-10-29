# adapters/claude

Claude Code `-p --input-format stream-json --output-format stream-json` (issue #19).

* `internal/streamjson` — the **stream-json family**: framing, the bidirectional
  transport (`Launch`/`Conn`) and the shared fold (`Folder` + `Profile`). **Issue #22
  (Cursor) must import this package** (`streamjson.Launch`, `streamjson.NewFolder`,
  `streamjson.Profile`, `streamjson.ParseRec`/`Rec`/`AsRec`/`AsStr`/`AsNum`/`AsList`
  helpers, `ToolResults`) and implement only a `Profile`; it must not add a parser.
* `adapters/claude` — what is Claude's alone: argv, refused options, usage, context,
  failure classification, tasks, steer echo, resume, `ContextBreakdownReader`.

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

## Verification

Live on claude `2.1.312` against a third-party Anthropic-compatible endpoint
(no official account): `TestClaudePassesSuite` 9/9, `TestLiveSteerEcho` (real
matching-uuid `isReplay` echo), and `TestLivePromptAndWait`. The opt-in tests are
gated by `OAR_CLAUDE_LIVE=1`; when run as root, set `IS_SANDBOX=1`. Without that
environment they skip, never fake a pass.

## Gaps

* Refused until implemented: `mcpServers`, `mcpServerCredentials`, `serviceTier`,
  `userConfiguration`. Images, `Withdrawer`, `cancel_queued` and permission answers
  are not implemented.
* No open-time version gate: `Open` runs the CLI it is given; the ops probe reads
  `--version`, but a forced old binary is not refused at session open yet.
