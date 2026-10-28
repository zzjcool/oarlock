# Contract — issue #18 scripted runtime (frozen)

Package `scripted`, dir `internal/testing/scripted`, import path
`github.com/zzjcool/oarlock/internal/testing/scripted`. Go 1.23, stdlib only.
Do NOT touch: docs/adr/, issues/, adapters/, internal/kernel (semantics), root *.go.

## Semantics (all mirror adapters/pi/session.go; the real kernel is reused)
* `*Session` implements `oarlock.Session` only (NOT Steerer / Withdrawer, like Pi).
* Built on `kernel.New(sessionID, opt.Redact)`; every control goes through `k.Control`;
  frames through `k.Frame`; exit through `k.Respond`. No private liveness flag decides a control.
* One turn = one goroutine, zero goroutines while idle. A turn starts only AFTER its accepted
  response is recorded: stream reads `request(prompt) , response(accepted) , frame(turn_active) , ...`.
  Every turn begins with a `scripted/turn_active` frame (event turn_active) and ends with ONE
  `scripted/turn_end` frame carrying events `[turn_ended{Outcome}, usage]`.
* Script n (0-based) runs the n-th turn started in the session (prompt or drained queue input).
  Exhausted scripts => empty turn that completes at once.
* busy = fold.StatusOf(records before the request) says running. Abort while idle => rejected `no_active_turn`.
* Abort: signals the turn, arms a take-over timer (AbortGrace, shared by repeated aborts), waits for
  the turn's own `turn_ended` (then accepted) or the take-over: exited response with RequestID "",
  ExitCode nil + ExitCodeReported true, NO turn_ended, then abort is still accepted.
  `IgnoreAbort()` makes the runtime not respond, so only the take-over (or Dispose/Die) ends it.
* Queue: held in the session (Capabilities.Queue.Durable=false); drained as an ordinary prompt request with
  `Carries=[inputID or queue request id]` when the stream says idle; after exit/dispose held input is dropped.
* Dispose idempotent: first call records ONE dispose request (+accepted) then ONE `exited` response whose
  RequestID is the dispose request id, ends the turn WITHOUT a turn_ended, waits for all session goroutines.
  Dispose after an observed exit is accepted by the kernel. Later calls return the first result.
* `Die(code)`: process dies by itself: ONE standalone `exited` response (RequestID "", ExitCode=&code,
  ExitCodeReported=true), no turn_ended; afterwards every control is rejected `runtime_exited` by the kernel
  without the session being consulted.
* `Frame`/`UnknownEvent` frames are kept verbatim in Native; types outside the scripted known set are counted
  in `(*Session).Unknown()`; they never get Events invented.
* Callbacks passed to Events run inline on the appending goroutine and must not call controls (same as Pi).

## API (exact)
```go
type Step interface{ /* sealed: only constructors below */ }
type Turn []Step                       // the script of one turn

func Say(text string) Step             // text_delta frame
func Think(text string) Step           // reasoning frame
func Echo() Step                       // Say(input of this turn)
func Tool(name, input, output string) ToolStep   // tool_call_started, then tool_call_ended{ok, text content}
func (ToolStep) Taking(d time.Duration) ToolStep // call stays in flight for d (abortable)
func (ToolStep) Failing(reason string) ToolStep  // tool_call_ended{failed, text=reason}
func Delay(d time.Duration) Step       // abortable sleep
func Fail(reason string) Step          // runtime ends the turn failed{unknown, reason}; later steps ignored
func Frame(native string, events ...oarlock.Event) Step  // raw native frame, verbatim
func UnknownEvent(typ string) Step     // Frame(`{"type":typ}`) with no events (counted in Unknown())
func Handled() Step                    // FIRST step: prompt handled, no turn (input_handled event); rest ignored
func Die(exitCode int) Step            // inject: process dies mid-turn (exited)
func Hang() Step                       // inject: turn never ends by itself (ends only via abort / take-over / dispose / Die)
func IgnoreAbort() Step                // inject: from here the runtime does not respond to abort

type Options struct {
	SessionID  string        // "" => "scripted-session"
	Model      string        // "" => "scripted-1" (reported by a model frame at open)
	AbortGrace time.Duration // 0 => DefaultAbortGrace; negative => error
	Images     bool          // accept images (Capabilities.Images)
	Redact     kernel.Redactor
}
const DefaultAbortGrace = time.Second

func New(script ...Turn) *Session
func NewWithOptions(opt Options, script ...Turn) (*Session, error)
func (*Session) Unknown() map[string]int
// + the full oarlock.Session method set; Capabilities() = {Queue{Durable:false}, AttributionNone, Images: opt.Images}

// suite.go (non-test file, imports "testing")
type Behaviour int
const ( Quick Behaviour = iota; Long )        // Quick: turn ends by itself soon; Long: runs until aborted
type Target struct {
	Name   string
	Open   func(t *testing.T, plan ...Behaviour) oarlock.Session // fresh idle session; plan = behaviour of the 1st, 2nd, ... turn the subtest will start
	Prompt func(b Behaviour) string                               // prompt text that triggers b
	Timeout time.Duration                                         // per-wait bound; 0 => 10s
}
func RunSuite(t *testing.T, target Target)  // sub-tests: prompt_turn_end_idle, busy, abort, abort_idle, empty_input, queue, dispose, dispose_mid_turn, events_replay
```
The suite disposes every session it opens (t.Cleanup) and checks only what EVERY runtime must do.

## Ownership (one writer per file)
| file | owner |
|---|---|
| scripted.go, steps.go | centurion |
| suite.go, suite_test.go, pi_live_test.go | worker-suite |
| fault_test.go, leak_test.go | worker-faults |
| doc.go, README.md, example_test.go | worker-docs |

## Acceptance (issue #18) -> evidence
1. suite passes on scripted (suite_test.go) and on live Pi gated by OAR_PI_LIVE=1 (pi_live_test.go; default Skip with an explicit "NOT VERIFIED" message).
2. fault injection: after `Die`, Prompt/Queue/Abort rejected runtime_exited, session not consulted; Dispose idempotent; abort with no response is taken over after grace; Hang/UnknownEvent/IgnoreAbort covered (fault_test.go).
3. no goroutine leak: runtime.NumGoroutine converges after Dispose in every scenario (leak_test.go), `go test -race`.
4. doc example: README.md + doc.go + example_test.go (Example funcs with `// Output:`).
Gates: gofmt -l . empty, go vet ./..., go test -race ./..., GOOS=windows go build ./... , GOOS=darwin go build ./...
