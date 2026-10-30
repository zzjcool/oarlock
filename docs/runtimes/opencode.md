# OpenCode over ACP — the one ACP family member with a live session (#43, #55)

Oarlock drives OpenCode as an ACP agent (`opencode acp`), like the other five
members of the family. This page is the live record: what was actually run,
against what, and which claims the run does **not** support. The profile code
is [`adapters/acp/profile_opencode.go`](../../adapters/acp/profile_opencode.go);
the shared transport is [`adapters/acp/open.go`](../../adapters/acp/open.go) and
[`session.go`](../../adapters/acp/session.go).

Status: **v1 and v2 live sessions verified** on 2026-10-29 — opencode `1.19.5`
(the v1 release line) and `2.0.26` (the v2 line), a third-party
OpenAI-compatible endpoint, no vendor account. Issue #55 added a second live run
on the v1 installation for the depth claims: usage ledger, `session/resume`,
steer and images (§5.1). Issue #77 installed the **other** release line —
`@opencode/cli@2.0.26` into a one-off temporary npm prefix, the v1 installation
untouched (§1) — and ran the v2 live tests: all three green, recorded in §5.2
(turn, nested child session, and usage reported on the wire but not billed).
Everything else in this page is marked UNVERIFIED where it is unverified.

## 1. Installation used

```sh
# v1 line — 2026-10-29, node 22.23.1, linux/amd64; untouched by the v2 work
npm i -g opencode-ai   # installs 1.19.5
opencode --version            # 1.19.5

# v2 line — issue #77, 2026-10-29; a temporary prefix, so the global v1 stays put
npm i --prefix /tmp/opencode-v2 @opencode/cli@2.0.26   # same as npm init -y && npm i in that dir
/tmp/opencode-v2/node_modules/.bin/opencode --version  # opencode v2.0.26
```

OpenCode ships two release lines that do not install side by side, and the
reason is the package name: **v1 is the npm package `opencode-ai`** (1.x,
binary `opencode`) and **v2 is the npm package `@opencode/cli`** (2.x, same binary
name). One global npm prefix therefore holds one of them at a time. The v2
install above is a second prefix (`/tmp/opencode-v2`), reached by absolute path
or through `OAR_ACP_LIVE_OPENCODE_V2` (§4); the v1 installation was never
overwritten and `opencode --version` still answers `1.19.5`.

The profile picks the line from `Installation.Version` (v1 when unknown). The
recorded baseline for the v2 line is the fixture
`adapters/acp/testdata/opencode-acp-v2-child.json` (opencode 2.0.26, recorded
2026-10-09); it stays the baseline the offline projection replays. Issue #77
also ran the v2 line live against 2.0.26 on the temporary installation (§5.2),
so the `nested` tier and the usage gap no longer rest on the fixture alone.

`/tmp/opencode-v2` is a one-off scratch prefix on this host, not something
oarlock knows about: it exists because a second global install would have
displaced v1 (§4). Any path works through `OAR_ACP_LIVE_OPENCODE_V2`.

## 2. The invocation and the wire

`opencode acp` speaks ACP v1 over stdio. What the live agent answered and did:

| step | live answer (2026-10-29) |
|---|---|
| `initialize` | `protocolVersion: 1`, `loadSession: true`, `promptCapabilities: {image, embeddedContext}`, `sessionCapabilities: {close, fork, list, resume}`, `authMethods: [opencode-login]`, `agentInfo: {OpenCode, 1.19.5}` |
| `authenticate` | **not called**: the profile has no `AuthMethod`, so no credentials ever pass through oarlock. Providers come from configuration and environment (below). |
| `session/new` | `{sessionId, configOptions}` with `model` (select) and `mode` (`build`/`plan`) |
| model | `Options.Model` is applied through `session/set_config_option model=<provider>/<model>`; the answer carries every option and the new `currentValue`, which the projection reports as a `model` event |
| `session/prompt` | blocks streamed as `session/update` (`agent_message_chunk`, `agent_thought_chunk`, `usage_update`, `config_option_update`, `available_commands_update`) |
| `session/prompt` answer `usage` | a top-level **per-turn** object: `{"inputTokens":6440,"outputTokens":2,"totalTokens":8106,"cachedReadTokens":1664}` (turn 1), then `{"inputTokens":180,"outputTokens":2,"totalTokens":8118,"cachedReadTokens":7936}` (turn 2). `inputTokens` excludes the cache; `totalTokens` is per turn (`input + output + thought + cachedRead`), not a session view. Projected as `Usage/Tokens` (cumulative; cache folded into input, thought into output and reasoning); no `Usage/Total` because the vendor reports no session-wide figure (§5.1, issue #55) |
| `turn end` | the runtime's own `session/prompt` **answer**: `{"stopReason":"end_turn","usage":{…}}`. Nothing is synthesized: `turn_ended{completed}` is read off that answer frame. |
| `session/cancel` | the answer becomes `{"stopReason":"cancelled","usage":{…0…}}` → `turn_ended{aborted}`. The cancel itself is the accepted `abort` control record. |
| `session/request_permission` | issued only when the config asks (see `permission` below); the default rule answers `allow_always` (`"optionId":"always"`), the request and its answer are both recorded verbatim |
| dispose | the process tree is ended (stdin closed, then the owned process tree killed) and one `exited` response is recorded; a second `Dispose` adds no record |

Notes that are true of this agent, not of the protocol:

- A `session/cancel` that lands **before the turn produced anything** (the
  abort is written in the same millisecond as the prompt) makes opencode answer
  the prompt with `{"code":-32603,"message":"Internal error: Internal service
  failure"}` — a `failed` turn, not a cancelled one. The live test therefore
  lets the turn start (~2 s) before cancelling, and the observation is recorded
  here so nobody reads "cancel is always a clean `stopReason: cancelled`".
- OpenCode advertises `sessionCapabilities.close`; oarlock's `Dispose` does not
  call `session/close` — it closes the transport and reaps the process tree.
  The advertised call is **UNVERIFIED**.
- A second `session/prompt` sent while a turn runs is admitted as user input and
  read at the running loop's next step (steer): the original prompt's answer is
  the only turn end, and the steer's own RPC is answered at idle with the same
  usage object. A steer the runtime refuses would answer with a JSON-RPC error;
  oarlock records that as `input_dropped{runtime_refused}` and never as a turn
  end (§5.1).
- The prompt answer carries a `usage` object; the profile's `PromptTokens`
  hook bills it (§5.1) as the running `Usage/Tokens`. There is no session-wide
  figure, so `UsageReport.Total` stays nil, and a turn that reports no `usage`
  projects nothing (never a zero).

### 2.1 Models: the `session/new` `configOptions` (issue #98)

The catalogue a host asks for (`Runtime().Models`, `adapters/acp/models.go`) is
read over this same channel — oarlock never shells out to a second surface:

- `initialize` (protocol version 1, `fs` and `terminal` capabilities declined,
  and **without** the profile's own `_meta` — those carry session-shaping
  hints) → `authenticate` only when the profile can name a method from the
  answer (opencode has none, so none is sent) → `session/new {cwd,
  mcpServers: []}`.
- The `model` config option is the catalogue: the option whose `category` is
  `model` (or, the spelling OAR tolerated too, no `category` at all with
  `id: "model"`) carries the models in `options` — grouped or flat, both
  flattened — and the selected one in `currentValue`. `value` → `ID`, `name`
  (else the id) → `Name`, provider `opencode`.
- The effort menu is the `thought_level` option (v2's `effort`, §5.2). It is
  **session-scoped**, so it is attached only to the model whose id equals the
  model option's `currentValue`; every other entry gets no `EffortLevels`. The
  literal `off` is dropped: kimi and opencode send it, and it is the absence of
  an effort level, not a level.
- Dropped because `oarlock.Model` has no field for it: the default-model marker
  (`currentModelId` / `current_model_id`, and `currentValue` as a flag) and the
  group labels of a grouped options list.
- The session the read opened is closed best-effort (`session/close`, 2 s, every
  error swallowed) and the process is killed afterwards, so a listing leaves no
  session and no agent behind. Each request carries its own 30 s deadline.
- No `model` option in the answer is `*oarlock.UnsupportedOperation`, never an
  empty list; a `model` option with **no `options` array at all** is a plain
  error, because an absent catalogue is not "zero models" (an explicit
  `"options": []` is, and returns an empty non-nil slice). `-32000` **with** auth
  wording is the adapter's `ErrNotAuthenticated`, while a generic server error
  and a stream that ended before the answer are plain errors.

**The deliberate difference from OAR**: OAR lists opencode's models through the
`opencode models` CLI; oarlock reads the ACP handshake it already speaks rather
than adding a second transport and a second output format to parse.

**UNVERIFIED**: this listing has only run against the in-process fake agent
(`adapters/acp/models_test.go`). What §2's table and §5.2 report is the `model`
config option inside a real open session, not this read.

## 3. Provider configuration (no user config touched)

OpenCode reads `$XDG_CONFIG_HOME/opencode/opencode.json`. The live test builds
that file in a temporary directory, so the user's own
`~/.config/opencode/opencode.json` is **never read and never written**. The
credential is an `{env:...}` reference; the value only ever exists in the
child's environment.

```json
{
  "$schema": "https://opencode.ai/config.json",
  "model": "live/<model-id>",
  "provider": {
    "live": {
      "name": "oarlock live ACP",
      "npm": "@ai-sdk/openai-compatible",
      "models": {
        "<model-id>": {
          "name": "live <model-id>",
          "tool_call": true,
          "attachment": false,
          "limit": { "context": 128000, "output": 16000 }
        }
      },
      "options": {
        "baseURL": "<OpenAI-compatible base URL, e.g. https://host/v1>",
        "apiKey": "{env:OAR_LIVE_PROVIDER_KEY}"
      }
    }
  },
  "permission": { "bash": "ask" }
}
```

- `permission.bash: "ask"` is what makes the runtime issue
  `session/request_permission` instead of running the tool unasked. In a fresh
  config without it, the endpoint-driven turn ran `echo` with no permission
  request at all (observed) — so the permission path is only exercised when the
  config asks for it.
- A model id containing `/` (e.g. `workbuddy/deepseek-v4.1-flash`) is fine: the
  session model value is `provider/model`, split on the first `/`.
- An `npm` provider package is fetched by opencode at startup on first use
  (cache stays warm afterwards); the live test needs network for that and for
  the model call anyway.

## 4. Running the live test

Both lines run through the same harness against the same endpoint; only the
binary and the run pattern differ. The key is read from the auth store into the
**environment of the `go test` process** and never becomes a file, a fixture, an
argv argument or a committed config value (§7 has the mechanics).

**v1 — the default line (`OAR_ACP_LIVE_OPENCODE`)**

```sh
# The key is read from the auth store into the environment; it is never
# printed, written to a file, or committed.
export OAR_ACP_LIVE=1
export OAR_ACP_LIVE_BASE_URL='<OpenAI-compatible base URL>'
export OAR_ACP_LIVE_API_KEY="$(…read from your auth store…)"
export OAR_ACP_LIVE_MODEL='workbuddy/deepseek-v4.1-flash'   # optional default
go test -race ./adapters/acp -run 'TestLiveOpenCode' -v -count=1
```

`OAR_ACP_LIVE_OPENCODE=/path/to/opencode` overrides the binary (default:
`opencode` on `PATH`). `-run 'TestLiveOpenCode'` is a substring match, so it
also selects the `TestLiveOpenCodeV2*` tests below; with
`OAR_ACP_LIVE_OPENCODE_V2` unset those skip with their own reason.

**v2 — the second line (`OAR_ACP_LIVE_OPENCODE_V2`, issue #77)**

```sh
export OAR_ACP_LIVE_OPENCODE_V2=/tmp/opencode-v2/node_modules/.bin/opencode   # a 2.x binary, §1
go test ./adapters/acp -run 'TestLiveOpenCodeV2' -v -count=1
```

The path in that line is this host's one-off scratch prefix (§1), quoted as an
example — substitute wherever the 2.x package was installed. The three tests
take ~29 s warm; a cold data directory can push the first `session/new` past
20 s, and the 60 s `openTimeout` covers it (§5.2).

`OAR_ACP_LIVE_OPENCODE_V2` is a filesystem path, not a version: the v2 tests
open the binary it names, the v1 tests keep reading `OAR_ACP_LIVE_OPENCODE` or
`PATH`. So the two lines are measured without either one being reinstalled, and
nothing in the v2 recipe writes to the v1 prefix, to a checked-in file or to the
user's own `~/.config/opencode` (§3).

Without `OAR_ACP_LIVE=1`, or without the base URL/key, or without the binary a
test needs, the test **skips with the reason** — a skipped run is `NOT VERIFIED`,
never a pass. Since issue #77 that also holds per line: a **v1** test whose
binary answers a 2.x version **skips** (reason starting `NOT VERIFIED:`) rather
than failing, because the v1 path was never measured against a 2.x agent; a
**v2** test with `OAR_ACP_LIVE_OPENCODE_V2` unset skips with a reason naming that
variable.

The family's older gate `OARLOCK_LIVE_ACP=opencode` still works and runs the
compact smoke test (`TestLiveSmoke/opencode`) through the same harness.

## 5. What the live run proved

`go test -race ./adapters/acp -run 'TestLiveOpenCode' -count=1` on 2026-10-29,
opencode 1.19.5, model `workbuddy/deepseek-v4.1-flash`:

| test | assertions (all green) |
|---|---|
| `TestLiveOpenCodeTurn` | `initialize` negotiated (agent name+version, `image=true` matching `Capabilities().Images`), `session/new` id non-empty, model in effect = the requested `provider/model`, record stream **dense** (`seq` 1..n), `turn_ended` exactly once with `completed`, the ending record is the runtime's own answer frame containing `stopReason`, `Text` non-empty ("pong"), status `idle`, exactly one `dispose` + one `exited`, status `exited`, and the provider key appears **nowhere** in the records |
| `TestLiveOpenCodeCancel` | prompt accepted → turn running → `abort` accepted → the runtime answers `stopReason: cancelled` → exactly one `turn_ended{aborted}` → status `idle` → a second `abort` is rejected `no_active_turn` |
| `TestLiveOpenCodePermissionPath` | a `session/request_permission` request is recorded (toApp, verbatim), answered `allow_always` (`"optionId":"always"`), and the turn still ends `completed`. If a run's model calls no tool, this test skips as `NOT VERIFIED` instead of passing |

A one-turn run produced 11 records (5 handshake/session frames, the prompt
request+accepted response, the streamed update frames, the answer frame) — the
stream is the wire, nothing invented.

The same day, with the v2 work living in a separate prefix (§1), the v1
installation was re-run and did not regress: `TestLiveOpenCodeTurn` **PASS**
(9.76 s, attribution `opaque`, 11 records) and `TestLiveOpenCodeCancel` **PASS**
(7.16 s, turn `aborted`); `opencode --version` still answers `1.19.5`.

### 5.1 The depth run (issue #55, 2026-10-29)

The same installation and endpoint, `go test ./adapters/acp -run
'TestLiveOpenCode…' -count=1` with `OAR_ACP_LIVE=1`:

| test | assertions (all green) |
|---|---|
| `TestLiveOpenCodeUsage` | two prompts bill two per-turn `usage` objects; the production mapping equals the wire numbers; the folded total is their sum (not a session re-read); exactly two token-bearing `usage` events, the last one cumulative |
| `TestLiveOpenCodeSteer` | a second `session/prompt` mid-turn is accepted; both answers arrive at idle; exactly **one** `turn_ended` (the original prompt's answer); the steered instruction reached the model (the final answer carried its marker word); a steer with no turn is rejected `no_active_turn`. That the steered answer bills nothing is pinned by the projector test `TestOpenCodeProjectorSteerAnswerBillsNothing` (the live run asserts no token claim of its own) |
| `TestLiveOpenCodeImagePrompt` | the image prompt is accepted and the turn completes; the configured model answered "I can't view images with this model." The native wire shape (`ContentBlock` `type:"image"`, `"image/png"`) is pinned process-free by `TestOpenCodeImageBlockShape`, because this adapter records no outbound native frames |
| `TestLiveOpenCodeResume` | resume in a new process: same session id, new stream `Seq` starts at 1, no replay frames and no replayed turn events, status `idle`, and the next turn still recalled the previous turn's phrase. The record stream has no outbound request, so the test proves the restored session, not the method name; `open.go` takes `session/resume` because `initialize` advertised `sessionCapabilities.resume` (inferred, not read off the stream) |

What the depth run did **not** exercise: a live steer the runtime refuses, a
live missing-session resume (the refusal is a plain `-32603 Internal error`,
see §6), `session/load` itself, `fork`, `close`, and any `task` subagent.

### 5.2 The v2 run (issue #77, 2026-10-29)

opencode `2.0.26` from the one-off temporary prefix (§1), the same endpoint as
the v1 runs, the same harness, model `workbuddy/deepseek-v4.1-flash`; the line
comes from the binary, not from a second runtime id:

```sh
OAR_ACP_LIVE=1 OAR_ACP_LIVE_OPENCODE_V2=/tmp/opencode-v2/node_modules/.bin/opencode \
  go test ./adapters/acp -run 'TestLiveOpenCodeV2' -v -count=1
```

All three tests **PASS** (28.9 s total, warm data directory):

| test | what it asserts | result |
|---|---|---|
| `TestLiveOpenCodeV2Turn` | the `initialize` answer names the agent (this run: `OpenCode` 2.0.26; the test requires a non-empty name and version and a 2.x version) and `promptCapabilities.image=true` matching `Capabilities().Images`; `session/new` id non-empty; the model in effect is the requested `live/workbuddy/deepseek-v4.1-flash`; a dense `Seq` and, in this run, 9 records; `session/prompt` → the runtime's **own** answer frame ends the turn (`stopReason` in it), exactly one `turn_ended{completed}`; text contains "pong"; status `idle`; `Capabilities().Attribution == nested`; dispose is idempotent (one `dispose` + one `exited`, status `exited`); the provider key appears nowhere in the records. It also pins the concrete type: the open returned `*Session`, **not** an `oarlock.Steerer` | **PASS** (6.75 s) |
| `TestLiveOpenCodeV2Usage` | the v2 wire **does** report usage: the `session/prompt` answer's `result.usage` carries at least `inputTokens`, `outputTokens` and `totalTokens` (the test fails the run if any is missing; `cachedReadTokens` was also present in this run, and a child-agent turn in the nested run carried `thoughtTokens`; `3806+152+75+7680 = 11713 = totalTokens`, the same relation as v1). The v2 profile has **no** `PromptTokens` hook, so the record stream carries **no** token-bearing `EventUsage` (no `Usage/Tokens` at all) and `fold.UsageOf(...).Total` stays `nil` — the runtime's figures are not folded away, and nothing becomes a `0` | **PASS** |
| `TestLiveOpenCodeV2Nested` | the run made a root `tool_call` record for the delegation (this run: `subagent`; the test requires a root tool call, and only the no-link branch pins the tool's name) and the child frames carry `_meta["opencode/child-session"]{id,parentID,depth,title}`; **every** `session_linked` event in the stream has `Parent` = the root session id, a `Child` that is not the root and `Via` = `opencode/child-session`, and records are attributed to that child session (`owned > 0`), so the child's frames are not flattened onto the root; the root still has exactly one `completed` turn end. This run: one child session, 6 marker-carrying child frames = 6 `session_linked` events and 6 child-owned records — the events are per record, not deduplicated, so the count tracks child frames rather than distinct edges (earlier runs of the same code recorded 5 of each, at 22 and 32 records) | **PASS** (16.20 s) |

`nested` is therefore **live-verified** on v2; the fixture
`testdata/opencode-acp-v2-child.json` stays the recorded baseline the offline
projection replays.

The v2 wire, as this run measured it:

- **No extra flag.** `opencode acp` plus the same temporary XDG config as v1
  (`$XDG_CONFIG_HOME/opencode/opencode.json`, `provider.<id>.npm =
  @ai-sdk/openai-compatible`, an `{env:…}` apiKey, `permission.bash: "ask"`)
  works unchanged on 2.0.26; `--standalone` only quiets server logging, and ACP
  does not need it. §3 is the v2 configuration too.
- **`initialize`** answered `agentInfo {"name":"OpenCode","version":"2.0.26"}`,
  `promptCapabilities {image:true, embeddedContext:true}`,
  `sessionCapabilities {additionalDirectories, close, delete, fork, list,
  resume}`, and a new
  `agentCapabilities._meta {"opencode/child-session-updates": true}` — the
  vendor saying it forwards child-session updates. The fixture carries no
  `initialize`, so that key is new evidence from this run.
- **`session/new`** returned `configOptions` for `model` (native
  `provider/model`), `effort` (`category: thought_level`; low, medium, high,
  default) and `mode` (`build`/`plan`, **no** `yolo`);
  `session/set_config_option model=<native id>` took effect — the same path as
  v1.
- **`session/prompt` answer**: `{"stopReason":"end_turn","usage":{…},"_meta":{}}`.
  The turn end is still read off that answer, never synthesized. The
  `usage_update` notification still carries `{used,size,cost{amount,currency}}`
  and describes the context window only.
- **Child frames**: `_meta["opencode/child-session"]{id,parentID,depth,title}`
  rides `tool_call`, `tool_call_update` and `agent_message_chunk`. A child's
  tool call id has the shape `<childSessionID>:<callID>`; the parent call is
  `tool_call name:"subagent"` with `rawInput {agent, description, prompt}`, and
  its result is a `<subagent sessionID="…" state="completed">…</subagent>` text
  block.
- **Child permission requests** arrive as `session/request_permission` with
  `params.sessionId` = the **root**; the child marker sits only in
  `toolCall._meta`. The adapter's `Annotate` runs on notifications, so that
  toApp request record still hangs off the root session. The request was
  **observed** in the nested run; the v2 tests assert neither the option the
  adapter chose nor that the gated child tool ran — that stays v1 evidence.
- **Cold start**: a brand-new data directory made the first `session/new` take
  more than 20 s; warm, a whole turn takes 6–17 s, well inside the 60 s
  `openTimeout`.

What the v2 run did **not** measure — each stays UNVERIFIED, not "absent":

- **steer.** `OpenCodeV2.SteerMeta` is unset, so `open.go` never wraps a v2
  session in `steerSession`, and the run asserts the opened value is `*Session`
  and **not** an `oarlock.Steerer`. Go has no "does not implement" assertion, so
  the static half is only: `session.go` declares `var _ oarlock.Steerer =
  steerSession{}` (the one positive assertion), `open.go` wraps `*Session` in
  `steerSession` **only** when `prof.SteerMeta != nil`, and the v2 profile sets
  none. The conclusion "v2 is not a `Steerer`" is therefore pinned by the
  run-time type assertion in `TestLiveOpenCodeV2Turn` (the fake-side
  `TestOpenCodeLineIsChosenFromTheInstallation` picks the same line), not by the
  compiler: a future edit could give `*Session` a `Steer` method and only a
  review or that assertion would notice. That v2 refuses a concurrent second
  prompt is an existing comment, not re-measured in this run.
- **`session/resume`** (the v1 test owns it; the v2 profile declares no
  `ResumeAbsence`), `session/load`, `close`, `delete`, `fork`.
- **images**: only the `initialize` advertisement (`image:true`, matching the
  capability) was checked — no image was sent on v2.
- **the v2 `session/request_permission` answer path**: a child-scoped request
  was observed in the nested run (§5.2), but neither its answer nor its effect
  is asserted there; the permission ladder remains v1 evidence.
- **one turn, one machine, one model.** A green v2 run says this 2.0.26 build
  with this endpoint and this model did what the table says; it says nothing
  about another model or another 2.x build.

## 6. What this does not prove

- **Attribution tiers, per line.** The v1 line stays `opaque` on source-derived
  evidence (`opencode 1.18.34 acp/event.ts`): the live 1.19.5 depth run was a
  plain turn, a steer and a resume, with no `task` subagent, so it saw no
  child-session frames and neither confirms nor refutes how they are forwarded
  (v1's `task` tool is still unexercised). The v2 line is `nested` on the
  recorded fixture (2.0.26) **and** on the live §5.2 run, which watched the
  vendor's own `opencode/child-session` marker name the child and the stream
  state the edges from the root. That is nested by marker, not by content: the
  run did not compare child text against root text.
- **Images**: the block rides the wire natively and the turn completes (§5.1);
  whether the model can see it depends on the configured provider/model. The
  capability oarlock exposes is the agent's `initialize` advertisement, never a
  guess. On v2 only that advertisement was checked (§5.2): `image:true` matched
  `Capabilities().Images`, and no image was sent.
- **Steer**: live-verified on v1 (§5.1). v2 has no steer and the open returns
  `*Session`, not an `oarlock.Steerer`; that is pinned by the run-time type
  assertion in `TestLiveOpenCodeV2Turn`, not by the compiler — Go cannot state a
  negative interface assertion. The static facts are `session.go`'s one positive
  assertion `var _ oarlock.Steerer = steerSession{}`, `open.go` wrapping
  `*Session` in `steerSession` only when `prof.SteerMeta != nil`, and
  `OpenCodeV2.SteerMeta` being unset; §5.2 spells out what that does and does
  not hold.
- **Resume/load**: the resume path is live-verified (§5.1). `initialize` also
  advertises `loadSession`, but oarlock prefers `session/resume`, so the
  `session/load` branch itself is unexercised here. A **missing** session has no
  unique native code — opencode answers `-32603 "Internal error: OpenCode
  service failure"` with `data.service = "session"` (live). The adapter
  therefore reports `*oarlock.SessionNotFound` only after a **complete**
  `session/list` walk that lacks the id (opencode advertises
  `sessionCapabilities.list`); any list error or incomplete walk leaves the
  runtime's error as-is. The walk is unit-tested; a live missing-id resume is
  the one branch of this run that stays **UNVERIFIED**. Residual risk: if the
  vendor's `session/list` were itself cwd-scoped or truncated while still
  returning no `nextCursor`, a "complete" walk could lack an existing id and
  produce a false `SessionNotFound`; the walk is unfiltered and bounded, but
  that vendor property is not measured. On **v2** the resume/load branch was not
  exercised at all (§5.2).
- **Usage/token totals**: the **v1** line bills the prompt answer's own `usage`
  object (§5.1). There is no session-wide total — the vendor reports per turn,
  so `UsageReport.Total` stays nil, and a turn that reports no `usage` stays nil
  instead of a zero. The **v2** line reports usage on the wire and bills
  **none**: its prompt answer does carry `usage` (measured, §5.2), but the v2
  profile has no `PromptTokens` hook, so no token-bearing `EventUsage` is
  recorded and `fold.UsageOf(...).Total` stays nil. That is a known gap and a
  follow-up item — **not** a runtime that reports nothing, and not a zero.
- **The other five ACP runtimes (grok, kimi, qwen, qoder, antigravity) remain
  UNVERIFIED live**: no binary and no account on this host. Their profiles,
  refusals and attribution claims are untouched by this run; `TestLiveSmoke`
  still skips each of them with that reason.
- **OpenCode v2** (2.x) was installed into a one-off temporary prefix (§1) and
  its live run is §5.2: a turn, a `nested` child session and the gap between the
  reported and the billed usage are measured; steer, images beyond the
  advertisement, `session/resume`, `session/load`, `close`/`delete`/`fork` and
  the permission answer path on v2 stay **UNVERIFIED**.

## 7. Credentials

The live test never writes the key anywhere: the config file holds
`{env:OAR_LIVE_PROVIDER_KEY}`, the value travels in `oarlock.Options.Env` (which
also makes it a redaction candidate), and the test asserts the raw stream does
not contain it. No fixture, log or testdata file records it.
