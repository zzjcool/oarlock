# Quickstart

**Goal:** a host program that opens one agent session, sends one prompt, reads
the answer — and the runtime's own status, usage, context, model and effort —
back, then disposes the session and exits 0.

Every identifier on this page exists in this tree. The condensed program in §2
is the shape of [`examples/quickstart/main.go`](../examples/quickstart/main.go),
which is the runnable original: same calls, one print per value.

## 1. What you get in 60 seconds

```sh
go run ./examples/quickstart                        # offline: probe only
OAR_QUICKSTART_LIVE=1 go run ./examples/quickstart   # one real pi turn
```

The first command **opens no session, starts no agent child and makes no
network call**: it registers the pi runtime, runs pi's read-only `--version`
probe, prints what the machine has, and says how to opt in. The second one
opens a real session in a throwaway directory, sends one prompt, waits for pi's
own turn end, prints the folded readback, and disposes.

A setup that cannot run the live half — no pi on `PATH`, not signed in — prints
`skipped: <reason>` and **exits 0**: a quickstart must never fake a success and
must never panic to look like a failure. A real failure is an error line on
stderr and exit status 1.

## 2. The whole program

The calls, in the order a host makes them. `examples/quickstart/main.go` is
this, with the printing and the per-step commentary:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/zzjcool/oarlock"
	_ "github.com/zzjcool/oarlock/adapters/pi" // its init registers the "pi" runtime
	"github.com/zzjcool/oarlock/observe"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "quickstart:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	// 1. Registration by side effect. Without the blank import above this is
	//    oarlock.ErrRuntimeNotFound.
	rt, err := oarlock.Require("pi")
	if err != nil {
		return err
	}

	// 2. Read-only probe: `pi --version` on PATH, no account, no session, no
	//    network. That is why it can run before the live opt-in.
	inst, probeErr := rt.Installation(ctx)
	fmt.Printf("runtime=%s available=%v version=%q path=%q source=%q\n",
		rt.ID, inst.Available, inst.Version, inst.Path, inst.Source)
	if !inst.Available {
		fmt.Println("skipped: the probe found no pi on PATH")
		return nil
	}
	if probeErr != nil { // installed but unusable: for pi, a version outside the window
		return fmt.Errorf("probing the pi installation: %w", probeErr)
	}

	// 3. The live gate. Nothing below it runs unless the caller opted in.
	if os.Getenv("OAR_QUICKSTART_LIVE") != "1" {
		fmt.Println("skipped: re-run with OAR_QUICKSTART_LIVE=1 (needs a logged-in pi 1.4.0)")
		return nil
	}

	// 4. A throwaway working directory, so the example never reads or writes
	//    the checkout it was launched from.
	cwd, err := os.MkdirTemp("", "oarlock-quickstart-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(cwd) }()

	// Session takes the Installation the probe returned. Model and Effort stay
	// empty on purpose: empty means "the runtime's own default", and a model id
	// this file cannot verify would be a claim about your account.
	sess, err := rt.Session(ctx, inst, oarlock.Options{Cwd: cwd})
	if err != nil {
		// Installed but no session — most often "not signed in".
		fmt.Printf("skipped: opening the pi session: %v\n", err)
		return nil
	}
	// The dispose must survive the cancellation that ended the turn.
	defer sess.Dispose(context.WithoutCancel(ctx))

	// 5. One prompt, and the runtime's own turn end — never a synthesized one.
	res, err := oarlock.PromptAndWait(ctx, sess,
		"Reply with the single word: ok",
		oarlock.WithTimeout(2*time.Minute))
	if err != nil {
		return fmt.Errorf("prompt: %w", err)
	}
	fmt.Printf("outcome=%s\n", res.Outcome)
	if res.Turn != nil {
		fmt.Printf("turn=%s\n", res.Turn.Kind)
	}
	fmt.Printf("text=%q\n", res.Text)

	// 6. Readback: one type assertion for the whole read surface.
	rb, ok := sess.(observe.Readback)
	if !ok {
		return fmt.Errorf("%T does not implement observe.Readback", sess)
	}
	fmt.Printf("status:  %s (seq %d)\n", rb.Status().Value.Kind, rb.Status().Seq)
	if u := rb.Usage().Value.Total; u != nil { // nil Total means "not reported", not zero
		fmt.Printf("usage:   input=%d output=%d\n", u.Input, u.Output)
	}
	if c := rb.ContextUsage().Value; c != nil && c.Usage.Tokens != nil {
		fmt.Printf("context: tokens=%d (seq %d)\n", *c.Usage.Tokens, c.Seq)
	}
	fmt.Printf("model:   %q effort: %q\n", rb.Model().Value, rb.Effort().Value)

	// 7. The same records, folded by hand at a cursor you control.
	folded := observe.StatusOf(sess.Records(oarlock.Cursor{}))
	fmt.Printf("status(from records, seq %d)=%s\n", folded.Seq, folded.Value.Kind)
	return nil
}
```

The order is the point: **register → probe → gate → session → wait → read →
dispose**. Nothing before the gate starts a session, and nothing after it
invents an outcome the runtime did not report.

## 3. Run it

```sh
go run ./examples/quickstart
```

prints what the probe found and stops:

```
runtime:   pi
available: true
version:   1.4.0
path:      /path/to/pi
source:    path

notice:    no session was opened. The live half starts `pi --mode rpc`
           in a throwaway directory, sends one prompt, and prints
           the reply plus the folded status, usage and context.
           It needs a logged-in pi 1.4.0 (it uses your account).
           Opt in with: OAR_QUICKSTART_LIVE=1 go run ./examples/quickstart
```

Exit status 0. `version` and `source` are the probe's own answer (`source` is
`bundled`, `path` or `config`), so this is also how you find out *which* pi
your process would drive.

```sh
OAR_QUICKSTART_LIVE=1 go run ./examples/quickstart
```

opens a real session. It needs:

- a **pi on `PATH` inside the supported version window** — the gate is an exact
  match on `pi 1.4.0` (see [compat-matrix.md](compat-matrix.md)), so `1.3.9`,
  `1.4.1` and `v1.4.0` are all refused before a session process exists;
- a **pi that is already signed in**, because the pi adapter deliberately has
  no login operation ([ops.md](ops.md) §2).

Progressively more output follows (`session:`, `outcome:`, `turn:`, `text:`,
the `readback` section, then the `fold` section and a record count). On a
healthy machine `outcome: completed`, `turn: completed`, `text: "ok"`, and each
status line reads `idle (seq N)`: `idle` is correct — the turn is over and the
runtime said so. The two reads can land on different Seq values; that `match:`
line then says `n/a (records advanced between the reads: seq A vs B)` instead
of calling it a disagreement.

The skips, all exit status 0:

- `skipped: the probe found no pi on PATH` — nothing to drive here.
- `skipped: opening the pi session: ...` — installed but not usable as a
  session, most often not signed in.

A pi that **is** installed but whose version probe fails is *not* a skip: a
broken setup is not a missing one, so it prints `quickstart: probing the pi
installation: ...` on stderr and exits 1. Same for a failure after the session
is open.

## 4. The read surface

A session's reads are five folds over the same record stream: `Status()`,
`Usage()`, `ContextUsage()`, `Model()`, `Effort()`. Asserting them one at a
time is five type assertions of boilerplate that every host writes once and
drifts on later, so `observe.Readback` names the whole surface as one
interface:

```go
if rb, ok := sess.(observe.Readback); ok {
	_ = rb.Status().Value.Kind
}
```

- It is a **union, not a fallback**: a session that lacks any one of the five
  readers does not satisfy it.
- **Every first-party adapter asserts it at compile time** in its own
  `adapters/<x>/readback_assert.go` (`pi`, `claude`, `codex`, `cursor`, `acp`):

  ```go
  var _ observe.Readback = (*Session)(nil)
  ```

  so a break in any adapter's read surface fails `go build ./...` here, not in
  your program.
- **Absence is still the capability discovery mechanism.** You learn that a
  session cannot be read back with the two-result form of a type assertion,
  never as a runtime error — and the same rule governs the optional whole
  operations: `oarlock.Steerer`, `oarlock.Withdrawer` and
  `oarlock.ContextBreakdownReader` exist only as interfaces, so their presence
  *is* the declaration. There is no `SupportsSteer` bool to drift out of sync
  with the implementation.
- `QueryResult[T]` carries the `Seq` the answer was computed from, so a read
  can be aligned with the stream and rebuilt after a restart. Nothing is cached
  in the adapter: every call folds the records again, and every pointer, slice
  and raw message is cloned on the way out. That last part is why two reads of
  the same status are **not** `==` as whole structs (`Status` carries a
  `*TurnOutcome`) — compare `Seq` and the fields you care about.
- If you would rather not assert at all, fold the records yourself:
  `observe.StatusOf(sess.Records(oarlock.Cursor{}))` is the same fold the
  adapter's `Status()` calls.

## 5. What the stream guarantees

One session owns one append-only record stream, and `Records(cursor)` is your
position in it: a zero `Cursor{}` replays everything retained, a stored
`Cursor{SessionID, AfterSeq}` replays only records you have not seen yet, and
`Events(cursor, fn)` is the one call that keeps going live after that replay.
oarlock never synthesizes a turn end — a turn starts at the
prompt request and ends at the runtime's own completion event — and
`PromptAndWait` reports `RunInterrupted` rather than a fabricated `RunCompleted`
when its `WithTimeout` deadline or your context fires. A request whose outcome
was not observed stays a request; records the adapter could not see are
missing, not guessed. The invariants are in [architecture.md](architecture.md)
§1 and the frozen spec in [spec/record-stream.md](spec/record-stream.md).

## 6. What this does not do

- No session, no agent child and no network unless `OAR_QUICKSTART_LIVE=1`.
- No API key, token or credential: oarlock holds none and writes none — the
  vendor CLI keeps its own store ([ops.md](ops.md) §1).
- No model or effort selected: empty `Options` fields mean the runtime's own
  defaults.
- No retries, no queue, no persisted history. Those are the host's job.

## 7. Troubleshooting

| What you see | Why | Fix |
|---|---|---|
| `oarlock: runtime not registered: "pi" (import its adapter package)` (`oarlock.ErrRuntimeNotFound`) | the adapter package was never imported, so its `init` never called `oarlock.Register` | add `import _ "github.com/zzjcool/oarlock/adapters/pi"` |
| `available: false` + `skipped: the probe found no pi on PATH` | the probe found no pi to drive | install pi 1.4.0 or put it on `PATH`; `source` says where the probe looked (`bundled` / `path` / `config`) |
| `quickstart: probing the pi installation: ...` and exit 1 (a `*pi.VersionError`, `errors.Is(err, pi.ErrVersionMismatch)`) | the gate is an exact match, so any pi that is not exactly `1.4.0` is refused before a session process exists | select a pi `1.4.0` binary |
| `skipped: opening the pi session: ...` | installed but no usable session — usually not signed in | for pi: sign in with `pi` itself; the pi adapter has no login op. For a runtime that exposes one the ops surface is `rt.Login` (`Login` / `AuthStatus` / `Logout`, [ops.md](ops.md) §1) |
| the turn ends with a failure class `auth` (`oarlock.FailureAuth`), i.e. `turn: failed (auth)` | the runtime answered "not authenticated" mid-turn | sign in with the runtime's own CLI, then run again |
| `outcome: rejected` + a `rejected: <code>: <reason>` line (code `busy`) | a prompt arrived while a turn was running; a session has one control plane and at most one active turn | wait for the turn to end (or abort it) before prompting |
| `outcome: interrupted` | your `oarlock.WithTimeout` deadline fired; oarlock aborted the turn and waited for the runtime's own end | it is not a vendor outcome — the run is over, and the stream still holds what happened. A cancelled *caller* context instead surfaces as `quickstart: prompt: context canceled` and exit 1, because the caller's error is returned, not swallowed |
| `readback` and `fold` print the same `seq`, but you compared the structs and they differed | fold results are cloned on the way out, so a `Status` carrying a `*TurnOutcome` is not `==` to an equal one | compare `QueryResult.Seq` and the fields, not the whole value |
