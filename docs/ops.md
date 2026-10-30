# Operations: install, login, account usage, update, inventory

**Status:** issues [#23](../issues/0023-operations.md),
[#69](../issues/0069-ops-breadth.md) and
[#89](../issues/0089-account-usage.md). Types:
[`runtime.go`](../runtime.go) (`Installer`, `LoginProvider`,
`AccountUsageReader`, `Upgrader`, `InventoryReader`, `ModelLister`,
`InstallationProbe`). Runner: [`internal/ops`](../internal/ops/ops.go).
Capability model: [architecture.md §2](architecture.md#2-capability-model).

Sessions became usable in #19–#22; what is left is the "dirty work" around
them: is the runtime installed, can it sign in, what does the account have,
is there an update, what can it run. Every runtime answers those through the
same shape, and every capability answers one of two questions: **what would
happen** (read-only) or **do it** (explicit, changes the machine).

Two rules run through the whole surface:

- **Nothing is faked.** An operation this adapter cannot perform is either an
  absent interface (the runtime has no such thing) or a typed
  `*oarlock.UnsupportedOperation` (the runtime has it, this adapter cannot do
  it). Neither is ever a success.
- **`internal/ops` is the only place that starts a vendor CLI for an
  operating capability.** It runs everything through `internal/exec`, so the
  whole process tree is owned and killed on timeout; stdin is always an
  empty reader (`strings.NewReader("")`), so a CLI that prompts gets EOF
  instead of blocking the host; output is never forwarded to a log. Nothing
  in this path writes oarlock configuration — that is what makes install and
  upgrade retryable and a failure harmless.

## 1. The shared shape

### Probe — read-only

`InstallationProbe.Installation` answers "is it installed, which version,
where from" with **no account and no network I/O**. A runtime without a probe
is not verified as installed, and the zero value says so. "Present but
broken" is `Available: true` **plus** the read error, so a host can tell it
apart from "absent". The ACP family probes `LookPath` first and then the
vendor's known install path, still with no process start and no version read
(opencode's v1/v2 selection depends on `Installation.Version` staying empty);
the others read `--version`.

### `InstallPlan` (read-only) vs `Install` (explicit, changes the machine)

- `InstallPlan` is pure: it returns the argv that would run, plus
  `ChangesMachine: true` and the reason. It executes nothing. For a vendor
  whose installer is a shell line the plan is exactly
  `["sh", "-c", <line>]`. A runtime that ships parallel release lines does
  not get a line chosen for it: `InstallRequest.Line` names one, and an empty
  or unknown line is a typed refusal with nothing run (§5).
- `Install` is the only method that executes it. It probes first (already
  available ⇒ no execution at all), runs the line once with no stdin under a
  10 minute bound, then **re-probes**. The probe, not the installer's exit
  code, decides success.

### `Login` / `Logout` / `AuthStatus` — vendor CLI, challenge returned without stdin

Login is the vendor's own flow, relayed: `Login` starts the vendor CLI,
captures the first challenge line (the URL today) into `LoginChallenge{Kind,
URL, Code, Instructions}`, and **stops the helper**. It never writes stdin,
never waits for a prompt, and never claims the sign-in completed —
`AuthStatus` is the only completion source. **Tokens never pass through
oarlock**: it holds no credential, writes none, and the vendor CLI keeps its
own store. `Logout` runs the vendor's own sign-out.

### `CheckUpdate` vs `Upgrade` — vendor updater, success only if the version changed

- `CheckUpdate` is read-only. Where this adapter has no check-only read — no
  local command to run, or the vendor's check is a release lookup rather
  than a local command — it is a typed `*UnsupportedOperation`, not a guess.
- `Upgrade` reads `--version` before, runs the vendor's updater, and reads
  `--version` again. **The version read-back decides**: an unchanged version
  is an explicit error (`ops.ErrVersionUnchanged`), even when the updater
  exited 0; a changed version is success even when the updater exited
  non-zero. The vendor's exit code is not evidence about the machine.

### `AccountUsage` — two readers, both without a live run, never faked

`AccountUsageReader` is present for claude and codex since issue
[#89](../issues/0089-account-usage.md), and nil for pi, cursor and the ACP
family. A window reports the vendor's own percentage (`Used`, `Limit` 100,
`Unit` "percent") and an optional UTC `ResetsAt`; no absolute quota, no plan
and no reset time is invented from anything else.

| runtime | native source | what this adapter reads |
|---|---|---|
| claude | `api.anthropic.com/api/oauth/usage` (OAR's pre-0.3.1 baseline; OAR itself moved to the control channel's `get_usage` in 0.3.1) | the endpoint over stdlib HTTP, bearer token from `~/.claude/.credentials.json` (`claudeAiOauth.accessToken`) or `OARLOCK_CLAUDE_OAUTH_TOKEN`, plan from `claudeAiOauth.subscriptionType` |
| codex | `account/rateLimits/read` on `codex app-server` (there is no HTTP usage endpoint: no `codex usage` subcommand, and the `wham/*` backend paths carry no usage route) | one throwaway `codex app-server --listen stdio://`: `initialize` → `initialized` → optional `account/read` → `account/rateLimits/read` |

A failed read is a typed `*ops.UsageError`, never an empty snapshot. `Kind` is
the callers' branch; `Reason` is the stable code (`internal/ops/usage.go` is
the authority, and a test pins every spelling):

| outcome | kind / reason |
|---|---|
| 401 or 403 | `reauth_required` / `credentials_rejected` |
| no usable persisted credential and no env token | `reauth_required` / `credentials_missing` |
| the vendor says this machine is signed out | `reauth_required` / `not_authenticated` |
| an account whose mode has no subscription quota: codex `account.type` apiKey or amazonBedrock, or a reply stating `requiresOpenaiAuth: false` | `unsupported` / `unsupported_auth_mode` |
| the endpoint or method does not exist | `unsupported` / `endpoint_unavailable` |
| the answer carries no quota configuration | `malformed` / `quota_unavailable` |
| the provider rate-limited the usage read itself | `rate_limited` / `request_rejected` |
| the context bound expired | `timeout` |
| any other non-2xx, a network failure, or a dead child process | `transport` |

None of these carry a token, a credential path or a provider body; the
credential type redacts itself in `%v`/`%+v`/`%#v`/JSON.

`unsupported_auth_mode` matters for codex: a machine signed in with an API key
gets `"ChatGPT authentication required"` from the rate-limit endpoint even
though codex is correctly authenticated, so that configuration is classified as
*no subscription quota available* — a `reauth` error there would send the host
after a login that cannot fix it. `request_rejected` is oarlock's own code
(OAR has no 429 mapping) and is documented here so a caller can match it.

**UNVERIFIED**: both readers have fake-transport tests only (an `httptest`
server for claude, a fake `codex app-server` script for codex). This host is
not signed in to either vendor, and no real quota answer has ever been parsed
by oarlock. The claude endpoint's field names come from OAR's HTTP-era
projector and the installed binary; treat the live shape as unconfirmed.

Three gaps worth knowing: claude's HTTP payload has no model-scoped weekly
bucket (the portable `model_scoped[]` shape appears only in the native reply);
a window above 100% is dropped rather than clamped to 100 (OAR clamps); and a
body that answers with valid JSON but no quota is `malformed/quota_unavailable`
where OAR reports `unsupported/quota_unavailable` — the same information under
a different bucket, because "the endpoint answered garbage" and "there is no
quota" are not distinguishable on the wire.

### `Inventory` — per-section status

`Inventory` is four sections (models, skills, commands, tools), each with its
own `InventoryStatus` (`ok`, `unsupported`, `error`), so one unreadable
section never erases the others. A section a runtime cannot answer is
`unsupported` **with a reason** — never an empty list that reads like "none".

## 2. Do / not do per runtime

Legend: ✅ implemented · 🟡 partial, or present as an explicit typed refusal
(`*oarlock.UnsupportedOperation`) instead of an answer · ❌ absent (the
`Runtime` field is nil; the runtime has no such operation) · ⚠️ the reader
exists but every section is explicitly `unsupported`. **UNVERIFIED** means
the only evidence is a fake — a fake process or a fake HTTP server; no real
vendor answer was ever collected.

| capability | pi | claude | codex | cursor | ACP family |
|---|---|---|---|---|---|
| installation probe | ✅ `pi --version`, live | ✅ `claude --version`, fallback `~/.local/bin/claude`, UNVERIFIED | ✅ `codex --version`, UNVERIFIED | ✅ `cursor-agent --version`, live | ✅ `LookPath` + known path, no process |
| models | ✅ `pi --list-models`, live | ✅ `list_models` control request, fake-process only, UNVERIFIED | ✅ app-server `model/list`, fake app-server only, UNVERIFIED | ✅ `cursor-agent models`, live | ✅ `session/new` `configOptions` (grok: `_x.ai/models/list` first), fake agent only, UNVERIFIED |
| install plan | ❌ absent (bundled) | ✅ pure plan, UNVERIFIED | ✅ pure plan, UNVERIFIED | ❌ absent (the host installs the CLI) | 🟡 five profiles pure plan, UNVERIFIED; ❌ antigravity (no installer) |
| install | ❌ absent | ✅ re-probe decides success, UNVERIFIED | ✅ re-probe decides success, UNVERIFIED | ❌ absent | 🟡 the same five, re-probe decides success, UNVERIFIED; ❌ antigravity |
| login challenge | ❌ absent | ✅ URL relay, helper stopped, UNVERIFIED | 🟡 typed `UnsupportedOperation` (device code needs the app-server) | ✅ URL relay, helper stopped, UNVERIFIED (fake-process only, never live) | 🟡 opencode typed `UnsupportedOperation` (interactive TUI); five profiles absent |
| logout | ❌ absent | ✅ own CLI, UNVERIFIED | ✅ `codex logout`, UNVERIFIED | ✅ `cursor-agent logout`, UNVERIFIED | 🟡 opencode typed `UnsupportedOperation` (stdin prompt); five profiles absent |
| auth status | ❌ absent | ✅ own CLI, UNVERIFIED | ✅ `codex login status`, UNVERIFIED | ✅ `cursor-agent status --format json`, live | 🟡 opencode `opencode auth list`, live (0 credentials); five profiles absent |
| account usage | ❌ absent | ✅ `api.anthropic.com/api/oauth/usage`, UNVERIFIED | ✅ app-server `account/rateLimits/read`, UNVERIFIED | ❌ absent | ❌ absent |
| check update | ❌ absent (no `Upgrader`) | 🟡 typed `UnsupportedOperation` (`update` is check+install, no `--check`) | ✅ `codex doctor --json` `updates.status`; live shape, UNVERIFIED verdict | 🟡 typed `UnsupportedOperation` (no check-only sibling), UNVERIFIED (no binary here) | ❌ absent |
| upgrade | ❌ absent (exact 1.4.0 gate) | ✅ version read-back, UNVERIFIED | ✅ version read-back, UNVERIFIED | ✅ version read-back, UNVERIFIED | ❌ absent |
| inventory | 🟡 models/skills/commands; tools `unsupported`, live | ⚠️ 4 sections `unsupported`, UNVERIFIED | ⚠️ 4 sections `unsupported`, UNVERIFIED | 🟡 models only; 3 sections `unsupported`, UNVERIFIED | ⚠️ 4 sections `unsupported`, per profile |

Notes on individual cells:

- **pi** has no `Installer`, `LoginProvider`, `AccountUsageReader` or
  `Upgrader`, by decision: Pi ships with its host, the adapter pins Pi
  **1.4.0 exactly**, so a self-update would break its own version gate, and
  Pi's auth surface needs a provider/model and can print secrets. `pi
  --list-models` is the only vendor command behind its models read; the
  inventory reads skills/commands locally.
- **claude** and **codex** implement all four machine-changing calls plus
  probe, logout and auth status. `Login` is where they differ: claude relays
  the URL, codex refuses with a typed error because its device-code flow
  needs the app-server, which this adapter does not drive for login.
- **cursor** installs its CLI on the host (so no installer), but has login,
  logout, status, models and its own updater.
- **ACP family** (grok, kimi, opencode, qwen, qoder, antigravity) shares the
  transport and one inventory that declares its four sections `unsupported`;
  what differs per profile is the vendor's own install line (issue
  [#69](../issues/0069-ops-breadth.md)). grok, kimi, qwen and qoder plan their
  official script and re-probe after `Install`; opencode ships **two** npm
  lines (`v1`: `npm install -g opencode-ai`, `v2`:
  `npm install -g @opencode/cli`) that oarlock never chooses — an empty or
  unknown `InstallRequest.Line` is a typed `*oarlock.UnsupportedOperation` and
  nothing runs, a named line runs that line only. antigravity has **no
  installer**: OAR's `requires_gui` — the runtime is installed from an
  editor's agent registry, so `Install` stays nil instead of a
  refusal-shaped fake. The plans are pure and **UNVERIFIED**: no ACP
  installer was ever run.
- **opencode is the only ACP profile with an auth surface.** `AuthStatus` is
  `opencode auth list` (read-only; live on 1.19.5 with `0 credentials` and
  exit 0, while the `N > 0` parse is a fake-store test). `Login` and `Logout`
  are typed `*oarlock.UnsupportedOperation`s because both are interactive:
  `auth login` needs provider selection on stdin and `auth logout` asks for a
  provider when it is not named, and oarlock relays no prompt. Every other ACP
  profile keeps `Login`/`Logout`/`AuthStatus` nil — no verified channel, so
  none is guessed.
- **codex check-update is the one check that runs a command.** `codex doctor
  --json` carries the verdict in row `updates.status`; the JSON decides, never
  doctor's exit code (live on codex-cli 0.165.0: exit **1** with valid JSON).
  claude and cursor stay typed refusals: `claude update` is check and install
  in one step (`update --help` on 2.1.312 lists no `--check`) and its native
  check is an HTTP release lookup, not a CLI command; `cursor-agent update`
  has no check-only sibling.

## 3. The exact commands behind each call

These are the exact commands, and they exist only behind the named call. All
but the last change the machine:

- **claude — `Install`**: `curl -fsSL https://claude.ai/install.sh | bash` —
  run as `["sh", "-c", <line>]`; `InstallPlan` only names this line.
- **claude — `Upgrade`**: `claude update`.
- **codex — `Install`**:
  `curl -fsSL https://chatgpt.com/codex/install.sh | CODEX_NON_INTERACTIVE=1 sh`
  — run as `["sh", "-c", <line>]`.
- **codex — `Upgrade`**: `codex update`.
- **grok — `Install`**: `curl -fsSL https://x.ai/cli/install.sh | bash` — run
  as `["sh", "-c", <line>]`.
- **kimi — `Install`**:
  `curl -fsSL https://code.kimi.com/kimi-code/install.sh | bash` — run as
  `["sh", "-c", <line>]`.
- **opencode — `Install`**: `npm install -g opencode-ai` for line `v1`,
  `npm install -g @opencode/cli` for line `v2` — run as
  `["sh", "-c", <line>]`; `InstallRequest.Line` picks one and nothing else
  does (§5).
- **qwen — `Install`**: `curl -fsSL https://qwen-code-assets.oss-cn-hangzhou.aliyuncs.com/installation/install-qwen-standalone.sh | bash -s -- --method standalone --no-modify-path` — run as `["sh", "-c", <line>]`.
- **qoder — `Install`**: `curl -fsSL https://qoder.com/install | bash -s -- --skip-path` — run as `["sh", "-c", <line>]`.
- **cursor — `Login`**: `cursor-agent login` (writes the machine's auth
  state).
- **cursor — `Logout`**: `cursor-agent logout` (clears that auth state).
- **cursor — `Upgrade`**: `cursor-agent update`.
- **codex — `CheckUpdate`**: `codex doctor --json` — the one update check that
  runs a command, and it **reads only**: the `updates.status` row carries the
  verdict and the JSON decides even when doctor exits 1 (§5).

Nothing else in this surface touches the machine. **No probe, plan, status,
model list or inventory call ever runs an install line or an updater**; an
installer is never reached indirectly, and a `curl | sh` line is only ever the
plan's payload until `Install` is called. `codex doctor --json` reads and
changes nothing.

## 4. Explicit opt-in

Machine-changing calls — a host must ask the user before running any of
these:

| call | changes |
|---|---|
| `Install` | installs the vendor CLI |
| `Upgrade` | replaces the installed CLI |
| `Login` | the machine's vendor auth state |
| `Logout` | the same, by clearing it |

Read-only calls, safe to run for a status page or a pre-flight check:

| call | answers |
|---|---|
| `Installation` (probe) | installed? which version? from where? |
| `ListModels` | what can this runtime run now |
| `InstallPlan` | what would `Install` execute, and why (pure for every ACP profile that has one; antigravity has none) |
| `CheckUpdate` | is a newer version reported (codex runs `codex doctor --json`; claude and cursor are typed refusals) |
| `AuthStatus` | is this machine signed in |
| `Inventory` | models/skills/commands/tools, per section |

The split is structural, not polite: the read-only calls do not receive an
`Installer`/`Upgrader`/`LoginProvider` action, and the machine-changing calls
are separate methods that a host has to name.

## 5. Failure semantics

- **An installer is judged by a re-probe.** `curl ... | sh` can exit 0 after a
  failed download, so the exit code is only echoed into the error, never
  trusted: if the re-probe still finds no installation, `Install` returns the
  `InstallResult` (with the before/after `Installation`) **and** an error.
  An already-installed runtime short-circuits: `Install` returns without
  executing anything. A failed install writes no oarlock config, so a second
  attempt is a clean retry.
- **An unnamed install line is a refusal, not a guess.** opencode ships two
  lines, so `InstallRequest.Line` must name one: `""` is a typed
  `*oarlock.UnsupportedOperation` whose reason lists `v1` and `v2`, any other
  value is the same type with `unknown install line "<x>": opencode installs
  v1 or v2`, and neither executes anything. Naming a line on a profile that
  ships one (`grok`, `kimi`, `qwen`, `qoder`, `claude`, `codex`) is refused
  the same way (`ops.Installer`: this runtime ships one release line). The
  line selects argv and nothing else; oarlock never picks one for the host.
- **An upgrade is judged by the version re-read.** Same version ⇒
  `ops.ErrVersionUnchanged` (an explicit error, `errors.Is`-testable), even
  when the updater exited 0. Changed version ⇒ success, even when the
  updater's own exit was non-zero.
- **The login helper is stopped after the challenge.** `Login` kills the
  process tree as soon as the URL line is seen, so a vendor CLI left waiting
  for a browser cannot outlive the call. The returned `LoginChallenge` is
  `Kind: "url"` today; it is a *challenge*, not a completion claim.
- **opencode auth status: an unreadable answer is an error, not "signed
  out".** `opencode auth list` prints an ANSI box; the adapter strips the CSI
  sequences and requires exactly one `N credentials` line. `N > 0` is
  `Authenticated: true` with a `Detail` naming the providers (or the count),
  `N == 0` is `Authenticated: false`, and no count line, a non-zero exit or
  unparsable output is an **error** — "signed out" is only reported when the
  vendor said zero.
- **codex doctor: the JSON decides, not the exit code.** `codex doctor` exits
  1 whenever any row fails, so a valid `checks["updates.status"]` object in
  stdout still answers. No JSON in stdout is an error, a missing `latest
  version` is an error naming the `latest version probe` detail (live:
  `HTTP 403 Forbidden`), and `update action == "manual or unknown"` is an
  error — codex itself says it cannot update this copy.
- **No stdin, ever.** Every vendor command runs with an empty reader as
  stdin; a prompt receives EOF instead of a host process blocked on a pipe.
  Bounds: 15 s for a read-only command, 10 min for an install, 10 min for an
  upgrade, 2 min as the login-capture backstop (the helper is normally
  stopped much earlier).
- **Typed errors where the operation exists but this adapter cannot do it.**
  `*oarlock.UnsupportedOperation{Operation, Reason}` — branch with
  `errors.As`, never on prose. Used for codex login (device-code flow needs
  the app-server), opencode login/logout (both are interactive prompts),
  `CheckUpdate` on claude (`update` is check and install in one step; the
  native check is an HTTP release lookup) and cursor (no check-only sibling),
  and for an install line that is unnamed or unknown. It is never used to
  claim success.
- **Absent interface where the runtime has none.** pi leaves every field nil,
  and no ACP profile declares an `Upgrader`: a profile has an `Installer`
  exactly where the vendor documents one (all of them but antigravity) and a
  `LoginProvider` only where a channel is verified (opencode). Nil is the
  presence check; there is no `SupportsInstall()` beside it that could
  disagree with the behaviour.
- **Partial failure keeps its own state.** An `Inventory` returns every
  section with its own status, so a broken reader is one `error` section, not
  a failed call. `Install`/`Upgrade` failures return whatever they learned
  (installation, from/to version, output tail) alongside the error.

## 6. Verification status

What is actually proven, and what is only proven against a fake process:

| claim | evidence |
|---|---|
| pi probe, models, inventory | live-verified (`OAR_PI_LIVE=1`) |
| cursor probe, models, auth status | live-verified (`OAR_CURSOR_LIVE=1`, on a host that is logged in) |
| claude probe/install/login/logout/status/upgrade, codex ditto | **UNVERIFIED** — fake-process tests only; none of those flows was ever run against a real binary |
| every real install and upgrade (claude, codex, cursor) | **UNVERIFIED** — no installer or updater was ever run |
| ACP install plan (grok, kimi, opencode, qwen, qoder) | **plan path live, installer UNVERIFIED** — a read-only live run of all six `Runtime` values on a host with opencode 1.19.5 printed every plan and executed nothing: opencode's probe answered `available=true …/bin/opencode`, `v1` was `["sh","-c","npm install -g opencode-ai"]`, `v2` `["sh","-c","npm install -g @opencode/cli"]` and `v3` the typed `unknown install line` refusal. The other four profiles printed their vendor line and antigravity nothing (`Install` nil). A fake-process test pins that a plan runs nothing (a fake shell that would leave a marker file is never invoked). **No ACP installer was ever run** |
| ACP install, login, logout | nothing live to run: antigravity has no installer, and the five profiles other than opencode keep `Login`/`Logout` nil; opencode's two refusals are tested to start no process |
| opencode auth status | **live read** on opencode 1.19.5: `opencode auth list` → `0 credentials`, exit 0. The `N > 0` parse (two providers named) is a fake-store test, not a live account |
| codex check update | **live shape**: codex-cli 0.165.0 `codex doctor --json` exits **1** with valid JSON and no `latest version` (row detail `HTTP 403 Forbidden`), which is the error path. The newer/not-older verdicts are fake-process only |
| claude and cursor check update | claude's refusal reason is live-checked (`claude update --help` on 2.1.312 lists no `--check`); cursor's is **UNVERIFIED** — `cursor-agent` was not installed on the host where this was written |
| cursor login URL capture | fake-process only; `cursor-agent login` was **never** run live (the live test is gated to status + models) |
| ACP inventory refusal | fake-process/profile test only |
| account usage | **UNVERIFIED** for both readers — claude's HTTP quota endpoint and codex's app-server `account/rateLimits/read` were exercised only against fake transports (`httptest`, a fake `codex app-server` script); no signed-in account was ever read, so the live payload shape, the plan field and every reset time are unconfirmed |

The `internal/ops` rules themselves (plan does not execute, install runs once
and the second call no-ops, unchanged version is an error, a challenge is
captured without waiting on stdin, a timeout is an error) are covered by
fake-executable tests, POSIX-only — so **there is no Windows live
verification** for any of this.

## 7. Known gaps

- **claude, codex and the ACP family models are fake-tested only** — issue #98
  wired `ListModels` for all eight runtimes (`claude list_models`, codex app-server
  `model/list`, ACP `session/new` `configOptions`), but none of them has been
  read against a signed-in account: every branch runs against a fake
  executable or the in-process fake agent. A signed-out account is a typed
  `ErrNotAuthenticated` and a build without the surface is an
  `UnsupportedOperation`, never an empty list.
- **claude, codex and ACP inventories are explicitly unsupported**: the
  reader exists and declares four `unsupported` sections rather than
  returning an empty list.
- **Account usage exists for claude and codex only, and neither has run
  live.** claude reads the vendor's HTTP quota endpoint with a token from
  Claude's own credential document; codex reads `account/rateLimits/read`
  from a short-lived app-server. Both are **UNVERIFIED** against a real
  account (fake transports only), both are typed-error rather than
  empty-snapshot on failure, and neither is inferred from session usage. pi,
  cursor and the ACP family stay nil: they expose no quota surface this
  adapter can read.
- **claude and cursor check-update stay typed refusals**: `claude update` is
  check and install in one step (no `--check`) and its native channel read is
  an HTTP release lookup, not a CLI command; `cursor-agent update` has no
  check-only sibling. codex is the one wired check (`codex doctor --json`),
  but where the `latest version` row is missing it answers with that error,
  never with "up to date".
- **No ACP installer was ever run.** grok, kimi, opencode, qwen and qoder have
  pure plans over the vendor's documented line; only the argv, the
  `ChangesMachine` flag and "nothing executes" are tested.
- **antigravity has no installer** — OAR's `requires_gui`: the runtime is
  installed from an editor's agent registry, so `Install` is nil rather than a
  refusal-shaped fake. It has no login either.
- **opencode login and logout are not driven**: `opencode auth login` is an
  interactive TUI that needs provider selection on stdin, and `opencode auth
  logout` asks for a provider when it is not named, so both are typed
  `*oarlock.UnsupportedOperation` and start nothing; `opencode auth list` is
  the only auth call that runs.
- **No ACP login or status beyond opencode**: grok, kimi, qwen, qoder and
  antigravity keep `Login`/`Logout`/`AuthStatus` nil — no verified channel, so
  none is guessed.
- **codex login is not driven**: the device-code flow needs the app-server,
  so login is a typed refusal while logout and auth status work.
- **pi login/usage/upgrade are absent** — the exact `1.4.0` version gate is
  the reason an upgrade cannot exist, and `pi auth` needs a provider/model
  and can print secrets.
- **No Windows live verification**: the fake-executable tests are POSIX
  scripts that skip on Windows.
- **No sudo/privilege handling**: an installer that needs elevation is run as
  the current user and fails as the vendor's script fails; oarlock never
  escalates.
