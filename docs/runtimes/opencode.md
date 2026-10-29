# OpenCode over ACP — the one ACP family member with a live session (#43, #55)

Oarlock drives OpenCode as an ACP agent (`opencode acp`), like the other five
members of the family. This page is the live record: what was actually run,
against what, and which claims the run does **not** support. The profile code
is [`adapters/acp/profile_opencode.go`](../../adapters/acp/profile_opencode.go);
the shared transport is [`adapters/acp/open.go`](../../adapters/acp/open.go) and
[`session.go`](../../adapters/acp/session.go).

Status: **live session verified** on 2026-10-29 — opencode `1.19.5` (the v1
release line), a third-party OpenAI-compatible endpoint, no vendor account.
Issue #55 added a second live run on the same installation for the depth
claims: usage ledger, `session/resume`, steer and images (§5.1). Everything
else in this page is marked UNVERIFIED where it is unverified.

## 1. Installation used

```sh
npm i -g opencode-ai   # 2026-10-29, node 22.23.1, linux/amd64 (installs 1.19.5)
opencode --version            # 1.19.5
```

OpenCode ships two release lines that do not install side by side: v1
(`opencode-ai`, 1.x) and v2 (`@opencode/cli`, 2.x). The profile picks the line
from `Installation.Version` (v1 when unknown). Only **v1** was exercised here;
every v2 claim in the profile still rests on the recorded fixture
`adapters/acp/testdata/opencode-acp-v2-child.json` (opencode 2.0.26), not on a
live v2 run.

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
`opencode` on `PATH`). Without `OAR_ACP_LIVE=1`, or without the base URL/key,
or without the binary, every test **skips with the reason** — a skipped run is
`NOT VERIFIED`, never a pass.

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

## 6. What this does not prove

- **Attribution tiers are unchanged.** The v1 line stays `opaque` on
  source-derived evidence (`opencode 1.18.34 acp/event.ts`): the live 1.19.5
  depth run was a plain turn, a steer and a resume, with no `task` subagent, so
  it saw no child-session frames and neither confirms nor refutes how they are
  forwarded. The v2 line stays `nested` on the recorded fixture (2.0.26). No
  tier is upgraded by either live run.
- **Images**: the block rides the wire natively and the turn completes (§5.1);
  whether the model can see it depends on the configured provider/model. The
  capability oarlock exposes is the agent's `initialize` advertisement, never a
  guess.
- **Steer**: live-verified on v1 (§5.1). v2 is **unsupported at compile time**:
  its profile has no `SteerMeta`, so a v2 session does not satisfy
  `oarlock.Steerer` (concurrent native prompts are refused in v2).
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
  that vendor property is not measured.
- **Usage/token totals** are projected from the prompt answer's own `usage`
  object for the **v1** line (§5.1). There is no session-wide total: the vendor
  reports per turn, so `UsageReport.Total` stays nil. A turn that reports no
  `usage` stays nil — never a zero. The v2 line has no usage hook and no
  `ResumeAbsence`: it was never installed live and its fixture carries no
  `usage` object, so nothing is claimed for it.
- **The other five ACP runtimes (grok, kimi, qwen, qoder, antigravity) remain
  UNVERIFIED live**: no binary and no account on this host. Their profiles,
  refusals and attribution claims are untouched by this run; `TestLiveSmoke`
  still skips each of them with that reason.
- **OpenCode v2** (2.x) is not installed here; its claims rest on the recorded
  fixture only.

## 7. Credentials

The live test never writes the key anywhere: the config file holds
`{env:OAR_LIVE_PROVIDER_KEY}`, the value travels in `oarlock.Options.Env` (which
also makes it a redaction candidate), and the test asserts the raw stream does
not contain it. No fixture, log or testdata file records it.
