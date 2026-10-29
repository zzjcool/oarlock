package acp

// opencode_steer_test.go — the one fact a steer refusal must state: the native
// side refused the input oarlock had already accepted, so the answer frame
// carries input_dropped{runtime_refused} for that input and nothing else. The
// original prompt's answer stays the only turn end (SteerSupersedes=false for
// the v1 line, where a steer is a concurrent session/prompt with empty _meta).
//
// Pure sink level: the wire lines are hand-written JSON-RPC, no agent process.

import (
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/kernel"
)

// steerLine is oarlock's own steering session/prompt as the writer tap sees
// it: the v1 shape (empty _meta), one text block.
const steerLine = `{"jsonrpc":"2.0","id":7,"method":"session/prompt","params":{"sessionId":"sid","prompt":[{"type":"text","text":"IMPORTANT: end your reply with BANANA."}],"_meta":{}}}`

// steerSink is a v1 sink attached to a fresh kernel, with one armed steer whose
// request the writer tap has already seen.
func steerSink(t *testing.T) (*sink, kernel.Kernel, *promptCall) {
	t.Helper()
	s := newSink(OpenCodeV1)
	k := kernel.New("sid", nil)
	s.attach(k)
	call := &promptCall{kind: kindSteer, inputID: "in-1", written: make(chan struct{})}
	s.arm(call)
	s.toAgent([]byte(steerLine))
	if call.id != "7" {
		t.Fatalf("the writer tap bound the steer to %q, want the wire id 7", call.id)
	}
	return s, k, call
}

func TestOpenCodeSteerRefusalDropsInput(t *testing.T) {
	s, k, _ := steerSink(t)
	// The runtime refuses the steer RPC: the input was dropped natively.
	s.fromAgent([]byte(`{"jsonrpc":"2.0","id":7,"error":{"code":-32603,"message":"steer refused"}}`))

	recs := k.Records(oarlock.Cursor{})
	drops := events(recs, oarlock.EventInputDropped)
	if len(drops) != 1 {
		t.Fatalf("input_dropped = %d, want exactly one:\n%s", len(drops), dump(recs))
	}
	if drops[0].InputID != "in-1" || drops[0].Dropped != oarlock.DroppedRuntimeRefused {
		t.Fatalf("input_dropped = %+v, want {in-1 runtime_refused}", drops[0])
	}
	// The drop is stated on the answer frame, not synthesized elsewhere.
	if r := frameOf(t, recs, oarlock.EventInputDropped); !strings.Contains(string(r.Native), `"error"`) {
		t.Fatalf("the drop is not on the runtime's error answer: %s", r.Native)
	}
	// A refused steer never ends the turn.
	if ends := events(recs, oarlock.EventTurnEnded); len(ends) != 0 {
		t.Fatalf("turn_ended = %d, want none from a steer answer:\n%s", len(ends), dump(recs))
	}
}

func TestOpenCodeSteerAnswerDropsNothing(t *testing.T) {
	s, k, _ := steerSink(t)
	// A steer answered without an error: recorded, no drop, no turn end.
	s.fromAgent([]byte(`{"jsonrpc":"2.0","id":7,"result":{"stopReason":"end_turn","_meta":{}}}`))

	recs := k.Records(oarlock.Cursor{})
	// The answer is recorded (the absence of events must not be the absence of
	// the frame itself).
	var answered bool
	for _, r := range recs {
		if r.Kind == oarlock.KindFrame && strings.Contains(string(r.Native), `"stopReason"`) {
			answered = true
		}
	}
	if !answered {
		t.Fatalf("the steer answer frame was not recorded:\n%s", dump(recs))
	}
	if drops := events(recs, oarlock.EventInputDropped); len(drops) != 0 {
		t.Fatalf("input_dropped = %d, want none for an answered steer:\n%s", len(drops), dump(recs))
	}
	if ends := events(recs, oarlock.EventTurnEnded); len(ends) != 0 {
		t.Fatalf("turn_ended = %d, want none from a steer answer:\n%s", len(ends), dump(recs))
	}
}

func TestOpenCodeTurnErrorDropsNoInput(t *testing.T) {
	s := newSink(OpenCodeV1)
	k := kernel.New("sid", nil)
	s.attach(k)
	call := &promptCall{kind: kindTurn, inputID: "in-2", written: make(chan struct{})}
	s.arm(call)
	s.toAgent([]byte(`{"jsonrpc":"2.0","id":9,"method":"session/prompt","params":{"sessionId":"sid","prompt":[{"type":"text","text":"hi"}]}}`))
	s.fromAgent([]byte(`{"jsonrpc":"2.0","id":9,"error":{"code":-32000,"message":"Authentication required"}}`))

	recs := k.Records(oarlock.Cursor{})
	// A failed turn is a turn end (the runtime's own answer), never a drop.
	if drops := events(recs, oarlock.EventInputDropped); len(drops) != 0 {
		t.Fatalf("input_dropped = %d, want none for a turn answer:\n%s", len(drops), dump(recs))
	}
	if ends := events(recs, oarlock.EventTurnEnded); len(ends) != 1 {
		t.Fatalf("turn_ended = %d, want exactly one for a failed turn:\n%s", len(ends), dump(recs))
	}
}

// frameOf returns the frame record that carries the given event kind.
func frameOf(t *testing.T, recs []oarlock.Record, kind oarlock.EventKind) oarlock.Record {
	t.Helper()
	for _, r := range recs {
		for _, e := range r.Events {
			if e.Kind == kind {
				return r
			}
		}
	}
	t.Fatalf("no frame carries %s\n%s", kind, dump(recs))
	return oarlock.Record{}
}
