# Benchmarks and fuzz targets

How to run the long-lived regression defences for the fold, the stream-json
parser and the redactor. This document is the "how to run it" half of
[issue #84](../issues/0084-benchmarks-fuzz.md); the assertions themselves live
in the test files named below.

## Policy

- **No benchmark runs in CI, and no timing threshold is asserted.** A wall
  clock on a shared runner is noise, not a signal: a 10% budget there fails on
  a noisy neighbour and passes on a regression that the runner happens to be
  fast for. Benchmarks are read by a human when a path changes, not by the
  gate. The one exception already in the tree is
  `BenchmarkAppend_SlowBufferedObserver`, which asserts an *ordering* bound
  (append must not inherit a 100ms consumer sleep), not a speed target.
- **No long fuzz run in CI.** `go test -race ./...` executes each fuzz target's
  seed corpus, which is a fast, deterministic smoke test. `-fuzz` runs are
  manual, time-boxed and reported when they find something.
- **Test-only.** These files add no production code and no dependency. If a
  fuzz target finds a crash, the fix is a production change reviewed on its own,
  not a weakened assertion.

## Benchmarks

```sh
# everything in this tree
go test -run '^$' -bench . -benchmem ./internal/fold ./internal/streamjson ./internal/kernel

# one family, more samples on a quiet machine
go test -run '^$' -bench 'Fold_|ParseRec' -benchtime 2s -count 5 ./internal/streamjson

# the existing observer budget (issue #8), unchanged
go test -run '^$' -bench 'Append_' ./internal/kernel

# compare before/after a change: keep the raw output for both revisions
# (`-count 10` is what makes the comparison meaningful) and diff it, or feed
# it to whatever statistics tool is already on the machine — none is a module
# dependency of this repo.
go test -run '^$' -bench 'Of_10kRecords' -count 10 ./internal/fold > old.txt
go test -run '^$' -bench 'Of_10kRecords' -count 10 ./internal/fold > new.txt
```

`-run '^$'` keeps the package's ordinary tests out of the run; `-benchmem`
makes allocations part of the reading, which is what moves first when a fold
starts copying more than it used to.

### `internal/fold` — `bench_test.go`

| benchmark | what it measures |
|---|---|
| `BenchmarkOf_10kRecords` | the full fold: `Of(records)` over a synthetic 10k-record stream that exercises status, usage, context and conversation. One number for "what is the batch path worth". |
| `BenchmarkFeed_SteadyState` | the incremental path a live pump pays: one record per iteration into one long-lived `Fold`, with a monotonic `Seq`. State stays bounded because the recycled records converge (same tool call id, latest-wins usage), so the number does not drift with `b.N`. |
| `BenchmarkQueries_10k` | `Status` + `Usage` + `ContextUsage` + `Conversation` over a pre-built 10k fold. This is the read a host pays per refresh, **including** the defensive copies that keep an answer from aliasing the fold. |

A regression here usually means the conversation fold started re-scanning or
the clones started copying more than they need.

### `internal/streamjson` — `bench_test.go`

| benchmark | what it measures |
|---|---|
| `BenchmarkFold_LargeFrame` | parse + content projection of one ≥128 KiB assistant text frame. `b.SetBytes` makes MB/s meaningful; most of the cost is the decode (`BenchmarkParseRec` on the same bytes is ~7× faster), so read the two together. |
| `BenchmarkFold_MultiFrame` | a ~1000-frame realistic stream (init, text deltas, tool_use, tool_result, result) through a fresh `Folder` per op. `frames/op` is the throughput a live pump sees. |
| `BenchmarkParseRec` | the decode step alone on the same large frame, so a regression can be attributed to decoding rather than to projection. |

### `internal/kernel` — `observe_buffered_bench_test.go`

Unchanged by #84 and kept as the reference: `BenchmarkAppend_SlowBufferedObserver`
proves the append path does not inherit a buffered observer's sleep (100ms per
record) under each of `DropOldest`, `DropNewest` and `SignalGap` — an ordering
bound, not a speed target. Its rationale is in [spec/observers.md](spec/observers.md).

`PromptAndWait` (issue #75) has no benchmark: a fake session's cost is dominated
by channel scheduling, so the number would describe the test harness, not the
call. The event-driven behaviour is pinned by its own tests.

## Fuzz targets

```sh
# seed corpus only — this is also what CI runs via `go test -race ./...`
go test ./internal/redact

# manual, time-boxed runs
go test -run '^$' -fuzz 'FuzzRedactor_Text'   -fuzztime 60s ./internal/redact
go test -run '^$' -fuzz 'FuzzRedactor_Native' -fuzztime 60s ./internal/redact
go test -run '^$' -fuzz 'FuzzRedactor_Bytes'  -fuzztime 60s ./internal/redact
```

A found input is written to `internal/redact/testdata/fuzz/<target>/<hash>`.
Commit it if it encodes a real defect (that is the regression test); delete it
if the target's own assertion was wrong. The last run and its duration belong in
the issue or the PR, not in this document.

### `internal/redact/fuzz_test.go`

Every target asserts the same three classes, which is the whole point of
fuzzing this package:

1. **never panics** on arbitrary input;
2. **must redact**: a known session credential must not survive by any route
   the rules cover (plain text, JSON string, JSON key, the value after an
   `Authorization` scheme, the password inside a connection URL). One route is
   known to escape today — a path-shaped value under a credential-named key;
   see [the known gap](#known-gap-found-while-writing-these-targets-out-of-scope-here)
   below, and the follow-up issue it asks for;
3. **must not over-redact**: `PATH`, `HOME` and `SSH_AUTH_SOCK` stay readable,
   and a short value that the value rule rejects (fewer than eight characters)
   is left alone rather than mangled.

| target | input | invariants beyond the three classes |
|---|---|---|
| `FuzzRedactor_Text` | arbitrary string | `changed == (out != s)`; idempotent (a second pass reports no change) |
| `FuzzRedactor_Native` | a decoded JSON value | `Any` never mutates its argument (`reflect.DeepEqual` against a fresh decode); a changed result still marshals to valid JSON |
| `FuzzRedactor_Bytes` | arbitrary bytes | `!changed` returns the input byte-identical; valid JSON in ⇒ valid JSON out |

The seed corpus covers exactly the shapes the spec calls out: a URL with a
password, an `Authorization: Bearer …` header, env-var and header maps,
`short`/empty values, a ≥256-byte value, the safe names, `%`-escapes, invalid
UTF-8, a NUL byte and non-ASCII text.

Rules are specified in [spec/redaction.md](spec/redaction.md).

## Acceptance for #84

| criterion | evidence |
|---|---|
| at least three runnable benchmarks | `internal/fold/bench_test.go`, `internal/streamjson/bench_test.go`, the existing `internal/kernel/observe_buffered_bench_test.go` |
| two or more fuzz targets with a full seed corpus | `FuzzRedactor_Text`, `FuzzRedactor_Native`, `FuzzRedactor_Bytes` |
| assertions cover no-panic / must-redact / no-over-redact | the three classes above, table test `TestRedactorFuzzCorpus_SafeNamesSurvive` |
| gofmt / vet / `test -race` / Windows build green | the standard gate in [CONTRIBUTING.md](../CONTRIBUTING.md); this change adds no new gate step |

## Gap found while writing these targets (fixed)

Writing the corpus surfaced one pre-existing production defect: `(*Redactor).child`
short-circuited the known-value rule for a credential-named key whose value fails
the value rule (`SelectValue` rejects paths), so a known session value embedded
in a path-shaped value under such a key survived:

```go
known := "known-value-1234"
r := redact.New([]string{known})
out, changed := r.Any(map[string]any{"password": "/opt/" + known})
// before the fix: changed == false, and the known value was still in out
```

The fix falls through to `r.string` when the structural rule declines, so the
known value is replaced while an unrelated path stays readable. Regression:
`TestAny_KnownValueInsideAPathShapedCredentialIsStillRedacted` in
`internal/redact/redact_test.go`.
