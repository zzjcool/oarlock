package observe_test

import (
	"reflect"
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

// readbackStub implements exactly the five readers Readback is composed of and
// nothing else, so assigning it to observe.Readback can only compile if the
// union needs no more than these five methods.
type readbackStub struct{}

func (readbackStub) Status() observe.QueryResult[observe.Status] {
	return observe.QueryResult[observe.Status]{}
}

func (readbackStub) Usage() observe.QueryResult[observe.Usage] {
	return observe.QueryResult[observe.Usage]{}
}

func (readbackStub) ContextUsage() observe.QueryResult[*observe.ContextReport] {
	return observe.QueryResult[*observe.ContextReport]{}
}

func (readbackStub) Model() observe.QueryResult[string]  { return observe.QueryResult[string]{} }
func (readbackStub) Effort() observe.QueryResult[string] { return observe.QueryResult[string]{} }

// readbackStubNoEffort is readbackStub minus Effort: four of the five readers,
// one short of what observe.Readback asks for.
type readbackStubNoEffort struct{}

func (readbackStubNoEffort) Status() observe.QueryResult[observe.Status] {
	return observe.QueryResult[observe.Status]{}
}

func (readbackStubNoEffort) Usage() observe.QueryResult[observe.Usage] {
	return observe.QueryResult[observe.Usage]{}
}

func (readbackStubNoEffort) ContextUsage() observe.QueryResult[*observe.ContextReport] {
	return observe.QueryResult[*observe.ContextReport]{}
}

func (readbackStubNoEffort) Model() observe.QueryResult[string] { return observe.QueryResult[string]{} }

// readbackMethodNames lists a type's methods in reflect's (lexical) order.
func readbackMethodNames(t reflect.Type) []string {
	names := make([]string, t.NumMethod())
	for i := range names {
		names[i] = t.Method(i).Name
	}
	return names
}

// TestReadbackIsTheUnionOfTheFiveReaders pins observe.Readback to exactly the
// five reader folds, so the union cannot silently gain or lose a member.
func TestReadbackIsTheUnionOfTheFiveReaders(t *testing.T) {
	// Positive half, checked by the compiler: a type whose method set is exactly
	// the five readers is a Readback. If Readback ever grows a sixth member, this
	// line stops building.
	var _ observe.Readback = readbackStub{}

	// Negative half. `var _ observe.Readback = readbackStubNoEffort{}` would be a
	// permanent compile error in this package and a //go:build never file to hold
	// it is not worth shipping, so the same fact is asserted at run time from the
	// interface's own method set: Readback must name exactly the five readers.
	// Dropping a member fails the count, and the missing-Effort stub below fails
	// the type assertion.
	want := []string{"ContextUsage", "Effort", "Model", "Status", "Usage"} // reflect sorts
	rt := reflect.TypeOf((*observe.Readback)(nil)).Elem()
	if got := readbackMethodNames(rt); rt.NumMethod() != len(want) {
		t.Fatalf("observe.Readback has methods %v, want exactly %v", got, want)
	}
	for i, name := range want {
		if got := rt.Method(i).Name; got != name {
			t.Fatalf("observe.Readback method %d = %q, want %q", i, got, name)
		}
	}

	// The four-reader stub really does satisfy the four readers it implements, so
	// the negative assertion below cannot pass merely because the stub matches
	// nothing.
	var (
		_ observe.StatusReader       = readbackStubNoEffort{}
		_ observe.UsageReader        = readbackStubNoEffort{}
		_ observe.ContextUsageReader = readbackStubNoEffort{}
		_ observe.ModelReader        = readbackStubNoEffort{}
	)
	if _, ok := any(readbackStubNoEffort{}).(observe.Readback); ok {
		t.Fatalf("a value lacking Effort satisfied observe.Readback; absence must be the capability, never a runtime error")
	}
	if _, ok := any(readbackStub{}).(observe.Readback); !ok {
		t.Fatalf("a value implementing all five readers did not satisfy observe.Readback")
	}
}
