package kernel

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/redact"
)

// frameBody builds a minimal frame whose Native carries a marker and whose
// Events are empty: exactly what an adapter pushes for a native event that
// says nothing oarlock understands.
func frameBody(marker string) FrameBody {
	return FrameBody{Native: json.RawMessage(`{"type":"` + marker + `"}`)}
}

// acceptAll is a decide that takes the action over.
func acceptAll(RequestRecord) oarlock.ResponseBody {
	return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
}

// seqsOf returns the Seq of every record, in the order given.
func seqsOf(records []Record) []uint64 {
	out := make([]uint64, len(records))
	for i, record := range records {
		out[i] = record.Seq
	}
	return out
}

func intPtr(value int) *int { return &value }

func int64Ptr(value int64) *int64 { return &value }

func float64Ptr(value float64) *float64 { return &value }

// TestInvariant1_NoSynthesizedFrames pins that a frame is retained exactly as
// pushed, one record per native frame, with no invented event-in particular no
// turn_ended for a prompt the runtime handled without opening a turn.
func TestInvariant1_NoSynthesizedFrames(t *testing.T) {
	k := newKernel("sess-1", nil)

	// A slash command that the runtime handled: it reports no turn events at
	// all, so the stream must say nothing happened.
	handled := k.Frame(FrameBody{Native: json.RawMessage(`{"type":"response","disposition":"handled"}`)}, RecordAt{})
	if handled.Kind != oarlock.KindFrame {
		t.Fatalf("Frame returned Kind %q, want frame", handled.Kind)
	}
	if len(handled.Events) != 0 {
		t.Fatalf("the kernel synthesized %d events for a frame that carried none", len(handled.Events))
	}

	outcome := k.Control(
		oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "/help"},
		acceptAll,
		RecordAt{},
	)
	if outcome.Kind != oarlock.ControlAccepted {
		t.Fatalf("want accepted, got %+v", outcome)
	}

	records := k.Records(Cursor{})
	if len(records) != 3 {
		t.Fatalf("want frame+request+response, got %d records", len(records))
	}
	frames := 0
	for index, record := range records {
		if record.Seq != uint64(index+1) {
			t.Fatalf("Seq %d at index %d is not dense", record.Seq, index)
		}
		if record.Kind == oarlock.KindFrame {
			frames++
		}
		for _, event := range record.Events {
			if event.Kind == oarlock.EventTurnEnded {
				t.Fatalf("record %d carries a turn_ended the runtime never reported", record.Seq)
			}
		}
	}
	if frames != 1 {
		t.Fatalf("want exactly the one frame that was pushed, got %d", frames)
	}
}

// TestInvariant2_ControlNeverPrunesFacts pins that there is no settled gate: a
// frame that races a turn end, and a frame that arrives after the session was
// disposed, are still retained and observed.
func TestInvariant2_ControlNeverPrunesFacts(t *testing.T) {
	k := newKernel("sess-1", nil)

	var seen []uint64
	k.Observe(func(record Record) { seen = append(seen, record.Seq) })

	k.Frame(FrameBody{
		Native: json.RawMessage(`{"type":"agent_settled"}`),
		Events: []oarlock.Event{{
			Kind:    oarlock.EventTurnEnded,
			Outcome: &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted},
		}},
	}, RecordAt{})

	// The tool result that raced the turn end: control owns no span here, so
	// nothing may drop it.
	late := k.Frame(FrameBody{
		Native: json.RawMessage(`{"type":"tool_end"}`),
		Events: []oarlock.Event{{
			Kind:   oarlock.EventToolCallEnded,
			CallID: "call-1",
			Result: oarlock.ToolOK,
		}},
	}, RecordAt{})
	if late.Seq != 2 {
		t.Fatalf("late frame got Seq %d, want 2", late.Seq)
	}
	if records := k.Records(Cursor{}); len(records) != 2 {
		t.Fatalf("late frame was pruned: %d records", len(records))
	}

	// Even after the session is being released, facts still enter the stream.
	if outcome := k.Control(oarlock.RequestBody{Action: oarlock.ActionDispose}, acceptAll, RecordAt{}); outcome.Kind != oarlock.ControlAccepted {
		t.Fatalf("dispose must be accepted, got %+v", outcome)
	}
	if unreachable := k.Unreachable(); unreachable == nil || unreachable.Code != oarlock.Disposed {
		t.Fatalf("want disposed after the dispose request, got %+v", unreachable)
	}
	afterDispose := k.Frame(frameBody("exit_notice"), RecordAt{})
	if afterDispose.Seq != 5 {
		t.Fatalf("frame after dispose got Seq %d, want 5", afterDispose.Seq)
	}

	records := k.Records(Cursor{})
	if len(records) != 5 {
		t.Fatalf("want 5 retained records, got %d", len(records))
	}
	if !reflect.DeepEqual(seen, []uint64{1, 2, 3, 4, 5}) {
		t.Fatalf("observer saw %v, want every record in Seq order", seen)
	}
}

// TestInvariant3_ReachabilityIsReadOffTheStream pins that an observed exit or
// dispose closes the control plane without consulting the adapter, and that
// neither gate drops late facts.
func TestInvariant3_ReachabilityIsReadOffTheStream(t *testing.T) {
	k := newKernel("sess-1", nil)
	if unreachable := k.Unreachable(); unreachable != nil {
		t.Fatalf("a fresh kernel is reachable, got %+v", unreachable)
	}

	exit := 0
	k.Respond("req-1", oarlock.ResponseBody{
		Kind:             oarlock.ResponseExited,
		ExitCode:         &exit,
		ExitCodeReported: true,
	}, RecordAt{})

	unreachable := k.Unreachable()
	if unreachable == nil || unreachable.Code != oarlock.RuntimeExited {
		t.Fatalf("want runtime_exited after an exited response, got %+v", unreachable)
	}

	decided := false
	outcome := k.Control(oarlock.RequestBody{Action: oarlock.ActionAbort}, func(request RequestRecord) oarlock.ResponseBody {
		decided = true
		return acceptAll(request)
	}, RecordAt{})
	if decided {
		t.Fatal("decide ran after the exit was already in the stream")
	}
	if outcome.Kind != oarlock.ControlRejected || outcome.Code != oarlock.RuntimeExited {
		t.Fatalf("want rejected runtime_exited, got %+v", outcome)
	}
	if outcome.Reason != "runtime exited" {
		t.Fatalf("want reason %q, got %q", "runtime exited", outcome.Reason)
	}

	// Invariant 2 still holds: a late frame after the exit is retained.
	k.Frame(frameBody("late_after_exit"), RecordAt{})
	if records := k.Records(Cursor{}); len(records) != 4 {
		t.Fatalf("want the exit, the rejected control and the late frame, got %d records", len(records))
	}

	// A dispose request makes the session unreachable as disposed.
	disposed := newKernel("sess-2", nil)
	if outcome := disposed.Control(oarlock.RequestBody{Action: oarlock.ActionDispose}, acceptAll, RecordAt{}); outcome.Kind != oarlock.ControlAccepted {
		t.Fatalf("want accepted dispose, got %+v", outcome)
	}
	if unreachable := disposed.Unreachable(); unreachable == nil || unreachable.Code != oarlock.Disposed {
		t.Fatalf("want disposed after a dispose request, got %+v", unreachable)
	} else if unreachable.Reason != "session disposed" {
		t.Fatalf("want the fixed reason %q, got %q", "session disposed", unreachable.Reason)
	}
}

// TestInvariant4_EmptyInputIsRefusedBeforeDecideButStillRecorded pins the
// input-level refusal and its exceptions.
func TestInvariant4_EmptyInputIsRefusedBeforeDecideButStillRecorded(t *testing.T) {
	k := newKernel("sess-1", nil)

	decided := false
	outcome := k.Control(oarlock.RequestBody{Action: oarlock.ActionPrompt}, func(request RequestRecord) oarlock.ResponseBody {
		decided = true
		return acceptAll(request)
	}, RecordAt{})
	if decided {
		t.Fatal("decide ran for empty input")
	}
	if outcome.Kind != oarlock.ControlRejected || outcome.Code != oarlock.Unsupported {
		t.Fatalf("want rejected unsupported, got %+v", outcome)
	}
	if outcome.Reason != "empty input: give text or images" {
		t.Fatalf("want the documented reason, got %q", outcome.Reason)
	}

	records := k.Records(Cursor{})
	if len(records) != 2 {
		t.Fatalf("the refused request must stay in the stream, got %d records", len(records))
	}
	if records[0].Kind != oarlock.KindRequest || records[0].Request == nil || records[0].Request.Input != "" {
		t.Fatalf("want the empty prompt request retained, got %+v", records[0])
	}
	if records[1].Kind != oarlock.KindResponse || records[1].Response == nil || records[1].Response.Kind != oarlock.ResponseRejected {
		t.Fatalf("want a rejected response retained, got %+v", records[1])
	}
	if records[1].RequestID != records[0].ID {
		t.Fatalf("response points at %q, request id is %q", records[1].RequestID, records[0].ID)
	}

	// Whitespace is not trimmed: only the empty string is refused.
	spaced := newKernel("sess-2", nil)
	decided = false
	spacedOutcome := spaced.Control(oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: " "}, func(request RequestRecord) oarlock.ResponseBody {
		decided = true
		return acceptAll(request)
	}, RecordAt{})
	if !decided || spacedOutcome.Kind != oarlock.ControlAccepted {
		t.Fatalf("a single space must reach decide, got decided=%v outcome=%+v", decided, spacedOutcome)
	}

	// Images without text are input, so they reach decide too.
	imaged := newKernel("sess-3", nil)
	decided = false
	imagedOutcome := imaged.Control(oarlock.RequestBody{
		Action: oarlock.ActionPrompt,
		Images: []oarlock.InputImage{{Path: "/tmp/a.png", MediaType: "image/png"}},
	}, func(request RequestRecord) oarlock.ResponseBody {
		decided = true
		return acceptAll(request)
	}, RecordAt{})
	if !decided || imagedOutcome.Kind != oarlock.ControlAccepted {
		t.Fatalf("images without text must reach decide, got decided=%v outcome=%+v", decided, imagedOutcome)
	}

	// The refusal is per action, not prompt-only: steer and queue are refused the
	// same way when they carry neither text nor images, and their requests stay in
	// the stream.
	for _, action := range []oarlock.Action{oarlock.ActionPrompt, oarlock.ActionSteer, oarlock.ActionQueue} {
		refusing := newKernel("sess-refusing", nil)
		decided = false
		refused := refusing.Control(oarlock.RequestBody{Action: action}, func(request RequestRecord) oarlock.ResponseBody {
			decided = true
			return acceptAll(request)
		}, RecordAt{})
		if decided {
			t.Fatalf("decide ran for an empty %s", action)
		}
		if refused.Kind != oarlock.ControlRejected || refused.Code != oarlock.Unsupported {
			t.Fatalf("empty %s: want rejected unsupported, got %+v", action, refused)
		}
		if refused.Reason != "empty input: give text or images" {
			t.Fatalf("empty %s: want the documented reason, got %q", action, refused.Reason)
		}
		refusedRecords := refusing.Records(Cursor{})
		if len(refusedRecords) != 2 {
			t.Fatalf("empty %s: the request must stay in the stream, got %d records", action, len(refusedRecords))
		}
		if refusedRecords[0].Kind != oarlock.KindRequest || refusedRecords[0].Request == nil ||
			refusedRecords[0].Request.Action != action {
			t.Fatalf("empty %s: want the request retained, got %+v", action, refusedRecords[0])
		}
		if refusedRecords[1].Response == nil || refusedRecords[1].Response.Kind != oarlock.ResponseRejected {
			t.Fatalf("empty %s: want a rejected response retained, got %+v", action, refusedRecords[1].Response)
		}
	}

	// An image is input for steer and queue as well, so those reach decide.
	for _, action := range []oarlock.Action{oarlock.ActionSteer, oarlock.ActionQueue} {
		imaging := newKernel("sess-imaging", nil)
		decided = false
		accepted := imaging.Control(oarlock.RequestBody{
			Action: action,
			Images: []oarlock.InputImage{{Path: "/tmp/b.png", MediaType: "image/png"}},
		}, func(request RequestRecord) oarlock.ResponseBody {
			decided = true
			return acceptAll(request)
		}, RecordAt{})
		if !decided {
			t.Fatalf("%s with an image must reach decide", action)
		}
		if accepted.Kind != oarlock.ControlAccepted {
			t.Fatalf("%s with an image: want accepted, got %+v", action, accepted)
		}
	}
}

// TestInvariant5_CredentialsAreRedactedBeforeRetainAndObserve pins that the
// retained record, the observed record and the replayed record are the same
// redacted value, while decide still receives the real one.
func TestInvariant5_CredentialsAreRedactedBeforeRetainAndObserve(t *testing.T) {
	const secret = "sk-live-abcdef123456"
	red := redact.New([]string{secret})
	k := New("sess-1", func(record oarlock.Record) (oarlock.Record, bool) {
		return red.Record(record)
	})

	var observed []Record
	k.Observe(func(record Record) { observed = append(observed, record) })

	native := json.RawMessage(`{"type":"env","env":{"ANTHROPIC_API_KEY":"` + secret + `","GIT_AUTHOR_EMAIL":"dev@example.com"}}`)
	retained := k.Frame(FrameBody{Native: native}, RecordAt{})
	if retained.Seq != 1 {
		t.Fatalf("redaction renumbered the record: Seq %d, want 1", retained.Seq)
	}

	env := func(record Record) map[string]string {
		t.Helper()
		var payload struct {
			Env map[string]string `json:"env"`
		}
		if err := json.Unmarshal(record.Native, &payload); err != nil {
			t.Fatalf("retained native is not the frame's JSON: %v (%s)", err, record.Native)
		}
		return payload.Env
	}

	kept := env(retained)
	if got := kept["ANTHROPIC_API_KEY"]; got != redact.RedactedPlaceholder {
		t.Fatalf("credential survived retention: %q", got)
	}
	if got := kept["GIT_AUTHOR_EMAIL"]; got != "dev@example.com" {
		t.Fatalf("a harmless variable was over-redacted: %q", got)
	}

	if len(observed) != 1 {
		t.Fatalf("want one observed record, got %d", len(observed))
	}
	if got := env(observed[0])["ANTHROPIC_API_KEY"]; got != redact.RedactedPlaceholder {
		t.Fatalf("the live observer saw the credential: %q", got)
	}
	if strings.Contains(string(observed[0].Native), secret) {
		t.Fatalf("live record still carries the secret: %s", observed[0].Native)
	}

	replayed := k.Records(Cursor{})
	if len(replayed) != 1 {
		t.Fatalf("want one replayed record, got %d", len(replayed))
	}
	if string(replayed[0].Native) != string(retained.Native) {
		t.Fatalf("replay and live disagree:\nlive %s\nreplay %s", retained.Native, replayed[0].Native)
	}

	// The adapter still decides on the original value; only retained copies
	// are redacted.
	decidedInput := ""
	outcome := k.Control(
		oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "use " + secret},
		func(request RequestRecord) oarlock.ResponseBody {
			decidedInput = request.Request.Input
			return acceptAll(request)
		},
		RecordAt{},
	)
	if decidedInput != "use "+secret {
		t.Fatalf("decide saw %q, want the original value", decidedInput)
	}
	if outcome.Kind != oarlock.ControlAccepted {
		t.Fatalf("want accepted, got %+v", outcome)
	}
	if outcome.Request.Request == nil || outcome.Request.Request.Input != "use "+redact.RedactedPlaceholder {
		t.Fatalf("the retained request is not redacted: %+v", outcome.Request.Request)
	}
}

// TestControl_RecordsRequestThenResponseAroundDecide pins the control order:
// request retained, then decide, then response.
func TestControl_RecordsRequestThenResponseAroundDecide(t *testing.T) {
	k := newKernel("sess-1", nil)

	var seen RequestRecord
	retainedAtDecide := -1
	outcome := k.Control(
		oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "hello"},
		func(request RequestRecord) oarlock.ResponseBody {
			seen = request
			retainedAtDecide = len(k.Records(Cursor{}))
			return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
		},
		RecordAt{},
	)

	if seen.Kind != oarlock.KindRequest || seen.Direction != oarlock.ToRuntime {
		t.Fatalf("decide got %+v, want a toRuntime request record", seen)
	}
	if seen.Request == nil || seen.Request.Input != "hello" {
		t.Fatalf("decide got the wrong body: %+v", seen.Request)
	}
	if seen.Seq != 1 || seen.ID != "req-1" {
		t.Fatalf("decide got Seq %d id %q, want Seq 1 id req-1", seen.Seq, seen.ID)
	}
	if retainedAtDecide != 1 {
		t.Fatalf("decide ran with %d retained records, want only the request", retainedAtDecide)
	}

	records := k.Records(Cursor{})
	if len(records) != 2 {
		t.Fatalf("want the request and its response, got %d records", len(records))
	}
	request, response := records[0], records[1]
	if request.Kind != oarlock.KindRequest || request.Seq != 1 || request.ID != "req-1" {
		t.Fatalf("want the request first, got %+v", request)
	}
	if response.Kind != oarlock.KindResponse || response.Seq != 2 {
		t.Fatalf("want the response second, got %+v", response)
	}
	if response.RequestID != "req-1" {
		t.Fatalf("response points at %q, want req-1", response.RequestID)
	}
	if response.Response == nil || response.Response.Kind != oarlock.ResponseAccepted {
		t.Fatalf("want an accepted response body, got %+v", response.Response)
	}

	if outcome.Kind != oarlock.ControlAccepted {
		t.Fatalf("want accepted outcome, got %+v", outcome)
	}
	if outcome.Seq != 1 || outcome.RequestID != "req-1" {
		t.Fatalf("outcome points at Seq %d id %q, want Seq 1 and req-1", outcome.Seq, outcome.RequestID)
	}
	if outcome.Request.Seq != 1 || outcome.Response.Seq != 2 {
		t.Fatalf("outcome carries Seq %d/%d, want the two control records", outcome.Request.Seq, outcome.Response.Seq)
	}
	if outcome.Code != "" || outcome.Reason != "" {
		t.Fatalf("an accepted outcome carries no code or reason, got %q %q", outcome.Code, outcome.Reason)
	}
}

// TestControl_DecidePanicBecomesRejectedErrorWithoutCrashing pins that an
// adapter panic is a rejection, not a crash, and that the request stays.
func TestControl_DecidePanicBecomesRejectedErrorWithoutCrashing(t *testing.T) {
	k := newKernel("sess-1", nil)

	outcome := k.Control(
		oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "hi"},
		func(RequestRecord) oarlock.ResponseBody { panic("boom") },
		RecordAt{},
	)
	if outcome.Kind != oarlock.ControlRejected || outcome.Code != oarlock.ErrorCode {
		t.Fatalf("want rejected error, got %+v", outcome)
	}
	if outcome.Reason != "boom" {
		t.Fatalf("want the panic text as reason, got %q", outcome.Reason)
	}

	records := k.Records(Cursor{})
	if len(records) != 2 {
		t.Fatalf("want request and response retained, got %d records", len(records))
	}
	response := records[1]
	if response.Response == nil || response.Response.Kind != oarlock.ResponseRejected ||
		response.Response.Code != oarlock.ErrorCode || response.Response.Reason != "boom" {
		t.Fatalf("want a retained rejected/error response, got %+v", response.Response)
	}

	// The kernel is still usable after the panic.
	if next := k.Control(oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "again"}, acceptAll, RecordAt{}); next.Kind != oarlock.ControlAccepted {
		t.Fatalf("the run did not survive the decide panic: %+v", next)
	}
}

// TestControl_NilDecideIsRejectedError pins the documented nil-decide answer: an
// adapter that supplies no decision yields a typed rejection, not a crash, and
// both records stay in the stream.
func TestControl_NilDecideIsRejectedError(t *testing.T) {
	k := newKernel("sess-1", nil)

	outcome := k.Control(oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "hi"}, nil, RecordAt{})
	if outcome.Kind != oarlock.ControlRejected || outcome.Code != oarlock.ErrorCode {
		t.Fatalf("want rejected error, got %+v", outcome)
	}
	if outcome.Reason != "no decide function" {
		t.Fatalf("want reason %q, got %q", "no decide function", outcome.Reason)
	}

	records := k.Records(Cursor{})
	if len(records) != 2 {
		t.Fatalf("want the request and its rejected response retained, got %d records", len(records))
	}
	if records[0].Kind != oarlock.KindRequest || records[0].Request == nil || records[0].Request.Input != "hi" {
		t.Fatalf("want the request retained, got %+v", records[0])
	}
	if records[1].Response == nil || records[1].Response.Kind != oarlock.ResponseRejected ||
		records[1].Response.Code != oarlock.ErrorCode {
		t.Fatalf("want a retained rejected/error response, got %+v", records[1].Response)
	}
	if records[1].RequestID != records[0].ID {
		t.Fatalf("response points at %q, request id is %q", records[1].RequestID, records[0].ID)
	}

	// The kernel stays usable.
	if next := k.Control(oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "again"}, acceptAll, RecordAt{}); next.Kind != oarlock.ControlAccepted {
		t.Fatalf("the kernel was left unusable: %+v", next)
	}
}

// TestControl_DisposeAfterObservedExitIsAcceptedWithoutDecide pins the one
// exception to the reachability gate.
func TestControl_DisposeAfterObservedExitIsAcceptedWithoutDecide(t *testing.T) {
	k := newKernel("sess-1", nil)
	k.Respond("", oarlock.ResponseBody{Kind: oarlock.ResponseExited}, RecordAt{})

	decided := false
	outcome := k.Control(oarlock.RequestBody{Action: oarlock.ActionDispose}, func(request RequestRecord) oarlock.ResponseBody {
		decided = true
		return acceptAll(request)
	}, RecordAt{})
	if decided {
		t.Fatal("decide ran for a dispose after an observed exit")
	}
	if outcome.Kind != oarlock.ControlAccepted {
		t.Fatalf("want accepted dispose cleanup, got %+v", outcome)
	}

	records := k.Records(Cursor{})
	if len(records) != 3 {
		t.Fatalf("want exit, dispose request and accepted response, got %d records", len(records))
	}
	if records[1].Kind != oarlock.KindRequest || records[1].Request == nil || records[1].Request.Action != oarlock.ActionDispose {
		t.Fatalf("want the dispose request retained, got %+v", records[1])
	}
	if records[2].Response == nil || records[2].Response.Kind != oarlock.ResponseAccepted {
		t.Fatalf("want an accepted response retained, got %+v", records[2].Response)
	}

	// Without an exit, a dispose after a dispose is rejected disposed.
	disposed := newKernel("sess-2", nil)
	if first := disposed.Control(oarlock.RequestBody{Action: oarlock.ActionDispose}, acceptAll, RecordAt{}); first.Kind != oarlock.ControlAccepted {
		t.Fatalf("the first dispose must be accepted, got %+v", first)
	}
	decided = false
	second := disposed.Control(oarlock.RequestBody{Action: oarlock.ActionDispose}, func(request RequestRecord) oarlock.ResponseBody {
		decided = true
		return acceptAll(request)
	}, RecordAt{})
	if decided {
		t.Fatal("decide ran for a dispose after a dispose")
	}
	if second.Kind != oarlock.ControlRejected || second.Code != oarlock.Disposed {
		t.Fatalf("want rejected disposed, got %+v", second)
	}
}

// TestControl_SecondDisposeAfterDisposedIsRejected pins that the first dispose
// of a reachable session reaches decide, and every later one is refused by the
// kernel without consulting the adapter.
func TestControl_SecondDisposeAfterDisposedIsRejected(t *testing.T) {
	k := newKernel("sess-1", nil)

	decided := false
	first := k.Control(oarlock.RequestBody{Action: oarlock.ActionDispose}, func(request RequestRecord) oarlock.ResponseBody {
		decided = true
		return acceptAll(request)
	}, RecordAt{})
	if !decided {
		t.Fatal("the first dispose of a reachable session must reach decide")
	}
	if first.Kind != oarlock.ControlAccepted {
		t.Fatalf("the first dispose must be accepted, got %+v", first)
	}

	decided = false
	second := k.Control(oarlock.RequestBody{Action: oarlock.ActionDispose}, func(request RequestRecord) oarlock.ResponseBody {
		decided = true
		return acceptAll(request)
	}, RecordAt{})
	if decided {
		t.Fatal("decide ran for a dispose after a dispose")
	}
	if second.Kind != oarlock.ControlRejected || second.Code != oarlock.Disposed {
		t.Fatalf("want rejected disposed, got %+v", second)
	}
	if second.Request.Request == nil || second.Request.Request.Action != oarlock.ActionDispose {
		t.Fatalf("the second dispose request must still be recorded, got %+v", second.Request)
	}
	if second.Response.Response == nil || second.Response.Response.Kind != oarlock.ResponseRejected {
		t.Fatalf("want a retained rejected response, got %+v", second.Response.Response)
	}
	if records := k.Records(Cursor{}); len(records) != 4 {
		t.Fatalf("want two request/response pairs retained, got %d records", len(records))
	}
}

// TestControl_ExitedPrecedenceOverDisposed pins that an observed exit outranks a
// dispose: Unreachable stays runtime_exited, and every later dispose is still
// accepted without decide, because exited is checked first.
func TestControl_ExitedPrecedenceOverDisposed(t *testing.T) {
	k := newKernel("sess-1", nil)

	// The exit arrives first, then a dispose is accepted (nothing is left to
	// release), so the stream holds both an exited response and a dispose request.
	k.Respond("", oarlock.ResponseBody{Kind: oarlock.ResponseExited, ExitCodeReported: true}, RecordAt{})
	if first := k.Control(oarlock.RequestBody{Action: oarlock.ActionDispose}, acceptAll, RecordAt{}); first.Kind != oarlock.ControlAccepted {
		t.Fatalf("a dispose after an observed exit must be accepted, got %+v", first)
	}

	unreachable := k.Unreachable()
	if unreachable == nil || unreachable.Code != oarlock.RuntimeExited {
		t.Fatalf("the exit must outrank the dispose, got %+v", unreachable)
	}
	if unreachable.Reason != "runtime exited" {
		t.Fatalf("want reason %q, got %q", "runtime exited", unreachable.Reason)
	}

	// Every further dispose is still accepted, and decide still never runs.
	decided := false
	further := k.Control(oarlock.RequestBody{Action: oarlock.ActionDispose}, func(request RequestRecord) oarlock.ResponseBody {
		decided = true
		return acceptAll(request)
	}, RecordAt{})
	if decided {
		t.Fatal("decide ran for a dispose after an observed exit")
	}
	if further.Kind != oarlock.ControlAccepted {
		t.Fatalf("want accepted, got %+v", further)
	}
	if again := k.Unreachable(); again == nil || again.Code != oarlock.RuntimeExited {
		t.Fatalf("want runtime_exited still, got %+v", again)
	}
}

// TestControl_ExitResponseMapsToRejectedRuntimeExited pins that an adapter that
// observes the process exit inside decide yields a rejected control, exactly
// like the reference controlOutcomeOf, and that reachability then follows the
// stream.
func TestControl_ExitResponseMapsToRejectedRuntimeExited(t *testing.T) {
	k := newKernel("sess-1", nil)
	code := 1

	outcome := k.Control(oarlock.RequestBody{Action: oarlock.ActionAbort}, func(RequestRecord) oarlock.ResponseBody {
		return oarlock.ResponseBody{
			Kind:             oarlock.ResponseExited,
			ExitCode:         &code,
			ExitCodeReported: true,
		}
	}, RecordAt{})

	if outcome.Kind != oarlock.ControlRejected {
		t.Fatalf("an exit body maps to a rejection, got %+v", outcome)
	}
	if outcome.Code != oarlock.RuntimeExited {
		t.Fatalf("want runtime_exited, got %q", outcome.Code)
	}
	if outcome.Reason != "runtime exited" {
		t.Fatalf("want reason %q, got %q", "runtime exited", outcome.Reason)
	}
	if outcome.Response.Response == nil || outcome.Response.Response.Kind != oarlock.ResponseExited {
		t.Fatalf("the exit itself stays the retained response body, got %+v", outcome.Response.Response)
	}
	if outcome.Response.Response.ExitCode == nil || *outcome.Response.Response.ExitCode != 1 ||
		!outcome.Response.Response.ExitCodeReported {
		t.Fatalf("the exit code must survive retention: %+v", outcome.Response.Response)
	}

	unreachable := k.Unreachable()
	if unreachable == nil || unreachable.Code != oarlock.RuntimeExited {
		t.Fatalf("the stream now holds the exit, want runtime_exited, got %+v", unreachable)
	}
}

// TestControl_ReachabilityTakesPrecedenceOverEmptyInput pins the gate order.
func TestControl_ReachabilityTakesPrecedenceOverEmptyInput(t *testing.T) {
	k := newKernel("sess-1", nil)
	k.Respond("", oarlock.ResponseBody{Kind: oarlock.ResponseExited}, RecordAt{})

	decided := false
	outcome := k.Control(oarlock.RequestBody{Action: oarlock.ActionPrompt}, func(request RequestRecord) oarlock.ResponseBody {
		decided = true
		return acceptAll(request)
	}, RecordAt{})
	if decided {
		t.Fatal("decide ran for an unreachable session")
	}
	if outcome.Kind != oarlock.ControlRejected {
		t.Fatalf("want rejected, got %+v", outcome)
	}
	if outcome.Code != oarlock.RuntimeExited {
		t.Fatalf("reachability must win over the empty-input refusal, got code %q", outcome.Code)
	}
	if outcome.Reason != "runtime exited" {
		t.Fatalf("want reason %q, got %q", "runtime exited", outcome.Reason)
	}
}

// TestRequestAndRespond_AreNotGatedAfterExitOrDispose pins that only Control
// consults reachability: recording a request or a response is never gated, so a
// late toApp request or an observed outcome still enters the stream.
func TestRequestAndRespond_AreNotGatedAfterExitOrDispose(t *testing.T) {
	k := newKernel("sess-1", nil)
	var observed []uint64
	k.Observe(func(record Record) { observed = append(observed, record.Seq) })

	k.Respond("", oarlock.ResponseBody{Kind: oarlock.ResponseExited}, RecordAt{})
	if k.Unreachable() == nil {
		t.Fatal("want an unreachable kernel after the exit")
	}

	late := k.Request(oarlock.ToApp, oarlock.RequestBody{
		Action:     oarlock.ActionNative,
		NativeType: "permission",
		Input:      "allow?",
	}, RecordAt{ID: "native-late"})
	if late.Seq != 2 || late.ID != "native-late" {
		t.Fatalf("a request after the exit must still be retained, got %+v", late)
	}
	answer := k.Respond(late.ID, oarlock.ResponseBody{Kind: oarlock.ResponseAnswered}, RecordAt{})
	if answer.Seq != 3 || answer.RequestID != "native-late" {
		t.Fatalf("a response after the exit must still be retained, got %+v", answer)
	}

	// A dispose request gates control too, and gates nothing else.
	disposed := newKernel("sess-1", nil)
	if outcome := disposed.Control(oarlock.RequestBody{Action: oarlock.ActionDispose}, acceptAll, RecordAt{}); outcome.Kind != oarlock.ControlAccepted {
		t.Fatalf("want accepted dispose, got %+v", outcome)
	}
	if disposed.Unreachable() == nil {
		t.Fatal("want an unreachable kernel after the dispose")
	}
	lateRequest := disposed.Request(oarlock.ToApp, oarlock.RequestBody{
		Action:     oarlock.ActionNative,
		NativeType: "question",
	}, RecordAt{ID: "native-q"})
	if lateRequest.Seq != 3 {
		t.Fatalf("a request after the dispose must still be retained, got %+v", lateRequest)
	}
	lateResponse := disposed.Respond(lateRequest.ID, oarlock.ResponseBody{Kind: oarlock.ResponseAnswered}, RecordAt{})
	if lateResponse.Seq != 4 {
		t.Fatalf("a response after the dispose must still be retained, got %+v", lateResponse)
	}

	if want := []uint64{1, 2, 3}; !reflect.DeepEqual(observed, want) {
		t.Fatalf("the live tap saw %v, want %v", observed, want)
	}
}

// TestObserver_PanicDoesNotStopLaterObserversOrTheRun pins that an observer is
// a side-tap: its panic is swallowed per observer per record.
func TestObserver_PanicDoesNotStopLaterObserversOrTheRun(t *testing.T) {
	k := newKernel("sess-1", nil)

	var mu sync.Mutex
	var seen []uint64
	panics := 0
	k.Observe(func(Record) {
		panics++
		panic("observer boom")
	})
	k.Observe(func(record Record) {
		mu.Lock()
		seen = append(seen, record.Seq)
		mu.Unlock()
	})

	k.Frame(frameBody("one"), RecordAt{})
	k.Frame(frameBody("two"), RecordAt{})

	mu.Lock()
	got := append([]uint64(nil), seen...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []uint64{1, 2}) {
		t.Fatalf("the healthy observer saw %v, want every record", got)
	}
	if panics != 2 {
		t.Fatalf("the panicking observer ran %d times, want once per record", panics)
	}

	// The run is unaffected: control still records both of its records.
	if outcome := k.Control(oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "hi"}, acceptAll, RecordAt{}); outcome.Kind != oarlock.ControlAccepted {
		t.Fatalf("the run was disturbed by an observer panic: %+v", outcome)
	}
	if records := k.Records(Cursor{}); len(records) != 4 {
		t.Fatalf("want 4 retained records, got %d", len(records))
	}
}

// TestObserver_CancelIsIdempotentAndStopsLaterDelivery pins the unsubscribe
// contract.
func TestObserver_CancelIsIdempotentAndStopsLaterDelivery(t *testing.T) {
	k := newKernel("sess-1", nil)

	var mu sync.Mutex
	var seen []uint64
	cancel := k.Observe(func(record Record) {
		mu.Lock()
		seen = append(seen, record.Seq)
		mu.Unlock()
	})

	k.Frame(frameBody("one"), RecordAt{})
	cancel()
	cancel() // idempotent: no panic, no double delete
	k.Frame(frameBody("two"), RecordAt{})

	mu.Lock()
	got := append([]uint64(nil), seen...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []uint64{1}) {
		t.Fatalf("canceled observer saw %v, want only the record before cancel", got)
	}

	k.mu.Lock()
	registered := len(k.observers)
	k.mu.Unlock()
	if registered != 0 {
		t.Fatalf("%d observers left after cancel", registered)
	}
}

// TestObserver_CancelStopsTheRestOfTheCurrentBatch pins that cancel bites for the
// batch in flight, not only for later ones: the callback that is already running
// may finish, and nothing after it is delivered.
func TestObserver_CancelStopsTheRestOfTheCurrentBatch(t *testing.T) {
	// Observers are called in registration order, so B (registered first) sees the
	// record whose callback then cancels it, and the second record is never
	// delivered to B.
	k := newKernel("sess-1", nil)

	var mu sync.Mutex
	bSeen, aSeen := 0, 0
	bCancel := k.Observe(func(Record) {
		mu.Lock()
		bSeen++
		mu.Unlock()
	})
	k.Observe(func(Record) {
		mu.Lock()
		aSeen++
		mu.Unlock()
		bCancel()
	})

	k.Frame(frameBody("one"), RecordAt{})
	k.Frame(frameBody("two"), RecordAt{})

	mu.Lock()
	gotB, gotA := bSeen, aSeen
	mu.Unlock()
	if gotB != 1 {
		t.Fatalf("the cancelled observer saw %d records, want only the in-flight one", gotB)
	}
	if gotA != 2 {
		t.Fatalf("the cancelling observer saw %d records, want both", gotA)
	}

	// White-box: one batch carrying two records, so the already-taken taps
	// snapshot still lists B after A cancels it. Only the cancelled flag can stop
	// that delivery; a fresh registry snapshot alone would not.
	batch := newKernel("sess-1", nil)
	var batchMu sync.Mutex
	batchB, batchA := 0, 0
	cancelB := batch.Observe(func(Record) {
		batchMu.Lock()
		batchB++
		batchMu.Unlock()
	})
	batch.Observe(func(Record) {
		batchMu.Lock()
		batchA++
		batchMu.Unlock()
		cancelB()
	})

	first := Record{Seq: 1, Kind: oarlock.KindFrame, SessionID: "sess-1"}
	second := Record{Seq: 2, Kind: oarlock.KindFrame, SessionID: "sess-1"}
	batch.mu.Lock()
	batch.seq = 2
	batch.log = append(batch.log, first, second)
	batch.notified = append(batch.notified, first, second)
	batch.mu.Unlock()
	batch.publish()

	batchMu.Lock()
	gotBatchB, gotBatchA := batchB, batchA
	batchMu.Unlock()
	if gotBatchB != 1 {
		t.Fatalf("in one batch, the cancelled observer saw %d records, want only the first", gotBatchB)
	}
	if gotBatchA != 2 {
		t.Fatalf("in one batch, the cancelling observer saw %d records, want both", gotBatchA)
	}
}

// TestObserver_ReentrantAppendIsDeliveredOnceInSeqOrder pins that a record
// appended from inside an observer is queued and dispatched by the active drain
// before the outer append returns.
func TestObserver_ReentrantAppendIsDeliveredOnceInSeqOrder(t *testing.T) {
	k := newKernel("sess-1", nil)

	var seen []uint64
	k.Observe(func(record Record) {
		seen = append(seen, record.Seq)
		if record.Seq == 1 {
			k.Frame(frameBody("nested"), RecordAt{})
		}
	})

	k.Frame(frameBody("outer"), RecordAt{})

	if !reflect.DeepEqual(seen, []uint64{1, 2}) {
		t.Fatalf("observer saw %v, want Seq order [1 2] delivered once each", seen)
	}
	if records := k.Records(Cursor{}); len(records) != 2 {
		t.Fatalf("want both records retained, got %d", len(records))
	}
}

// TestSeq_StartsAtOneAndIsDense pins the one total order shared by every kind.
func TestSeq_StartsAtOneAndIsDense(t *testing.T) {
	k := newKernel("sess-1", nil)

	for index := 0; index < 5; index++ {
		record := k.Frame(frameBody("f"), RecordAt{})
		if record.Seq != uint64(index+1) {
			t.Fatalf("append %d got Seq %d, want %d", index, record.Seq, index+1)
		}
	}

	records := k.Records(Cursor{})
	if len(records) != 5 {
		t.Fatalf("want 5 records, got %d", len(records))
	}
	for index, record := range records {
		if record.Seq != uint64(index+1) {
			t.Fatalf("record %d has Seq %d: the log is not in Seq order", index, record.Seq)
		}
	}

	// Requests and responses continue the same dense sequence.
	outcome := k.Control(oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "hi"}, acceptAll, RecordAt{})
	if outcome.Seq != 6 {
		t.Fatalf("control request got Seq %d, want 6", outcome.Seq)
	}
	if outcome.Response.Seq != 7 {
		t.Fatalf("control response got Seq %d, want 7", outcome.Response.Seq)
	}
}

// TestGraph_BuildsNodesAndEdgesFromSessionLinks pins that nodes come from the
// envelope, edges only from session_linked frame events, duplicates collapse,
// and the returned graph is a deep copy.
func TestGraph_BuildsNodesAndEdgesFromSessionLinks(t *testing.T) {
	k := newKernel("root", nil)
	empty := k.Graph()
	if len(empty.Nodes) != 1 || empty.Nodes[0].ID != "root" || len(empty.Edges) != 0 {
		t.Fatalf("a fresh graph is the kernel's own session and no edges, got %+v", empty)
	}

	k.Frame(FrameBody{
		Native: json.RawMessage(`{"type":"task_started"}`),
		Events: []oarlock.Event{Link(SessionEdge{Parent: "root", Child: "child-1", Via: "task"})},
	}, RecordAt{SessionID: "root"})
	// The runtime reports the same edge again: the graph keeps it once.
	k.Frame(FrameBody{
		Native: json.RawMessage(`{"type":"task_started"}`),
		Events: []oarlock.Event{Link(SessionEdge{Parent: "root", Child: "child-1", Via: "task"})},
	}, RecordAt{SessionID: "child-1"})
	k.Frame(FrameBody{
		Native: json.RawMessage(`{"type":"task_started"}`),
		Events: []oarlock.Event{Link(SessionEdge{Parent: "child-1", Child: "grandchild"})},
	}, RecordAt{SessionID: "child-1"})
	// A link may also name a parent that never emitted a record of its own in this
	// stream: the parent endpoint still establishes a node (both endpoints do).
	k.Frame(FrameBody{
		Native: json.RawMessage(`{"type":"task_started"}`),
		Events: []oarlock.Event{Link(SessionEdge{Parent: "unseen-parent", Child: "child-2", Via: "resume"})},
	}, RecordAt{SessionID: "child-2"})
	// A non-frame record establishes a node and never an edge.
	k.Request(oarlock.ToRuntime, oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "hi"}, RecordAt{SessionID: "other"})

	graph := k.Graph()
	if graph.Nodes[0].ID != "root" {
		t.Fatalf("the kernel's own session must be the first node, got %+v", graph.Nodes)
	}
	nodes := make(map[string]bool, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes[node.ID] = true
	}
	for _, want := range []string{"root", "child-1", "grandchild", "child-2", "unseen-parent", "other"} {
		if !nodes[want] {
			t.Fatalf("node %q missing from %+v", want, graph.Nodes)
		}
	}
	if len(graph.Nodes) != 6 {
		t.Fatalf("want 6 nodes, got %+v", graph.Nodes)
	}

	wantEdges := []SessionEdge{
		{Parent: "root", Child: "child-1", Via: "task"},
		{Parent: "child-1", Child: "grandchild"},
		{Parent: "unseen-parent", Child: "child-2", Via: "resume"},
	}
	if !reflect.DeepEqual(graph.Edges, wantEdges) {
		t.Fatalf("want edges %+v, got %+v", wantEdges, graph.Edges)
	}

	// The returned graph is a copy: mutating it cannot change kernel state.
	graph.Nodes[0].ID = "hacked"
	graph.Edges[0].Parent = "hacked"
	graph.Edges = graph.Edges[:0]

	again := k.Graph()
	if again.Nodes[0].ID != "root" {
		t.Fatalf("kernel state was mutated through Graph(): %+v", again.Nodes)
	}
	if !reflect.DeepEqual(again.Edges, wantEdges) {
		t.Fatalf("kernel edges were mutated through Graph(): %+v", again.Edges)
	}
}

// TestGraph_ReturnsACopyThatCannotMutateKernelState pins that a caller cannot
// reach kernel state through the returned graph.
func TestGraph_ReturnsACopyThatCannotMutateKernelState(t *testing.T) {
	k := newKernel("root", nil)
	k.Frame(FrameBody{
		Native: json.RawMessage(`{"type":"task_started"}`),
		Events: []oarlock.Event{Link(SessionEdge{Parent: "root", Child: "child-1", Via: "task"})},
	}, RecordAt{SessionID: "root"})

	before := k.Graph()
	if len(before.Nodes) != 2 || len(before.Edges) != 1 {
		t.Fatalf("want root, child-1 and one edge, got %+v", before)
	}

	// Vandalize the returned graph in every way a caller can.
	for index := range before.Nodes {
		before.Nodes[index].ID = "hijacked"
	}
	before.Edges[0].Parent = "hijacked"
	before.Edges[0].Child = "hijacked"
	before.Edges[0].Via = "hijacked"
	before.Nodes = append(before.Nodes, SessionNode{ID: "invented"})
	before.Edges = before.Edges[:0]

	wantNodes := []SessionNode{{ID: "root"}, {ID: "child-1"}}
	wantEdges := []SessionEdge{{Parent: "root", Child: "child-1", Via: "task"}}
	after := k.Graph()
	if !reflect.DeepEqual(after.Nodes, wantNodes) {
		t.Fatalf("nodes changed through a returned graph: %+v", after.Nodes)
	}
	if !reflect.DeepEqual(after.Edges, wantEdges) {
		t.Fatalf("edges changed through a returned graph: %+v", after.Edges)
	}

	// A second mutation pass cannot leak into a third read either.
	after.Nodes[0].ID = "hijacked-again"
	third := k.Graph()
	if third.Nodes[0].ID != "root" {
		t.Fatalf("nodes changed through the second graph: %+v", third.Nodes)
	}
}

// TestKernel_RetainedRecordDoesNotAliasAdapterBuffers pins that reusing a
// buffer after an append cannot rewrite the retained record.
func TestKernel_RetainedRecordDoesNotAliasAdapterBuffers(t *testing.T) {
	k := newKernel("sess-1", nil)

	exit := 7
	resets := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	wantResets := resets
	native := []byte(`{"type":"tool_end","nested":{"n":1}}`)
	events := []oarlock.Event{{
		Kind:             oarlock.EventToolCallEnded,
		CallID:           "call-1",
		Text:             "original text",
		Result:           oarlock.ToolFailed,
		ExitCode:         &exit,
		ExitCodeReported: true,
		Content:          []oarlock.ToolOutputPart{{Type: "text", Text: "original output", Value: json.RawMessage(`{"v":1}`)}},
		Outcome: &oarlock.TurnOutcome{
			Kind:             oarlock.OutcomeFailed,
			ResetsAt:         &resets,
			StructuredOutput: json.RawMessage(`{"o":1}`),
		},
		Usage: &oarlock.UsageReport{
			Context: &oarlock.ContextUsage{
				Tokens:        int64Ptr(12),
				ContextWindow: int64Ptr(200),
				Percent:       float64Ptr(6),
			},
			Tokens: &oarlock.TokenTotals{
				Input:      3,
				Reasoning:  int64Ptr(1),
				CacheRead:  int64Ptr(5),
				CacheWrite: int64Ptr(7),
			},
		},
		Task: &oarlock.TaskEvent{TaskID: "task-1", Status: oarlock.TaskRunning},
	}}
	path := []string{"root"}
	frame := k.Frame(FrameBody{Native: native, Events: events}, RecordAt{AgentPath: path})

	requestBody := oarlock.RequestBody{
		Action:  oarlock.ActionPrompt,
		Input:   "original input",
		Images:  []oarlock.InputImage{{Path: "/tmp/a.png", MediaType: "image/png"}},
		Carries: []string{"in-1"},
		Native:  []byte(`{"n":1}`),
	}
	request := k.Request(oarlock.ToRuntime, requestBody, RecordAt{})

	responseExit := 3
	k.Respond(request.ID, oarlock.ResponseBody{
		Kind:             oarlock.ResponseExited,
		ExitCode:         &responseExit,
		ExitCodeReported: true,
		Native:           json.RawMessage(`{"r":1}`),
	}, RecordAt{})

	// The adapter reuses every buffer it handed over.
	native[0] = 'X'
	events[0].Text = "mutated"
	events[0].Content[0].Text = "mutated"
	events[0].Content[0].Value[0] = 'X'
	exit = 99
	events[0].Outcome.Kind = oarlock.OutcomeCompleted
	*events[0].Outcome.ResetsAt = time.Date(1999, 12, 31, 23, 59, 59, 0, time.UTC)
	events[0].Outcome.StructuredOutput[0] = 'X'
	events[0].Usage.Context.Tokens = int64Ptr(999)
	events[0].Usage.Context.ContextWindow = int64Ptr(999)
	events[0].Usage.Context.Percent = float64Ptr(99)
	events[0].Usage.Tokens.Reasoning = int64Ptr(999)
	events[0].Usage.Tokens.CacheRead = int64Ptr(999)
	events[0].Usage.Tokens.CacheWrite = int64Ptr(999)
	events[0].Task.TaskID = "mutated"
	path[0] = "mutated"
	requestBody.Input = "mutated"
	requestBody.Images[0].Path = "/mutated"
	requestBody.Carries[0] = "mutated"
	requestBody.Native[0] = 'X'
	responseExit = 42

	if string(frame.Native) != `{"type":"tool_end","nested":{"n":1}}` {
		t.Fatalf("Native aliases the adapter buffer: %s", frame.Native)
	}
	if frame.AgentPath[0] != "root" {
		t.Fatalf("AgentPath aliases the adapter slice: %v", frame.AgentPath)
	}
	event := frame.Events[0]
	if event.Text != "original text" {
		t.Fatalf("event text changed: %q", event.Text)
	}
	if len(event.Content) != 1 || event.Content[0].Text != "original output" {
		t.Fatalf("tool output changed: %+v", event.Content)
	}
	if string(event.Content[0].Value) != `{"v":1}` {
		t.Fatalf("tool output value aliases the adapter slice: %s", event.Content[0].Value)
	}
	if event.ExitCode == nil || *event.ExitCode != 7 {
		t.Fatalf("event exit code changed: %v", event.ExitCode)
	}
	if event.Outcome == nil || event.Outcome.Kind != oarlock.OutcomeFailed {
		t.Fatalf("turn outcome changed: %+v", event.Outcome)
	}
	if event.Outcome.ResetsAt == nil || !event.Outcome.ResetsAt.Equal(wantResets) {
		t.Fatalf("turn outcome resets-at changed: %v", event.Outcome.ResetsAt)
	}
	if string(event.Outcome.StructuredOutput) != `{"o":1}` {
		t.Fatalf("structured output aliases the adapter slice: %s", event.Outcome.StructuredOutput)
	}
	if event.Usage == nil || event.Usage.Context == nil || event.Usage.Context.Tokens == nil || *event.Usage.Context.Tokens != 12 {
		t.Fatalf("usage context tokens changed: %+v", event.Usage)
	}
	if event.Usage.Context.ContextWindow == nil || *event.Usage.Context.ContextWindow != 200 {
		t.Fatalf("usage context window aliases the adapter pointer: %+v", event.Usage.Context)
	}
	if event.Usage.Context.Percent == nil || *event.Usage.Context.Percent != 6 {
		t.Fatalf("usage context percent aliases the adapter pointer: %+v", event.Usage.Context)
	}
	if event.Usage.Tokens == nil || event.Usage.Tokens.Reasoning == nil || *event.Usage.Tokens.Reasoning != 1 {
		t.Fatalf("usage reasoning changed: %+v", event.Usage.Tokens)
	}
	if event.Usage.Tokens.CacheRead == nil || *event.Usage.Tokens.CacheRead != 5 {
		t.Fatalf("usage cache-read aliases the adapter pointer: %+v", event.Usage.Tokens)
	}
	if event.Usage.Tokens.CacheWrite == nil || *event.Usage.Tokens.CacheWrite != 7 {
		t.Fatalf("usage cache-write aliases the adapter pointer: %+v", event.Usage.Tokens)
	}
	if event.Task == nil || event.Task.TaskID != "task-1" {
		t.Fatalf("task event changed: %+v", event.Task)
	}

	records := k.Records(Cursor{})
	if len(records) != 3 {
		t.Fatalf("want the frame, the request and the response, got %d records", len(records))
	}
	stored := records[1]
	if stored.Request == nil || stored.Request.Input != "original input" {
		t.Fatalf("request input changed: %+v", stored.Request)
	}
	if len(stored.Request.Images) != 1 || stored.Request.Images[0].Path != "/tmp/a.png" {
		t.Fatalf("request images changed: %+v", stored.Request.Images)
	}
	if len(stored.Request.Carries) != 1 || stored.Request.Carries[0] != "in-1" {
		t.Fatalf("request carries changed: %+v", stored.Request.Carries)
	}
	if string(stored.Request.Native) != `{"n":1}` {
		t.Fatalf("request native aliases the adapter slice: %s", stored.Request.Native)
	}

	storedResponse := records[2]
	if storedResponse.Response == nil || storedResponse.Response.ExitCode == nil || *storedResponse.Response.ExitCode != 3 {
		t.Fatalf("response exit code changed: %+v", storedResponse.Response)
	}
	if string(storedResponse.Response.Native) != `{"r":1}` {
		t.Fatalf("response native aliases the adapter slice: %s", storedResponse.Response.Native)
	}
}

// TestKernel_RedactorCannotChangeStreamStructure pins that a redactor may
// change content only: identity, order and reachability are restored from the
// pre-redaction record.
func TestKernel_RedactorCannotChangeStreamStructure(t *testing.T) {
	const redactedContent = "[redacted]"
	hook := func(record oarlock.Record) (oarlock.Record, bool) {
		// Content: allowed, and kept.
		if record.Request != nil {
			record.Request.Input = redactedContent
		}
		if record.Response != nil {
			record.Response.Reason = redactedContent
		}
		// Structure: not allowed, so the kernel restores it.
		record.Seq = 999
		record.Kind = oarlock.KindFrame
		record.SessionID = "hijacked-session"
		record.AgentPath = []string{"hijacked"}
		record.SpanID = "hijacked-span"
		record.ID = "hijacked-id"
		record.Direction = oarlock.ToApp
		record.RequestID = "hijacked-request"
		if record.Request != nil {
			record.Request.Action = oarlock.ActionPrompt
		}
		if record.Response != nil {
			record.Response.Kind = oarlock.ResponseExited
		}
		return record, true
	}

	// A dispose request must stay a dispose request, so reachability still flips.
	disposing := New("sess-1", hook)
	outcome := disposing.Control(
		oarlock.RequestBody{Action: oarlock.ActionDispose},
		acceptAll,
		RecordAt{AgentPath: []string{"root"}, SpanID: "turn-1"},
	)
	if outcome.Kind != oarlock.ControlAccepted {
		t.Fatalf("want accepted dispose, got %+v", outcome)
	}

	unreachable := disposing.Unreachable()
	if unreachable == nil || unreachable.Code != oarlock.Disposed {
		t.Fatalf("the redactor hid the dispose: got %+v", unreachable)
	}

	records := disposing.Records(Cursor{})
	if len(records) != 2 {
		t.Fatalf("want the dispose request and its response, got %d records", len(records))
	}
	request := records[0]
	if request.Seq != 1 || request.Kind != oarlock.KindRequest || request.SessionID != "sess-1" {
		t.Fatalf("request structure was rewritten: %+v", request)
	}
	if request.Direction != oarlock.ToRuntime || request.ID != "req-1" || request.RequestID != "" {
		t.Fatalf("request identity was rewritten: %+v", request)
	}
	if !reflect.DeepEqual(request.AgentPath, []string{"root"}) || request.SpanID != "turn-1" {
		t.Fatalf("request attribution was rewritten: %+v", request)
	}
	if request.Request == nil || request.Request.Action != oarlock.ActionDispose {
		t.Fatalf("the redactor rewrote Request.Action: %+v", request.Request)
	}
	if request.Request.Input != redactedContent {
		t.Fatalf("content changes must be kept, got %q", request.Request.Input)
	}
	response := records[1]
	if response.Seq != 2 || response.Kind != oarlock.KindResponse || response.RequestID != "req-1" {
		t.Fatalf("response structure was rewritten: %+v", response)
	}
	if response.Response == nil || response.Response.Kind != oarlock.ResponseAccepted {
		t.Fatalf("the redactor rewrote Response.Kind: %+v", response.Response)
	}

	// A rejection must not be rewritable into an exit.
	rejecting := New("sess-1", hook)
	rejected := rejecting.Control(
		oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "hi"},
		func(RequestRecord) oarlock.ResponseBody {
			return oarlock.ResponseBody{
				Kind:   oarlock.ResponseRejected,
				Code:   oarlock.RuntimeRefused,
				Reason: "real reason",
			}
		},
		RecordAt{},
	)
	if rejecting.Unreachable() != nil {
		t.Fatalf("the redactor fabricated an exit: %+v", rejecting.Unreachable())
	}
	if rejected.Kind != oarlock.ControlRejected || rejected.Code != oarlock.RuntimeRefused {
		t.Fatalf("want rejected runtime_refused, got %+v", rejected)
	}
	if rejected.Request.ID != rejected.Response.RequestID {
		t.Fatalf("the response no longer points at the request: %q vs %q", rejected.Response.RequestID, rejected.Request.ID)
	}
	if rejected.Response.RequestID != "req-1" {
		t.Fatalf("want the pre-redaction request id req-1, got %q", rejected.Response.RequestID)
	}
	if rejected.Response.Response == nil || rejected.Response.Response.Kind != oarlock.ResponseRejected {
		t.Fatalf("the redactor rewrote Response.Kind: %+v", rejected.Response.Response)
	}
	if rejected.Reason != redactedContent {
		t.Fatalf("content changes must be kept, got %q", rejected.Reason)
	}

	// A hook that drops the bodies entirely must not be able to hide reachability:
	// the original Request/Response body is restored.
	dropping := New("sess-1", func(record oarlock.Record) (oarlock.Record, bool) {
		record.Request = nil
		record.Response = nil
		return record, true
	})
	if droppedOutcome := dropping.Control(oarlock.RequestBody{Action: oarlock.ActionDispose}, acceptAll, RecordAt{}); droppedOutcome.Kind != oarlock.ControlAccepted {
		t.Fatalf("want accepted dispose, got %+v", droppedOutcome)
	}
	if unreachable := dropping.Unreachable(); unreachable == nil || unreachable.Code != oarlock.Disposed {
		t.Fatalf("a dropped body hid the dispose: got %+v", unreachable)
	}
	droppedRecords := dropping.Records(Cursor{})
	if len(droppedRecords) != 2 {
		t.Fatalf("want the dispose request and its response, got %d records", len(droppedRecords))
	}
	if droppedRecords[0].Request == nil || droppedRecords[0].Request.Action != oarlock.ActionDispose {
		t.Fatalf("the dispose body was not restored: %+v", droppedRecords[0].Request)
	}
	if droppedRecords[1].Response == nil || droppedRecords[1].Response.Kind != oarlock.ResponseAccepted {
		t.Fatalf("the response body was not restored: %+v", droppedRecords[1].Response)
	}

	// The same for an exit response: a dropped body cannot hide it either.
	hidingExit := New("sess-1", func(record oarlock.Record) (oarlock.Record, bool) {
		record.Response = nil
		return record, true
	})
	hidingExit.Respond("", oarlock.ResponseBody{Kind: oarlock.ResponseExited}, RecordAt{})
	if unreachable := hidingExit.Unreachable(); unreachable == nil || unreachable.Code != oarlock.RuntimeExited {
		t.Fatalf("a dropped response body hid the exit: got %+v", unreachable)
	}
	exitRecords := hidingExit.Records(Cursor{})
	if len(exitRecords) != 1 || exitRecords[0].Response == nil || exitRecords[0].Response.Kind != oarlock.ResponseExited {
		t.Fatalf("the exit body was not restored: %+v", exitRecords)
	}
}

// TestKernel_PanickingRedactorCannotChangeStreamStructure pins the panic half of
// the content-only rule: a hook that mutates the record it was handed and then
// panics leaves nothing behind, because the record is retained as it was built.
// Otherwise a broken hook could turn a dispose into a prompt (hiding reachability)
// or a rejection into an exit (fabricating one).
func TestKernel_PanickingRedactorCannotChangeStreamStructure(t *testing.T) {
	const panickingContent = "mutated before the panic"
	mutating := func(record oarlock.Record) (oarlock.Record, bool) {
		// Structure, mutated in place through the pointers the clone carries.
		if record.Request != nil {
			record.Request.Action = oarlock.ActionPrompt
			record.Request.Input = panickingContent
		}
		if record.Response != nil {
			record.Response.Kind = oarlock.ResponseExited
			record.Response.Reason = panickingContent
		}
		panic("hook boom")
	}

	// A dispose request must stay a dispose request, so the session still ends up
	// disposed instead of the kernel believing the runtime exited.
	k := New("sess-1", mutating)
	outcome := k.Control(oarlock.RequestBody{Action: oarlock.ActionDispose}, acceptAll, RecordAt{})
	if outcome.Kind != oarlock.ControlAccepted || outcome.Code != "" {
		t.Fatalf("a panicking hook rewrote the control answer: %+v", outcome)
	}
	unreachable := k.Unreachable()
	if unreachable == nil || unreachable.Code != oarlock.Disposed {
		t.Fatalf("a panicking hook hid the dispose: %+v", unreachable)
	}
	records := k.Records(Cursor{})
	if len(records) != 2 {
		t.Fatalf("want the dispose request and its response, got %d records", len(records))
	}
	if records[0].Request == nil || records[0].Request.Action != oarlock.ActionDispose {
		t.Fatalf("the dispose request was rewritten: %+v", records[0].Request)
	}
	if records[0].Request.Input != "" {
		t.Fatalf("a panicking hook kept a content mutation: %q", records[0].Request.Input)
	}
	if records[1].Response == nil || records[1].Response.Kind != oarlock.ResponseAccepted {
		t.Fatalf("the response body was rewritten: %+v", records[1].Response)
	}

	// A rejection must not become an exit: reachability keeps agreeing with the
	// stream the kernel actually retained.
	rejecting := New("sess-1", mutating)
	rejected := rejecting.Control(
		oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "hi"},
		func(RequestRecord) oarlock.ResponseBody {
			return oarlock.ResponseBody{Kind: oarlock.ResponseRejected, Code: oarlock.RuntimeRefused, Reason: "real reason"}
		},
		RecordAt{},
	)
	if rejected.Kind != oarlock.ControlRejected || rejected.Code != oarlock.RuntimeRefused {
		t.Fatalf("a panicking hook changed the control answer: %+v", rejected)
	}
	if rejected.Reason != "real reason" {
		t.Fatalf("a panicking hook kept a content mutation: %q", rejected.Reason)
	}
	if unreachable := rejecting.Unreachable(); unreachable != nil {
		t.Fatalf("a panicking hook fabricated an exit: %+v", unreachable)
	}
	response := rejected.Response.Response
	if response == nil || response.Kind != oarlock.ResponseRejected || response.Reason != "real reason" {
		t.Fatalf("the rejected response body was rewritten: %+v", response)
	}

	// An exit recorded without the hook must still be an exit: containment must not
	// go the other way and swallow structure the kernel built itself.
	exiting := New("sess-1", mutating)
	exiting.Respond("", oarlock.ResponseBody{Kind: oarlock.ResponseExited, ExitCodeReported: true}, RecordAt{})
	if unreachable := exiting.Unreachable(); unreachable == nil || unreachable.Code != oarlock.RuntimeExited {
		t.Fatalf("containing the hook lost the real exit: %+v", unreachable)
	}
}

// TestKernel_RedactsEachRecordExactlyOnce pins the hook contract: one call per
// retained record, the observed and retained values are the same, and decide
// still sees the original (unredacted) request.
func TestKernel_RedactsEachRecordExactlyOnce(t *testing.T) {
	const (
		secret   = "sk-live-cafebabe1234"
		redacted = "[redacted]"
	)

	calls := 0
	k := New("sess-1", func(record oarlock.Record) (oarlock.Record, bool) {
		calls++
		if record.Request != nil {
			record.Request.Input = redacted
		}
		return record, true
	})

	var observed []Record
	k.Observe(func(record Record) { observed = append(observed, record) })

	k.Frame(frameBody("f"), RecordAt{})
	decidedInput := ""
	outcome := k.Control(
		oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: secret},
		func(request RequestRecord) oarlock.ResponseBody {
			decidedInput = request.Request.Input
			return acceptAll(request)
		},
		RecordAt{},
	)
	if outcome.Kind != oarlock.ControlAccepted {
		t.Fatalf("want accepted, got %+v", outcome)
	}

	if calls != 3 {
		t.Fatalf("the redactor ran %d times, want once per retained record (frame + request + response)", calls)
	}
	if decidedInput != secret {
		t.Fatalf("decide saw %q, want the original value", decidedInput)
	}

	records := k.Records(Cursor{})
	if len(records) != 3 {
		t.Fatalf("want 3 retained records, got %d", len(records))
	}
	if len(observed) != len(records) {
		t.Fatalf("the live tap saw %d records, the log holds %d", len(observed), len(records))
	}
	for index := range records {
		if !reflect.DeepEqual(observed[index], records[index]) {
			t.Fatalf("record %d differs between the live tap and the retained log", records[index].Seq)
		}
	}
	if records[1].Request == nil || records[1].Request.Input != redacted {
		t.Fatalf("the retained request is not redacted: %+v", records[1].Request)
	}
}

// TestKernel_PanickingRedactorIsContained pins that a redactor that panics on
// every record cannot deadlock the kernel or lose the stream: the record is
// retained as it was built and the kernel stays usable.
func TestKernel_PanickingRedactorIsContained(t *testing.T) {
	calls := 0
	k := New("sess-1", func(oarlock.Record) (oarlock.Record, bool) {
		calls++
		panic("redactor boom")
	})

	frame := k.Frame(frameBody("f"), RecordAt{})
	if frame.Seq != 1 || frame.Kind != oarlock.KindFrame {
		t.Fatalf("the frame was not retained as built: %+v", frame)
	}
	if string(frame.Native) != `{"type":"f"}` {
		t.Fatalf("the frame content was lost: %s", frame.Native)
	}

	// A panicking hook must not hold the mutex: Control and the reachability read
	// below would hang if it did.
	outcome := k.Control(oarlock.RequestBody{Action: oarlock.ActionDispose}, acceptAll, RecordAt{})
	if outcome.Kind != oarlock.ControlAccepted {
		t.Fatalf("want accepted dispose, got %+v", outcome)
	}
	unreachable := k.Unreachable()
	if unreachable == nil || unreachable.Code != oarlock.Disposed || unreachable.Reason != "session disposed" {
		t.Fatalf("want disposed, got %+v", unreachable)
	}
	if calls != 3 {
		t.Fatalf("the redactor ran %d times, want once per record", calls)
	}

	records := k.Records(Cursor{})
	if got := seqsOf(records); !reflect.DeepEqual(got, []uint64{1, 2, 3}) {
		t.Fatalf("the stream is not dense after the panics: %v", got)
	}
	if records[1].Request == nil || records[1].Request.Action != oarlock.ActionDispose {
		t.Fatalf("the unredacted request was not retained as built: %+v", records[1].Request)
	}
	if records[2].Response == nil || records[2].Response.Kind != oarlock.ResponseAccepted {
		t.Fatalf("the unredacted response was not retained as built: %+v", records[2].Response)
	}

	// The run continues.
	if next := k.Frame(frameBody("after"), RecordAt{}); next.Seq != 4 {
		t.Fatalf("the kernel was left unusable: %+v", next)
	}
}

// TestRequest_UsesRuntimeIssuedIDAndControlGeneratesItsOwn pins the id rules: a
// runtime-issued toApp id keeps its own spelling, a missing one becomes
// req-<seq>, and Control ignores RecordAt.ID.
func TestRequest_UsesRuntimeIssuedIDAndControlGeneratesItsOwn(t *testing.T) {
	k := newKernel("sess-1", nil)

	issued := k.Request(oarlock.ToApp, oarlock.RequestBody{
		Action:     oarlock.ActionNative,
		NativeType: "permission",
		Input:      "allow?",
	}, RecordAt{ID: "native-42"})
	if issued.ID != "native-42" {
		t.Fatalf("want the runtime-issued id kept, got %q", issued.ID)
	}
	if issued.Direction != oarlock.ToApp {
		t.Fatalf("want a toApp request, got %q", issued.Direction)
	}

	generated := k.Request(oarlock.ToRuntime, oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "hi"}, RecordAt{})
	if generated.ID != "req-2" {
		t.Fatalf("want req-2 for the second record, got %q", generated.ID)
	}

	controlled := k.Control(oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "hi"}, acceptAll, RecordAt{ID: "ignored"})
	if controlled.RequestID != "req-3" {
		t.Fatalf("Control must generate its own id, got %q", controlled.RequestID)
	}
	if controlled.Request.ID != "req-3" {
		t.Fatalf("the retained request id is %q, want req-3", controlled.Request.ID)
	}
	if controlled.Response.RequestID != controlled.Request.ID {
		t.Fatalf("the response points at %q, the request is %q", controlled.Response.RequestID, controlled.Request.ID)
	}

	records := k.Records(Cursor{})
	last := records[len(records)-1]
	if last.Kind != oarlock.KindResponse || last.RequestID != "req-3" {
		t.Fatalf("want the retained response pointing at req-3, got %+v", last)
	}
}

// TestRecordAt_StampsAttributionAndRequestIDs pins the envelope the kernel
// stamps: attribution from RecordAt, the kernel's session and the root agent as
// the defaults, runtime-issued request ids kept, and Control generating its own.
func TestRecordAt_StampsAttributionAndRequestIDs(t *testing.T) {
	k := newKernel("sess-1", nil)

	// The exported accessor reports the instance identity the cursors match.
	if got := k.SessionID(); got != "sess-1" {
		t.Fatalf("SessionID() = %q, want sess-1", got)
	}

	stamp := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	attributed := k.Frame(frameBody("attributed"), RecordAt{
		SessionID:  "child-1",
		AgentPath:  []string{"root", "child"},
		SpanID:     "turn-9",
		ReceivedAt: stamp,
	})
	if attributed.SessionID != "child-1" {
		t.Fatalf("SessionID %q, want the child session", attributed.SessionID)
	}
	if !reflect.DeepEqual(attributed.AgentPath, []string{"root", "child"}) {
		t.Fatalf("AgentPath %v, want the sub-agent lineage", attributed.AgentPath)
	}
	if attributed.SpanID != "turn-9" {
		t.Fatalf("SpanID %q, want the runtime-native span", attributed.SpanID)
	}
	if !attributed.ReceivedAt.Equal(stamp) {
		t.Fatalf("ReceivedAt %v, want the supplied %v", attributed.ReceivedAt, stamp)
	}

	// Empty attribution means the kernel's own session and the root agent, and
	// the kernel never invents a span id.
	defaulted := k.Frame(frameBody("defaulted"), RecordAt{})
	if defaulted.SessionID != "sess-1" || defaulted.SpanID != "" || defaulted.AgentPath != nil {
		t.Fatalf("want the kernel session, no span and no agent path, got %+v", defaulted)
	}
	if defaulted.ReceivedAt.IsZero() {
		t.Fatal("ReceivedAt was not stamped")
	}

	// A runtime-issued request id is kept; a missing one becomes req-<seq>.
	issued := k.Request(oarlock.ToApp, oarlock.RequestBody{Action: oarlock.ActionNative, NativeType: "permission"}, RecordAt{ID: "native-42"})
	if issued.ID != "native-42" || issued.Direction != oarlock.ToApp {
		t.Fatalf("want the runtime-issued id kept, got %+v", issued)
	}
	generated := k.Request(oarlock.ToRuntime, oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "hi"}, RecordAt{})
	if generated.ID != "req-4" {
		t.Fatalf("want req-4, got %q", generated.ID)
	}

	// Control ignores RecordAt.ID and generates its own id.
	outcome := k.Control(oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "hi"}, acceptAll, RecordAt{ID: "ignored"})
	if outcome.RequestID != "req-5" {
		t.Fatalf("Control used %q, want its own req-5", outcome.RequestID)
	}
	if outcome.Request.ID != outcome.RequestID {
		t.Fatalf("outcome request id %q differs from RequestID %q", outcome.Request.ID, outcome.RequestID)
	}
}
