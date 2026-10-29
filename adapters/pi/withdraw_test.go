package pi

// Withdrawer contract tests for the Pi adapter (issue #52, contract §3 items
// 7-11). The queue belongs to the adapter, so taking an input back is one
// record pair plus an edit of the held list: no command on the Conn, and the
// queue's own request/response are never rewritten.
//
// The fake Conn and the helpers (newCoreSession, conn.next, startTurn, settle,
// coreWait, statusOf, assertDense, watchdog) come from session_test.go: no
// process, no network.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

// ---- withdraw helpers ------------------------------------------------------

// withdrawPair is a retained withdraw request and the response answering it.
type withdrawPair struct {
	req  oarlock.Record
	resp oarlock.Record
}

// withdrawPairs returns every withdraw request in the stream, in order, with the
// response that points at it (the zero Record when the request has none).
func withdrawPairs(records []oarlock.Record) (out []withdrawPair) {
	for _, r := range records {
		if r.Kind != oarlock.KindRequest || r.Request == nil || r.Request.Action != oarlock.ActionWithdraw {
			continue
		}
		p := withdrawPair{req: r}
		for _, s := range records {
			if s.Kind == oarlock.KindResponse && s.RequestID == r.ID {
				p.resp = s
				break
			}
		}
		out = append(out, p)
	}
	return out
}

// withdrawRecordJSON is the record exactly as it is retained, so two of them
// compare equal iff nothing about the record changed.
func withdrawRecordJSON(t *testing.T, r oarlock.Record, what string) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal the %s record %d: %v", what, r.Seq, err)
	}
	return b
}

// withdrawQueuePair returns the retained queue request for inputID and the
// response answering it.
func withdrawQueuePair(t *testing.T, records []oarlock.Record, inputID string) (oarlock.Record, oarlock.Record) {
	t.Helper()
	for _, r := range records {
		if r.Kind != oarlock.KindRequest || r.Request == nil ||
			r.Request.Action != oarlock.ActionQueue || r.Request.InputID != inputID {
			continue
		}
		for _, s := range records {
			if s.Kind == oarlock.KindResponse && s.RequestID == r.ID {
				return r, s
			}
		}
		t.Fatalf("the queue request for inputId %q has no retained response", inputID)
	}
	t.Fatalf("no retained queue request for inputId %q", inputID)
	return oarlock.Record{}, oarlock.Record{}
}

// withdrawPromptInputs is the Input of every prompt request in the stream, in
// order: what the run was asked to do, as the stream recorded it.
func withdrawPromptInputs(records []oarlock.Record) (out []string) {
	for _, r := range records {
		if r.Kind == oarlock.KindRequest && r.Request != nil && r.Request.Action == oarlock.ActionPrompt {
			out = append(out, r.Request.Input)
		}
	}
	return out
}

func withdrawHeldLen(s *Session) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.held)
}

func withdrawDraining(s *Session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

// withdrawAwaitSettled waits until a turn end has been fully processed: the
// stream says idle, the post-settlement stats read that releases the queue
// drain has been written to the Conn, and no drain is running. Then it leaves a
// late drain time to send, so "nothing was sent" is proved, not raced.
func withdrawAwaitSettled(t *testing.T, sess *Session, conn *coreConn) {
	t.Helper()
	coreWait(t, "idle", func() bool { return statusOf(sess).Kind == fold.StatusIdle })
	coreWait(t, "the post-settlement stats read that releases the queue drain", func() bool {
		return conn.countSent("get_session_stats") > 0
	})
	coreWait(t, "the drain the settlement kicks", func() bool { return !withdrawDraining(sess) })
	time.Sleep(50 * time.Millisecond)
}

// ---- item 7: the interface is the capability -------------------------------

func TestSessionIsAWithdrawer(t *testing.T) {
	sess, _ := newCoreSession(t, time.Minute)
	var s any = sess
	if _, ok := s.(oarlock.Withdrawer); !ok {
		t.Fatal("*Session must implement oarlock.Withdrawer: it holds the queue itself, so it can take input back")
	}
	// Withdraw is an interface, not a capability flag; the queue it edits is
	// still the adapter's (Capabilities as a whole is asserted in
	// session_test.go, which owns the images field).
	if got := sess.Capabilities().Queue; got != (oarlock.QueueCapability{Durable: false}) {
		t.Fatalf("Queue capability = %+v, want adapter-held (Durable false): withdraw is possible because the queue is ours", got)
	}
}

// ---- item 8: withdraw of an input held while a turn runs ---------------------

func TestSessionWithdrawRemovesAHeldInputWithoutTouchingTheWire(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	startTurn(t, sess, conn, "the running turn")

	const (
		inputID = "in-withdrawn"
		marker  = "WITHDRAWN-PAYLOAD-must-never-be-sent"
	)
	queued, err := sess.Queue(context.Background(), marker, oarlock.InputOptions{InputID: inputID, Origin: "host"})
	if err != nil {
		t.Fatal(err)
	}
	if queued.Kind != oarlock.ControlAccepted {
		t.Fatalf("Queue = %+v, want accepted while a turn is running", queued)
	}
	if n := withdrawHeldLen(sess); n != 1 {
		t.Fatalf("held inputs = %d, want the queued one", n)
	}

	// The queue pair as it must stay: a withdraw never rewrites it.
	queueReq, queueResp := withdrawQueuePair(t, sess.Records(oarlock.Cursor{}), inputID)
	queueReqBefore := withdrawRecordJSON(t, queueReq, "queue request")
	queueRespBefore := withdrawRecordJSON(t, queueResp, "queue response")

	sentBefore := len(conn.sentTypes())
	withdrawn, err := sess.Withdraw(context.Background(), inputID)
	if err != nil {
		t.Fatal(err)
	}
	if withdrawn.Kind != oarlock.ControlAccepted {
		t.Fatalf("Withdraw = %+v, want accepted for a held input", withdrawn)
	}
	if n := len(conn.sentTypes()); n != sentBefore {
		t.Fatalf("commands on the Conn went %d -> %d: Pi has no withdraw command, the queue is the adapter's", sentBefore, n)
	}
	if n := withdrawHeldLen(sess); n != 0 {
		t.Fatalf("held inputs after the withdraw = %d, want 0", n)
	}

	// One withdraw request, in the frozen body shape (action + inputId only),
	// and one accepted response pointing at it.
	records := sess.Records(oarlock.Cursor{})
	pairs := withdrawPairs(records)
	if len(pairs) != 1 {
		t.Fatalf("withdraw requests = %d, want exactly 1", len(pairs))
	}
	req, resp := pairs[0].req, pairs[0].resp
	if req.Direction != oarlock.ToRuntime || req.Request == nil {
		t.Fatalf("withdraw request record = %+v, want a toRuntime request", req)
	}
	if body, err := json.Marshal(req.Request); err != nil {
		t.Fatalf("marshal the withdraw request body: %v", err)
	} else if got, want := string(body), `{"action":"withdraw","inputId":"in-withdrawn"}`; got != want {
		t.Fatalf("withdraw request body = %s, want %s", got, want)
	}
	if resp.Kind != oarlock.KindResponse || resp.RequestID != req.ID {
		t.Fatalf("withdraw response = %+v, want a response pointing at %q", resp, req.ID)
	}
	if resp.Response == nil || resp.Response.Kind != oarlock.ResponseAccepted || resp.Response.Code != "" || resp.Response.Reason != "" {
		t.Fatalf("withdraw response body = %+v, want a bare accepted", resp.Response)
	}
	if withdrawn.RequestID != req.ID || withdrawn.Request.Seq != req.Seq || withdrawn.Response.Seq != resp.Seq {
		t.Fatalf("ControlOutcome = %+v, want the retained pair (%q at seq %d, response at seq %d)",
			withdrawn, req.ID, req.Seq, resp.Seq)
	}

	// The queue's own pair is byte-identical: same ids, same seqs, same bodies.
	queueReq2, queueResp2 := withdrawQueuePair(t, sess.Records(oarlock.Cursor{}), inputID)
	if got := withdrawRecordJSON(t, queueReq2, "queue request"); !bytes.Equal(got, queueReqBefore) {
		t.Fatalf("the queue request was rewritten by the withdraw:\nbefore %s\n after %s", queueReqBefore, got)
	}
	if got := withdrawRecordJSON(t, queueResp2, "queue response"); !bytes.Equal(got, queueRespBefore) {
		t.Fatalf("the queue response was rewritten by the withdraw:\nbefore %s\n after %s", queueRespBefore, got)
	}

	// The turn ends; the withdrawn input is drained into nothing.
	settle(conn, "stop", false)
	withdrawAwaitSettled(t, sess, conn)
	if n := conn.countSent("prompt"); n != 1 {
		t.Fatalf("prompt commands = %d, want only the running turn's own (%v): the withdrawn input must never be drained", n, conn.sentTypes())
	}
	records = sess.Records(oarlock.Cursor{})
	for _, in := range withdrawPromptInputs(records) {
		if in == marker {
			t.Fatalf("a prompt for the withdrawn input was recorded: %q", in)
		}
	}
	assertDense(t, records)
}

// ---- item 9: withdraw after the input was already sent ----------------------

func TestSessionWithdrawAfterTheInputWasSentIsNotQueued(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	startTurn(t, sess, conn, "A")

	const (
		inputID = "in-sent"
		marker  = "SENT-PAYLOAD"
	)
	if out, err := sess.Queue(context.Background(), marker, oarlock.InputOptions{InputID: inputID}); err != nil {
		t.Fatal(err)
	} else if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Queue = %+v, want accepted", out)
	}

	// The turn ends and the held input drains into a prompt of its own.
	settle(conn, "stop", false)
	fc := conn.next(t, "prompt")
	if fc.params["message"] != marker {
		t.Fatalf("drained %v, want the queued %q", fc.params["message"], marker)
	}
	fc.ok(`{"disposition":"started"}`)
	conn.push(`{"type":"agent_start"}`)
	coreWait(t, "running again", func() bool { return statusOf(sess).Kind == fold.StatusRunning })

	before := sess.Records(oarlock.Cursor{})
	if got := withdrawPromptInputs(before); len(got) != 2 || got[1] != marker {
		t.Fatalf("prompt requests = %v, want the running turn and the drained input", got)
	}
	sentBefore := len(conn.sentTypes())

	late, err := sess.Withdraw(context.Background(), inputID)
	if err != nil {
		t.Fatal(err)
	}
	if late.Kind != oarlock.ControlRejected || late.Code != oarlock.NotQueued {
		t.Fatalf("Withdraw of an input already sent = %+v, want rejected/not_queued", late)
	}
	const reason = "no held input with this inputId is waiting"
	if late.Reason != reason || late.Response.Response.Reason != reason {
		t.Fatalf("not_queued reason = %q (recorded %q), want %q", late.Reason, late.Response.Response.Reason, reason)
	}
	if n := len(conn.sentTypes()); n != sentBefore {
		t.Fatalf("commands on the Conn went %d -> %d for a rejected withdraw", sentBefore, n)
	}

	// Nothing disappears: the sent input's own prompt request is still there,
	// and the rejection is a pair of its own in the stream.
	after := sess.Records(oarlock.Cursor{})
	if got := withdrawPromptInputs(after); len(got) != 2 || got[1] != marker {
		t.Fatalf("prompt requests after the rejected withdraw = %v, want the sent input still recorded", got)
	}
	pairs := withdrawPairs(after)
	if len(pairs) != 1 {
		t.Fatalf("withdraw requests = %d, want the one rejected request", len(pairs))
	}
	if pairs[0].resp.Response == nil || pairs[0].resp.Response.Kind != oarlock.ResponseRejected ||
		pairs[0].resp.Response.Code != oarlock.NotQueued {
		t.Fatalf("recorded withdraw response = %+v, want rejected/not_queued in the stream", pairs[0].resp.Response)
	}
	assertDense(t, after)
}

// ---- item 10: unknown id and the already-withdrawn id -----------------------

func TestSessionWithdrawUnknownAndAlreadyWithdrawnAreNotQueued(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	startTurn(t, sess, conn, "A")
	if out, err := sess.Queue(context.Background(), "held", oarlock.InputOptions{InputID: "in-held"}); err != nil {
		t.Fatal(err)
	} else if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Queue = %+v, want accepted", out)
	}

	sentBefore := len(conn.sentTypes())
	unknown, err := sess.Withdraw(context.Background(), "in-never-queued")
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Kind != oarlock.ControlRejected || unknown.Code != oarlock.NotQueued {
		t.Fatalf("Withdraw of an unknown id = %+v, want rejected/not_queued", unknown)
	}
	if n := withdrawHeldLen(sess); n != 1 {
		t.Fatalf("held inputs after a not_queued withdraw = %d, want the unrelated one untouched", n)
	}

	first, err := sess.Withdraw(context.Background(), "in-held")
	if err != nil {
		t.Fatal(err)
	}
	if first.Kind != oarlock.ControlAccepted {
		t.Fatalf("Withdraw of the held id = %+v, want accepted", first)
	}
	again, err := sess.Withdraw(context.Background(), "in-held")
	if err != nil {
		t.Fatal(err)
	}
	if again.Kind != oarlock.ControlRejected || again.Code != oarlock.NotQueued {
		t.Fatalf("second Withdraw of the same id = %+v, want rejected/not_queued (already withdrawn)", again)
	}
	if n := len(conn.sentTypes()); n != sentBefore {
		t.Fatalf("commands on the Conn went %d -> %d across three withdraws", sentBefore, n)
	}

	records := sess.Records(oarlock.Cursor{})
	pairs := withdrawPairs(records)
	if len(pairs) != 3 {
		t.Fatalf("withdraw requests = %d, want 3 (unknown, accepted, already withdrawn)", len(pairs))
	}
	want := []oarlock.ResponseKind{oarlock.ResponseRejected, oarlock.ResponseAccepted, oarlock.ResponseRejected}
	for i, p := range pairs {
		if p.resp.Response == nil || p.resp.Response.Kind != want[i] {
			t.Fatalf("withdraw #%d response = %+v, want %s", i+1, p.resp.Response, want[i])
		}
	}
	if got := withdrawHeldLen(sess); got != 0 {
		t.Fatalf("held inputs = %d, want 0 after the accepted withdraw", got)
	}

	// The turn ends and the withdrawn input is not sent.
	settle(conn, "stop", false)
	withdrawAwaitSettled(t, sess, conn)
	if n := conn.countSent("prompt"); n != 1 {
		t.Fatalf("prompt commands = %d, want only the running turn's own (%v)", n, conn.sentTypes())
	}
	assertDense(t, sess.Records(oarlock.Cursor{}))
}

// ---- the same inputID held twice is fully removed ---------------------------

// One inputID names one logical input, so a second queue under the same id must
// not survive the withdraw: OAR's withdrawHeld removes every match, and a
// leftover copy would be drained into a prompt the caller believes it took back.
func TestSessionWithdrawRemovesEveryCopyOfTheSameInputID(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	startTurn(t, sess, conn, "A")

	const inputID = "in-duplicate"
	for _, in := range []string{"DUPLICATE-PAYLOAD-1", "DUPLICATE-PAYLOAD-2"} {
		out, err := sess.Queue(context.Background(), in, oarlock.InputOptions{InputID: inputID})
		if err != nil {
			t.Fatal(err)
		}
		if out.Kind != oarlock.ControlAccepted {
			t.Fatalf("Queue(%q) = %+v, want accepted", in, out)
		}
	}
	if n := withdrawHeldLen(sess); n != 2 {
		t.Fatalf("held inputs = %d, want both copies under the same inputId", n)
	}

	out, err := sess.Withdraw(context.Background(), inputID)
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Withdraw = %+v, want accepted", out)
	}
	if n := withdrawHeldLen(sess); n != 0 {
		t.Fatalf("held inputs after the withdraw = %d, want 0: every copy must go", n)
	}
	again, err := sess.Withdraw(context.Background(), inputID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Kind != oarlock.ControlRejected || again.Code != oarlock.NotQueued {
		t.Fatalf("second Withdraw = %+v, want rejected/not_queued", again)
	}

	settle(conn, "stop", false)
	withdrawAwaitSettled(t, sess, conn)
	if n := conn.countSent("prompt"); n != 1 {
		t.Fatalf("prompt commands = %d, want only the running turn's own (%v): both copies were withdrawn", n, conn.sentTypes())
	}
	records := sess.Records(oarlock.Cursor{})
	if got := withdrawPromptInputs(records); fmt.Sprint(got) != "[A]" {
		t.Fatalf("prompt requests = %v, want [A] only", got)
	}
	assertDense(t, records)
}

// ---- item 11: after the process exit ---------------------------------------

// The exit gate wins over the decision: an input still held when the process
// died is not reported as withdrawn, because the runtime is gone and the stream
// says so.
func TestSessionWithdrawAfterTheProcessExitIsRuntimeExited(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	startTurn(t, sess, conn, "doomed")
	if out, err := sess.Queue(context.Background(), "never sent", oarlock.InputOptions{InputID: "in-orphan"}); err != nil {
		t.Fatal(err)
	} else if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Queue = %+v, want accepted", out)
	}

	conn.exitWith(ExitStatus{Code: 1, Cause: CauseExited})
	coreWait(t, "exited", func() bool { return statusOf(sess).Kind == fold.StatusExited })
	sentBefore := len(conn.sentTypes())

	out, err := sess.Withdraw(context.Background(), "in-orphan")
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.RuntimeExited {
		t.Fatalf("Withdraw after the exit = %+v, want rejected/runtime_exited", out)
	}
	if n := len(conn.sentTypes()); n != sentBefore {
		t.Fatalf("commands on the Conn went %d -> %d for a withdraw after the exit", sentBefore, n)
	}
	pairs := withdrawPairs(sess.Records(oarlock.Cursor{}))
	if len(pairs) != 1 {
		t.Fatalf("withdraw requests = %d, want the one refused by the exit gate", len(pairs))
	}
	if pairs[0].resp.Response == nil || pairs[0].resp.Response.Code != oarlock.RuntimeExited {
		t.Fatalf("recorded withdraw response = %+v, want the exit gate's rejection in the stream", pairs[0].resp.Response)
	}
	assertDense(t, sess.Records(oarlock.Cursor{}))
}
