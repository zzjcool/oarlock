# oarlock

**English** · [中文](README.zh.md)

One Go contract for driving coding-agent runtimes.

> An *oarlock* is the socket that holds an oar. It creates no force of its own —
> it only makes sure the force arrives where it is needed, unchanged.

oarlock is a Go implementation of the [OAR](https://github.com/botiverse/oar)
contract. It gives a host one API for ten coding-agent runtimes: detect an
installation, open a session, send a prompt, watch one ordered and lossless
record stream, steer or abort, read usage and context, and resume later.

No in-process vendor SDK, no cgo, no JavaScript in the Go module.

## Quick start

```go
package main

import (
	"context"
	"log"
	"time"

	"github.com/zzjcool/oarlock"
	_ "github.com/zzjcool/oarlock/adapters/pi" // registers "pi"
)

func main() {
	ctx := context.Background()

	rt, err := oarlock.Require("pi")
	if err != nil {
		log.Fatal(err)
	}
	inst, err := rt.Installation(ctx) // read-only probe, no account or network
	if err != nil {
		log.Fatal(err)
	}
	sess, err := rt.Session(ctx, inst, oarlock.Options{Cwd: "/repo"})
	if err != nil {
		log.Fatal(err)
	}
	defer sess.Dispose(context.WithoutCancel(ctx))

	run, err := oarlock.PromptAndWait(ctx, sess,
		"Fix the flaky test",
		oarlock.WithTimeout(10*time.Minute))
	if err != nil {
		log.Fatal(err)
	}
	log.Println(run.Outcome, run.Text) // completed | failed | aborted | interrupted
}
```

The runnable version, including the readback, is in
[`examples/quickstart`](examples/quickstart). See
[docs/quickstart.md](docs/quickstart.md) for the walkthrough and
[docs/quickstart.zh.md](docs/quickstart.zh.md) for Chinese.

## Runtimes

| Runtime | Transport | Evidence |
|---|---|---|
| **pi** | RPC child (`pi --mode rpc`, JSONL) | live: turns, tools, abort, compaction, retry, steer, images, withdraw, resume |
| **claude** | `-p` + bidirectional stream-json | live via a third-party endpoint: PromptAndWait, steer, images, cancel; official login unverified |
| **codex** | app-server JSON-RPC | live via a third-party endpoint: MCP, options, subagent, resume; official login unverified |
| **grok, kimi, opencode, qwen, qoder, antigravity** | ACP over stdio | opencode v1 + v2 live; the other five live-unverified |
| **cursor** | `cursor-agent -p --output-format stream-json` | live: PromptAndWait, explicit resume; declared degradations |

All ten run over stdio. Pi's runtime needs Node; the Go build does not.

## Design in five lines

- **`oarlock`** — the contract: three-state records, dense `Seq`, typed errors,
  capabilities as optional interfaces, and `PromptAndWait`.
- **`internal/kernel` + `internal/fold`** — the append-only stream, cursor
  replay, non-blocking observers, and pure status/usage/context/conversation
  folds.
- **`adapters/*`** — Pi, Claude, Codex, the ACP family and Cursor. Each adapter
  maps native frames into the same records; none wraps a vendor SDK.
- **`internal/ops`** — install, login/logout, auth status, account usage,
  updates and inventories, typed and honest about what it cannot do.
- **`internal/voyage` + `testdata`** — record once, replay forever. Live
  recordings are the regression fixtures for the folds.

Read [docs/architecture.md](docs/architecture.md) for the full model, and
[docs/compat-matrix.md](docs/compat-matrix.md) for per-runtime differences.

## Requirements

- Go 1.23 or newer.
- The vendor CLI you want to drive (Pi also needs Node >= 22.19 at runtime).
- The Go module has no cgo, no Node and no npm dependencies.

Credentials stay with the vendor CLI. oarlock holds none, writes none, and
redacts credential-shaped data before it reaches a record or an error.
See [ADR-0005](docs/adr/0005-node-boundary.md) for the Node boundary.

## Documentation

| Start here | |
|---|---|
| [docs/README.md](docs/README.md) | Documentation index, by topic |
| [docs/quickstart.md](docs/quickstart.md) · [中文](docs/quickstart.zh.md) | Probe → session → prompt → readback → dispose |
| [docs/architecture.md](docs/architecture.md) | Layers, records, capabilities |

| Reference | |
|---|---|
| [docs/compat-matrix.md](docs/compat-matrix.md) | Runtime support and declared degradations |
| [docs/ops.md](docs/ops.md) | Install, login, usage, update, inventory |
| [docs/runtimes/](docs/runtimes/) | One guide per runtime |

| Evidence & project | |
|---|---|
| [docs/voyage.md](docs/voyage.md) | Record/replay fixtures and live evidence |
| [docs/live-testing.md](docs/live-testing.md) | Live test gates and troubleshooting |
| [docs/benchmarks-and-fuzz.md](docs/benchmarks-and-fuzz.md) | Benchmarks and redaction fuzz |
| [docs/board.md](docs/board.md) · [docs/roadmap.md](docs/roadmap.md) | Milestones and remaining work |
| [docs/adr/](docs/adr/) | Decision records |
| [issues/](issues/) | The completed issue archive |

## Status

**M0–M11 complete — 46/46 issues; M12 CLI Phase 1 is in progress.**

- Live evidence: Pi, Cursor, Claude and Codex through a third-party endpoint,
  and opencode on both ACP release lines.
- Still unverified: official Claude/Codex logins, real AccountUsage payloads,
  the other five ACP runtimes, Pi sub-agents, summarization retry, and
  threshold/mid-turn compaction.
- CI is green on ubuntu, macOS and Windows, plus the cross-build job.

The current gate-by-gate record is in [docs/board.md](docs/board.md); what
remains and why is in [docs/roadmap.md](docs/roadmap.md).

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
