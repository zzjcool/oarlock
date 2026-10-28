# Contract — issue #15 Pi version gate and refused options (frozen)

Go 1.23, stdlib only. Package `pi` (`adapters/pi`). Do NOT touch: docs/adr/, issues/, root *.go,
internal/*. Time box: commit + PR by 16:15 UTC.

## Decisions (frozen)
1. **Gate runs before the session process**, after the pure steps: refused options -> env/agentDir/cwd ->
   `ResolveResume` -> **version probe** -> launch. So a refused option or a not-found resume starts no process at all
   (the probe is a process).
2. Probe = `<command> <prefix...> --version` in the session cwd/env, `internal/exec` (process group, Setpgid / Job
   Object), 15s (`VersionTimeout`). stdout `TrimSpace` must equal `SupportedVersion` = `"1.4.0"` EXACTLY.
   - start fails / non-zero exit / empty output / timeout / ctx cancelled -> `*VersionProbeError` (actionable text,
     stderr tail, `Unwrap` = cause; timeout: `Timeout=true`, cause `context.DeadlineExceeded`, group killed).
   - version differs -> `*VersionError{Command, Expected, Actual}`, `errors.Is(err, ErrVersionMismatch)`. Never a warning.
   - Bound: timeout + KillGrace (2s) + reap; a SIGTERM-deaf leader with a grandchild is SIGKILLed with its group.
3. **One table**: `refusedOptions` (`oarlock.RefusedOptions`) in gate.go, derived from explicit `refusedReasons` plus a
   fallback reason for any frozen option nobody decided. `Refused()` returns a COPY of it (for `oarlock.Runtime.Refused`);
   `open` checks the very same table. Honoured: `env` (and the non-refusable Cwd/Model/Effort/Resume).
   Refused with reasons: launchArgs, systemPrompt, appendSystemPrompt, mcpServers, mcpServerCredentials,
   disallowedTools, additionalDirectories, outputSchema, name, userConfiguration(exclude), serviceTier.
   `mcpServerCredentials` is masked by `mcpServers` in `Check` order (a credential entry implies a server): both are declared.
4. **init frame / native: GAP, not faked.** The stream's frames are the runtime's own words (record-stream.md invariant 1:
   oarlock never synthesizes a frame) and Pi has no init/handshake frame carrying the version (`get_state` has none).
   The verified version is exposed as `(*Session).PiVersion()` for hosts and recorders, NOT as a record.
5. `--thinking` level validation, live `--exclude-tools`/`--name`/prompt-file support and a version window other than exact
   are out of scope (listed as gaps in pi.md §13).

## API (exact)
```go
const SupportedVersion = "1.4.0"
const VersionTimeout = 15 * time.Second
var ErrVersionMismatch, ErrVersionProbe error
type VersionError struct{ Command, Expected, Actual string }
type VersionProbeError struct{ Command string; Timeout bool; ExitCode int; StderrTail string; Cause error }
type VersionCheck struct{ Command string; Args []string; Dir string; Env []string; Expected string; Timeout, KillGrace, WaitDelay time.Duration }
func CheckVersion(ctx context.Context, c VersionCheck) (string, error)
func Refused() oarlock.RefusedOptions
```

## Ownership (one writer per file)
| file | owner |
|---|---|
| adapters/pi/gate.go, adapters/pi/gate_test.go, adapters/pi/open.go, adapters/pi/session.go (PiVersion only), adapters/pi/resume_test.go (fake `--version` + refused-table test) | centurion |
| docs/compat-matrix.md, docs/runtimes/pi.md (new §13, pointer in §12.2.9) | worker-docs |

## Acceptance (issue #15) -> evidence
1. version mismatch -> typed error with expected/actual: `TestCheckVersionMismatchIsTypedWithExpectedAndActual`, `TestOpenRefusesAVersionMismatchBeforeLaunch`.
2. refused table: declared => always refused, undeclared => never refused by the table (drift): `TestRefusedTable*`, `TestOpenRefusesEveryDeclaredOptionBeforeAnyProcess`.
3. `--version` hang -> not hung, group killed: `TestCheckVersionTimeoutKillsTheGroup` (unix pid proof), `TestOpenVersionTimeoutDoesNotHang`.
4. window in docs/compat-matrix.md linking the issue: worker-docs.
5. version into record: documented gap (decision 4), `PiVersion()` tested.
Gates: gofmt -l . empty, go vet ./..., go test -race ./..., GOOS=windows go build ./..., GOOS=darwin go build ./...
