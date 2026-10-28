// Package scripted is a test double for oarlock.Session: a session whose
// runtime is a script, running on the real internal/kernel.
//
// A host that embeds oarlock has logic of its own to test — what it does when a
// prompt is rejected busy, when an abort is never answered, when the process
// dies mid-turn — and neither a vendor binary, a login nor a network belongs in
// that test. A *Session answers those questions in milliseconds, with the same
// record stream a real adapter writes, because nothing but the runtime is fake:
//
//   - The stream is the kernel's. Seq is dense, Records and Events replay by
//     cursor, and every control is recorded as request, decide, response by
//     Kernel.Control, so a host's folds (internal/fold) read exactly what they
//     would read from adapters/pi.
//   - Reachability is read off that stream. After an exited response every
//     control is rejected runtime_exited, and after a dispose request disposed,
//     by the kernel, without the session being consulted. The session holds no
//     liveness flag.
//   - busy is a fold, not a flag: a prompt is rejected busy exactly when
//     fold.StatusOf the records before the request says running.
//
// What is scripted is only the runtime: which frames it writes and in which
// order, how long it takes, and how it fails. Semantics mirror adapters/pi
// (session.go); where this package and Pi could differ, Pi is the reference.
//
// # What it is not
//
// It is not a model. Say writes the text it was given and nothing else; no
// output quality is simulated. It is not a network and not a process: no binary
// is started, no socket is opened, and an idle session owns no goroutine. It is
// also not evidence that a vendor behaves this way. A scripted turn is written
// with scripted/... native frames while Pi writes agent_start and
// agent_settled; what the two share is the behaviour suite (RunSuite), not the
// wire. Vendor dialects are covered by golden traces (internal/voyage), not by
// this package.
//
// # Turns and steps
//
// A Turn is the script of one turn: the Steps the fake runtime performs, in
// order, between its turn_active and its turn_ended. Steps come only from the
// constructors below; the interface is sealed.
//
//	Say(text)                  one text_delta frame
//	Think(text)                one reasoning frame
//	Echo()                     Say of the input that began this turn
//	Tool(name, input, output)  tool_call_started, then tool_call_ended{ok}
//	  .Taking(d)               the call stays in flight for d (abortable)
//	  .Failing(reason)         tool_call_ended{failed}, text = reason
//	Delay(d)                   an abortable wall-clock sleep
//	Fail(reason)               the runtime ends the turn failed{unknown, reason}
//	Frame(native, events...)   one native frame, kept verbatim
//	Handled()                  the prompt is handled and no turn opens
//
// An ordinary turn reads on the stream
//
//	request(prompt)  response(accepted)  frame(scripted/turn_active)
//	    frame(...one per frame-writing step...)  frame(scripted/turn_end)
//
// A turn starts only after its accepted response has been recorded, so a reader
// never meets a frame of a turn whose prompt has no answer yet. Every turn
// begins with one scripted/turn_active frame (event turn_active) and ends with
// exactly one scripted/turn_end frame carrying the events turn_ended{Outcome}
// and usage: the runtime's own end of the turn, never one the session invents.
// A turn is one goroutine that exists while the turn runs.
//
// # Which script runs which turn
//
// New(script...) takes one Turn per turn, and a script runs turn n for the n-th
// turn the session starts, counting from 0: an accepted prompt, or queued input
// the session sends as a prompt of its own. A prompt that is rejected (busy,
// runtime_exited, empty input) starts no turn and uses up no script. When the
// scripts run out, a turn is an empty one that completes at once, so a test can
// prompt more often than it scripted without hanging.
//
// # Fault injection
//
// Faults are steps, so a fault sits exactly where it happens in the turn:
//
//   - Die(code): the process dies by itself, mid-turn. The stream gets one
//     standalone exited response (RequestID empty, ExitCode code,
//     ExitCodeReported true) and no turn_ended; later steps never run. From
//     then on the kernel rejects every control runtime_exited, and Dispose is
//     the one control it still accepts.
//   - Hang(): the turn never ends by itself. It ends by an Abort the runtime
//     honours, by the take-over below, by Dispose, or by Die.
//   - IgnoreAbort(): from this step on the runtime does not respond to abort.
//     Only the take-over, Dispose or Die ends the turn.
//   - UnknownEvent(typ): a native frame of a type the session does not know. It
//     is kept verbatim in Native, carries no Events, and is counted by Unknown:
//     counted, never coerced.
//   - Fail(reason): the runtime ends the turn failed{unknown, reason}. Later
//     steps are ignored. The turn still ends with its own turn_ended.
//   - Handled(): the first step of a turn that is handled without opening one,
//     as a slash command is in Pi. The stream carries an input_handled event
//     and no turn_active and no turn_ended; the status returns to idle with no
//     turn outcome. The rest of that script is ignored.
//
// # Abort and the take-over rule
//
// Abort while idle is rejected no_active_turn. While a turn runs it signals the
// turn, arms a take-over timer and waits. If the runtime ends the turn with its
// own turn_ended, the abort is accepted. If it does not (IgnoreAbort, or a
// script that never gets there), then when Options.AbortGrace is up
// (DefaultAbortGrace when zero) the session takes the turn over, as adapters/pi
// does by killing the process group: it records one exited response with an
// empty RequestID, ExitCode nil and ExitCodeReported true (a kill has no
// number), no turn_ended, and the abort is still accepted, because the stop is
// ours. Repeated aborts share one deadline; none of them can postpone it.
//
// The take-over never invents a turn end. The stream says exited, and a host
// that waits for turn_ended must also watch for exited.
//
// # Queue and Dispose
//
// Queue holds input in the session (Capabilities().Queue.Durable is false) and,
// when the stream says idle, sends the head as an ordinary prompt request of its
// own whose Carries names the input (its InputID, else the queue request's id).
// Each queued input is therefore one more turn and consumes one more script.
// After an exit or a dispose, held input is dropped.
//
// Dispose is idempotent. The first call records one dispose request (answered
// accepted) and then one exited response whose RequestID is that request's id.
// A running turn ends without a turn_ended, and Dispose returns only after every
// goroutine the session started has returned. Later calls return the first
// call's result. Dispose after an observed exit is accepted by the kernel
// itself, because nothing is left to release.
//
// # Events callbacks
//
// Callbacks passed to Events run inline on the goroutine that appends the
// record, exactly as in adapters/pi. A callback must be cheap and must not call
// a control (Prompt, Queue, Abort, Dispose): the control would wait on the very
// goroutine that is running the callback. To react to the stream, signal a
// channel from the callback and act on the test goroutine.
//
// # The shared suite
//
// RunSuite (suite.go) is the behaviour suite the issue asks for: one list of
// sub-tests, run against a Target, that checks only what every runtime must do.
// The same code runs on a scripted session (suite_test.go) and on a live Pi
// (pi_live_test.go), so "the scripted runtime behaves like a real one" is a test
// and not a claim. suite.go is a non-test file and imports testing: import this
// package from _test.go files only, so that production code never links the
// testing package.
//
// # What is verified, and what is not
//
// The suite passes on the scripted runtime in every run of go test. The Pi leg
// (TestPiPassesSuite) runs only with OAR_PI_LIVE=1 and a logged-in pi on PATH;
// otherwise it skips with an explicit NOT VERIFIED message. A green go test
// ./... therefore proves the suite on the scripted runtime alone. The claim that
// the same suite passes on Pi holds only for a run of
//
//	OAR_PI_LIVE=1 go test -race -run TestPiPassesSuite ./internal/testing/scripted/
//
// on a machine that can reach a model, and CI does not make that run.
//
// # Limits
//
//   - Steer and Withdraw are not implemented, like Pi: a *Session is an
//     oarlock.Session and nothing more, so a host sees the absence as a failed
//     type assertion to oarlock.Steerer or oarlock.Withdrawer, never as a flag.
//   - Images are accepted only when Options.Images is set (and then
//     Capabilities().Images is true); the scripted runtime does not look at
//     them. Without it an input with images is rejected unsupported, as Pi does
//     today.
//   - Time is wall-clock. Delay, Tool.Taking and AbortGrace use real timers;
//     there is no fake clock, so keep delays in the millisecond range.
//   - Events callbacks run inline and must not call controls (above).
//   - The package is internal. Only code inside github.com/zzjcool/oarlock can
//     import it, and so can only that module's tests; an exported copy for
//     hosts in other modules is a follow-up, not part of this package.
//   - There are no sub-agents (Attribution is none) and no resume: a session is
//     one stream instance that starts at Seq 1.
package scripted
