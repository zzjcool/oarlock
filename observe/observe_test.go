package observe_test

import (
	"testing"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/observe"
)

func record(seq uint64, events ...oarlock.Event) oarlock.Record {
	return oarlock.Record{Seq: seq, Kind: oarlock.KindFrame, SessionID: "s", Events: events}
}

func TestEmptyStreamIsIdleWithNoUsageOrContext(t *testing.T) {
	if got := observe.StatusOf(nil); got.Value.Kind != observe.StatusIdle || got.Seq != 0 {
		t.Fatalf("status = %+v, want idle at seq 0", got)
	}
	if got := observe.UsageOf(nil); got.Value.Total != nil || got.Seq != 0 {
		t.Fatalf("usage = %+v, want nil at seq 0", got)
	}
	if got := observe.ContextUsageOf(nil); got.Value != nil || got.Seq != 0 {
		t.Fatalf("context = %+v, want nil at seq 0", got)
	}
}

func TestStatusRunsThenIdles(t *testing.T) {
	records := []oarlock.Record{
		record(1, oarlock.Event{Kind: oarlock.EventTurnActive}),
		record(2, oarlock.Event{Kind: oarlock.EventTurnEnded, Outcome: &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted}}),
	}
	if got := observe.StatusOf(records[:1]); got.Value.Kind != observe.StatusRunning || got.Seq != 1 {
		t.Fatalf("running = %+v", got)
	}
	got := observe.StatusOf(records)
	if got.Value.Kind != observe.StatusIdle || got.Seq != 2 || got.Value.LastTurnOutcome == nil {
		t.Fatalf("idle = %+v", got)
	}
}

func TestModelEffortAndConversation(t *testing.T) {
	records := []oarlock.Record{
		record(1,
			oarlock.Event{Kind: oarlock.EventModel, Model: "p/one"},
			oarlock.Event{Kind: oarlock.EventEffort, Effort: "high"},
		),
		record(2, oarlock.Event{Kind: oarlock.EventTextDelta, Text: "hello ", MessageID: "m1"}),
		record(3, oarlock.Event{Kind: oarlock.EventTextDelta, Text: "world", MessageID: "m1"}),
	}
	if got := observe.ModelOf(records); got.Value != "p/one" || got.Seq != 1 {
		t.Fatalf("model = %+v", got)
	}
	if got := observe.EffortOf(records); got.Value != "high" || got.Seq != 1 {
		t.Fatalf("effort = %+v", got)
	}
	conv := observe.ConversationOf(records)
	if len(conv.Value) != 1 || conv.Value[0].Text != "hello world" {
		t.Fatalf("conversation = %+v", conv.Value)
	}
}
