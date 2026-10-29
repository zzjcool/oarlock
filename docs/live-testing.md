# Live testing: one prompt against real runtimes

**Status:** issue [#41](../issues/0041-cross-runtime-live-matrix.md). Tests:
[`adapters/codex/live_test.go`](../adapters/codex/live_test.go),
[`adapters/cursor/live_test.go`](../adapters/cursor/live_test.go),
[`adapters/pi/live_test.go`](../adapters/pi/live_test.go). Public surface under
test: [`promptwait.go`](../promptwait.go) and
[`observe/observe.go`](../observe/observe.go).

Apart from its live tests, this repo is offline: projections replay recorded
fixtures, sessions run against fake agents, ops runs against fake executables.
Those tests answer "does oarlock read this stream correctly", not "does this
runtime, with this account and this endpoint, complete a turn through the public
API". Only a live run answers the second question, and only on the machine it
ran on.

Live tests do exist — Pi, Claude, Cursor and Codex each have some, every one of
them behind its own gate and skipped by default. Claude's are
[`adapters/claude/live_test.go`](../adapters/claude/live_test.go) and
[`internal/testing/scripted/claude_live_test.go`](../internal/testing/scripted/claude_live_test.go),
under `OAR_CLAUDE_LIVE=1`. What issue #41 adds is one specific shape of
evidence, not "live testing" in general: the public `Require` → `PromptAndWait`
→ `observe.StatusOf` → double-`Dispose` run for Codex and Cursor, a shape no
live test exercised before.

So the live tests exist, are opt-in, and are **all skipped by default**. This
document is how to turn one on, with no credential in the repository.

The rule that runs through all of it:

- **A live test is a measurement, not a build step.** It is skipped unless its
  gate environment variable is exactly `1`; `go test -race ./...` and CI (three
  OS legs, no secrets) need nothing — no login, no key, no vendor account.
- **A skip is not a pass.** A skipped live test prints why, and the reason says
  which variable to set or which `NOT VERIFIED` claim is still open.
- **A token never touches the repo.** It lives in the environment of the shell
  that runs `go test`, and nowhere else.

## 1. The matrix and its gates

| runtime | gate | test(s) | needs |
|---|---|---|---|
| codex | `OARLOCK_LIVE_CODEX=1` | `TestLivePromptAndWait`, `TestLiveCodex` | `codex` on `PATH` inside `[0.149.0, 0.166.0)`, a `CODEX_HOME` whose `config.toml` names a provider, `OPENAI_API_KEY` |
| cursor | `OAR_CURSOR_LIVE=1` | `TestLivePromptAndWait`, `TestLiveTurn` | `cursor-agent` on `PATH`, logged in (`cursor-agent login`) |
| pi | `OAR_PI_LIVE=1` | `TestLivePromptAndWait`, `TestLiveSteer` (steer into a running turn, then the idle refusal; ~1 min) (also the resume, inventory and smoke live tests) | a signed-in Pi and the model its config names |
| claude | `OAR_CLAUDE_LIVE=1` | `TestLiveSteerEcho`, `TestClaudePassesSuite` | a signed-in `claude` on `PATH`. No `TestLivePromptAndWait` yet: that public-surface test is issue [#42](../issues/0042-claude-steer-echo.md), deliberately not added here because it would edit `adapters/claude/live_test.go`, the file that issue writes |

`TestLivePromptAndWait` in `adapters/codex` and `adapters/cursor` is the
cross-runtime one: it drives **the public API only** — `oarlock.Require(id)` →
`Runtime.Session` → `oarlock.PromptAndWait` → `observe.StatusOf` → `Dispose` —
so a pass is evidence about the surface a host actually uses, not about an
adapter's internals. Its prompt is always the same
(`Reply with the single word: ok`), its workspace is always `t.TempDir()`, and
its wait is bounded (`oarlock.WithTimeout(2*time.Minute)`), so a live run cannot
hang a terminal or dirty the checkout.

Run everything without opting in and the suite is unchanged, only noisier with
skips:

```sh
go test -race ./...
```

## 2. Credentials: what is allowed to be where

- **The token is read from the environment only.** Codex's provider block names
  the variable (`env_key = "OPENAI_API_KEY"`); the value is exported in the
  shell. A live test never takes a key as a Go literal, an argv argument or a
  config value.
- **Never committed.** Not in `config.toml`, not in a fixture, not in a
  `testdata/` file, not in a log, not in an issue comment, not in a test report.
  The repository's `.gitignore` has no live-secret path, because there is no file
  holding one to ignore; the check is "grep the diff before you push", and the
  reviewer greps the staged diff again.
- **Never printed.** The live tests log outcomes, statuses and `Seq`s, never the
  environment. A report quotes the endpoint's model id and the command, not the
  key.
- **Endpoint configuration lives outside the repository.** Point `CODEX_HOME` at
  a scratch directory (the example below uses `/tmp/oarlock-live-codex`), so a
  machine-specific `base_url` never becomes a committed default and a checkout
  stays runnable on any machine.
- **A third-party endpoint is a normal endpoint.** Nothing in oarlock special-cases
  it: it is an OpenAI-compatible base URL, a model id and a key. The docs use
  `https://llm.example.com/v1` and `<your-key>` as placeholders; substitute your
  own and keep them out of git.

## 3. Codex against a third-party OpenAI-compatible endpoint

Codex is driven as `codex app-server --listen stdio://` (see
[docs/runtimes/codex.md](runtimes/codex.md)), so it reads its model and provider
from `$CODEX_HOME/config.toml`. Any endpoint that speaks the OpenAI **Responses**
API works, and no vendor login is involved.

### 3.1 The provider config

```sh
export CODEX_HOME=/tmp/oarlock-live-codex
mkdir -p "$CODEX_HOME"
cat > "$CODEX_HOME/config.toml" <<'TOML'
model = "<model-served-by-your-endpoint>"
model_provider = "thirdparty"

[model_providers.thirdparty]
name = "Third-party OpenAI Responses endpoint"
base_url = "https://llm.example.com/v1"
env_key = "OPENAI_API_KEY"
wire_api = "responses"
TOML
```

| key | what it decides |
|---|---|
| `model` | the exact model id your endpoint serves — the same string it lists, not a vendor alias |
| `model_provider` | which `[model_providers.<id>]` block below applies; `<id>` is a label you choose |
| `name` | the CLI's display name for that provider |
| `base_url` | the OpenAI-compatible base, `/v1` included, as the endpoint publishes it |
| `env_key` | the environment variable codex reads the token from — a **name**, never the token itself |
| `wire_api` | `"responses"`: the app-server talks the Responses API; a Chat-Completions-only endpoint is not this shape |

The file holds a base URL and a variable name, so it contains no secret — it
still lives outside the checkout, because the endpoint is the machine's choice,
not the repository's.

### 3.2 The two exports, in the shell that runs the test

```sh
export OPENAI_API_KEY=<your-key>
export CODEX_HOME=/tmp/oarlock-live-codex
```

`<your-key>` is a placeholder: paste the key your endpoint issued, or read it
from your own secret store into the variable. Do not write it into a file in
this repository. If you would rather not paste it into shell history, this
bash one-liner reads it without echo — still no file involved:

```sh
read -r -s -p 'OPENAI_API_KEY: ' OPENAI_API_KEY && export OPENAI_API_KEY
```

The test passes no `oarlock.Options.Env`, so `codex app-server` **inherits the
process environment** and sees both variables. That is the whole wiring: an
export that is not in this shell is not in the child.

### 3.3 Run it

```sh
OARLOCK_LIVE_CODEX=1 go test ./adapters/codex -run TestLivePromptAndWait -v -count=1
```

`-count=1` because the result depends on a remote endpoint: a cached `ok` from a
previous run would be a lie. Without `OARLOCK_LIVE_CODEX=1` the test skips.

It skips — never fails — when a precondition is absent: no `codex` on `PATH`, no
`CODEX_HOME` or no `$CODEX_HOME/config.toml`, no `OPENAI_API_KEY`, or the gate
unset. The skip text names the missing piece.

### 3.4 What the run asserts

One prompt, then everything a host is promised about it:

1. `run.Outcome == oarlock.RunCompleted` — the runtime reported a completed
   turn, not a failure, an abort or an interruption;
2. `run.Text` is non-empty after `strings.TrimSpace` — the answer arrived on the
   stream, not just in a `Native` frame;
3. `run.Turn != nil && run.Turn.Kind == oarlock.OutcomeCompleted` — the turn end
   is the runtime's own, never synthesized;
4. `observe.StatusOf(sess.Records(oarlock.Cursor{})).Value.Kind == observe.StatusIdle`
   — the folded status agrees with the outcome;
5. `Seq` strictly increases across `sess.Records(oarlock.Cursor{})` and
   `run.Seq > 0` — the dense monotonic kernel sequence holds on a real stream;
6. `Dispose` is idempotent — the first and the second call both return nil, each
   under its own bounded context (≤ 10 s), the second after the first completed.

Any of the six failing fails the test. A pass takes roughly one turn (tens of
seconds), bounded by the 2-minute wait.

### 3.5 The version window

The adapter supports `0.149.0 <= codex --version < 0.166.0`, enforced **before**
the session process starts: outside the window the open returns a
`*codex.VersionError`, and the live test fails with it rather than running
against a schema nobody recorded. The bound is oarlock's own decision — the
app-server fixtures were recorded up to `0.165.x` — not a vendor fact, and the
`initialize` reply's `userAgent` is checked against the same window. To move it,
move the adapter (and its fixtures); do not disable the gate.

```sh
codex --version    # must print a version inside the window
```

## 4. Cursor with the installed CLI

`cursor-agent` keeps its own credential store; oarlock never sees it and never
writes it (see [docs/runtimes/cursor.md](runtimes/cursor.md)). So the only setup
is the vendor's own login, once per machine:

```sh
cursor-agent login
```

Then:

```sh
export OAR_CURSOR_LIVE=1
go test ./adapters/cursor -run TestLivePromptAndWait -v -count=1
```

No key is involved and nothing is exported but the gate — the same command on
one line is `OAR_CURSOR_LIVE=1 go test ./adapters/cursor -run TestLivePromptAndWait -v -count=1`.
A pre-flight check is optional:

```sh
cursor-agent status
```

**Not logged in is a skip, not a failure.** If the live run reports a
login/credential failure the test calls the adapter's `skipIfNotLoggedIn`
helper and skips with a `NOT VERIFIED: cursor-agent is not logged in (…)`
message naming `cursor-agent login`. That is honest: the claim stays unverified
on that machine, and the suite stays green. The assertions are the same six as
§3.4.

## 5. Pi, and where Claude fits

Pi has an older, narrower live test: `TestLivePromptAndWait` in
[`adapters/pi/live_test.go`](../adapters/pi/live_test.go), gated on
`OAR_PI_LIVE=1`. It opens with `pi.Open`, waits with `oarlock.PromptAndWait` and
reads the folded status through `sess.(*Session).Status()` — not through
`oarlock.Require` and `observe.StatusOf` — and it asserts neither `Seq`
monotonicity nor `Dispose` idempotency. The six-assertion public-surface shape
of §3.4 is the Codex/Cursor one added in issue #41; Pi's test is not that shape.

It needs nothing beyond a signed-in Pi — Pi is installed with its host and names
its own provider and model in its config:

```sh
OAR_PI_LIVE=1 go test ./adapters/pi -run TestLivePromptAndWait -v -count=1
```

Pi's live suite under that gate also covers the resume, inventory and smoke
tests (see [docs/runtimes/pi.md](runtimes/pi.md) and
[`internal/testing/scripted/README.md`](../internal/testing/scripted/README.md)).

Claude already has live tests, both gated on `OAR_CLAUDE_LIVE=1`:
[`adapters/claude/live_test.go`](../adapters/claude/live_test.go) holds
`TestLiveSteerEcho`, which proves against a real `claude` that the steer echo
the protocol waits for really arrives, and
[`internal/testing/scripted/claude_live_test.go`](../internal/testing/scripted/claude_live_test.go)
holds `TestClaudePassesSuite`, the shared behaviour suite run on a live claude.
What Claude does not have yet is the public-surface `PromptAndWait` + status
live test — that is issue [#42](../issues/0042-claude-steer-echo.md), and it is
intentionally not added here: it would have to edit `adapters/claude/live_test.go`,
the same file that issue writes, so the two would conflict.

## 6. Troubleshooting

| symptom | cause | fix |
|---|---|---|
| `401`, `invalid_api_key`, `unauthorized` from the endpoint | `OPENAI_API_KEY` is not in the environment of the `go test` process, is empty/expired, or `env_key` names a variable the shell never exported | export it in the same shell as the test, and keep `env_key` and the variable name identical (`env_key = "OPENAI_API_KEY"` ⇔ `export OPENAI_API_KEY=<your-key>`) |
| `404`, `model_not_found`, "model not served" | `model` in `config.toml` names a model this endpoint does not serve (or `model_provider` names a block the file does not define) | set `model` to an id your endpoint lists, and make `model_provider` equal the `[model_providers.<id>]` label |
| `codex: unsupported codex version "…": oarlock supports codex >= 0.149.0 and < 0.166.0` | the installed codex is outside the pinned window | install a codex inside the window (`codex --version` to confirm); the gate is deliberate — the app-server schema is pinned, so a newer codex is a downgrade, not a warning |
| `NOT VERIFIED: cursor-agent is not logged in (…)` in a skip, naming `cursor-agent login` | no usable cursor credential on this machine | run `cursor-agent login`, then re-run the test |
| `--- SKIP: TestLivePromptAndWait (0.00s)` on the `go test -v` line, with the body `NOT VERIFIED: set OARLOCK_LIVE_CODEX=1 …` (or `… OAR_CURSOR_LIVE=1 …`; Pi's older test skips with `set OAR_PI_LIVE=1 to run PromptAndWait against real pi`) | the gate is unset, as designed | prefix the command with the gate, e.g. `OAR_CURSOR_LIVE=1 go test …` |
| `--- SKIP` naming `CODEX_HOME`, `config.toml` or `OPENAI_API_KEY` | a codex precondition is missing | §3.1 and §3.2, in the same shell |

## 7. What a live pass does not prove

- **One machine, one version, one account.** A green codex run says this codex
  build, this endpoint and this model completed one turn through the public API.
  It says nothing about another model on the same endpoint, or about the vendor's
  own login path.
- **No Windows coverage.** The fake-executable and fake-agent tests are POSIX
  scripts and skip on Windows; the live tests need a vendor CLI, so CI's Windows
  leg proves the build, never a live turn.
- **Not covered here:** Claude's public-surface `PromptAndWait` + status test
  (issue [#42](../issues/0042-claude-steer-echo.md)), the ACP family,
  `AccountUsage` (absent for every runtime), and official vendor-account
  logins.
