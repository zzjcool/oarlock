# Capabilities, declarations and refusals

**Status:** frozen for M0. Types: [`runtime.go`](../../runtime.go),
[`capabilities.go`](../../capabilities.go). Decision:
[ADR-0002](../adr/0002-capabilities-as-interfaces.md).

A host chooses the runtime at run time, from a user's choice or its own
configuration, so its code cannot know which runtime it holds. Runtimes differ:
some cannot steer, some take no system prompt, some have no usage surface. The
contract has to answer three questions without the host naming a runtime:

1. Can this runtime do a whole operation at all?
2. Can it honour one option of the open call?
3. What must the host know *before* it acts?

Go has no optional members, so the answers are three different mechanisms.

## 1. A whole operation is an interface the runtime may not implement

`Session` is the core. The operations that not every runtime has are separate
interfaces:

```go
type Session interface {
    Prompt(...) (ControlOutcome, error)
    Queue(...) (ControlOutcome, error)
    Abort(...) (ControlOutcome, error)
    Dispose(...) error
    Records(Cursor) []Record
    Events(*Cursor, func(Record)) (unsubscribe func())
    Capabilities() Capabilities
}

type Steerer interface { Steer(...) }
type Withdrawer interface { Withdraw(...) }
type ContextBreakdownReader interface { ContextBreakdown(...) }
```

The type assertion is the capability:

```go
if s, ok := sess.(oarlock.Steerer); ok {
    out, err := s.Steer(ctx, "also check the tests", oarlock.InputOptions{})
    // ...
}
// no Steer method: this runtime cannot steer at all. No request is recorded.
```

There is no `SupportsSteer() bool` anywhere.

*Why not the flag:* a boolean and the implementation are two facts that can
disagree. A runtime can advertise `true` and return `unsupported` forever, or
advertise `false` while the method works. The compiler cannot see the
disagreement, and a caller that trusts the flag silently loses a capability.
The interface cannot disagree with itself.

*Why not "throw at call time":* a compile-time fact (this runtime has no
steer) becomes a run-time failure, and every caller must carry a fallback for
a capability it could have checked once.

## 2. An option a runtime cannot honour is refused at open

Every option in `Options` is one a runtime may refuse. A refusal is
`*UnsupportedOption{Option, Reason}`, returned **before anything starts**:

```go
sess, err := rt.Session(ctx, inst, oarlock.Options{
    Cwd:        "/repo",
    LaunchArgs: []string{"--my-flag"},
})
var unsupported *oarlock.UnsupportedOption
if errors.As(err, &unsupported) {
    // unsupported.Option == oarlock.OptionLaunchArgs
    // fall back, or pick a runtime that accepts it
}
```

`Runtime.Refused` is the same table the open path checks. A host that must
decide before acting reads it; a host that does not simply calls and falls
back. The declaration and the behaviour cannot drift because they are one map.

The refusal is keyed by *presence*, not by the option's existence in the
struct. A host can send one `Options` value to every runtime; only a value it
actually set can fail:

```go
func (r RefusedOptions) Check(opt Options) error {
    if unsupported := r.Unsupported(OptionEnv); len(opt.Env) > 0 && unsupported != nil {
        return unsupported
    }
    // ...
}
```

*Why not ignore the option:* silently dropping it writes application logic
against a lie. The session opens, the host believes the environment is set,
and every tool call runs without it. OAR names this as multica's bug, not a
shortcut.

*Why not one `Capabilities` struct mirroring every option:* a second copy of
the contract is a second thing to keep true. The refusal is the only place
that decides; the declaration is for the host that must know before opening.
An option that is not declared can still be refused at open when only the
runtime can decide (a resumed directory, a model-specific effort).

### The frozen option set

| `Option` constant | Present when | Notes |
|---|---|---|
| `systemPrompt` | non-empty | replaces the built-in prompt |
| `appendSystemPrompt` | non-empty | appends, keeps harness behaviour |
| `env` | non-empty map | `nil` value removes a variable |
| `mcpServers` | non-empty list | the whole option is refused |
| `mcpServerCredentials` | any entry has a non-empty `Env` or `Headers` | derived from `mcpServers`, not a separate field |
| `disallowedTools` | non-empty list | native deny channel |
| `launchArgs` | non-empty list | pass-through arguments; a runtime that owns its RPC arguments refuses |
| `serviceTier` | non-empty | value-specific availability can still fail at open |
| `additionalDirectories` | non-empty list | extra workspace roots |
| `outputSchema` | non-empty JSON | schema is passed through, never validated |
| `name` | non-empty | display name in the native UI |
| `userConfiguration` | `"exclude"` | `include` and omission are always accepted |

`mcpServerCredentials` exists because "MCP servers are supported" and "MCP
credentials are supported" are different facts: an ACP agent that writes
`env`/`headers` to disk cannot take a credential-bearing entry even though it
takes the server itself.

## 3. A declaration only where the host must decide before acting

Some decisions come before any call: a host passes `env` to every session and
must leave it out for a runtime that refuses it; a UI shows an attach button
only where images go. For those:

```go
type Capabilities struct {
    Queue       QueueCapability   // Durable bool
    Attribution AttributionTier   // none | opaque | attributed | nested
    Images      bool
}
```

- `Queue.Durable` — where held input lives. Every runtime can hold input in
  the adapter (`false`); codex persists it in the runtime (`true`). Whether
  held input can be taken back is `Withdrawer`'s presence, not a flag here.
- `Attribution` — how much sub-agent attribution the runtime exposes:
  - `none`: no sub-agents;
  - `opaque`: they exist but the selected interface shows only the root;
  - `attributed`: child records carry `AgentPath`;
  - `nested`: children are their own sessions, linked in the graph.
- `Images` — prompt/steer/queue inputs may carry native images. For ACP this
  is known only after `initialize`, which is why it lives on the session and
  not on `Runtime`.

`ContextBreakdown` is **not** in `Capabilities`: it is a whole operation, so
it is the `ContextBreakdownReader` interface. This is the one place the
architecture sketch had a flag beside an operation; ADR-0002 wins and
[`docs/architecture.md`](../architecture.md) now matches.

A declaration is added for a real pre-action need, not mirrored for every
option. Otherwise the declaration becomes a second contract that can disagree
with the refusal.

## The same host code on two runtimes

```go
func ask(ctx context.Context, rt oarlock.Runtime, prompt string) (string, error) {
    inst, err := rt.Installation(ctx) // no account/network I/O
    if err != nil {
        return "", err
    }

    sess, err := rt.Session(ctx, inst, oarlock.Options{Cwd: "/repo"})
    if err != nil {
        var unsupported *oarlock.UnsupportedOption
        if errors.As(err, &unsupported) {
            // This runtime cannot open with that option; the host decides
            // whether to retry without it or choose another runtime.
            return "", err
        }
        return "", err
    }
    defer sess.Dispose(context.WithoutCancel(ctx))

    // A steering host does not name the runtime; it asks for the capability.
    // Half of that pair needs a turn already running (Pi refuses
    // no_active_turn), so this one is best-effort: steer inside a turn too.
    if steerer, ok := sess.(oarlock.Steerer); ok {
        _, _ = steerer.Steer(ctx, "remember to run the tests", oarlock.InputOptions{})
    }

    out, err := sess.Prompt(ctx, prompt, oarlock.PromptOptions{})
    if err != nil {
        return "", err
    }
    if out.Kind != oarlock.ControlAccepted {
        return "", fmt.Errorf("prompt rejected: %s: %s", out.Code, out.Reason)
    }
    return "accepted; follow the records for the turn", nil
}
```

This compiles once and runs on Pi and Claude (both have `Steerer` since issues
#45 and #19) and on the scripted session (no `Steerer`) without a runtime-name
switch. Waiting for the turn's own
completion event is a fold over the record stream; it lands with the kernel
([#6](../../issues/0006-kernel.md)).

## What this refuses

- **A lowest-common-denominator `Session` with every operation always
  present.** Strong runtimes are dragged down to the weakest, and every
  caller pays for an operation that cannot work.
- **A `Capabilities` struct with booleans for whole operations.** It compiles
  while being wrong; ADR-0002's rejected option.
- **Ignoring unsupported options.** Convenient, dishonest, and the reason
  the session's behaviour no longer matches the host's configuration.
- **A declaration for every option.** The table would drift from the
  refusal, and the host would have two sources of truth.

## Verification

```
gofmt -l .            # empty
go vet ./...
go test -race ./...
```

`capabilities_test.go` proves: a plain session does not satisfy `Steerer`; a
session with `Steer` does; every frozen option is actually refused when
declared and present; a declared-but-absent option is accepted; an undeclared
present option is not refused by the shared table; `mcpServerCredentials` only
fires on a real credential; `userConfiguration` only refuses `exclude`.
