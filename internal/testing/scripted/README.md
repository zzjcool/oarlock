# `internal/testing/scripted` — a scripted runtime for host tests

A `*scripted.Session` is an `oarlock.Session` whose runtime is a script. Everything a
host can observe is real — `Seq`, cursor replay, reachability, the request/response
records of every control — because it runs on the **real** `internal/kernel`. Only the
runtime behind it is fake. No binary, no login, no network, no model: a test takes
milliseconds and cannot flake on a vendor.

It is the Go counterpart of OAR's `@botiverse/oar/testing` scripted runtime
([issue #18](../../../issues/0018-scripted-runtime.md); layer 2 of
[docs/architecture.md §7](../../../docs/architecture.md)).

It is **not** a model (it writes the text you give it and nothing else) and **not** a
network. Its frames are `scripted/...`, not any vendor's dialect: it tests your host
logic, the kernel and the folds. Vendor dialects are the job of golden traces
(`internal/voyage`).

> ### Who can import it — read this first
>
> This is an `internal/` package. Only code **inside this module**
> (`github.com/zzjcool/oarlock`) can import `internal/testing/scripted` and
> `internal/fold`, so it serves **this module's own tests** (adapters, folds, the shared
> suite). A host in another module cannot import either package, and the Go toolchain
> will refuse the build. An exported copy for external hosts is a follow-up and is not
> part of this package.
>
> Everything a test *drives* is exported API of `github.com/zzjcool/oarlock`
> (`Session`, `Record`, `Cursor`, `ControlOutcome`, `Code`, ...). The two internal
> imports are the script itself and `fold.StatusOf`, which the snippets below use to
> read the status.
>
> `suite.go` imports `testing`. Import this package from `_test.go` files only, so
> production code never links the `testing` package.

## What the stream looks like

Seq 1 is always the `scripted/model` frame written when the session opens. An ordinary
turn then reads:

```
1  frame     scripted/model         events: model
2  request   prompt                 toRuntime
3  response  accepted               → req-2
4  frame     scripted/turn_active   events: turn_active
5  frame     scripted/text          events: text_delta      ← Say / Echo
6  frame     scripted/turn_end      events: turn_ended{completed}, usage
```

A turn starts only **after** its accepted response is in the stream, and ends with
exactly one `scripted/turn_end` frame — the runtime's own report, never one the session
invents. A turn is one goroutine; an idle session owns none.

## (a) The smallest test: prompt, wait for idle, assert

```go
package myhost_test

import (
	"context"
	"testing"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
	"github.com/zzjcool/oarlock/internal/testing/scripted"
)

func TestPromptRunsToIdle(t *testing.T) {
	sess := scripted.New(scripted.Turn{scripted.Say("Hello, "), scripted.Echo()})
	ctx := context.Background()
	defer sess.Dispose(ctx)

	out, err := sess.Prompt(ctx, "world", oarlock.PromptOptions{})
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	st := waitIdle(t, sess) // (b)

	if st.LastTurnOutcome == nil || st.LastTurnOutcome.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("outcome = %+v, want completed", st.LastTurnOutcome)
	}
	var text string
	for _, m := range fold.ConversationOf(sess.Records(oarlock.Cursor{})).Value {
		if m.Role == fold.RoleAssistant { // one message per Say: the events carry no MessageID
			text += m.Text
		}
	}
	if text != "Hello, world" {
		t.Fatalf("text = %q", text)
	}
}
```

`Prompt` returns once the session took the input over; the turn then runs on its own
goroutine. **Wait on the stream, never on a sleep.** `Dispose` is idempotent and always
worth deferring: it ends a running turn and waits for every goroutine the session
started.

## (b) Waiting: `fold.StatusOf` over `Records`, polled

The session keeps no status flag. Status is a fold over the retained records, exactly as
a host reads it from a real adapter:

```go
// waitStatus polls the stream until its folded status is want.
func waitStatus(t *testing.T, sess oarlock.Session, want fold.StatusKind) fold.Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := fold.StatusOf(sess.Records(oarlock.Cursor{})).Value
		if st.Kind == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("status is %s after 5s, want %s", st.Kind, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func waitIdle(t *testing.T, sess oarlock.Session) fold.Status {
	t.Helper()
	return waitStatus(t, sess, fold.StatusIdle)
}
```

An accepted `Prompt` already put the status at `running` (the prompt request is in the
stream), so `waitIdle` straight after it cannot return early. Three cases never reach
`idle`, so wait for `fold.StatusExited` instead: a `Die`, a take-over after an
ignored abort, and a `Dispose` mid-turn. None of them writes a `turn_ended`.

Callbacks given to `Events` run inline on the goroutine that appends the record. They
must be cheap and **must not call a control**; signal a channel and act on the test
goroutine.

## (c) Busy, then abort

One control plane, at most one active turn: a prompt while a turn runs is rejected
`busy`, never queued implicitly. A rejection is an outcome, not an error — branch on
`Code`, never on prose.

```go
func TestSecondPromptIsBusyThenAbort(t *testing.T) {
	sess := scripted.New(scripted.Turn{scripted.Say("working"), scripted.Hang()})
	ctx := context.Background()
	defer sess.Dispose(ctx)

	first, _ := sess.Prompt(ctx, "start a long job", oarlock.PromptOptions{})
	second, _ := sess.Prompt(ctx, "and another", oarlock.PromptOptions{})
	if first.Kind != oarlock.ControlAccepted || second.Code != oarlock.Busy {
		t.Fatalf("first = %+v, second = %+v", first, second)
	}

	abort, _ := sess.Abort(ctx) // returns once the turn ended with its own turn_ended
	if abort.Kind != oarlock.ControlAccepted {
		t.Fatalf("abort = %+v", abort)
	}
	st := waitIdle(t, sess)
	if st.LastTurnOutcome == nil || st.LastTurnOutcome.Kind != oarlock.OutcomeAborted {
		t.Fatalf("outcome = %+v, want aborted", st.LastTurnOutcome)
	}

	late, _ := sess.Abort(ctx) // the normal race, not an error
	if late.Code != oarlock.NoActiveTurn {
		t.Fatalf("late abort = %+v", late)
	}
}
```

`busy` is not a flag either: a prompt is rejected `busy` exactly when `fold.StatusOf`
the records **before** the request says `running`.

## Script reference

`Turn` is `[]Step`; steps come only from these constructors. Script *n* (0-based) runs
the *n*-th turn the session starts — an accepted prompt, or queued input at the moment
it is sent. A rejected prompt starts no turn and uses up no script. When the scripts run
out, a turn is empty and completes at once.

| Step | What the fake runtime does | Frame / events |
|---|---|---|
| `Say(text)` | streams a text chunk | `scripted/text` · `text_delta` |
| `Echo()` | `Say` of the input that began this turn | as `Say` |
| `Think(text)` | streams a reasoning item | `scripted/reasoning` · `reasoning` |
| `Tool(name, input, output)` | one tool call that succeeds | `scripted/tool_start` · `tool_call_started`; `scripted/tool_end` · `tool_call_ended{ok}` with one text part |
| `.Taking(d)` | the call stays in flight for `d`, abortable | an abort inside it leaves the call without its `tool_call_ended` |
| `.Failing(reason)` | the call ends failed, `reason` as its text | `tool_call_ended{failed}` |
| `Delay(d)` | an abortable wall-clock sleep | no frame |

`Options` (`NewWithOptions`): `SessionID` (default `scripted-session`), `Model`
(default `scripted-1`, reported by the opening `model` frame), `AbortGrace` (`0` means
`DefaultAbortGrace` = 1s, negative is an error), `Images`, `Redact`. Keep `AbortGrace`
in the tens of milliseconds in tests that exercise a take-over.

## (d) Fault injection

A fault is a step written into the turn it happens in. All of them act where they sit
in the script except `IgnoreAbort`, which marks its whole turn.

| Step | What happens | What the stream shows | What controls answer |
|---|---|---|---|
| `Die(code)` | the process dies by itself, mid-turn; later steps never run | one standalone `response exited` (`RequestID ""`, `ExitCode=&code`, `ExitCodeReported=true`); **no** `turn_ended`; status `exited` | Prompt, Queue, Abort: `rejected runtime_exited`, decided by the kernel without asking the session. Dispose is still `accepted` (one dispose request, no second exit) |
| `Hang()` | the turn never ends by itself | status stays `running` after the last frame | a second Prompt: `rejected busy`. An Abort ends it (`turn_ended{aborted}`); Dispose ends it with no `turn_ended`; so does Die |
| `IgnoreAbort()` | the runtime does not respond to abort during the turn it appears in (the whole turn, read when the turn is accepted, wherever the step sits); only the take-over after `AbortGrace`, Dispose or Die ends it | after the grace: `response exited` (`RequestID ""`, `ExitCode nil`, `ExitCodeReported true`), then the abort's `response accepted`; **no** `turn_ended` | Abort blocks for `AbortGrace`, then `accepted`; afterwards `runtime_exited`. Repeated aborts share one deadline |
| `UnknownEvent(typ)` | writes a frame of a type the session does not know | `{"type":typ}` kept verbatim in `Native`, `Events` empty; `Unknown()[typ]` counts it | none: counted, never coerced |
| `Frame(native, events...)` | writes `native` verbatim with exactly the events you list | what you wrote; a `type` outside the scripted set is counted in `Unknown()` | none |
| `Fail(reason)` | ends the turn failed; later steps are ignored | `scripted/turn_end` with `turn_ended{failed, unknown, reason}` | status `idle`, `LastTurnOutcome` failed |
| `Handled()` | first step only: the prompt is handled and no turn opens; the rest of that script is ignored | one `scripted/prompt_handled` frame with `input_handled` naming the prompt's `RequestID` and `InputID`; no `turn_active`, no `turn_ended` | status `idle`, `LastTurnOutcome` nil |

The take-over never invents a turn end: the stream says `exited`, so a host that waits
for `turn_ended` must also watch for `exited`.

`Dispose` records one dispose request (answered `accepted`) and then one `exited`
response whose `RequestID` is that request's id; a running turn ends without a
`turn_ended`. Later calls return the first call's result. Dispose after an observed exit
is accepted by the kernel itself. Queued input is held in the session
(`Capabilities().Queue.Durable` is false), is sent as an ordinary prompt with `Carries`
naming it once the stream says idle, and is dropped after an exit or a dispose.

## (e) Running the shared suite against your own adapter

`RunSuite` is one list of sub-tests that checks only what **every** runtime must do,
through `oarlock.Session` alone: `prompt_turn_end_idle`, `busy`, `abort`, `abort_idle`,
`empty_input`, `queue`, `dispose`, `dispose_mid_turn`, `events_replay`. The same code
runs on the scripted runtime (`suite_test.go`) and on a live Pi (`pi_live_test.go`).

```go
func TestMyAdapterPassesSuite(t *testing.T) {
	scripted.RunSuite(t, scripted.Target{
		Name: "myadapter",
		// A fresh, idle session. plan is the behaviour of the 1st, 2nd, ... turn
		// the sub-test will start. RunSuite disposes the session; Open need not.
		Open: func(t *testing.T, plan ...scripted.Behaviour) oarlock.Session {
			t.Helper()
			return openMyAdapter(t)
		},
		// The prompt text that makes your runtime show behaviour b.
		Prompt: func(b scripted.Behaviour) string {
			if b == scripted.Long {
				return "run a long turn" // must keep running until aborted
			}
			return "run a quick turn" // must end by itself, completed
		},
		Timeout: 30 * time.Second, // per wait and per control; 0 means 10s
	})
}
```

`Quick` is a turn that ends by itself soon; `Long` runs until it is aborted or the
session is disposed. For a scripted target, map `plan` to turns, as `suite_test.go`
does: `Quick` → `Turn{Say("ok")}`, `Long` → `Turn{Say("working"), Hang()}`. For a real
adapter the prompt text selects the behaviour and `plan` can be ignored.

## (f) The live Pi leg, and what is NOT verified

`TestPiPassesSuite` runs the same suite against `adapters/pi` and a real `pi`. It needs
a logged-in pi 1.4.0 and a reachable model (the frozen argv of
[docs/runtimes/pi.md §1](../../../docs/runtimes/pi.md)), so it is opt-in:

```sh
OAR_PI_LIVE=1 go test -race -run TestPiPassesSuite ./internal/testing/scripted/
```

Without `OAR_PI_LIVE=1` it is skipped with a `NOT VERIFIED: ...` message, and so it is
when pi cannot be launched. CI does not set the variable. **A green `go test ./...`
therefore proves the suite on the scripted runtime only; the claim "the same suite passes
on Pi" is NOT VERIFIED unless someone ran the command above on a machine with a logged-in
pi and read its output.**

## (g) Gaps, on purpose

- **No `Steer`, no `Withdraw`.** `*Session` is an `oarlock.Session` and nothing more: the
  absence shows as a failed type assertion to `oarlock.Steerer` / `oarlock.Withdrawer`,
  never as a flag. The Pi session does satisfy `oarlock.Steerer` (issue #45); this
  scripted runtime does not model steering.
- **Images only with `Options.Images`.** Then `Capabilities().Images` is true and the
  paths are recorded on the request; the runtime does not look at them. Without it an
  input with images is `rejected unsupported`, for Prompt and Queue alike.
- **No clock mocking.** `Delay`, `Tool(...).Taking` and `AbortGrace` use real timers.
  Keep them in the millisecond range.
- **`Events` callbacks are inline** and must not call controls (see (b)).
- **Usage is synthetic**: about one token per four bytes of input and `Say` text, a
  200000-token window. It is deterministic, not realistic.
- **No sub-agents, no resume, no durable queue.** `Attribution` is `none`, and a session
  is one stream instance that starts at Seq 1.
- **Internal only.** See the box at the top; an exported copy is a follow-up.
