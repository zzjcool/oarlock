# Operations: install, login, account usage, update, inventory

**Status:** issue [#23](../issues/0023-operations.md). Types:
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
apart from "absent". The ACP family probes by `LookPath` only (no version
read); the others read `--version`.

### `InstallPlan` (read-only) vs `Install` (explicit, changes the machine)

- `InstallPlan` is pure: it returns the argv that would run, plus
  `ChangesMachine: true` and the reason. It executes nothing. For a vendor
  whose installer is a shell line the plan is exactly
  `["sh", "-c", <line>]`.
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

### `AccountUsage` — absent everywhere in Go today, never faked

`AccountUsageReader` is a nil field for every runtime in scope. No plan, no
quota window and no reset time is invented from anything else. This is a
documented gap (§7), not an accident of code.

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
the only evidence is a fake-process test — no real vendor binary ran.

| capability | pi | claude | codex | cursor | ACP family |
|---|---|---|---|---|---|
| installation probe | ✅ `pi --version`, live | ✅ `claude --version`, fallback `~/.local/bin/claude`, UNVERIFIED | ✅ `codex --version`, UNVERIFIED | ✅ `cursor-agent --version`, live | ✅ `LookPath` only |
| models | ✅ `pi --list-models`, live | ❌ absent | ❌ absent | ✅ `cursor-agent models`, live | ❌ absent |
| install plan | ❌ absent (bundled) | ✅ pure plan, UNVERIFIED | ✅ pure plan, UNVERIFIED | ❌ absent (the host installs the CLI) | ❌ absent |
| install | ❌ absent | ✅ re-probe decides success, UNVERIFIED | ✅ re-probe decides success, UNVERIFIED | ❌ absent | ❌ absent |
| login challenge | ❌ absent | ✅ URL relay, helper stopped, UNVERIFIED | 🟡 typed `UnsupportedOperation` (device code needs the app-server) | ✅ URL relay, helper stopped, UNVERIFIED (fake-process only, never live) | ❌ absent |
| logout | ❌ absent | ✅ own CLI, UNVERIFIED | ✅ `codex logout`, UNVERIFIED | ✅ `cursor-agent logout`, UNVERIFIED | ❌ absent |
| auth status | ❌ absent | ✅ own CLI, UNVERIFIED | ✅ `codex login status`, UNVERIFIED | ✅ `cursor-agent status --format json`, live | ❌ absent |
| account usage | ❌ absent | ❌ absent | ❌ absent | ❌ absent | ❌ absent |
| check update | ❌ absent (no `Upgrader`) | 🟡 typed `UnsupportedOperation` (not wired), UNVERIFIED | 🟡 typed `UnsupportedOperation` (not wired), UNVERIFIED | 🟡 typed `UnsupportedOperation` (not wired), UNVERIFIED | ❌ absent |
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
- **ACP family** (grok, kimi, opencode, qwen, qoder, antigravity) shares one
  profile: probe and an inventory that declares its four sections
  `unsupported`. Everything else is absent through the profile — one
  implementation, not six.

## 3. Commands that change the machine

These are the exact commands, and they exist only behind the named call:

- **claude — `Install`**: `curl -fsSL https://claude.ai/install.sh | bash` —
  run as `["sh", "-c", <line>]`; `InstallPlan` only names this line.
- **claude — `Upgrade`**: `claude update`.
- **codex — `Install`**:
  `curl -fsSL https://chatgpt.com/codex/install.sh | CODEX_NON_INTERACTIVE=1 sh`
  — run as `["sh", "-c", <line>]`.
- **codex — `Upgrade`**: `codex update`.
- **cursor — `Login`**: `cursor-agent login` (writes the machine's auth
  state).
- **cursor — `Logout`**: `cursor-agent logout` (clears that auth state).
- **cursor — `Upgrade`**: `cursor-agent update`.

Nothing else in this surface touches the machine. **No probe, plan, status,
model list, update check or inventory call ever runs one of these commands**;
an installer is never reached indirectly, and a `curl | sh` line is only ever
the plan's payload until `Install` is called.

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
| `InstallPlan` | what would `Install` execute, and why |
| `CheckUpdate` | is a newer version reported |
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
- **An upgrade is judged by the version re-read.** Same version ⇒
  `ops.ErrVersionUnchanged` (an explicit error, `errors.Is`-testable), even
  when the updater exited 0. Changed version ⇒ success, even when the
  updater's own exit was non-zero.
- **The login helper is stopped after the challenge.** `Login` kills the
  process tree as soon as the URL line is seen, so a vendor CLI left waiting
  for a browser cannot outlive the call. The returned `LoginChallenge` is
  `Kind: "url"` today; it is a *challenge*, not a completion claim.
- **No stdin, ever.** Every vendor command runs with an empty reader as
  stdin; a prompt receives EOF instead of a host process blocked on a pipe.
  Bounds: 15 s for a read-only command, 10 min for an install, 10 min for an
  upgrade, 2 min as the login-capture backstop (the helper is normally
  stopped much earlier).
- **Typed errors where the operation exists but this adapter cannot do it.**
  `*oarlock.UnsupportedOperation{Operation, Reason}` — branch with
  `errors.As`, never on prose. Used for codex login (device-code flow needs
  the app-server) and for `CheckUpdate` on claude/codex/cursor, which is not
  wired: there is no check-only command, or the vendor's check is an HTTP
  release lookup rather than a local command. It is never used to claim
  success.
- **Absent interface where the runtime has none.** pi and the ACP family
  leave the field nil. Nil is the presence check; there is no
  `SupportsInstall()` beside it that could disagree with the behaviour.
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
| claude probe/install/login/logout/status/upgrade, codex ditto | **UNVERIFIED** — fake-process tests only; neither binary exists on the verification host |
| every real install and upgrade (claude, codex, cursor) | **UNVERIFIED** — no installer or updater was ever run |
| cursor login URL capture | fake-process only; `cursor-agent login` was **never** run live (the live test is gated to status + models) |
| ACP inventory refusal | fake-process/profile test only |
| account usage | absent for every runtime, so nothing to verify |

The `internal/ops` rules themselves (plan does not execute, install runs once
and the second call no-ops, unchanged version is an error, a challenge is
captured without waiting on stdin, a timeout is an error) are covered by
fake-executable tests, POSIX-only — so **there is no Windows live
verification** for any of this.

## 7. Known gaps

- **claude and codex models are not wired** — `ListModels` is absent for
  both; neither adapter has a model-listing surface yet.
- **claude, codex and ACP inventories are explicitly unsupported**: the
  reader exists and declares four `unsupported` sections rather than
  returning an empty list.
- **Account usage is absent everywhere.** No plan/quota surface exists in the
  Go side for any runtime; nothing is inferred from session usage.
- **claude check-update is not wired**; `CheckUpdate` is a typed
  `*UnsupportedOperation` (the vendor's check is an HTTP release lookup, not
  a local command), and codex/cursor answer the same way.
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
