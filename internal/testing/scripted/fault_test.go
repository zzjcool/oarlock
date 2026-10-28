package scripted

// Fault-injection tests (issue #18): a process that dies mid-turn, a turn that
// never ends, an abort nobody answers, an event nobody knows. Each asserts the
// stream, not a private flag: reachability is read off the records.

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

// faultWatchdog aborts the whole test binary, with every goroutine's stack, if
// the test is still running after d. Use it as `defer faultWatchdog(t, d)()`.
func faultWatchdog(t *testing.T, d time.Duration) func() {
	t.Helper()
	stop := make(chan struct{})
	go func() {
		select {
		case <-stop:
		case <-time.After(d):
			buf := make([]byte, 1<<20)
			buf = buf[:runtime.Stack(buf, true)]
			fmt.Fprintf(os.Stderr, "faultWatchdog: %s still running after %v\n%s\n", t.Name(), d, buf)
			os.Exit(2)
		}
	}()
	return func() { close(stop) }
}

var faultCtx = context.Background()

func faultSession(t *testing.T, grace time.Duration, script ...Turn) *Session {
	t.Helper()
	s, err := NewWithOptions(Options{AbortGrace: grace}, script...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Dispose(ctx)
	})
	return s
}

func faultWait(t *testing.T, s *Session, what string, ok func(fold.Status) bool) fold.Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := fold.StatusOf(s.Records(oarlock.Cursor{})).Value
		if ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s (status %+v)", what, st)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func faultKind(k fold.StatusKind) func(fold.Status) bool {
	return func(st fold.Status) bool { return st.Kind == k }
}

func faultCount(records []oarlock.Record, kind oarlock.EventKind) (n int) {
	for _, r := range records {
		for _, e := range r.Events {
			if e.Kind == kind {
				n++
			}
		}
	}
	return n
}

func faultDense(t *testing.T, records []oarlock.Record) {
	t.Helper()
	for i, r := range records {
		if r.Seq != uint64(i+1) {
			t.Fatalf("Seq %d at index %d: the stream is not dense", r.Seq, i)
		}
	}
}

func faultExits(records []oarlock.Record) (out []oarlock.Record) {
	for _, r := range records {
		if r.Kind == oarlock.KindResponse && r.Response != nil && r.Response.Kind == oarlock.ResponseExited {
			out = append(out, r)
		}
	}
	return out
}

func faultRequests(records []oarlock.Record, action oarlock.Action) (n int) {
	for _, r := range records {
		if r.Kind == oarlock.KindRequest && r.Request != nil && r.Request.Action == action {
			n++
		}
	}
	return n
}

func faultPrompt(t *testing.T, s *Session, text string) oarlock.ControlOutcome {
	t.Helper()
	out, err := s.Prompt(faultCtx, text, oarlock.PromptOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// ---- Die: the process exits by itself ---------------------------------------

func TestDieMidTurnRejectsEveryControl(t *testing.T) {
	defer faultWatchdog(t, 30*time.Second)()
	s := faultSession(t, 100*time.Millisecond, Turn{Say("a"), Die(7)}, Turn{Say("never runs")})

	if out := faultPrompt(t, s, "go"); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, want accepted", out)
	}
	faultWait(t, s, "exited", faultKind(fold.StatusExited))

	records := s.Records(oarlock.Cursor{})
	exits := faultExits(records)
	if len(exits) != 1 {
		t.Fatalf("exited responses = %d, want 1", len(exits))
	}
	body := exits[0].Response
	if exits[0].RequestID != "" {
		t.Fatalf("a process that died alone answers request %q; it must stand alone", exits[0].RequestID)
	}
	if body.ExitCode == nil || *body.ExitCode != 7 || !body.ExitCodeReported {
		t.Fatalf("exit = code %v reported %v, want 7 reported", body.ExitCode, body.ExitCodeReported)
	}
	if n := faultCount(records, oarlock.EventTurnEnded); n != 0 {
		t.Fatalf("a mid-turn death wrote %d turn_ended; the stream must not invent one", n)
	}

	before := len(records)
	frames := func() (n int) {
		for _, r := range s.Records(oarlock.Cursor{}) {
			if r.Kind == oarlock.KindFrame {
				n++
			}
		}
		return n
	}
	framesBefore := frames()

	controls := map[string]func() (oarlock.ControlOutcome, error){
		"prompt": func() (oarlock.ControlOutcome, error) { return s.Prompt(faultCtx, "again", oarlock.PromptOptions{}) },
		"queue":  func() (oarlock.ControlOutcome, error) { return s.Queue(faultCtx, "later", oarlock.InputOptions{}) },
		"abort":  func() (oarlock.ControlOutcome, error) { return s.Abort(faultCtx) },
	}
	for _, name := range []string{"prompt", "queue", "abort"} {
		out, err := controls[name]()
		if err != nil {
			t.Fatalf("%s after exit: %v", name, err)
		}
		if out.Kind != oarlock.ControlRejected || out.Code != oarlock.RuntimeExited {
			t.Fatalf("%s after exit = %+v, want rejected/runtime_exited", name, out)
		}
		if out.Request.Request == nil || out.Response.Response == nil {
			t.Fatalf("%s after exit: the request and its rejection must both stay in the stream", name)
		}
	}
	after := s.Records(oarlock.Cursor{})
	if len(after) != before+6 {
		t.Fatalf("3 rejected controls added %d records, want 6 (request + response each)", len(after)-before)
	}
	if got := frames(); got != framesBefore {
		t.Fatalf("frames %d -> %d: the session ran the script after it exited", framesBefore, got)
	}
	faultDense(t, after)
}

func TestDisposeAfterDieIsAccepted(t *testing.T) {
	defer faultWatchdog(t, 30*time.Second)()
	s := faultSession(t, 100*time.Millisecond, Turn{Die(3)})
	faultPrompt(t, s, "go")
	faultWait(t, s, "exited", faultKind(fold.StatusExited))

	for i := 0; i < 2; i++ {
		if err := s.Dispose(faultCtx); err != nil {
			t.Fatalf("Dispose #%d after exit = %v", i+1, err)
		}
	}
	records := s.Records(oarlock.Cursor{})
	if n := faultRequests(records, oarlock.ActionDispose); n != 1 {
		t.Fatalf("dispose requests = %d, want 1", n)
	}
	var accepted bool
	for _, r := range records {
		if r.Kind == oarlock.KindResponse && r.Response.Kind == oarlock.ResponseAccepted && r.RequestID != "" {
			for _, q := range records {
				if q.ID == r.RequestID && q.Request != nil && q.Request.Action == oarlock.ActionDispose {
					accepted = true
				}
			}
		}
	}
	if !accepted {
		t.Fatal("dispose after an observed exit must be answered accepted")
	}
	if n := len(faultExits(records)); n != 1 {
		t.Fatalf("exited responses = %d, want exactly the death", n)
	}
	faultDense(t, records)
}

// ---- Dispose ----------------------------------------------------------------

func TestDisposeIsIdempotent(t *testing.T) {
	defer faultWatchdog(t, 30*time.Second)()
	s := faultSession(t, time.Minute, Turn{Say("working"), Hang()})
	faultPrompt(t, s, "go")
	faultWait(t, s, "running", faultKind(fold.StatusRunning))

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Dispose(faultCtx)
		}()
	}
	wg.Wait()
	for i := 0; i < 3; i++ {
		errs <- s.Dispose(faultCtx)
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Dispose = %v, want nil every time", err)
		}
	}

	records := s.Records(oarlock.Cursor{})
	if n := faultRequests(records, oarlock.ActionDispose); n != 1 {
		t.Fatalf("dispose requests = %d, want 1", n)
	}
	var disposeID string
	for _, r := range records {
		if r.Kind == oarlock.KindRequest && r.Request.Action == oarlock.ActionDispose {
			disposeID = r.ID
		}
	}
	exits := faultExits(records)
	if len(exits) != 1 || exits[0].RequestID != disposeID {
		t.Fatalf("exits = %+v, want one that answers the dispose request %q", exits, disposeID)
	}
	if n := faultCount(records, oarlock.EventTurnEnded); n != 0 {
		t.Fatalf("a mid-turn dispose wrote %d turn_ended", n)
	}
	out := faultPrompt(t, s, "after")
	if out.Kind != oarlock.ControlRejected || (out.Code != oarlock.RuntimeExited && out.Code != oarlock.Disposed) {
		t.Fatalf("prompt after dispose = %+v, want rejected runtime_exited/disposed", out)
	}
	faultDense(t, s.Records(oarlock.Cursor{}))
}

// ---- Hang: a turn that never ends -------------------------------------------

func TestHangTurnNeverEndsByItself(t *testing.T) {
	defer faultWatchdog(t, 30*time.Second)()
	s := faultSession(t, time.Minute, Turn{Hang()})
	faultPrompt(t, s, "go")
	faultWait(t, s, "running", faultKind(fold.StatusRunning))

	time.Sleep(300 * time.Millisecond)
	records := s.Records(oarlock.Cursor{})
	if st := fold.StatusOf(records).Value; st.Kind != fold.StatusRunning {
		t.Fatalf("status = %q after 300ms, want the turn still running", st.Kind)
	}
	if n := faultCount(records, oarlock.EventTurnEnded); n != 0 {
		t.Fatalf("a hung turn wrote %d turn_ended", n)
	}
	if out := faultPrompt(t, s, "second"); out.Kind != oarlock.ControlRejected || out.Code != oarlock.Busy {
		t.Fatalf("prompt during a hung turn = %+v, want rejected/busy", out)
	}

	if err := s.Dispose(faultCtx); err != nil {
		t.Fatal(err)
	}
	records = s.Records(oarlock.Cursor{})
	if n := faultCount(records, oarlock.EventTurnEnded); n != 0 {
		t.Fatalf("dispose synthesized %d turn_ended for the hung turn", n)
	}
	if st := fold.StatusOf(records).Value; st.Kind != fold.StatusExited {
		t.Fatalf("status after dispose = %q, want exited", st.Kind)
	}
}

// ---- Abort that nobody answers: the take-over -------------------------------

func TestAbortIgnoredIsTakenOverAfterGrace(t *testing.T) {
	defer faultWatchdog(t, 30*time.Second)()
	const grace = 100 * time.Millisecond
	s := faultSession(t, grace, Turn{IgnoreAbort(), Hang()})
	faultPrompt(t, s, "go")
	faultWait(t, s, "running", faultKind(fold.StatusRunning))

	start := time.Now()
	out, err := s.Abort(faultCtx)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < grace {
		t.Fatalf("abort returned after %v, before the %v grace", took, grace)
	}
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("taken-over abort = %+v, want accepted (we took the stop), not exited", out)
	}
	faultWait(t, s, "exited", faultKind(fold.StatusExited))

	records := s.Records(oarlock.Cursor{})
	exits := faultExits(records)
	if len(exits) != 1 || exits[0].RequestID != "" {
		t.Fatalf("exits = %+v, want exactly one standalone exited response", exits)
	}
	if n := faultCount(records, oarlock.EventTurnEnded); n != 0 {
		t.Fatalf("take-over wrote %d turn_ended; the stream must not invent one", n)
	}
	if out := faultPrompt(t, s, "after"); out.Kind != oarlock.ControlRejected || out.Code != oarlock.RuntimeExited {
		t.Fatalf("prompt after take-over = %+v, want rejected/runtime_exited", out)
	}
	faultDense(t, records)
}

func TestConcurrentAbortsShareOneDeadline(t *testing.T) {
	defer faultWatchdog(t, 30*time.Second)()
	const grace = 150 * time.Millisecond
	s := faultSession(t, grace, Turn{IgnoreAbort(), Hang()})
	faultPrompt(t, s, "go")
	faultWait(t, s, "running", faultKind(fold.StatusRunning))

	start := time.Now()
	outs := make(chan oarlock.ControlOutcome, 3)
	for i := 0; i < 3; i++ {
		go func() {
			out, _ := s.Abort(faultCtx)
			outs <- out
		}()
		time.Sleep(20 * time.Millisecond) // later aborts must not postpone the first one's deadline
	}
	for i := 0; i < 3; i++ {
		if out := <-outs; out.Kind != oarlock.ControlAccepted {
			t.Fatalf("abort #%d = %+v, want accepted", i+1, out)
		}
	}
	if took := time.Since(start); took > grace+500*time.Millisecond {
		t.Fatalf("3 aborts took %v: later aborts postponed the shared %v deadline", took, grace)
	}
	if n := len(faultExits(s.Records(oarlock.Cursor{}))); n != 1 {
		t.Fatalf("exited responses = %d, want 1", n)
	}
}

func TestAbortHonouredEndsAbortedNotExited(t *testing.T) {
	defer faultWatchdog(t, 30*time.Second)()
	s := faultSession(t, time.Minute, Turn{Say("x"), Hang()}, Turn{Say("y")})
	faultPrompt(t, s, "go")
	faultWait(t, s, "running", faultKind(fold.StatusRunning))

	out, err := s.Abort(faultCtx)
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("abort = %+v, want accepted", out)
	}
	st := faultWait(t, s, "idle", faultKind(fold.StatusIdle))
	if st.LastTurnOutcome == nil || st.LastTurnOutcome.Kind != oarlock.OutcomeAborted {
		t.Fatalf("outcome = %+v, want the runtime's own aborted", st.LastTurnOutcome)
	}
	if len(faultExits(s.Records(oarlock.Cursor{}))) != 0 {
		t.Fatal("an honoured abort must not exit the process")
	}
	if out := faultPrompt(t, s, "next"); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt after an honoured abort = %+v, want accepted", out)
	}
	st = faultWait(t, s, "second turn done", func(st fold.Status) bool {
		return st.Kind == fold.StatusIdle && st.LastTurnOutcome != nil && st.LastTurnOutcome.Kind == oarlock.OutcomeCompleted
	})
	_ = st
}

// ---- Unknown, handled, failed, tools ----------------------------------------

func TestUnknownEventIsKeptAndCounted(t *testing.T) {
	defer faultWatchdog(t, 30*time.Second)()
	s := faultSession(t, time.Minute, Turn{UnknownEvent("future_event"), Say("x")})
	faultPrompt(t, s, "go")
	faultWait(t, s, "idle after the turn", func(st fold.Status) bool {
		return st.Kind == fold.StatusIdle && st.LastTurnOutcome != nil
	})

	if got := s.Unknown(); len(got) != 1 || got["future_event"] != 1 {
		t.Fatalf("Unknown() = %v, want only future_event:1 (scripted types are not unknown)", got)
	}
	var found bool
	for _, r := range s.Records(oarlock.Cursor{}) {
		if r.Kind == oarlock.KindFrame && string(r.Native) == `{"type":"future_event"}` {
			found = true
			if len(r.Events) != 0 {
				t.Fatalf("an unknown frame carries %d events; it must carry none", len(r.Events))
			}
		}
	}
	if !found {
		t.Fatal("the unknown frame was not retained verbatim")
	}
}

func TestHandledPromptHasNoTurn(t *testing.T) {
	defer faultWatchdog(t, 30*time.Second)()
	s := faultSession(t, time.Minute, Turn{Handled()})
	if out := faultPrompt(t, s, "/help"); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("handled prompt = %+v, want accepted", out)
	}
	st := faultWait(t, s, "idle", func(st fold.Status) bool {
		return st.Kind == fold.StatusIdle && faultCount(s.Records(oarlock.Cursor{}), oarlock.EventInputHandled) == 1
	})
	records := s.Records(oarlock.Cursor{})
	if st.LastTurnOutcome != nil {
		t.Fatalf("a handled prompt set an outcome: %+v", st.LastTurnOutcome)
	}
	if n := faultCount(records, oarlock.EventTurnActive); n != 0 {
		t.Fatalf("turn_active = %d, want 0", n)
	}
	if n := faultCount(records, oarlock.EventTurnEnded); n != 0 {
		t.Fatalf("turn_ended = %d, want 0 (no synthesized fact)", n)
	}
	faultDense(t, records)
	if out := faultPrompt(t, s, "real"); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt after a handled one = %+v, want accepted (not busy)", out)
	}
}

func TestFailEndsTurnFailedWithRuntimeWords(t *testing.T) {
	defer faultWatchdog(t, 30*time.Second)()
	s := faultSession(t, time.Minute, Turn{Fail("boom"), Say("ignored")})
	faultPrompt(t, s, "go")
	st := faultWait(t, s, "idle", func(st fold.Status) bool { return st.Kind == fold.StatusIdle && st.LastTurnOutcome != nil })
	if o := st.LastTurnOutcome; o.Kind != oarlock.OutcomeFailed || o.Reason != "boom" {
		t.Fatalf("outcome = %+v, want failed/boom", o)
	}
	if n := faultCount(s.Records(oarlock.Cursor{}), oarlock.EventTextDelta); n != 0 {
		t.Fatalf("a step after Fail ran: %d text_delta", n)
	}
}

func TestToolAndDelayAreRecorded(t *testing.T) {
	defer faultWatchdog(t, 30*time.Second)()
	s := faultSession(t, time.Minute, Turn{
		Tool("bash", "ls", "out"),
		Delay(20 * time.Millisecond),
		Tool("x", "y", "").Failing("bad"),
	})
	start := time.Now()
	faultPrompt(t, s, "go")
	faultWait(t, s, "idle", func(st fold.Status) bool { return st.Kind == fold.StatusIdle && st.LastTurnOutcome != nil })
	if took := time.Since(start); took < 20*time.Millisecond {
		t.Fatalf("turn took %v, want at least the 20ms Delay", took)
	}
	var results []oarlock.ToolResult
	for _, m := range fold.ConversationOf(s.Records(oarlock.Cursor{})).Value {
		if m.Role == fold.RoleTool {
			results = append(results, m.Result)
		}
	}
	if fmt.Sprint(results) != "[ok failed]" {
		t.Fatalf("tool results = %v, want [ok failed]", results)
	}
}

func TestQueueDrainsAsOrdinaryPromptsAfterTheRunningTurn(t *testing.T) {
	defer faultWatchdog(t, 30*time.Second)()
	s := faultSession(t, time.Minute, Turn{Hang()}, Turn{Say("B")})
	if s.Capabilities().Queue.Durable {
		t.Fatal("the scripted queue lives in the session: Durable must be false")
	}
	faultPrompt(t, s, "A")
	faultWait(t, s, "running", faultKind(fold.StatusRunning))
	if out, _ := s.Queue(faultCtx, "B", oarlock.InputOptions{InputID: "in-B"}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Queue = %+v, want accepted", out)
	}
	if n := faultRequests(s.Records(oarlock.Cursor{}), oarlock.ActionPrompt); n != 1 {
		t.Fatalf("queued input was sent while a turn was running: %d prompts", n)
	}
	if out, _ := s.Abort(faultCtx); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("abort = %+v", out)
	}
	faultWait(t, s, "queued turn done", func(st fold.Status) bool {
		return st.Kind == fold.StatusIdle && faultCount(s.Records(oarlock.Cursor{}), oarlock.EventTurnEnded) == 2
	})
	var carried []string
	for _, r := range s.Records(oarlock.Cursor{}) {
		if r.Kind == oarlock.KindRequest && r.Request.Action == oarlock.ActionPrompt && len(r.Request.Carries) > 0 {
			carried = r.Request.Carries
		}
	}
	if fmt.Sprint(carried) != "[in-B]" {
		t.Fatalf("the drained prompt carries %v, want [in-B]", carried)
	}
}

func TestOptionsAndCapabilities(t *testing.T) {
	if _, err := NewWithOptions(Options{AbortGrace: -1}); err == nil {
		t.Fatal("NewWithOptions accepted a negative AbortGrace")
	}
	s := New()
	defer func() { _ = s.Dispose(faultCtx) }()
	var v any = s
	if _, ok := v.(oarlock.Steerer); ok {
		t.Fatal("*Session implements Steerer: the scripted runtime cannot steer, like Pi")
	}
	if _, ok := v.(oarlock.Withdrawer); ok {
		t.Fatal("*Session implements Withdrawer")
	}
	want := oarlock.Capabilities{Queue: oarlock.QueueCapability{Durable: false}, Attribution: oarlock.AttributionNone}
	if got := s.Capabilities(); got != want {
		t.Fatalf("Capabilities() = %+v, want %+v", got, want)
	}
	img := oarlock.PromptOptions{InputOptions: oarlock.InputOptions{Images: []oarlock.InputImage{{Path: "/tmp/a.png"}}}}
	if out, _ := s.Prompt(faultCtx, "look", img); out.Kind != oarlock.ControlRejected || out.Code != oarlock.Unsupported {
		t.Fatalf("prompt with images on a session without Options.Images = %+v, want rejected/unsupported", out)
	}
}
