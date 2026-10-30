# Roadmap

**Status: M0–M13 complete — 50/50 issues.** The issue board is the live source of truth
([board.md](board.md)); this page records what shipped and what remains
environment-blocked.

## Completed

| Milestone | Outcome | Done |
|---|---|---|
| **M0 Spec** | record stream, capability model, error taxonomy, redaction | 4/4 |
| **M1 Pi** | Pi RPC transport, controls, projection, resume, readback, inventories | 12/12 |
| **M2 Kernel** | voyage, scripted runtime, Claude stream-json on the same kernel | 3/3 |
| **M3 Reach** | Codex, ACP family, Cursor CLI with declared degradations | 3/3 |
| **M4 Ops** | install/login/auth/update/inventory, typed and honest | 1/1 |
| **M5 Polish** | cross-runtime live matrix, public API, Pi steer | 5/5 |
| **M6 Capability depth** | input, control, options and ACP depth | 4/4 |
| **M7 Live proof** | third-party/local live evidence for M6 gaps | 3/3 |
| **M8 Ops & live hardening** | ACP install plans, Codex MCP live, voyage tool coverage | 3/3 |
| **M9 Depth & reliability** | event-driven wait, voyage breadth, opencode v2 live | 3/3 |
| **M10 Evidence & hardening** | compaction/subagent live, benchmarks, fuzz | 3/3 |
| **M11 Retry & accounting** | Pi retry live, typed AccountUsage for Claude/Codex | 2/2 |
| **M12 CLI Phase 1** | `oarlock list/probe/run/models`, stable DTOs, `--cwd`, no-overwrite record | 1/1 |
| **M13 CLI depth** | read-only ops, Claude/Codex/ACP models, crash-safe record | 3/3 |

## Remaining: environment-blocked or deliberately optional

- **Real AccountUsage payloads** for Claude/Codex: the readers and fake HTTP
  tests exist; no signed-in vendor account has been read.
- **Official Claude/Codex live**: both were exercised through a third-party
  endpoint, not the vendor's own login.
- **The other five ACP runtimes** (grok, kimi, qwen, qoder, antigravity): no
  binary or account on this host. opencode is live on v1 and v2.
- **Pi sub-agent, summarization retry and threshold/mid-turn compaction**:
  this Pi installation has no delegating tool and no bounded way to overflow
  its context. Provider retry is live; manual compaction is live.
- **Claude `cancel_queued`** is a CLI race (4/7 hits); a miss skips honestly.
- **Compaction usage accounting**: Pi's summarization usage is in the native
  frame but no `Usage` ledger counts it (OAR parity, documented gap).

## Historical note

The original one-engineer estimate for M0–M4 (11–13 weeks) is no longer a plan.
The completed milestones above and the issue board are the record; estimates
are not maintained here.

## Standing risks

| Risk | Mitigation |
|---|---|
| Upstream OAR moves fast | Sync behaviour, not code: regenerate golden traces and run the differential suite |
| Vendor CLI ships a breaking stream change | unknown-event tally + fixture diff turns it into a red test, not a silent gap |
| Codex app-server schema churn | pin a schema revision; refuse versions outside the window |
| Pi RPC is JS-only | the Go module stays dependency-free; the Node boundary is documented in ADR-0005 |
| Scope creep into orchestration | non-goals in README; queues/DAGs/worktrees belong to the host |
