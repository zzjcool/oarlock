package pi

// Live evidence for issue #52 (image input + Withdraw) against the real Pi on
// this host: pi 1.4.0, model any-endpoint/workbuddy/deepseek-v4.1-flash. Both
// tests are opt-in on OAR_PI_LIVE=1 and skip otherwise, so the default suite
// stays offline and deterministic.
//
// What may be green here is only what the runtime actually did:
//
//   - the image prompt is green only when a real turn completed; a refusal or a
//     failed turn skips with Pi's own words (this endpoint may not take images),
//     it never asserts success on the endpoint's behalf.
//   - the withdraw proof is absence on the wire (no prompt request record, no
//     user_message event and no native frame carrying the marker) plus the
//     adapter's own rejections, and a queue that still drains afterwards. A run
//     that settled before the held input landed is retried once with a longer
//     turn and then skipped; the assertion is never weakened to a pass.
//
// The tests log the frames the evidence file quotes; they write no files and
// carry no evidence paths of their own.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/observe"
)

// liveInputPNGBase64 is a real 8x8 PNG, every pixel #dc141e. The bytes are
// hardcoded so the test needs no fixture file, no encoder and no dependency;
// liveInputPNG decodes them once and proves they are a PNG of that size before
// Pi sees anything.
const liveInputPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAgAAAAICAIAAABLbSncAAAAGUlEQVR4nGK5IyLHgA0wYRUdtBKAAAAA///a8gEhasmLyQAAAABJRU5ErkJggg=="

func liveInputPNG(t *testing.T) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(liveInputPNGBase64)
	if err != nil {
		t.Fatalf("the hardcoded image is not base64: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the hardcoded bytes are not a PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 8 || b.Dy() != 8 {
		t.Fatalf("the hardcoded PNG is %dx%d, want 8x8", b.Dx(), b.Dy())
	}
	return raw
}

// TestLiveImagePrompt is the opt-in, real-Pi proof of issue #52's image path: a
// prompt carrying one real PNG is accepted, the retained request keeps the path
// and never the bytes, and a real turn completes. If Pi rejects the prompt or
// the turn fails, the endpoint does not take the image here and the test skips
// with the runtime's own words.
func TestLiveImagePrompt(t *testing.T) {
	if os.Getenv("OAR_PI_LIVE") != "1" {
		t.Skip("set OAR_PI_LIVE=1 to send an image prompt to real pi")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	pngBytes := liveInputPNG(t)
	path := filepath.Join(t.TempDir(), "red.png")
	if err := os.WriteFile(path, pngBytes, 0o600); err != nil {
		t.Fatalf("writing the test image: %v", err)
	}
	// The one string that must never reach a request record.
	encoded := base64.StdEncoding.EncodeToString(pngBytes)

	opened, err := Open(ctx, oarlock.Installation{}, oarlock.Options{Cwd: t.TempDir()})
	if err != nil {
		t.Skipf("pi could not be opened here: %v", err)
	}
	sess := opened.(*Session)
	defer func() { _ = sess.Dispose(context.Background()) }()
	if !sess.Capabilities().Images {
		t.Fatalf("Capabilities().Images = false: the adapter must declare image input")
	}

	const input = "What colour is this image? Answer with one word."
	out, err := sess.Prompt(ctx, input, oarlock.PromptOptions{
		InputOptions: oarlock.InputOptions{Images: []oarlock.InputImage{{Path: path}}},
	})
	if err != nil {
		t.Fatalf("Prompt with an image: %v", err)
	}
	if out.Kind != oarlock.ControlAccepted {
		// Pi's own words, never a forged pass: the endpoint may not take images.
		t.Skipf("pi rejected the image prompt: %s: %s", out.Code, out.Reason)
	}
	if imgs := out.Request.Request.Images; len(imgs) != 1 || imgs[0].Path != path || imgs[0].MediaType != "" {
		t.Fatalf("the recorded prompt request keeps %+v, want exactly the path %q and no bytes", imgs, path)
	}
	t.Logf("prompt accepted: request seq=%d id=%s images=%+v", out.Seq, out.RequestID, out.Request.Request.Images)

	deadline := time.Now().Add(5 * time.Minute)
	liveInputWait(t, deadline, "the image turn to end", func() bool {
		return liveInputCountEvents(sess.Records(oarlock.Cursor{}), oarlock.EventTurnEnded) >= 1
	})
	records := sess.Records(oarlock.Cursor{})
	liveInputDump(t, "image prompt run", records)

	outcome := liveInputFirstOutcome(records)
	if outcome == nil {
		t.Fatalf("the turn ended with no turn_ended outcome: %v", liveInputRecordLines(records))
	}
	if outcome.Kind != oarlock.OutcomeCompleted {
		if outcome.Failure == oarlock.FailureRuntimeExited {
			t.Fatalf("the image turn ended failed because the runtime exited: %+v", outcome)
		}
		// The endpoint's own report, not our guess: unverified, so skip.
		t.Skipf("the image turn did not complete, so this endpoint does not take the image here: outcome=%s failure=%s status=%d reason=%q",
			outcome.Kind, outcome.Failure, outcome.Status, outcome.Reason)
	}

	// The retained record keeps the path; base64 may appear only in Pi's own
	// frames, if at all (contract §1.4). A request record carrying it is the bug.
	for _, r := range records {
		if r.Kind != oarlock.KindRequest {
			continue
		}
		if body := liveInputJSON(t, r); strings.Contains(body, encoded) {
			t.Fatalf("request record seq=%d carries the image base64: the record must keep only the path", r.Seq)
		}
	}
	var framesWithBytes []uint64
	for _, r := range records {
		if r.Kind == oarlock.KindFrame && strings.Contains(string(r.Native), encoded) {
			framesWithBytes = append(framesWithBytes, r.Seq)
		}
	}
	t.Logf("frames carrying the image base64 verbatim (pi's own words, never the record's): %v", framesWithBytes)

	answer := liveInputAnswer(records, 0)
	t.Logf("assistant answer to %q: %q", input, answer)
	if strings.TrimSpace(answer) == "" {
		t.Fatalf("the completed image turn streamed no assistant text")
	}
}

// TestLiveQueueAndWithdraw is the opt-in, real-Pi proof of issue #52's withdraw:
// an input queued into the adapter's own queue while a real turn runs is taken
// back by Withdraw, accepted, with nothing on the wire; after the run settles
// the marker never reached Pi; a second withdraw of the same id is
// rejected{not_queued}; and the queue itself still drains a later input into a
// real prompt of its own.
func TestLiveQueueAndWithdraw(t *testing.T) {
	if os.Getenv("OAR_PI_LIVE") != "1" {
		t.Skip("set OAR_PI_LIVE=1 to drive Queue and Withdraw against real pi")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	opened, err := Open(ctx, oarlock.Installation{}, oarlock.Options{Cwd: t.TempDir()})
	if err != nil {
		t.Skipf("pi could not be opened here: %v", err)
	}
	sess := opened.(*Session)
	defer func() { _ = sess.Dispose(context.Background()) }()
	if _, ok := any(sess).(oarlock.Withdrawer); !ok {
		t.Fatalf("the live session does not implement oarlock.Withdrawer")
	}
	deadline := time.Now().Add(11 * time.Minute)

	// A bash `sleep` the model runs itself holds the run open long enough for the
	// held input to be withdrawn: open.go's argv carries no --no-tools, and the
	// issue #45 live run measured Pi executing a bash turn under it. The second
	// attempt is the same shape with a longer sleep, for a first run that settled
	// before the held input landed.
	var (
		marker  string
		inputID string
		held    bool
		missed  []string
	)
	for i, a := range []struct{ marker, inputID, hold string }{
		{"REPLY WITH THE SINGLE WORD ZUCCHINI", "live-withdraw-1",
			"Run this exact bash command and wait for it to finish, then reply with the single word: done. Command: sleep 8"},
		{"REPLY WITH THE SINGLE WORD BROCCOLI", "live-withdraw-2",
			"Run this exact bash command and wait for it to finish, then reply with the single word: done. Command: sleep 30"},
	} {
		obs, ok := liveInputHoldAndWithdraw(t, ctx, sess, deadline, a.hold, a.marker, a.inputID)
		if ok {
			marker, inputID, held = a.marker, a.inputID, true
			break
		}
		t.Logf("hold/withdraw attempt %d came back not_queued: %s", i+1, obs)
		missed = append(missed, obs)
	}
	if !held {
		// Both attempts raced the settlement. That is not a pass and not a
		// failure of the adapter: skip with the frames observed.
		t.Skipf("the run settled before the held input could be withdrawn, on both attempts; frames observed: %s", strings.Join(missed, " || "))
	}
	t.Logf("held input %q (%s) was withdrawn while the run still held it", marker, inputID)

	// The same id again: already withdrawn is not_queued, never accepted.
	again, err := sess.Withdraw(ctx, inputID)
	if err != nil {
		t.Fatalf("the second Withdraw: %v", err)
	}
	if again.Kind != oarlock.ControlRejected || again.Code != oarlock.NotQueued {
		t.Fatalf("the second Withdraw(%q) = %+v, want rejected{%s}", inputID, again, oarlock.NotQueued)
	}
	if again.Response.Response == nil || again.Response.Response.Kind != oarlock.ResponseRejected || again.Response.Response.Reason == "" {
		t.Fatalf("the second withdraw's recorded response = %+v, want the rejection in the stream", again.Response.Response)
	}
	t.Logf("second Withdraw(%q): %+v reason=%q requestSeq=%d responseSeq=%d", inputID, again.Kind, again.Reason, again.Request.Seq, again.Response.Seq)

	// The queue itself still works: an input queued while idle drains into a real
	// prompt with its own turn, and the drained request keeps the text and the id.
	const (
		queueMarker = "REPLY WITH THE SINGLE WORD PINEAPPLE"
		queueID     = "live-queue-2"
	)
	endedBefore := liveInputCountEvents(sess.Records(oarlock.Cursor{}), oarlock.EventTurnEnded)
	drained, err := sess.Queue(ctx, queueMarker, oarlock.InputOptions{InputID: queueID})
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if drained.Kind != oarlock.ControlAccepted {
		t.Fatalf("Queue(%q) = %+v, want accepted", queueID, drained)
	}
	liveInputWait(t, deadline, "the held input to drain into a prompt", func() bool {
		return liveInputPromptFor(sess.Records(oarlock.Cursor{}), queueMarker) != nil
	})
	records := sess.Records(oarlock.Cursor{})
	prompt := liveInputPromptFor(records, queueMarker)
	if prompt == nil {
		t.Fatalf("no prompt request record carries the drained input %q", queueMarker)
	}
	response := liveInputResponseFor(records, prompt.ID)
	if response == nil || response.Response == nil || response.Response.Kind != oarlock.ResponseAccepted {
		t.Fatalf("the drained prompt %s has no accepted response in the stream", prompt.ID)
	}
	if !liveInputContains(prompt.Request.Carries, queueID) {
		t.Fatalf("the drained prompt request carries %v, want the queued input id %q", prompt.Request.Carries, queueID)
	}
	t.Logf("drained prompt: request seq=%d id=%s response seq=%d carries=%v", prompt.Seq, prompt.ID, response.Seq, prompt.Request.Carries)

	liveInputWait(t, deadline, "the drained turn to settle", func() bool {
		recs := sess.Records(oarlock.Cursor{})
		return liveInputCountEvents(recs, oarlock.EventTurnEnded) > endedBefore && sess.Status().Value.Kind == observe.StatusIdle
	})
	records = sess.Records(oarlock.Cursor{})
	liveInputDump(t, "queued turn run", records)
	answer := liveInputAnswer(records, prompt.Seq)
	t.Logf("assistant answer to the drained %q: %q", queueMarker, answer)
	if strings.TrimSpace(answer) == "" {
		t.Fatalf("the drained turn streamed no assistant text")
	}
}

// liveInputHoldAndWithdraw runs one attempt: a real turn held open by a bash
// sleep, an input queued into the adapter's own queue while it runs, and the
// withdraw that takes it back. ok=false is the one case the contract lets be
// retried: the withdraw answered not_queued because the run settled before the
// held input landed. Everything else is a failure.
//
// On success it proves the withdrawn input never went on the wire: no request
// record, no user_message event and no native frame carries the marker, the
// withdraw wrote exactly one request/response pair and nothing else, and the
// queue's own pair is byte-identical across it.
func liveInputHoldAndWithdraw(t *testing.T, ctx context.Context, sess *Session, deadline time.Time, hold, marker, inputID string) (string, bool) {
	t.Helper()

	liveInputWait(t, deadline, "the session to be idle before the hold prompt", func() bool {
		return sess.Status().Value.Kind == observe.StatusIdle
	})
	endedBefore := liveInputCountEvents(sess.Records(oarlock.Cursor{}), oarlock.EventTurnEnded)
	toolCallsBefore := liveInputCountEvents(sess.Records(oarlock.Cursor{}), oarlock.EventToolCallStarted)

	holdOut, err := sess.Prompt(ctx, hold, oarlock.PromptOptions{})
	if err != nil {
		t.Fatalf("the hold prompt: %v", err)
	}
	if holdOut.Kind != oarlock.ControlAccepted {
		t.Fatalf("the hold prompt = %+v, want accepted", holdOut)
	}
	t.Logf("hold prompt accepted (request seq=%d); waiting for the run to be visibly running", holdOut.Seq)
	liveInputWait(t, deadline, "the hold run to start", func() bool {
		return sess.Status().Value.Kind == observe.StatusRunning &&
			liveInputCountEvents(sess.Records(oarlock.Cursor{}), oarlock.EventTurnActive) >= 1
	})
	if liveInputPoll(time.Now().Add(60*time.Second), func() bool {
		return liveInputCountEvents(sess.Records(oarlock.Cursor{}), oarlock.EventToolCallStarted) > toolCallsBefore
	}) {
		t.Logf("the hold run started a tool call (the bash sleep)")
	} else {
		t.Logf("no tool call seen within 60s; queueing anyway, the stream still says the turn is running")
	}

	// Queued without blocking the run: the adapter holds it, nothing is sent.
	queueOut, err := sess.Queue(ctx, marker, oarlock.InputOptions{InputID: inputID})
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if queueOut.Kind != oarlock.ControlAccepted {
		t.Fatalf("Queue(%q) = %+v, want accepted (the adapter holds input itself)", marker, queueOut)
	}
	queueRec, queueResp := queueOut.Request, queueOut.Response
	t.Logf("queue request: seq=%d id=%s inputId=%q input=%q", queueRec.Seq, queueRec.ID, queueRec.Request.InputID, queueRec.Request.Input)
	t.Logf("queue response: seq=%d kind=%s", queueResp.Seq, queueResp.Response.Kind)

	// The withdraw's own slice of the stream starts here: anything after lastSeq
	// but the one withdraw pair would be a command written for a held input.
	before := sess.Records(oarlock.Cursor{})
	lastSeq := before[len(before)-1].Seq
	promptsBefore := len(liveInputRequestsWithAction(before, oarlock.ActionPrompt))

	withdrawOut, err := sess.Withdraw(ctx, inputID)
	if err != nil {
		t.Fatalf("Withdraw(%q): %v", inputID, err)
	}
	if withdrawOut.Kind == oarlock.ControlRejected && withdrawOut.Code == oarlock.NotQueued {
		liveInputDump(t, "not_queued attempt", sess.Records(oarlock.Cursor{}))
		return fmt.Sprintf("Withdraw(%q) = rejected{%s}: %q (queue request seq=%d, status now %s)",
			inputID, withdrawOut.Code, withdrawOut.Reason, queueRec.Seq, sess.Status().Value.Kind), false
	}
	if withdrawOut.Kind != oarlock.ControlAccepted {
		t.Fatalf("Withdraw(%q) = %+v, want accepted", inputID, withdrawOut)
	}
	if got := withdrawOut.Request.Request; got.Action != oarlock.ActionWithdraw || got.InputID != inputID {
		t.Fatalf("the withdraw request record = %+v, want action %s with inputId %q", got, oarlock.ActionWithdraw, inputID)
	}
	if withdrawOut.Request.Direction != oarlock.ToRuntime {
		t.Fatalf("the withdraw request direction = %q, want %q", withdrawOut.Request.Direction, oarlock.ToRuntime)
	}
	if withdrawOut.Response.Response == nil || withdrawOut.Response.Response.Kind != oarlock.ResponseAccepted {
		t.Fatalf("the withdraw response record = %+v, want accepted", withdrawOut.Response.Response)
	}

	// Nothing went on the wire for the withdraw: exactly one new request record
	// (the withdraw's own), no new input command, and no frame carrying a
	// withdraw command (Pi has none to carry).
	after := sess.Records(oarlock.Cursor{})
	var newRequests []oarlock.Record
	for _, r := range after {
		if r.Seq > lastSeq && r.Kind == oarlock.KindRequest {
			newRequests = append(newRequests, r)
		}
	}
	if len(newRequests) != 1 || newRequests[0].ID != withdrawOut.Request.ID {
		t.Fatalf("records after the withdraw carry %d new requests (%v), want exactly the withdraw request %s",
			len(newRequests), liveInputRecordLines(newRequests), withdrawOut.Request.ID)
	}
	if n := len(liveInputRequestsWithAction(after, oarlock.ActionPrompt)); n != promptsBefore {
		t.Fatalf("prompt request records = %d after the withdraw, want the %d before it: nothing may be sent for a withdrawn input", n, promptsBefore)
	}
	for _, r := range after {
		if r.Seq <= lastSeq || r.Kind != oarlock.KindFrame {
			continue
		}
		if strings.Contains(string(r.Native), `"command":"withdraw"`) {
			t.Fatalf("frame seq=%d carries a withdraw command to Pi, which has none: %s", r.Seq, liveInputCompact(string(r.Native)))
		}
	}

	// The queue's own request and response are never rewritten.
	queueNow := liveInputRecordBySeq(after, queueRec.Seq)
	queueRespNow := liveInputRecordBySeq(after, queueResp.Seq)
	if queueNow == nil || liveInputJSON(t, *queueNow) != liveInputJSON(t, queueRec) {
		t.Fatalf("the queue request record (seq=%d) was rewritten by the withdraw", queueRec.Seq)
	}
	if queueRespNow == nil || liveInputJSON(t, *queueRespNow) != liveInputJSON(t, queueResp) {
		t.Fatalf("the queue response record (seq=%d) was rewritten by the withdraw", queueResp.Seq)
	}

	// The run settles, and the withdrawn input is nowhere: not sent, not shown.
	liveInputWait(t, deadline, "the hold run to settle after the withdraw", func() bool {
		recs := sess.Records(oarlock.Cursor{})
		return liveInputCountEvents(recs, oarlock.EventTurnEnded) > endedBefore && sess.Status().Value.Kind == observe.StatusIdle
	})
	records := sess.Records(oarlock.Cursor{})
	liveInputDump(t, "hold+withdraw run", records)
	// The marker may exist in exactly one request record: the queue request that
	// held it. Any other request record carrying it means it was sent, and so does
	// a user_message event or any native frame carrying it.
	markerRecords := 0
	for _, r := range records {
		if r.Kind == oarlock.KindRequest && r.Request != nil && strings.Contains(r.Request.Input, marker) {
			if r.Request.Action != oarlock.ActionQueue || r.ID != queueRec.ID {
				t.Fatalf("request record seq=%d (action %s) carries the withdrawn input %q: only the queue request that held it may keep it, nothing may send it", r.Seq, r.Request.Action, marker)
			}
			markerRecords++
		}
		for _, ev := range r.Events {
			if ev.Kind == oarlock.EventUserMessage && strings.Contains(ev.Text, marker) {
				t.Fatalf("a user_message event at seq=%d carries the withdrawn input %q: it reached Pi", r.Seq, marker)
			}
		}
		if r.Kind == oarlock.KindFrame && strings.Contains(string(r.Native), marker) {
			t.Fatalf("frame seq=%d carries the withdrawn input %q: it went on the wire", r.Seq, marker)
		}
	}
	if markerRecords != 1 {
		t.Fatalf("the withdrawn input %q appears in %d request records, want exactly the one queue request %s", marker, markerRecords, queueRec.ID)
	}
	t.Logf("withdraw request record: %s", liveInputJSON(t, withdrawOut.Request))
	t.Logf("withdraw response record: %s", liveInputJSON(t, withdrawOut.Response))
	t.Logf("queue request record: %s", liveInputJSON(t, queueRec))
	t.Logf("queue response record: %s", liveInputJSON(t, queueResp))
	return "", true
}

// ---- reads over the retained stream ----------------------------------------

func liveInputWait(t *testing.T, deadline time.Time, what string, cond func() bool) {
	t.Helper()
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// liveInputPoll waits without failing: it answers whether cond held in time.
func liveInputPoll(until time.Time, cond func() bool) bool {
	for {
		if cond() {
			return true
		}
		if time.Now().After(until) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func liveInputCountEvents(records []oarlock.Record, kind oarlock.EventKind) (n int) {
	for _, r := range records {
		for _, ev := range r.Events {
			if ev.Kind == kind {
				n++
			}
		}
	}
	return n
}

// liveInputFirstOutcome is the runtime's own first turn end in the stream.
func liveInputFirstOutcome(records []oarlock.Record) *oarlock.TurnOutcome {
	for _, r := range records {
		for _, ev := range r.Events {
			if ev.Kind == oarlock.EventTurnEnded && ev.Outcome != nil {
				out := *ev.Outcome
				return &out
			}
		}
	}
	return nil
}

// liveInputAnswer is the assistant text of the run after afterSeq: the
// text_delta events the runtime streamed, in order.
func liveInputAnswer(records []oarlock.Record, afterSeq uint64) string {
	var b strings.Builder
	for _, r := range records {
		if r.Seq <= afterSeq {
			continue
		}
		for _, ev := range r.Events {
			if ev.Kind == oarlock.EventTextDelta {
				b.WriteString(ev.Text)
			}
		}
	}
	return b.String()
}

func liveInputPromptFor(records []oarlock.Record, substr string) *oarlock.Record {
	for i := range records {
		r := &records[i]
		if r.Kind == oarlock.KindRequest && r.Request != nil &&
			r.Request.Action == oarlock.ActionPrompt && strings.Contains(r.Request.Input, substr) {
			return r
		}
	}
	return nil
}

func liveInputResponseFor(records []oarlock.Record, id string) *oarlock.Record {
	for i := range records {
		if records[i].Kind == oarlock.KindResponse && records[i].RequestID == id {
			return &records[i]
		}
	}
	return nil
}

func liveInputRecordBySeq(records []oarlock.Record, seq uint64) *oarlock.Record {
	for i := range records {
		if records[i].Seq == seq {
			return &records[i]
		}
	}
	return nil
}

func liveInputRequestsWithAction(records []oarlock.Record, action oarlock.Action) (out []oarlock.Record) {
	for _, r := range records {
		if r.Kind == oarlock.KindRequest && r.Request != nil && r.Request.Action == action {
			out = append(out, r)
		}
	}
	return out
}

func liveInputContains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// liveInputJSON is one record's retained shape, as the stream keeps it.
func liveInputJSON(t *testing.T, r oarlock.Record) string {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshalling record seq=%d: %v", r.Seq, err)
	}
	return string(raw)
}

// ---- log shape -------------------------------------------------------------

// liveInputDump logs the stream in the shape the evidence file needs: one line
// per record, then the native frames that carry the facts (responses, run
// boundaries, conversation edges, errors), each compacted so a base64 image
// cannot flood the log.
func liveInputDump(t *testing.T, label string, records []oarlock.Record) {
	t.Helper()
	t.Logf("---- %s: %d records ----", label, len(records))
	for _, r := range records {
		t.Logf("%s", liveInputRecordLine(r))
	}
	for _, r := range records {
		if r.Kind == oarlock.KindFrame && liveInputNotableFrame(r.Native) {
			t.Logf("%s frame seq=%d: %s", label, r.Seq, liveInputCompact(string(r.Native)))
		}
	}
	t.Logf("---- end %s ----", label)
}

func liveInputRecordLines(records []oarlock.Record) []string {
	out := make([]string, 0, len(records))
	for _, r := range records {
		out = append(out, liveInputRecordLine(r))
	}
	return out
}

func liveInputRecordLine(r oarlock.Record) string {
	switch r.Kind {
	case oarlock.KindRequest:
		if r.Request == nil {
			return fmt.Sprintf("seq=%d request id=%s (no body)", r.Seq, r.ID)
		}
		return fmt.Sprintf("seq=%d request id=%s action=%s inputId=%q input=%q images=%+v carries=%v",
			r.Seq, r.ID, r.Request.Action, r.Request.InputID,
			liveInputShort(r.Request.Input, 80), r.Request.Images, r.Request.Carries)
	case oarlock.KindResponse:
		if r.Response == nil {
			return fmt.Sprintf("seq=%d response -> %s (no body)", r.Seq, r.RequestID)
		}
		return fmt.Sprintf("seq=%d response -> %s kind=%s code=%s reason=%q",
			r.Seq, r.RequestID, r.Response.Kind, r.Response.Code, liveInputShort(r.Response.Reason, 120))
	default:
		kinds := make([]string, 0, len(r.Events))
		for _, ev := range r.Events {
			kinds = append(kinds, string(ev.Kind))
		}
		return fmt.Sprintf("seq=%d frame type=%s events=%v", r.Seq, liveInputFrameType(r.Native), kinds)
	}
}

// liveInputNotableFrame picks the frames the evidence quotes: command answers,
// run boundaries and conversation edges (where a user message would show a
// withdrawn input), and anything carrying an error.
func liveInputNotableFrame(native json.RawMessage) bool {
	switch liveInputFrameType(native) {
	case "response", "agent_start", "agent_settled", "turn_end",
		"message_start", "message_end", "tool_execution_start", "tool_execution_end":
		return true
	}
	return strings.Contains(string(native), `"error"`)
}

func liveInputFrameType(native json.RawMessage) string {
	var m struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(native, &m) != nil {
		return "?"
	}
	return m.Type
}

// liveInputBase64Run matches a base64 run long enough to be image data, so a log
// line can say "N chars, head ..." instead of pasting megabytes.
var liveInputBase64Run = regexp.MustCompile(`[A-Za-z0-9+/]{80,}={0,2}`)

func liveInputCompact(s string) string {
	s = liveInputBase64Run.ReplaceAllStringFunc(s, func(m string) string {
		return fmt.Sprintf("<base64 %d chars, head %s>", len(m), liveInputShort(m, 24))
	})
	return liveInputShort(s, 2000)
}

func liveInputShort(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("...<%d more chars>", len(s)-n)
}
