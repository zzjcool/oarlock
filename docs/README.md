# Documentation

**English** · [中文](README.zh.md)

Start with the quickstart if you want to run something; read the architecture
if you want to understand the contract. Everything else is reference or
evidence for a specific decision.

## Start here

| Document | What it answers |
|---|---|
| [quickstart.md](quickstart.md) · [中文](quickstart.zh.md) | How do I probe, open a session, prompt, read back and dispose? |
| [cli.md](cli.md) · [中文](cli.zh.md) | What can the `oarlock` command do — `list`, `probe`, `models` and `run`? |
| [architecture.md](architecture.md) | What are the layers, the record stream and the capability model? |
| [../examples/quickstart](../examples/quickstart) | The same flow as a runnable program |

## Reference

| Document | What it answers |
|---|---|
| [compat-matrix.md](compat-matrix.md) | Which runtime supports what, and which differences are declared? |
| [ops.md](ops.md) | Install, login, auth status, account usage, update and inventory |
| [runtimes/pi.md](runtimes/pi.md) | Pi RPC, controls, projection, resume, readback and versions |
| [runtimes/pi-projection.md](runtimes/pi-projection.md) | The Pi frame → record mapping in detail |
| [runtimes/claude.md](runtimes/claude.md) | Claude stream-json, steer, images, cancel and permissions |
| [runtimes/codex.md](runtimes/codex.md) | Codex app-server, MCP, options, sub-agents and resume |
| [runtimes/cursor.md](runtimes/cursor.md) | The Cursor CLI path and its declared degradations |
| [runtimes/opencode.md](runtimes/opencode.md) | The ACP opencode v1/v2 lines |
| [spec/record-stream.md](spec/record-stream.md) | The frozen record-stream contract |
| [spec/capabilities.md](spec/capabilities.md) | How capability differences are declared |
| [spec/errors.md](spec/errors.md) | Error and rejection taxonomy |
| [spec/observers.md](spec/observers.md) | Observer delivery and backpressure |
| [spec/redaction.md](spec/redaction.md) | Credential rules and what is kept |

## Evidence and testing

| Document | What it covers |
|---|---|
| [voyage.md](voyage.md) | Record/replay format, live fixtures and their pinned facts |
| [live-testing.md](live-testing.md) | Opt-in live gates, environment setup and troubleshooting |
| [benchmarks-and-fuzz.md](benchmarks-and-fuzz.md) | Fold/stream-json benchmarks and redaction fuzz targets |

## Project

| Document | What it covers |
|---|---|
| [board.md](board.md) | Milestones, issues and their state |
| [roadmap.md](roadmap.md) | What shipped and what remains environment-blocked |
| [adr/](adr/) | Decision records and rejected alternatives |
| [../issues/](../issues/) | The completed issue archive |

## Language

The deep specs and runtime guides are currently English-only. Chinese entry
points:

- [../README.zh.md](../README.zh.md) — project overview
- [README.zh.md](README.zh.md) — this index in Chinese
- [quickstart.zh.md](quickstart.zh.md) — the runnable path in Chinese
- [cli.zh.md](cli.zh.md) — the command line in Chinese

When a Chinese page and an English page disagree, the English page is the
normative one until the translation is updated.
