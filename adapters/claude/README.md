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

## Gaps

* Not verified against a real claude (no binary here): `TestClaudePassesSuite`
  (internal/testing/scripted) and `TestLiveSteerEcho` skip with `NOT VERIFIED`.
* Refused until implemented: `mcpServers`, `mcpServerCredentials`, `serviceTier`,
  `userConfiguration`. Images, `Withdrawer`, `cancel_queued` and permission answers
  are not implemented.
