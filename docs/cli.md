# CLI

**English** · [中文](cli.zh.md)

`oarlock` is a command line over the same public API a host program uses. Phase
1 has four commands:

| Command | Question it answers |
|---|---|
| `oarlock list` | Which runtimes are registered, and what does each declare? |
| `oarlock probe [<runtime>\|all]` | Is the vendor CLI installed, and which version? |
| `oarlock models [<runtime>\|all]` | Which models can this runtime run right now? |
| `oarlock run <runtime> <prompt>` | One turn, its result, optionally recorded |

Not in Phase 1: `ops`, `login`, `orient` and `mcp`. Those are a later
milestone. There is no TUI: the human views are plain tables and a
line-per-event transcript, and every command has a `--json` document for
scripts.

## 1. Build and run

```sh
go build ./cmd/oarlock        # ./oarlock (oarlock.exe on Windows)
go run ./cmd/oarlock list     # same thing, without leaving a binary behind
```

`cmd/oarlock` blank-imports every adapter (pi, claude, codex, cursor and the ACP
family), which is why `list` sees the whole registry. A host program that
imports one adapter sees one runtime. The CLI uses the standard library only —
`flag` for parsing, no third-party dependency.

The commands take these shapes:

```text
oarlock list [--json]
oarlock probe [<runtime>|all] [--json]
oarlock models [<runtime>|all] [--json]
oarlock run <runtime> <prompt> [flags]
oarlock help
```

`help` (also `-h`) prints the usage block on stdout and exits 0. No command, an
unknown command or a bad flag prints a diagnostic on stderr and exits 2. `run`
accepts flags before, between and after its two positional arguments
(`oarlock run pi 'fix it' --cwd /repo`); a lone `--` ends flag parsing, so a
prompt that itself begins with `-` must follow `--`
(`oarlock run pi -- '-hello'`).

## 2. `list`

One row per registered runtime: the id and a yes/no per optional operation.
`yes` means the runtime *implements* the operation, not that this account may
use it — oarlock never claims an account capability. Abridged to three rows:

```text
RUNTIME      SESSION  PROBE  MODELS  INSTALL  LOGIN  USAGE  UPGRADE  INVENTORY
claude       yes      yes    no      yes      yes    yes    yes      yes
cursor       yes      yes    yes     no       yes    no     yes      yes
pi           yes      yes    yes     no       no     no     no       yes
```

Exit 0. A registry that lists an id it cannot resolve is a broken registry, not
a runtime capability: the id is reported on stderr and the command exits 1.

`--json`:

```json
{"schema":"oarlock.cli/v1","command":"list","runtimes":[{"id":"pi","capabilities":{"session":true,"probe":true,"models":true,"install":false,"login":false,"usage":false,"upgrade":false,"inventory":true}}]}
```

`capabilities` always carries all eight booleans; `session` is true for every
registered runtime.

## 3. `probe [<runtime>|all]`

No argument and `all` both mean every registered runtime. Each runtime is
probed in its own goroutine and the results are kept in registry order (the
registry lists its ids sorted), so concurrent probing cannot reorder the table
or the JSON array.

One runtime's failure does not hide another's answer. The probe error travels in
that runtime's own result (`error`, plus whatever `installation` the probe read),
every other result is still printed, and the command exits 1. An unknown runtime
id is such an error: the registry has no such runtime.

`installation.available: false` is **not** an error. The probe ran and the
answer is "not installed", which exits 0. A probe error — a version gate that
refuses, a broken executable — is the only reason this command exits 1. A
runtime that declares no probe at all reports `available: false` too: that is
"not verified", the zero `Installation`, never a fabricated version.

The `ERROR` column is always present (empty when the probe succeeded). Abridged
to two rows:

```text
RUNTIME      AVAILABLE  VERSION  PATH              SOURCE  ERROR
antigravity  no         -        -                 -       -
pi           yes        1.4.0    /usr/local/bin/pi path    -
```

`--json`:

```json
{"schema":"oarlock.cli/v1","command":"probe","results":[{"runtime":"antigravity","installation":{"available":false}},{"runtime":"pi","installation":{"available":true,"version":"1.4.0","path":"/usr/local/bin/pi","source":"path"}}]}
```

An error result carries the installation only when the probe managed to read
one (a version-gate refusal includes `available` and `version`), plus `error`:
`{"runtime":"pi","installation":{…},"error":"probing pi: …"}`. A failure that
read nothing — or an unresolvable runtime id — has no `installation` key.

## 4. `models [<runtime>|all]`

Same argument, concurrency and ordering rules as `probe`. Each result is exactly
one of three honest answers, and an empty list is never invented:

| Result | Meaning | Exit |
|---|---|---|
| `models` | what the runtime reported, verbatim | 0 |
| `unsupported` | this runtime has no model listing: not registered, not installed, `Models` not implemented, or `ListModels` returned `*oarlock.UnsupportedOperation` | 0 |
| `error` | the probe or `ListModels` failed for another reason | 1 |

A runtime that returns an empty list with no error stays empty: the human table
says `(no models)` and the JSON result carries neither `models` nor
`unsupported`. That is the runtime's own answer, not a placeholder.

Abridged:

```text
RUNTIME  MODEL                         NAME                          EFFORT  SERVICE TIERS
claude   (unsupported: model listing is not implemented)
pi       workbuddy/deepseek-v4.1-flash workbuddy/deepseek-v4.1-flash -       -
```

`--json`:

```json
{"schema":"oarlock.cli/v1","command":"models","results":[{"runtime":"claude","installation":{"available":true,"version":"2.1.312 (Claude Code)","path":"/usr/local/bin/claude","source":"path"},"unsupported":"model listing is not implemented"},{"runtime":"pi","installation":{"available":true,"version":"1.4.0","path":"/usr/local/bin/pi","source":"path"},"models":[{"provider":"any-endpoint","id":"workbuddy/deepseek-v4.1-flash","name":"workbuddy/deepseek-v4.1-flash"}]}]}
```

`provider`, `name`, `effortLevels` and `serviceTiers` are present only when the
runtime reports them.

## 5. `run <runtime> <prompt>`

Opens one session, sends one prompt, waits for the runtime's own turn end,
reports the result and disposes. Every check made before that point is a pre-run
check, and a pre-run failure opens no session and starts no runtime.

### 5.1 Flags

| Flag | Meaning |
|---|---|
| `--cwd DIR` | Directory the runtime operates in. Default: the process cwd. Resolved absolute; must be an existing directory. |
| `--model NAME` | Runtime-native model id. |
| `--effort LEVEL` | Runtime-native reasoning level. |
| `--service-tier TIER` | Runtime-native service tier. |
| `--resume ID` | Reopen a native conversation. |
| `--image FILE` | Attach an image. Repeatable. Resolved absolute against the **process** cwd, not `--cwd`: a shell argument is relative to the shell that typed it. |
| `--timeout DURATION` | Deadline for the runtime's turn end. Default `10m`; `0` disables it, a negative value is a usage error (exit 2). |
| `--json` | Emit one JSON DTO instead of progress and the summary. |
| `--record FILE` | Write a voyage log after the run. Never overwrites. |

`--cwd`, `--model`, `--effort`, `--service-tier` and `--resume` travel in
`oarlock.Options`; the runtime's own `CheckOptions` refuses a declared-and-
unsupported combination before the session starts. `--image` paths reach the
runtime through `PromptOptions.InputOptions.Images` (`WithPromptOptions`).

The installation is probed before opening (when the runtime implements
`Probe`). A probe error, or a runtime that is not installed, is a pre-run error
(exit 1), not a failed turn.

### 5.2 Exit codes

| Code | Meaning |
|---|---|
| 0 | the turn completed (`outcome: completed`) |
| 1 | failed, aborted, rejected, interrupted by `--timeout`, a pre-run error, or a record write failure |
| 2 | usage error: bad flag, wrong number of positional arguments, unknown command |
| 130 | the first SIGINT interrupted the run and no completion won the race |

A usage error (2) never produces a DTO: it exits before a command decides how to
report.

### 5.3 Signals

The first SIGINT cancels the prompt context: `PromptAndWait` aborts the turn,
waits a bounded grace for the runtime's own end, and reports `interrupted`; the
session is then disposed and the command exits 130. A real completion that raced
the signal wins and exits 0.

The second SIGINT does not wait: the session is disposed (bounded to two
seconds) and the command exits 130. A `--record` log is **not** written on that
forced path. The first-SIGINT path does write one, with `end.reason` set to
`interrupted`.

`--timeout` expiry is also `interrupted`, but no signal fired, so it exits 1.
The JSON DTO tells the two apart: an interrupted run carries `error` (the
cancelled context), a timeout's DTO carries none.

### 5.4 Human output

The first line names the runtime, the session's directory and the model/effort
the stream already reported (falling back to the requested flags). Progress
lines then come from `Session.Events`, one line per event, and only for the
root agent — a child agent's text is not part of the transcript.

```text
[runtime pi] cwd=/repo model=claude-sonnet-4-5 effort=low
[thinking] reading internal/kernel
[text] The failing test is TestParseOdd...
[tool] bash started
[tool] bash ok
[turn] completed
```

The prefixes are `[text]` (streamed assistant text), `[thinking]` (with
`(redacted)` when the runtime redacted the reasoning), `[tool] <name> started` /
`[tool] <name> <ok|failed>` (`ended` when the runtime reported no result),
`[turn] <kind>`, `[compaction] started (<trigger>)` / `[compaction] <result>`,
and `[retry] attempt <n>/<max>: <reason>`.

After the turn, one summary block:

```text
outcome: completed
turn: completed
text: The failing test is TestParseOdd...
session: 01J...
model: claude-sonnet-4-5
effort: low
usage: input=1200 output=340
status: idle
```

`rejected: <code>` with its reason appears when the prompt was refused, and
`error:` when something failed. The summary repeats the answer that streamed
above it, so a terminal capture and a log end with the same text.

### 5.5 `--json`

Progress is off. Stdout carries exactly one line: the final DTO, printed after
the turn and after dispose. Pre-run errors also produce a DTO, with `error` set,
and exit 1, so a script can parse stdout unconditionally.

```json
{"schema":"oarlock.cli/v1","command":"run","runtime":"pi","sessionId":"01J...","outcome":"completed","text":"The failing test is TestParseOdd...","turn":{"kind":"completed"},"status":{"kind":"idle"},"usage":{"input":1200,"output":340},"context":{"tokens":8421,"window":1000000},"model":"claude-sonnet-4-5","effort":"low","serviceTier":"default","requestId":"req-1","seq":42}
```

Fields are omitted when the stream never carried them: unknown usage numbers and
context values stay absent instead of becoming a guessed zero. `model`, `effort`
and `serviceTier` report what the runtime folded from the stream and fall back
to the requested values. `status` is folded before dispose, so a finished turn
reads `idle` here while the record file still keeps every record, dispose
included.

A pre-run failure is one DTO too:

```json
{"schema":"oarlock.cli/v1","command":"run","runtime":"pi","error":"record file already exists: /tmp/run.voyage (refusing to overwrite)"}
```

The forced second-SIGINT path emits a DTO that no turn produced:
`{"schema":"oarlock.cli/v1","command":"run","runtime":"pi","outcome":"interrupted","error":"interrupted by second SIGINT"}`,
exit 130.

### 5.6 `--record`

The path is resolved absolute against the process cwd and checked with
`os.Lstat` **before** a session opens. An existing file fails with exit 1 and
starts no runtime:

```text
$ oarlock run pi 'fix it' --record /tmp/run.voyage
oarlock: run: record file already exists: /tmp/run.voyage (refusing to overwrite)
```

In JSON mode the same failure is a DTO with that `error`, still exit 1. The
parent directory must already exist.

After the turn and the dispose, the command writes the log with
`internal/voyage`: `Create` writes the header with `O_EXCL`, every retained
record — the dispose record included — is appended in dense `Seq` order, and
`End` writes the run's outcome as the reason. A write failure is a command failure (exit 1; a run that was already
an interrupt stays 130), because the record is evidence.

Header fields:

| Field | Value |
|---|---|
| `runtime` | the runtime id |
| `model`, `effort`, `serviceTier` | what was requested |
| `cwd` | the absolute `--cwd` |
| `sessionId` | the root session id the stream carried (empty when it carried none) |
| `startedAt` | Unix epoch milliseconds |
| `recorder` | `oarlock-cli/<version>`; a local `go build` records `oarlock-cli/dev` |

The header names what was requested; the records carry what the runtime
actually reported.

There are no live incremental writes: the file is created when the run
finishes, so a crash (or a second SIGINT) during the run leaves no file at all,
and a crash while the log is being written can leave a partial one. A log with
no `end` line is truncated: `voyage.Read` and `voyage.Decode` return the
salvaged prefix together with `ErrTruncated`, and `voyage.Replay` refuses it.
See [voyage.md](voyage.md) for the format and the truncation rules.

## 6. Tests and limits

- Phase 1 only: `list`, `probe`, `models`, `run`. `ops`, `login`, `orient` and
  `mcp` are not part of this command yet.
- No TUI: one turn per invocation, no steering, no interactive loop.
- Standard library only: `flag` for parsing, `encoding/json` for the DTOs; no
  third-party dependency and no change to `go.mod`.
- The DTOs live in `internal/cli` and *are* the schema. The CLI never marshals a
  root-package struct, so the library's own types can evolve without silently
  changing `--json`.
- What the tests pin: the DTO key sets and the `oarlock.cli/v1` schema, exit
  codes 0/1/2/130, per-runtime isolation and order for `probe` and `models`, and
  a scripted session for `run` — `--cwd`, `--image`, options, `--record`
  no-overwrite, `--timeout` and the first/second SIGINT. They are in
  `internal/cli/*_test.go` and need no real runtime installed.
