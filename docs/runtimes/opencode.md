# OpenCode over ACP — the one ACP family member with a live session (issue #43)

Oarlock drives OpenCode as an ACP agent (`opencode acp`), like the other five
members of the family. This page is the live record: what was actually run,
against what, and which claims the run does **not** support. The profile code
is [`adapters/acp/profile_opencode.go`](../../adapters/acp/profile_opencode.go);
the shared transport is [`adapters/acp/open.go`](../../adapters/acp/open.go) and
[`session.go`](../../adapters/acp/session.go).

Status: **live session verified** on 2026-10-29 — opencode `1.19.5` (the v1
release line), a third-party OpenAI-compatible endpoint, no vendor account.
Everything else in this page is marked UNVERIFIED where it is unverified.

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
| turn end | the runtime's own `session/prompt` **answer**: `{"stopReason":"end_turn","usage":{…}}`. Nothing is synthesized: `turn_ended{completed}` is read off that answer frame. |
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
- The prompt answer carries a `usage` object, but the OpenCode profile has no
  `PromptTokens`/`PromptContext` hook, so no token ledger is projected for this
  runtime (consistent with the ❌ in
  [`docs/compat-matrix.md`](../compat-matrix.md)).

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

## 6. What this does not prove

- **Attribution tiers are unchanged.** The v1 line stays `opaque` on
  source-derived evidence (`opencode 1.18.34 acp/event.ts`): a plain prompt
  turn has no `task` subagent, so it cannot prove how child-session frames are
  forwarded. The v2 line stays `nested` on the recorded fixture. No tier is
  upgraded by this live run.
- **Images**: `image: true` was advertised and is reported by
  `Capabilities().Images`, but no image prompt was sent live.
- **Steer**: the v1 profile declares a steering `_meta`; the live session did
  not steer. **UNVERIFIED**.
- **Resume/load/fork/close**: advertised by the agent, not exercised live.
- **Usage/token totals** for this runtime: no projection hook, so absent — a
  gap, not a live result.
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
