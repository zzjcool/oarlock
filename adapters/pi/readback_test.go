package pi

import (
	"testing"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/kernel"
)

func readbackSession(t *testing.T) (*Session, kernel.Kernel) {
	t.Helper()
	k := kernel.New("sess-readback", nil)
	return &Session{k: k}, k
}

func TestReadback_EmptyStreamIsHonestAboutAbsence(t *testing.T) {
	s, _ := readbackSession(t)

	if got := s.Status(); got.Value.Kind != "idle" || got.Seq != 0 {
		t.Fatalf("status = %+v, want idle at seq 0", got)
	}
	if got := s.Usage(); got.Value.Total != nil || got.Seq != 0 {
		t.Fatalf("usage = %+v, want nil at seq 0", got)
	}
	if got := s.ContextUsage(); got.Value != nil || got.Seq != 0 {
		t.Fatalf("context = %+v, want nil at seq 0", got)
	}
	if got := s.Model(); got.Value != "" || got.Seq != 0 {
		t.Fatalf("model = %+v, want empty at seq 0", got)
	}
	if got := s.Effort(); got.Value != "" || got.Seq != 0 {
		t.Fatalf("effort = %+v, want empty at seq 0", got)
	}
	if got := s.PiVersion(); got != "" {
		t.Fatalf("PiVersion = %q, want empty", got)
	}
}

func TestReadback_ModelAndEffortComeFromNativeEvents(t *testing.T) {
	s, k := readbackSession(t)
	k.Frame(kernel.FrameBody{Native: []byte(`{"type":"response","command":"get_state"}`), Events: []oarlock.Event{
		{Kind: oarlock.EventModel, Model: "provider/one"},
		{Kind: oarlock.EventEffort, Effort: "high"},
	}}, kernel.RecordAt{})
	k.Frame(kernel.FrameBody{Native: []byte(`{"type":"model_changed"}`), Events: []oarlock.Event{
		{Kind: oarlock.EventModel, Model: "provider/two"},
		{Kind: oarlock.EventEffort, Effort: "low"},
	}}, kernel.RecordAt{})

	model := s.Model()
	if model.Value != "provider/two" || model.Seq != 2 {
		t.Fatalf("model = %+v, want provider/two at seq 2", model)
	}
	effort := s.Effort()
	if effort.Value != "low" || effort.Seq != 2 {
		t.Fatalf("effort = %+v, want low at seq 2", effort)
	}
}

func TestReadback_ContextAndUsageAreFolded(t *testing.T) {
	s, k := readbackSession(t)
	tokens := int64(1234)
	window := int64(200000)
	percent := 0.617
	in := int64(100)
	out := int64(42)
	k.Frame(kernel.FrameBody{Native: []byte(`{"type":"usage"}`), Events: []oarlock.Event{{
		Kind: oarlock.EventUsage,
		Usage: &oarlock.UsageReport{
			Context: &oarlock.ContextUsage{Tokens: &tokens, ContextWindow: &window, Percent: &percent},
			Tokens:  &oarlock.TokenTotals{Input: in, Output: out},
		},
	}}}, kernel.RecordAt{})

	ctx := s.ContextUsage()
	if ctx.Value == nil || ctx.Value.Usage.Tokens == nil || *ctx.Value.Usage.Tokens != tokens {
		t.Fatalf("context = %+v, want tokens %d", ctx, tokens)
	}
	if ctx.Value.Seq != 1 || ctx.Seq != 1 {
		t.Fatalf("context seq = %d/%d, want 1", ctx.Value.Seq, ctx.Seq)
	}
	usage := s.Usage()
	if usage.Value.Total == nil || usage.Value.Total.Input != in || usage.Value.Total.Output != out {
		t.Fatalf("usage = %+v, want input %d output %d", usage, in, out)
	}
}

func TestReadback_StatusGoesRunningThenIdle(t *testing.T) {
	s, k := readbackSession(t)
	k.Frame(kernel.FrameBody{Native: []byte(`{"type":"agent_start"}`), Events: []oarlock.Event{{Kind: oarlock.EventTurnActive}}}, kernel.RecordAt{})
	if got := s.Status(); got.Value.Kind != "running" || got.Seq != 1 {
		t.Fatalf("status = %+v, want running at seq 1", got)
	}
	k.Frame(kernel.FrameBody{Native: []byte(`{"type":"agent_settled"}`), Events: []oarlock.Event{{
		Kind:    oarlock.EventTurnEnded,
		Outcome: &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted},
	}}}, kernel.RecordAt{})
	got := s.Status()
	if got.Value.Kind != "idle" || got.Seq != 2 {
		t.Fatalf("status = %+v, want idle at seq 2", got)
	}
	if got.Value.LastTurnOutcome == nil || got.Value.LastTurnOutcome.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("last outcome = %+v, want completed", got.Value.LastTurnOutcome)
	}
}
