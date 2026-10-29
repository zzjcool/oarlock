package pi

// Core state-machine tests for the Pi session control plane (issue #10). The
// fake Conn here is driven by the test: a command parks until the test replies
// to it, and every reply (like the real Conn) satisfies Do first and is then
// pushed on Events as a frame. No process, no network.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
	"github.com/zzjcool/oarlock/internal/redact"
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
	message string // the response's "error" member on a refusal; "" = "nope"
	err     error
}

// errorText is the error member the fake Pi writes, and the text of the RPCError
// it returns, so a test can prove the adapter carries Pi's own words.
func (r fakeReply) errorText() string {
	if r.message == "" {
		return "nope"
	}
	return r.message
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
	// stats scripts the answer to the post-settlement get_session_stats read
	// (issue #11). nil = the transport fails: nothing is queued for the test
	// and no response frame appears, so context stays unreported. The command
	// never reaches cmds, so tests that count prompts are not disturbed.
	stats func() fakeReply

	gone chan struct{} // closed on exit: a parked Do fails with *ExitedError
}

func newCoreConn() *coreConn {
	return &coreConn{
		events:  make(chan Message, 64),
		exit:    make(chan ExitStatus, 1),
		cmds:    make(chan *fakeCmd, 16),
		closeCh: make(chan struct{}, 4),
		gone:    make(chan struct{}),
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
	close(c.gone)
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
	if cmd.Type == "get_session_stats" {
		c.nextID++
		id := fmt.Sprintf("c-%d", c.nextID)
		c.sent = append(c.sent, cmd.Type)
		hook := c.stats
		c.mu.Unlock()
		if hook == nil {
			return errors.New("fake conn: get_session_stats is not scripted")
		}
		r := hook()
		if r.err != nil {
			return r.err
		}
		raw := fmt.Sprintf(`{"type":"response","id":%q,"command":"get_session_stats","success":%v`, id, r.success)
		if r.data != "" {
			raw += `,"data":` + r.data
		}
		if !r.success {
			raw += fmt.Sprintf(`,"error":%q`, r.errorText())
		}
		c.push(raw + `}`)
		if !r.success {
			return &RPCError{Command: cmd.Type, Message: r.errorText()}
		}
		return nil
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
			raw += fmt.Sprintf(`,"error":%q`, r.errorText())
		}
		raw += `}`
		c.push(raw)
		if !r.success {
			return &RPCError{Command: fc.typ, Message: r.errorText()}
		}
		return nil
	case <-c.gone:
		return &ExitedError{Status: ExitStatus{Code: 0, Cause: CauseClosed}}
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
	case <-time.After(15 * time.Second):
		t.Fatalf("session never sent %q", want)
		return nil
	}
}

func (fc *fakeCmd) ok(data string) { fc.reply <- fakeReply{success: true, data: data} }
func (fc *fakeCmd) refuse()        { fc.reply <- fakeReply{success: false} }

// refuseWith answers success:false carrying message as Pi's own error text.
func (fc *fakeCmd) refuseWith(message string) {
	fc.reply <- fakeReply{success: false, message: message}
}

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
	deadline := time.Now().Add(15 * time.Second)
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
// It returns only once that agent_start frame is in the stream: the fold already
// calls the status running for the recorded prompt request, so the status alone
// does not prove the frame landed.
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
	before := len(sess.Records(oarlock.Cursor{}))
	conn.push(`{"type":"agent_start"}`)
	coreWait(t, "running", func() bool { return statusOf(sess).Kind == fold.StatusRunning })
	coreWait(t, "the agent_start frame in the stream", func() bool {
		for _, rec := range sess.Records(oarlock.Cursor{})[before:] {
			if strings.Contains(string(rec.Native), `"type":"agent_start"`) {
				return true
			}
		}
		return false
	})
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

// ---- steer (issue #45) ------------------------------------------------------

// Pi's steer command lands in the running turn: one command, params are the
// message only (the Conn matches the answer by its own id).
func TestSessionSteerInRunningTurnIsAcceptedAndSentOnce(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	startTurn(t, sess, conn, "run bash: sleep 12")

	const steer = "IMPORTANT: your final answer must include the exact word BANANA"
	done := make(chan oarlock.ControlOutcome, 1)
	go func() {
		out, _ := sess.Steer(context.Background(), steer, oarlock.InputOptions{InputID: "in-banana"})
		done <- out
	}()
	fc := conn.next(t, "steer")
	if len(fc.params) != 1 || fc.params["message"] != steer {
		t.Fatalf("steer params = %v, want only message:%q", fc.params, steer)
	}
	if _, has := fc.params["id"]; has {
		t.Fatalf("steer params carry an id key: %v", fc.params)
	}
	fc.ok(`{"disposition":"queued"}`)

	out := <-done
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("steer = %+v, want accepted", out)
	}
	if out.Request.Request.Action != oarlock.ActionSteer || out.Request.Request.Input != steer || out.Request.Request.InputID != "in-banana" {
		t.Fatalf("recorded steer request = %+v, want the input on the request", out.Request.Request)
	}
	if out.Response.Response.Kind != oarlock.ResponseAccepted || out.Response.RequestID != out.RequestID {
		t.Fatalf("recorded steer response = %+v, want accepted for %q", out.Response.Response, out.RequestID)
	}
	if n := conn.countSent("steer"); n != 1 {
		t.Fatalf("steer commands = %d, want exactly 1", n)
	}
	assertDense(t, sess.Records(oarlock.Cursor{}))
}

// No turn to steer into is refused here, with no command on the wire at all:
// Pi would answer success, hold the text in its steering buffer and inject it
// into whatever turn comes next.
func TestSessionSteerWithNoTurnIsRejectedWithoutACommand(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)

	out, err := sess.Steer(context.Background(), "too early", oarlock.InputOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.NoActiveTurn {
		t.Fatalf("steer on an idle session = %+v, want rejected/no_active_turn", out)
	}
	if n := len(conn.sentTypes()); n != 0 {
		t.Fatalf("%d commands reached Pi for a steer with no turn running", n)
	}

	// The same after a real turn ended: the stream says idle again.
	startTurn(t, sess, conn, "quick")
	settle(conn, "stop", false)
	coreWait(t, "idle", func() bool { return statusOf(sess).Kind == fold.StatusIdle })
	out, err = sess.Steer(context.Background(), "too late", oarlock.InputOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.NoActiveTurn {
		t.Fatalf("steer after the turn = %+v, want rejected/no_active_turn", out)
	}
	if n := conn.countSent("steer"); n != 0 {
		t.Fatalf("steer commands = %d, want none: an idle steer is refused locally", n)
	}
	assertDense(t, sess.Records(oarlock.Cursor{}))
}

// success:false is Pi refusing: the rejection carries Pi's own error text and
// the input is never reported as accepted.
func TestSessionSteerRefusedByPiIsRuntimeRefused(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	startTurn(t, sess, conn, "work")

	const refusal = "cannot steer: the session is shutting down"
	done := make(chan oarlock.ControlOutcome, 1)
	go func() {
		out, _ := sess.Steer(context.Background(), "more", oarlock.InputOptions{})
		done <- out
	}()
	conn.next(t, "steer").refuseWith(refusal)

	out := <-done
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.RuntimeRefused || out.Reason != refusal {
		t.Fatalf("refused steer = %+v, want rejected/runtime_refused carrying Pi's %q", out, refusal)
	}
	if out.Response.Response.Kind != oarlock.ResponseRejected || out.Response.Response.Code != oarlock.RuntimeRefused || out.Response.Response.Reason != refusal {
		t.Fatalf("recorded steer response = %+v, want the rejection in the stream", out.Response.Response)
	}
	if st := statusOf(sess); st.Kind != fold.StatusRunning {
		t.Fatalf("status after a refused steer = %q, want the turn still running", st.Kind)
	}
	assertDense(t, sess.Records(oarlock.Cursor{}))
}

func TestSessionSteerAfterExitIsRuntimeExited(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	conn.exitWith(ExitStatus{Code: 1, Cause: CauseExited})
	coreWait(t, "exited", func() bool { return statusOf(sess).Kind == fold.StatusExited })

	out, err := sess.Steer(context.Background(), "gone", oarlock.InputOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.RuntimeExited {
		t.Fatalf("steer after the exit = %+v, want rejected/runtime_exited", out)
	}
	if n := conn.countSent("steer"); n != 0 {
		t.Fatalf("steer commands = %d, want none after the exit", n)
	}
}

// Images are checked on a running turn (the status gate comes first, exactly as
// for a prompt) and travel with the steer: Pi's steer command takes the same
// ImageContent parts as prompt (issue #52), so the part is on the wire and the
// input is accepted.
func TestSessionSteerWithImageCarriesPiImagePart(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	startTurn(t, sess, conn, "look")

	path := writeImage(t, "shot.png", pngFixture)
	done := make(chan oarlock.ControlOutcome, 1)
	go func() {
		out, _ := sess.Steer(context.Background(), "what is this", oarlock.InputOptions{
			Images: []oarlock.InputImage{{Path: path}},
		})
		done <- out
	}()
	fc := conn.next(t, "steer")
	if fc.params["message"] != "what is this" {
		t.Fatalf("steer message = %v, want the text intact", fc.params["message"])
	}
	if got, want := wireImages(t, fc.params), wantImagePart(pngFixture, "image/png"); !reflect.DeepEqual(got, want) {
		t.Fatalf("steer params[images] = %v, want %v", got, want)
	}
	fc.ok(`{"disposition":"queued"}`)

	out := <-done
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("steer with an image = %+v, want accepted", out)
	}
	if imgs := out.Request.Request.Images; len(imgs) != 1 || imgs[0].Path != path {
		t.Fatalf("steer request Images = %+v, want the path", imgs)
	}
	if n := conn.countSent("steer"); n != 1 {
		t.Fatalf("steer commands = %d, want exactly 1", n)
	}
	if st := statusOf(sess); st.Kind != fold.StatusRunning {
		t.Fatalf("status after an accepted image steer = %q, want running", st.Kind)
	}
	records := sess.Records(oarlock.Cursor{})
	assertNoImageBytes(t, records, pngFixture)
	assertDense(t, records)
}

// A successful steer adds its own request, Pi's response frame and the accepted
// response - and nothing else: no turn boundary, no input_handled. The steer is
// not a prompt: Pi's disposition "queued" is not "handled".
func TestSessionSteerAddsNoTurnBoundaryAndNoInputHandled(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	startTurn(t, sess, conn, "long turn")

	// Watch the live tap as well as the retained stream: neither may show a
	// synthesized boundary.
	var mu sync.Mutex
	var live []oarlock.EventKind
	unsub := sess.Events(nil, func(r oarlock.Record) {
		mu.Lock()
		for _, e := range r.Events {
			live = append(live, e.Kind)
		}
		mu.Unlock()
	})
	defer unsub()

	before := len(sess.Records(oarlock.Cursor{}))
	const steer = "also mention BANANA"
	done := make(chan oarlock.ControlOutcome, 1)
	go func() {
		out, _ := sess.Steer(context.Background(), steer, oarlock.InputOptions{})
		done <- out
	}()
	conn.next(t, "steer").ok(`{"disposition":"queued"}`)
	if out := <-done; out.Kind != oarlock.ControlAccepted {
		t.Fatalf("steer = %+v, want accepted", out)
	}

	coreWait(t, "the steer response in the stream", func() bool { return len(sess.Records(oarlock.Cursor{})) >= before+3 })
	records := sess.Records(oarlock.Cursor{})
	if n := len(records) - before; n != 3 {
		t.Fatalf("the steer added %d records, want 3 (request, the native response frame and the response; the Conn may append the frame after the response record, so the order is not asserted): %v", n, kinds(records[before:]))
	}
	tail := records[before:]
	var request, response *oarlock.Record
	frames := 0
	for i := range tail {
		switch tail[i].Kind {
		case oarlock.KindRequest:
			request = &tail[i]
		case oarlock.KindResponse:
			response = &tail[i]
		case oarlock.KindFrame:
			frames++
			if !strings.Contains(string(tail[i].Native), `"command":"steer"`) {
				t.Fatalf("the steer frame = %s, want Pi's own steer response", tail[i].Native)
			}
		}
	}
	if request == nil || request.Request.Action != oarlock.ActionSteer || request.Request.Input != steer {
		t.Fatalf("steer request = %+v, want the steer input", request)
	}
	if response == nil || response.Response.Kind != oarlock.ResponseAccepted || response.RequestID != request.ID {
		t.Fatalf("steer response = %+v, want accepted for %q", response, request.ID)
	}
	if frames != 1 {
		t.Fatalf("native frames after the steer = %d, want exactly Pi's response", frames)
	}

	for _, kind := range []oarlock.EventKind{oarlock.EventTurnEnded, oarlock.EventTurnActive, oarlock.EventInputHandled} {
		if n := countEvents(tail, kind); n != 0 {
			t.Fatalf("a steer wrote %d %s records/events", n, kind)
		}
	}
	if n := countEvents(records, oarlock.EventTurnActive); n != 1 {
		t.Fatalf("turn_active events = %d, want only the prompt's one", n)
	}
	if n := countEvents(records, oarlock.EventTurnEnded); n != 0 {
		t.Fatalf("turn_ended events = %d, want none: nothing ends the turn for a steer", n)
	}
	if n := countEvents(records, oarlock.EventInputHandled); n != 0 {
		t.Fatalf("input_handled events = %d, want none: disposition queued is not handled", n)
	}
	mu.Lock()
	delivered := append([]oarlock.EventKind(nil), live...)
	mu.Unlock()
	for _, kind := range delivered {
		switch kind {
		case oarlock.EventTurnEnded, oarlock.EventTurnActive, oarlock.EventInputHandled:
			t.Fatalf("the live tap saw a %s event for a steer: %v", kind, delivered)
		}
	}
	if st := statusOf(sess); st.Kind != fold.StatusRunning {
		t.Fatalf("status after the steer = %q, want the turn still running", st.Kind)
	}
	assertDense(t, records)
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

func TestSessionIsASteererAndDeclaresItsCapabilities(t *testing.T) {
	sess, _ := newCoreSession(t, time.Minute)
	var s any = sess
	if _, ok := s.(oarlock.Steerer); !ok {
		t.Fatal("*Session must implement Steerer: Pi's steer command lands in the running turn (issue #45)")
	}
	// Withdrawer belongs to withdraw_test.go, so each worker's tree builds on
	// its own; images are native as of issue #52.
	want := oarlock.Capabilities{Queue: oarlock.QueueCapability{Durable: false}, Attribution: oarlock.AttributionNone, Images: true}
	if got := sess.Capabilities(); got != want {
		t.Fatalf("Capabilities() = %+v, want %+v", got, want)
	}
}

// Empty text with no images stays refused by the kernel before Pi sees
// anything; an image is enough of an input on its own (issue #52), so the old
// "images are unsupported" half of this test is replaced by the wire shape
// asserted in images_test.go.
func TestSessionEmptyInputIsRejectedBeforePiAndAnImageIsNot(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)

	out, _ := sess.Prompt(context.Background(), "", oarlock.PromptOptions{})
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.Unsupported {
		t.Fatalf("empty prompt = %+v, want rejected/unsupported", out)
	}
	if q, _ := sess.Queue(context.Background(), "", oarlock.InputOptions{}); q.Kind != oarlock.ControlRejected || q.Code != oarlock.Unsupported {
		t.Fatalf("empty queue = %+v, want rejected/unsupported", q)
	}
	if n := len(conn.sentTypes()); n != 0 {
		t.Fatalf("%d commands reached Pi for empty inputs", n)
	}

	// No text, one image: the prompt goes, with message "" and the part.
	path := writeImage(t, "shot.png", pngFixture)
	done := make(chan oarlock.ControlOutcome, 1)
	go func() {
		out, _ := sess.Prompt(context.Background(), "", oarlock.PromptOptions{
			InputOptions: oarlock.InputOptions{Images: []oarlock.InputImage{{Path: path}}},
		})
		done <- out
	}()
	fc := conn.next(t, "prompt")
	if msg, has := fc.params["message"]; !has || msg != "" {
		t.Fatalf("image-only prompt message = %v (present %v), want \"\"", msg, has)
	}
	if parts := wireImages(t, fc.params); len(parts) != 1 {
		t.Fatalf("image-only prompt carries %d image parts, want 1", len(parts))
	}
	fc.ok(`{"disposition":"started"}`)
	if out := <-done; out.Kind != oarlock.ControlAccepted {
		t.Fatalf("image-only prompt = %+v, want accepted", out)
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

// ---- MINOR coverage from the independent review ---------------------------------

// A prompt parked in Do when the session is disposed is released with
// runtime_exited (the Conn fails every pending command on exit), not leaked.
func TestSessionDisposeReleasesAParkedPrompt(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)

	done := make(chan oarlock.ControlOutcome, 1)
	go func() {
		out, _ := sess.Prompt(context.Background(), "parked", oarlock.PromptOptions{})
		done <- out
	}()
	conn.next(t, "prompt") // Pi never answers.
	if err := sess.Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if out.Kind != oarlock.ControlRejected || out.Code != oarlock.RuntimeExited {
			t.Fatalf("parked prompt = %+v, want rejected/runtime_exited", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the parked prompt was leaked by Dispose")
	}
	assertDense(t, sess.Records(oarlock.Cursor{}))
}

// The abort command is withheld until agent_start (Pi answers an earlier abort
// with success and then runs the turn anyway); once the run starts it goes out.
func TestSessionAbortIsSentOnlyOnceTheRunHasStarted(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)

	done := make(chan oarlock.ControlOutcome, 1)
	go func() {
		out, _ := sess.Prompt(context.Background(), "slow start", oarlock.PromptOptions{})
		done <- out
	}()
	conn.next(t, "prompt").ok(`{"disposition":"started"}`)
	<-done // accepted, status running, agent_start not seen yet

	aborted := make(chan oarlock.ControlOutcome, 1)
	go func() {
		out, _ := sess.Abort(context.Background())
		aborted <- out
	}()
	time.Sleep(100 * time.Millisecond)
	if n := conn.countSent("abort"); n != 0 {
		t.Fatalf("abort was sent %d time(s) before agent_start", n)
	}
	conn.push(`{"type":"agent_start"}`)
	ack := conn.next(t, "abort")
	settle(conn, "aborted", true)
	ack.ok("")
	if out := <-aborted; out.Kind != oarlock.ControlAccepted {
		t.Fatalf("abort = %+v, want accepted", out)
	}
	if conn.closeCount() != 0 {
		t.Fatal("a healthy Pi that started within the grace must not be killed")
	}
}

// The exit Reason is the raw stderr tail; the kernel redacts it before retention.
func TestSessionExitReasonIsRedactedByTheKernel(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	conn := newCoreConn()
	red := redact.New([]string{"hunter2-super-secret"})
	sess, err := NewSession(conn, SessionOptions{SessionID: "sess-r", Redact: red.Record})
	if err != nil {
		t.Fatal(err)
	}
	conn.exitWith(ExitStatus{Code: 1, Cause: CauseExited, StderrTail: "auth failed with token hunter2-super-secret"})
	coreWait(t, "exited", func() bool { return statusOf(sess).Kind == fold.StatusExited })
	for _, r := range sess.Records(oarlock.Cursor{}) {
		if r.Kind == oarlock.KindResponse && r.Response.Kind == oarlock.ResponseExited {
			if strings.Contains(r.Response.Reason, "hunter2-super-secret") {
				t.Fatalf("the secret reached the stream: %q", r.Response.Reason)
			}
			return
		}
	}
	t.Fatal("no exited response")
}
