# `experiments/pi-rpc` — issue #5 spike

Proves four things against a real Pi 1.4.0 RPC child, then freezes the evidence:

1. `pi --mode rpc …` launched as its own process group, LF-framed JSONL read/written
   line by line, with a buffer that survives a 5 MiB line;
2. a `prompt` answered by a `response` correlated by `id`;
3. the event stream read through `agent_settled`, with `turn_end.message.stopReason`;
4. `abort` mid-turn, `get_state` recovery **without a restart**, and `dispose` with the
   process group proven empty (`kill(-pgid, 0) == ESRCH`).

## Layout

| path | role |
|---|---|
| `client.go` (237 lines) | the reusable small client: framing, `id` correlation, 30 s command timeout, group dispose |
| `cmd/pi-rpc-probe/main.go` (232 lines) | the live probe: asserts the claims and records the fixtures |
| `client_test.go` (88) · `replay_test.go` (273) | offline: framing/5 MiB/dispatch, and the golden replay of the recorded turns |
| `../../testdata/pi/*-turn.{frames,requests}.jsonl` | the real wire, redacted with `internal/redact` + `$HOME` normalization |

The probe is 469 lines against the issue's ≤400 target — **69 over**, reported rather than
hidden. It is one executable plus one client; the two test files are not counted above.

## Reproduce

```sh
# offline, free, no network, no process: the default
go test -race ./...

# live (needs a Pi login and the frozen model; two short prompts, a few hundred tokens)
OAR_PI_LIVE=1 go run ./experiments/pi-rpc/cmd/pi-rpc-probe -scenario normal -out testdata/pi
OAR_PI_LIVE=1 go run ./experiments/pi-rpc/cmd/pi-rpc-probe -scenario abort  -out testdata/pi
```

Without `OAR_PI_LIVE=1` the probe prints usage and exits 2 before starting anything.
The recorded argv is frozen in `main.go` and in `docs/runtimes/pi.md` §1; it must not gain
`--no-extensions` (that removes the host-extension provider and the model stops resolving).

Both live runs printed `failed=` (empty) and exited 0 on 2026-10-28, pi 1.4.0 / Node v22.23.1.

Meaning, caveats and the verdict (可做, with nine conditions for #9/#10) live in
[`docs/runtimes/pi.md`](../../docs/runtimes/pi.md).

## Gaps and deviations (measured, not hidden)

- **Line budget.** The probe is 469 lines against the issue's ≤400 target: **69 over**
  for `client.go` + `cmd/`, and **430 over** for all Go in this directory (830 lines,
  tests included). The extra is the fixture writer, the live proof harness and the
  offline assertions; none of it is a second implementation.
- **Probe output shape.** Contract §4.2 asked for one JSON proof object plus a human
  summary; the delivered probe prints one `name ok=<bool> <detail>` line per proof and
  a `failed=` trailer. The contract was amended rather than silently re-read.
- **`extension_ui_request` noise.** The frozen argv keeps host extensions (they register
  the model provider), so every fixture carries 7 such frames, with UUID ids and host
  paths (`$HOME/.pi/...`, `$HOME/.config/...`). They are real stream traffic and are
  kept byte-for-byte; the replay test pins their type, not their content.
- **Unverified** (also `docs/runtimes/pi.md` §10): tool turns, images, steering/queueing,
  resume, Windows, the 30 s timeout actually firing, `extension_ui_response` round trip,
  `SIGKILL` truncation, and every command the spike did not call.
