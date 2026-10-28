# oarlock

**One Go contract for driving coding-agent runtimes.**

> An *oarlock* is the socket that holds an oar. It creates no force of its own —
> it only makes sure the force arrives where it is needed, unchanged.

oarlock is a Go implementation of the [OAR](https://github.com/botiverse/oar)
contract: detect an agent runtime, open a session, send a prompt, watch one
ordered and lossless record stream, steer or abort, read usage and context, and
resume later — with the same code for every runtime.

```go
rt, _ := oarlock.Require("pi")
inst, _ := rt.Installation(ctx)          // no account or network I/O
sess, _ := rt.Session(ctx, oarlock.Options{Cwd: "/repo", Model: "anthropic/claude-sonnet-4-5"})
defer sess.Dispose(context.WithoutCancel(ctx))

run, _ := oarlock.PromptAndWait(ctx, sess, "Fix the flaky test", oarlock.WithTimeout(10*time.Minute))
fmt.Println(run.Outcome, run.Text)       // completed | failed | aborted | interrupted
```

---

## Why this exists

Every agent harness ships a different access mechanism: an SDK, a CLI, an
app-server, an ACP variant, a subprocess protocol. A product that embeds more
than one writes N integrations, and each one is lossy in its own way.

OAR publicised the position that the hard parts are **protocol** problems, not
adapter problems:

- one ordered, lossless, attributed record stream,
- session identity, resume and a resumable cursor,
- sub-agent and token attribution,
- capability differences that are declared, never faked.

oarlock takes that contract, drops the parts that only make sense in
JavaScript, and pays for it with an explicit, documented set of degradations.

## Scope

Ten runtimes, all driven over stdio — **no in-process vendor SDK**:

| Runtime | Transport | Status |
|---|---|---|
| **pi** | RPC child (`pi --mode rpc`, JSONL) | **P0 — first** |
| claude | `-p` + stream-json (bidirectional stdin) | P1 |
| codex | app-server JSON-RPC | P2 |
| grok, kimi, opencode, qwen, qoder, antigravity | ACP over stdio | P2 |
| cursor | `cursor-agent -p --output-format stream-json` | P2, **degraded by design** |

Excluded on purpose:

- **`pi-durable`** — its value is operating a host-owned in-process Harness.
  Upstream is under active development; the semantics do not survive a
  process boundary.
- **`cursor` via `@cursor/sdk`** — a compiled, in-process JS package with no
  public source. See [ADR-0004](docs/adr/0004-cursor-via-cli.md).

## The Node boundary (read this before assuming "pure Go")

oarlock contains no JavaScript. But the runtimes it drives may.

> **Pi's RPC entry is `dist/bundle/rpc-entry.js` and its package declares
> `engines: node >= 22.19`.** Driving Pi therefore means running a Node
> process (or a single-file host you ship yourself), and speaking JSONL to it.

This is a *runtime* dependency, not a build dependency: the Go module has no
cgo, no Node, and no npm. See [ADR-0005](docs/adr/0005-node-boundary.md).

## What is implemented today

The M0 specifications are frozen ([#1](issues/0001-record-stream-spec.md)–[#4](issues/0004-redaction-spec.md)),
and the shared machinery is in:

- `internal/kernel` — the one record stream: dense `Seq`, cursor replay,
  reachability, observers and the session graph ([#6](issues/0006-kernel.md));
- `internal/exec` — process groups, Windows Job Objects, stdin delivery,
  argv limits and `.cmd`→`.ps1` ([#16](issues/0016-platform-process.md));
- `experiments/pi-rpc` + `testdata/pi` — a real Pi 1.4.0 RPC spike with
  recorded normal and abort turns ([#5](issues/0005-spike-pi-rpc.md));
- `internal/redact` — the credential rules ([#4](issues/0004-redaction-spec.md)).

The Pi adapter itself ([#9](issues/0009-pi-transport.md)–[#15](issues/0015-pi-version-gate.md))
is in progress; the public `Session` does not work end to end yet.

## Documents

| File | Contents |
|---|---|
| [docs/architecture.md](docs/architecture.md) | Layers, record stream, capability model |
| [docs/compat-matrix.md](docs/compat-matrix.md) | Per-runtime differences and declared degradations |
| [docs/roadmap.md](docs/roadmap.md) | M0–M4, estimates, exit criteria |
| [docs/adr/](docs/adr/) | Decision records with rejected alternatives |
| [issues/](issues/) | The backlog, mirrored from GitHub in priority order |
| [docs/board.md](docs/board.md) | Swimlanes by milestone, columns by priority |

## Where we are

| Milestone | Gate | Status |
|---|---|---|
| M0 Spec | record stream / capabilities / errors frozen | 4/4 |
| M1 Pi | a real Pi session replays to the same status and usage | 4/12 |
| M2 Kernel | Claude passes the same suite with no new kernel code | 0/3 |
| M3 Reach | ten runtimes, each with a golden trace | 0/3 |
| M4 Ops | install/login/usage/upgrade, typed and honest | 0/1 |

**Board:** [github.com/users/zzjcool/projects/5](https://github.com/users/zzjcool/projects/5)

Filters that act as a board: [P0](https://github.com/zzjcool/oarlock/issues?q=is%3Aissue+label%3Apriority%2Fp0) ·
[M1 Pi](https://github.com/zzjcool/oarlock/issues?q=is%3Aissue+milestone%3A%22M1+Pi%22) ·
[all open](https://github.com/zzjcool/oarlock/issues)

## Non-goals

- A general agent orchestration framework (queues, DAGs, worktrees, mailboxes).
  Those are the host's job.
- A storage or replay service. oarlock emits records; the host persists them.
- A browser runtime.
- Reading native session history back through the library.
- Being a drop-in replacement for OAR's TypeScript API.

## Prior art

- [botiverse/oar](https://github.com/botiverse/oar) — the contract this follows
- [liliang-cn/agentexec](https://github.com/liliang-cn/agentexec) — Go CLI adapter layer (MIT)
- [multica](https://github.com/multica-ai/multica) — production Go agent backend
- [coder/acp-go-sdk](https://github.com/coder/acp-go-sdk) — ACP client

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
