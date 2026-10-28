# Architecture

Five layers, one dependency direction. A projection never becomes a second
source of truth; an adapter never depends on host storage or UI.

```
Discover → Declare → Control → Record → Project
install     capability  session   one        folds
+ models    refusals    + steer   ordered    (status,
            + tiers     + abort   stream     usage,
                                              context,
                                              conversation)
```

## 1. Record stream

One session owns one append-only stream. Three record kinds, discriminated by
*obligation*, not by shape:

| kind | meaning | rules |
|---|---|---|
| `frame` | the runtime's own words | append-only, dense `Seq`, never synthesized by oarlock |
| `request` | an action expecting an outcome | `toRuntime` (prompt/steer/queue/withdraw/abort/dispose) or `toApp` (permission, question, external tool) |
| `response` | points at a `RequestID` | exists **only** when oarlock observed an outcome the runtime would not state itself |

```go
type Record struct {
    Seq       uint64          // dense, monotonic, per stream instance
    ReceivedAt time.Time      // best-effort observation time
    Kind      Kind            // Frame | Request | Response
    SessionID string          // runtime-native id; a derived child session carries its own
    AgentPath []string        // [] = root; a sub-agent has its own path
    SpanID    string          // native span, when the runtime has one
    Native    json.RawMessage // verbatim vendor payload, never dropped
    Events    []Event         // facts read out of Native
}
```

Non-negotiable invariants, each with a failure it prevents:

1. **No synthesized facts.** A turn starts at the prompt request and ends at the
   runtime's own completion event. If a runtime reports neither, the stream says
   so. (Prevents fabricated `turn_ended`, the mistake OAR names explicitly.)
2. **Control never prunes facts.** There is no settled-gate. A frame that
   arrives after its span ended still enters the stream.
3. **Reachability is read off the stream.** Once an `exited` response or a
   `dispose` request exists, every later control is rejected by the kernel
   (`runtime_exited` / `disposed`) without consulting the adapter. No private
   `isAlive` flags.
4. **Empty input without images is refused before the adapter runs.**
5. **Credential values never enter records** (see ADR-0006).

The cursor is `{SessionID, AfterSeq}`: `Records(cursor)` replays retained
records with `Seq > AfterSeq`, then continues live. Sequence numbers restart
when a stream instance is recreated; the host must keep the instance identity
it stored with the cursor.

## 2. Capability model

Go has no "optional member", so capability is expressed twice, on purpose:

```go
// Compile time: a control that a runtime cannot do is an interface it does not implement.
type Session interface {
    Prompt(ctx context.Context, input string, opt PromptOptions) (ControlOutcome, error)
    Queue(ctx context.Context, input string, opt InputOptions) (ControlOutcome, error)
    Abort(ctx context.Context) (ControlOutcome, error)
    Events(...) (unsubscribe func())
    Records(Cursor) []Record
    Dispose(ctx context.Context) error
}

type Steerer interface { Steer(ctx context.Context, input string, opt InputOptions) (ControlOutcome, error) }
type Withdrawer interface { Withdraw(ctx context.Context, inputID string) (ControlOutcome, error) }
```

```go
// Run time: what a host must know *before* acting, as data.
type Capabilities struct {
    Queue       QueueCapability   // Durable bool
    Attribution AttributionTier   // None | Opaque | Attributed | Nested
    Images      ImageSupport      // Unsupported | Supported
    ContextBreakdown bool
}

type Runtime struct {
    ID        string
    Session   StartSession
    Refused   RefusedOptions // option -> reason, never silently dropped
    Probe     InstallationProbe   // optional members below
    Models    ModelLister
    // install, login, usage, upgrade: present only where implemented
}
```

Three rules follow OAR and must not drift:

- **No boolean `SupportsXxx()` sitting beside the implementation.** The
  interface assertion cannot disagree with itself.
- **A refused option is a typed error at open**, never a session that quietly
  runs without it.
- **Only declare what a host must decide before acting**; everything else stays
  an error the host can try and fall back from.

## 3. Adapters

An adapter is a transport plus a *projection* (a pure fold from native messages
to records). Shared machinery lives once:

```
internal/kernel      seq, cursor, reachability, control recording
internal/fold        status, usage, context, conversation, tasks
internal/redact      credential rules
internal/exec        process tree, stdin, deadlines, line framing
adapters/pi          JSONL RPC
adapters/claude      stream-json (stdin + stdout)
adapters/codex       app-server JSON-RPC
adapters/acp         shared ACP client + per-runtime quirk profile
adapters/cursor      stream-json dialect shared with claude
```

`acp` and `stream-json` are **families**: six runtimes share the ACP client,
and claude + cursor share one parser. A per-runtime file may only contain what
that vendor does differently.

## 4. Folds

Every read surface is a fold over `Records()`:

```go
func (s *session) Status() QueryResult[Status]      // idle | running(phase) | exited
func (s *session) Usage() QueryResult[Usage]        // Total = ByAgent + Unattributed
func (s *session) ContextUsage() QueryResult[*ContextUsage]
```

No cached adapter state. A `QueryResult` carries the `Seq` it was computed from
so it can be aligned with the stream and rebuilt after a restart.

Usage rules that differ per runtime are the adapter's decision, not the
consumer's: which native view is authoritative, how to deduplicate, and what
cannot be summed (grok). `null` means "not reported", never zero.

## 5. Concurrency and backpressure

- One session, one control plane, **at most one active turn**. A prompt while
  running is rejected `busy`, never implicitly queued.
- Observers are a side tap: dispatched synchronously, never awaited, panics
  recovered, and **never able to block the run**. A slow consumer gets a
  bounded buffer and a documented drop policy, not backpressure into the agent.
- `context.Context` flows everywhere. `Cancel` means *abort*; a deadline that
  fires is taken over by oarlock (accept the abort, then kill the process group
  after a grace period).

## 6. Errors

```go
type UnsupportedOption struct{ Option, Reason string }
type Rejected struct{ Code Code; Reason string; Seq uint64; RequestID string }

const (
    Busy, NoActiveTurn, NotQueued, Unsupported,
    RuntimeExited, Disposed, RuntimeRefused, Error Code = ...
)
```

Branch on `Code`, never on the vendor's prose. `errors.Is` / `errors.As` is the
only inspection a consumer should need.

## 7. Testing strategy

Three layers, borrowed from OAR's sea-trial and multica's fixtures:

| Layer | What it proves | Cost |
|---|---|---|
| golden traces | a recorded vendor stream folds identically after any change | free, deterministic |
| scripted runtime | the kernel's control/record semantics without a binary | free |
| live smoke | the vendor binary still speaks today's dialect | needs login, opt-in |

Unknown native event types are **counted, never coerced**. A non-zero tally is a
test signal, not a log line.
