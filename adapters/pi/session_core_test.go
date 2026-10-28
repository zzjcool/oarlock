package pi

// Core state-machine tests for the Pi session control plane (issue #10). The
// fake Conn here is driven by the test: a command parks until the test replies
// to it, and every reply (like the real Conn) satisfies Do first and is then
// pushed on Events as a frame. No process, no network.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

// ---- fake Conn -------------------------------------------------------------

type fakeCmd struct {
	id     string
	typ    string
	params map[string]any
	reply  chan fakeReply
}

type fakeReply struct {
	success bool
	data    string
	err     error
}

type coreConn struct {
	events chan Message
	exit   chan ExitStatus

	emitMu  sync.Mutex // serialises push against the close of events, like the real reader goroutine
	mu      sync.Mutex
	nextID  int
	sent    []string
	cmds    chan *fakeCmd
	exited  bool
	closes  int
	killOn  bool
	closeCh chan struct{}
}

func newCoreConn() *coreConn {
	return &coreConn{
		events:  make(chan Message, 64),
		exit:    make(chan ExitStatus, 1),
		cmds:    make(chan *fakeCmd, 16),
		closeCh: make(chan struct{}, 4),
	}
}

func (c *coreConn) Events() <-chan Message  { return c.events }
func (c *coreConn) Exit() <-chan ExitStatus { return c.exit }
func (c *coreConn) Close() error            { return c.close() }
func (c *coreConn) closeCount() int         { c.mu.Lock(); defer c.mu.Unlock(); return c.closes }
func (c *coreConn) sentTypes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.sent...)
}
func (c *coreConn) countSent(typ string) (n int) {
	for _, t := range c.sentTypes() {
		if t == typ {
			n++
		}
	}
	return n
}

func (c *coreConn) close() error {
	c.mu.Lock()
	c.closes++
	kill := c.killOn
	c.mu.Unlock()
	select {
	case c.closeCh <- struct{}{}:
	default:
	}
	if kill {
		c.exitWith(ExitStatus{Code: -1, Signal: os.Kill, Cause: CauseClosed})
	} else {
		c.exitWith(ExitStatus{Code: 0, Cause: CauseClosed})
	}
	return nil
}

func (c *coreConn) exitWith(st ExitStatus) {
	c.mu.Lock()
	if c.exited {
		c.mu.Unlock()
		return
	}
	c.exited = true
	c.mu.Unlock()
	c.emitMu.Lock()
	close(c.events)
	c.emitMu.Unlock()
	c.exit <- st
	close(c.exit)
}

func (c *coreConn) Do(ctx context.Context, cmd Command, out any) error {
	c.mu.Lock()
	if c.exited {
		c.mu.Unlock()
		return &ExitedError{Status: ExitStatus{Code: 0, Cause: CauseExited}}
	}
	c.nextID++
	fc := &fakeCmd{id: fmt.Sprintf("c-%d", c.nextID), typ: cmd.Type, reply: make(chan fakeReply, 1)}
	if m, ok := cmd.Params.(map[string]any); ok {
		fc.params = m
	}
	c.sent = append(c.sent, cmd.Type)
	c.mu.Unlock()
	c.cmds <- fc

	select {
	case r := <-fc.reply:
		if r.err != nil {
			return r.err
		}
		raw := fmt.Sprintf(`{"type":"response","id":%q,"command":%q,"success":%v`, fc.id, fc.typ, r.success)
		if r.data != "" {
			raw += `,"data":` + r.data
		}
		if !r.success {
			raw += `,"error":"nope"`
		}
		raw += `}`
		c.push(raw)
		if !r.success {
			return &RPCError{Command: fc.typ, Message: "nope"}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// push puts one raw message on Events as the Conn would.
func (c *coreConn) push(raw string) {
	var h struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	_ = json.Unmarshal([]byte(raw), &h)
	c.emitMu.Lock()
	defer c.emitMu.Unlock()
	c.mu.Lock()
	done := c.exited
	c.mu.Unlock()
	if done {
		return
	}
	c.events <- Message{Type: h.Type, ID: h.ID, Raw: json.RawMessage(raw)}
}

// next waits for the next command the session sent.
func (c *coreConn) next(t *testing.T, want string) *fakeCmd {
	t.Helper()
	select {
	case fc := <-c.cmds:
		if fc.typ != want {
			t.Fatalf("session sent %q, want %q", fc.typ, want)
		}
		return fc
	case <-time.After(5 * time.Second):
		t.Fatalf("session never sent %q", want)
		return nil
	}
}

func (fc *fakeCmd) ok(data string) { fc.reply <- fakeReply{success: true, data: data} }
func (fc *fakeCmd) refuse()        { fc.reply <- fakeReply{success: false} }

// ---- helpers ---------------------------------------------------------------

func newCoreSession(t *testing.T, grace time.Duration) (*Session, *coreConn) {
	t.Helper()
	conn := newCoreConn()
	sess, err := NewSession(conn, SessionOptions{SessionID: "sess-1", AbortGrace: grace})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sess.Dispose(ctx)
	})
	return sess, conn
}

func coreWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func statusOf(s *Session) fold.Status {
	return fold.StatusOf(s.Records(oarlock.Cursor{})).Value
}

func countEvents(records []oarlock.Record, kind oarlock.EventKind) (n int) {
	for _, r := range records {
		for _, e := range r.Events {
			if e.Kind == kind {
				n++
			}
		}
	}
	return n
}

func turnOutcomes(records []oarlock.Record) (out []oarlock.OutcomeKind) {
	for _, r := range records {
		for _, e := range r.Events {
			if e.Kind == oarlock.EventTurnEnded && e.Outcome != nil {
				out = append(out, e.Outcome.Kind)
			}
		}
	}
	return out
}

func assertDense(t *testing.T, records []oarlock.Record) {
	t.Helper()
	for i, r := range records {
		if r.Seq != uint64(i+1) {
			t.Fatalf("Seq %d at index %d: the stream is not dense", r.Seq, i)
		}
	}
}

// startTurn drives the Pi side of a started prompt: response(started), then agent_start.
func startTurn(t *testing.T, sess *Session, conn *coreConn, text string) oarlock.ControlOutcome {
	t.Helper()
	type res struct {
		out oarlock.ControlOutcome
		err error
	}
	done := make(chan res, 1)
	go func() {
		out, err := sess.Prompt(context.Background(), text, oarlock.PromptOptions{})
		done <- res{out, err}
	}()
	conn.next(t, "prompt").ok(`{"disposition":"started"}`)
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	conn.push(`{"type":"agent_start"}`)
	coreWait(t, "running", func() bool { return statusOf(sess).Kind == fold.StatusRunning })
	return r.out
}

func settle(conn *coreConn, stop string, aborted bool) {
	conn.push(fmt.Sprintf(`{"type":"turn_end","message":{"role":"assistant","stopReason":%q}}`, stop))
	conn.push(fmt.Sprintf(`{"type":"agent_end","willRetry":false}`))
	conn.push(fmt.Sprintf(`{"type":"agent_settled","aborted":%v}`, aborted))
}

// ---- T1: idle → prompt → busy → turn end → idle ----------------------------

func TestSessionPromptBusyThenIdleAgain(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)

	if out := startTurn(t, sess, conn, "hello"); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("first prompt = %+v, want accepted", out)
	}

	busy, err := sess.Prompt(context.Background(), "again", oarlock.PromptOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if busy.Kind != oarlock.ControlRejected || busy.Code != oarlock.Busy {
		t.Fatalf("prompt while running = %+v, want rejected/busy", busy)
	}
	if n := conn.countSent("prompt"); n != 1 {
		t.Fatalf("a busy prompt was sent to Pi: %d prompt commands", n)
	}

	settle(conn, "stop", false)
	coreWait(t, "idle", func() bool { return statusOf(sess).Kind == fold.StatusIdle })
	if got := turnOutcomes(sess.Records(oarlock.Cursor{})); len(got) != 1 || got[0] != oarlock.OutcomeCompleted {
		t.Fatalf("turn outcomes = %v, want [completed]", got)
	}

	done := make(chan oarlock.ControlOutcome, 1)
	go func() {
		out, _ := sess.Prompt(context.Background(), "third", oarlock.PromptOptions{})
		done <- out
	}()
	conn.next(t, "prompt").ok(`{"disposition":"started"}`)
	if out := <-done; out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt after the turn ended = %+v, want accepted", out)
	}
	assertDense(t, sess.Records(oarlock.Cursor{}))
}

// ---- T2: handled -------------------------------------------------------------

func TestSessionHandledPromptHasNoTurnAndReturnsToIdle(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)

	done := make(chan oarlock.ControlOutcome, 1)
	go func() {
		out, _ := sess.Prompt(context.Background(), "/help", oarlock.PromptOptions{})
		done <- out
	}()
	conn.next(t, "prompt").ok(`{"disposition":"handled"}`)
	if out := <-done; out.Kind != oarlock.ControlAccepted {
		t.Fatalf("handled prompt = %+v, want accepted", out)
	}

	records := sess.Records(oarlock.Cursor{})
	if n := countEvents(records, oarlock.EventTurnEnded); n != 0 {
		t.Fatalf("a handled prompt produced %d turn_ended", n)
	}
	if n := countEvents(records, oarlock.EventTurnActive); n != 0 {
		t.Fatalf("a handled prompt produced %d turn_active", n)
	}
	if n := countEvents(records, oarlock.EventInputHandled); n != 1 {
		t.Fatalf("input_handled events = %d, want 1", n)
	}
	if st := statusOf(sess); st.Kind != fold.StatusIdle || st.LastTurnOutcome != nil {
		t.Fatalf("status after a handled prompt = %+v, want idle without an outcome", st)
	}
	assertDense(t, records)
	// request, frame(response), response(accepted).
	if len(records) != 3 || records[0].Kind != oarlock.KindRequest || records[1].Kind != oarlock.KindFrame || records[2].Kind != oarlock.KindResponse {
		t.Fatalf("stream = %v, want request, frame, response", kinds(records))
	}

	// The next prompt is not busy.
	next := make(chan oarlock.ControlOutcome, 1)
	go func() {
		out, _ := sess.Prompt(context.Background(), "real", oarlock.PromptOptions{})
		next <- out
	}()
	conn.next(t, "prompt").ok(`{"disposition":"started"}`)
	if out := <-next; out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt after a handled one = %+v, want accepted", out)
	}
}

func kinds(records []oarlock.Record) (out []oarlock.Kind) {
	for _, r := range records {
		out = append(out, r.Kind)
	}
	return out
}

// ---- T3: abort inside a turn; the ack lands after agent_settled --------------

func TestSessionAbortInTurnEndsAbortedAndAckIsAFrame(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	startTurn(t, sess, conn, "count")

	done := make(chan oarlock.ControlOutcome, 1)
	go func() {
		out, _ := sess.Abort(context.Background())
		done <- out
	}()
	ack := conn.next(t, "abort")
	settle(conn, "aborted", true) // Pi settles first ...
	ack.ok("")                    // ... and the ack arrives after (testdata/pi/abort-turn).

	out := <-done
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("abort = %+v, want accepted", out)
	}
	coreWait(t, "idle", func() bool { return statusOf(sess).Kind == fold.StatusIdle })
	records := sess.Records(oarlock.Cursor{})
	if got := turnOutcomes(records); len(got) != 1 || got[0] != oarlock.OutcomeAborted {
		t.Fatalf("turn outcomes = %v, want [aborted] (the runtime's own report)", got)
	}
	if conn.closeCount() != 0 {
		t.Fatal("a turn that ended in the grace period must not be taken over")
	}
	assertDense(t, records)
}

// ---- T4: abort after the turn ended --------------------------------------------

func TestSessionAbortAfterTurnEndedIsRejectedAndTurnStaysCompleted(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	startTurn(t, sess, conn, "quick")
	settle(conn, "stop", false)
	coreWait(t, "idle", func() bool { return statusOf(sess).Kind == fold.StatusIdle })

	out, err := sess.Abort(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.NoActiveTurn {
		t.Fatalf("abort after the turn = %+v, want rejected/no_active_turn", out)
	}
	if conn.countSent("abort") != 0 {
		t.Fatal("an abort with no active turn was sent to Pi")
	}
	if got := turnOutcomes(sess.Records(oarlock.Cursor{})); len(got) != 1 || got[0] != oarlock.OutcomeCompleted {
		t.Fatalf("turn outcomes = %v, want [completed]", got)
	}
}

// ---- T5: Pi hangs after the abort → take-over after the grace ----------------

func TestSessionAbortTakesOverAHungProcessWithoutInventingATurnEnd(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	const grace = 80 * time.Millisecond
	sess, conn := newCoreSession(t, grace)
	conn.killOn = true
	startTurn(t, sess, conn, "hang")

	start := time.Now()
	done := make(chan oarlock.ControlOutcome, 1)
	go func() {
		out, _ := sess.Abort(context.Background())
		done <- out
	}()
	conn.next(t, "abort") // Pi never answers and never settles.
	out := <-done
	if took := time.Since(start); took < grace {
		t.Fatalf("abort returned after %v, before the %v grace", took, grace)
	}
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("taken-over abort = %+v, want accepted (not 'exited')", out)
	}
	if conn.closeCount() < 1 {
		t.Fatal("the process group was not closed at the grace deadline")
	}
	coreWait(t, "exited", func() bool { return statusOf(sess).Kind == fold.StatusExited })

	records := sess.Records(oarlock.Cursor{})
	if n := countEvents(records, oarlock.EventTurnEnded); n != 0 {
		t.Fatalf("take-over wrote %d turn_ended; the stream must not invent one", n)
	}
	var exited int
	for _, r := range records {
		if r.Kind == oarlock.KindResponse && r.Response.Kind == oarlock.ResponseExited {
			exited++
			if r.RequestID != "" {
				t.Fatalf("a take-over exit answers request %q; it must stand alone", r.RequestID)
			}
		}
	}
	if exited != 1 {
		t.Fatalf("exited responses = %d, want 1", exited)
	}
	assertDense(t, records)
}

// ---- T6: native refusal of the abort ---------------------------------------------

func TestSessionNativeAbortRefusalKillsNothing(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, 50*time.Millisecond)
	startTurn(t, sess, conn, "work")

	done := make(chan oarlock.ControlOutcome, 1)
	go func() {
		out, _ := sess.Abort(context.Background())
		done <- out
	}()
	conn.next(t, "abort").refuse()
	out := <-done
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.RuntimeRefused {
		t.Fatalf("refused abort = %+v, want rejected/runtime_refused", out)
	}
	time.Sleep(200 * time.Millisecond) // past the grace: a refused attempt must have withdrawn it
	if conn.closeCount() != 0 {
		t.Fatal("a natively refused abort must not kill the process")
	}
	if st := statusOf(sess); st.Kind != fold.StatusRunning {
		t.Fatalf("status = %q, want the turn still running", st.Kind)
	}
}

// ---- T7/T8: dispose ------------------------------------------------------------

func TestSessionDisposeIsIdempotentAndAnswersTheExit(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	startTurn(t, sess, conn, "running when disposed")

	for i := 0; i < 3; i++ {
		if err := sess.Dispose(context.Background()); err != nil {
			t.Fatalf("Dispose #%d = %v", i+1, err)
		}
	}
	if conn.closeCount() != 1 {
		t.Fatalf("Close called %d times, want 1", conn.closeCount())
	}
	records := sess.Records(oarlock.Cursor{})
	var disposeID string
	var requests, exited int
	for _, r := range records {
		if r.Kind == oarlock.KindRequest && r.Request.Action == oarlock.ActionDispose {
			requests++
			disposeID = r.ID
		}
	}
	for _, r := range records {
		if r.Kind == oarlock.KindResponse && r.Response.Kind == oarlock.ResponseExited {
			exited++
			if r.RequestID != disposeID {
				t.Fatalf("exited answers %q, want the dispose request %q", r.RequestID, disposeID)
			}
		}
	}
	if requests != 1 || exited != 1 {
		t.Fatalf("dispose requests = %d, exited responses = %d, want 1 and 1", requests, exited)
	}
	if n := countEvents(records, oarlock.EventTurnEnded); n != 0 {
		t.Fatalf("a mid-turn dispose wrote %d turn_ended", n)
	}

	out, err := sess.Prompt(context.Background(), "after", oarlock.PromptOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != oarlock.ControlRejected || (out.Code != oarlock.RuntimeExited && out.Code != oarlock.Disposed) {
		t.Fatalf("prompt after dispose = %+v, want rejected runtime_exited/disposed", out)
	}
	if conn.countSent("prompt") != 1 {
		t.Fatal("a prompt after dispose reached Pi")
	}
	assertDense(t, sess.Records(oarlock.Cursor{}))
}

func TestSessionDisposeAfterExitIsAccepted(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	conn.exitWith(ExitStatus{Code: 3, Cause: CauseExited})
	coreWait(t, "exited", func() bool { return statusOf(sess).Kind == fold.StatusExited })

	if err := sess.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
	var accepted bool
	for _, r := range sess.Records(oarlock.Cursor{}) {
		if r.Kind == oarlock.KindResponse && r.Response.Kind == oarlock.ResponseAccepted {
			accepted = true
		}
	}
	if !accepted {
		t.Fatal("dispose after an observed exit must be answered accepted")
	}
}

// ---- T9: queue -------------------------------------------------------------------

func TestSessionQueueRunsHeldInputsAsTurnsOfTheirOwn(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	if sess.Capabilities().Queue.Durable {
		t.Fatal("the Pi queue lives in the adapter: Durable must be false")
	}
	startTurn(t, sess, conn, "A")

	for _, in := range []string{"B", "C"} {
		out, err := sess.Queue(context.Background(), in, oarlock.InputOptions{InputID: "in-" + in})
		if err != nil {
			t.Fatal(err)
		}
		if out.Kind != oarlock.ControlAccepted {
			t.Fatalf("Queue(%s) = %+v, want accepted", in, out)
		}
	}
	if conn.countSent("prompt") != 1 {
		t.Fatal("queued input was sent while a turn was running")
	}

	prompts := func() (got []string) {
		for _, r := range sess.Records(oarlock.Cursor{}) {
			if r.Kind == oarlock.KindRequest && r.Request.Action == oarlock.ActionPrompt {
				got = append(got, r.Request.Input)
			}
		}
		return got
	}

	for _, want := range []string{"B", "C"} {
		settle(conn, "stop", false) // the running turn ends → the next held input is drained
		fc := conn.next(t, "prompt")
		if fc.params["message"] != want {
			t.Fatalf("drained %v, want %q", fc.params["message"], want)
		}
		fc.ok(`{"disposition":"started"}`)
		conn.push(`{"type":"agent_start"}`)
		coreWait(t, "running again", func() bool { return statusOf(sess).Kind == fold.StatusRunning })
	}
	settle(conn, "stop", false)
	coreWait(t, "idle", func() bool { return statusOf(sess).Kind == fold.StatusIdle })

	records := sess.Records(oarlock.Cursor{})
	if got := prompts(); fmt.Sprint(got) != "[A B C]" {
		t.Fatalf("prompt requests = %v, want [A B C]", got)
	}
	if n := countEvents(records, oarlock.EventTurnActive); n != 3 {
		t.Fatalf("turn_active = %d, want 3 (one turn per input)", n)
	}
	if got := turnOutcomes(records); len(got) != 3 {
		t.Fatalf("turn outcomes = %v, want 3", got)
	}
	assertDense(t, records)
}

func TestSessionQueueRejectedItemIsRecordedAndDoesNotBlockTheNext(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	startTurn(t, sess, conn, "A")
	for _, in := range []string{"B", "C"} {
		if out, _ := sess.Queue(context.Background(), in, oarlock.InputOptions{}); out.Kind != oarlock.ControlAccepted {
			t.Fatalf("Queue(%s) = %+v", in, out)
		}
	}
	settle(conn, "stop", false)
	conn.next(t, "prompt").refuse() // Pi refuses B
	conn.next(t, "prompt").ok(`{"disposition":"started"}`)

	var refused bool
	coreWait(t, "B's rejection recorded", func() bool {
		for _, r := range sess.Records(oarlock.Cursor{}) {
			if r.Kind == oarlock.KindResponse && r.Response.Kind == oarlock.ResponseRejected &&
				r.Response.Code == oarlock.RuntimeRefused && r.Response.Reason != "" {
				refused = true
			}
		}
		return refused
	})
}

// ---- T10-T12 ---------------------------------------------------------------------

func TestSessionUnknownNativeTypesAreKeptAndCounted(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	conn.push(`{"type":"future_event","n":1}`)
	conn.push(`{"type":"extension_ui_request","method":"notify"}`)
	conn.push(`{"n":2}`)
	coreWait(t, "three frames", func() bool { return len(sess.Records(oarlock.Cursor{})) == 3 })

	if got := sess.Unknown(); len(got) != 2 || got["future_event"] != 1 || got[""] != 1 {
		t.Fatalf("Unknown() = %v, want future_event:1 and \"\":1 only", got)
	}
	records := sess.Records(oarlock.Cursor{})
	if string(records[0].Native) != `{"type":"future_event","n":1}` {
		t.Fatalf("unknown frame was not retained verbatim: %s", records[0].Native)
	}
}

func TestSessionHasNoSteerAndDeclaresItsCapabilities(t *testing.T) {
	sess, _ := newCoreSession(t, time.Minute)
	var s any = sess
	if _, ok := s.(oarlock.Steerer); ok {
		t.Fatal("*Session implements Steerer: issue #10 item 5 says it must not")
	}
	if _, ok := s.(oarlock.Withdrawer); ok {
		t.Fatal("*Session implements Withdrawer: held input is not withdrawable here")
	}
	want := oarlock.Capabilities{Queue: oarlock.QueueCapability{Durable: false}, Attribution: oarlock.AttributionNone, Images: false}
	if got := sess.Capabilities(); got != want {
		t.Fatalf("Capabilities() = %+v, want %+v", got, want)
	}
}

func TestSessionEmptyInputAndImagesAreRejectedBeforePi(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)

	out, _ := sess.Prompt(context.Background(), "", oarlock.PromptOptions{})
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.Unsupported {
		t.Fatalf("empty prompt = %+v, want rejected/unsupported", out)
	}
	img := oarlock.PromptOptions{InputOptions: oarlock.InputOptions{Images: []oarlock.InputImage{{Path: "/tmp/a.png"}}}}
	out, _ = sess.Prompt(context.Background(), "look", img)
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.Unsupported {
		t.Fatalf("prompt with images = %+v, want rejected/unsupported", out)
	}
	if n := len(conn.sentTypes()); n != 0 {
		t.Fatalf("%d commands reached Pi for rejected inputs", n)
	}
	assertDense(t, sess.Records(oarlock.Cursor{}))
}

func TestSessionRequiresASessionID(t *testing.T) {
	if _, err := NewSession(newCoreConn(), SessionOptions{}); err == nil {
		t.Fatal("NewSession accepted an empty SessionID")
	}
	if _, err := NewSession(newCoreConn(), SessionOptions{SessionID: "s", AbortGrace: -1}); err == nil {
		t.Fatal("NewSession accepted a negative AbortGrace")
	}
}

// ---- T13: live smoke ---------------------------------------------------------------

func TestSessionLiveSmoke(t *testing.T) {
	if os.Getenv("OAR_PI_LIVE") != "1" {
		t.Skip("set OAR_PI_LIVE=1 to run the live Pi smoke")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	p, err := Launch(ctx, Options{Args: []string{"--mode", "rpc", "--offline", "--no-session", "--no-tools", "--no-mcp", "--no-skills", "--no-prompt-templates", "--no-context-files", "--model", "any-endpoint/workbuddy/deepseek-v4.1-flash"}})
	if err != nil {
		t.Skipf("pi is not launchable here: %v", err)
	}
	var state struct {
		SessionID string `json:"sessionId"`
	}
	if err := p.Do(ctx, Command{Type: "get_state"}, &state); err != nil {
		t.Fatal(err)
	}
	sess, err := NewSession(p, SessionOptions{SessionID: state.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Dispose(context.Background()) }()

	if out, err := sess.Prompt(ctx, "Reply with the single word: ok", oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	coreWait(t, "turn_ended", func() bool { return countEvents(sess.Records(oarlock.Cursor{}), oarlock.EventTurnEnded) == 1 })
	if out, _ := sess.Abort(ctx); out.Code != oarlock.NoActiveTurn {
		t.Fatalf("abort after the turn = %+v, want no_active_turn", out)
	}
	if err := sess.Dispose(ctx); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(sess); st.Kind != fold.StatusExited {
		t.Fatalf("status after dispose = %q, want exited", st.Kind)
	}
}
