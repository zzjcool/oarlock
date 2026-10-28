package scripted

import (
	"context"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

// The core smoke tests pin the contract's headline behaviours with the fewest
// moving parts; the fault, leak and suite tests exercise the same paths wider.

func smokeWait(t *testing.T, s *Session, what string, ok func(fold.Status) bool) fold.Status {
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

func smokeIs(k fold.StatusKind) func(fold.Status) bool {
	return func(st fold.Status) bool { return st.Kind == k }
}

func TestSmokePromptBusyAbortDispose(t *testing.T) {
	s, err := NewWithOptions(Options{AbortGrace: 100 * time.Millisecond},
		Turn{Say("ok")}, Turn{Say("working"), Hang()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	out, _ := s.Prompt(ctx, "one", oarlock.PromptOptions{})
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v", out)
	}
	st := smokeWait(t, s, "idle after turn", smokeIs(fold.StatusIdle))
	if st.LastTurnOutcome == nil || st.LastTurnOutcome.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("outcome = %+v", st.LastTurnOutcome)
	}

	if out, _ = s.Prompt(ctx, "two", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("second prompt = %+v", out)
	}
	smokeWait(t, s, "running", smokeIs(fold.StatusRunning))
	if out, _ = s.Prompt(ctx, "three", oarlock.PromptOptions{}); out.Code != oarlock.Busy {
		t.Fatalf("prompt while running = %+v, want busy", out)
	}
	if out, _ = s.Abort(ctx); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("abort = %+v", out)
	}
	st = smokeWait(t, s, "idle after abort", smokeIs(fold.StatusIdle))
	if st.LastTurnOutcome == nil || st.LastTurnOutcome.Kind != oarlock.OutcomeAborted {
		t.Fatalf("abort outcome = %+v", st.LastTurnOutcome)
	}
	if out, _ = s.Abort(ctx); out.Code != oarlock.NoActiveTurn {
		t.Fatalf("abort while idle = %+v", out)
	}

	if err := s.Dispose(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Dispose(ctx); err != nil {
		t.Fatal(err)
	}
	if out, _ = s.Prompt(ctx, "late", oarlock.PromptOptions{}); out.Kind != oarlock.ControlRejected {
		t.Fatalf("prompt after dispose = %+v", out)
	}
	for i, r := range s.Records(oarlock.Cursor{}) {
		if r.Seq != uint64(i+1) {
			t.Fatalf("Seq %d at %d: not dense", r.Seq, i)
		}
	}
}

func TestSmokeDieAndTakeOver(t *testing.T) {
	s := New(Turn{Say("a"), Die(7)})
	ctx := context.Background()
	if out, _ := s.Prompt(ctx, "x", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v", out)
	}
	smokeWait(t, s, "exited", smokeIs(fold.StatusExited))
	for name, call := range map[string]func() (oarlock.ControlOutcome, error){
		"prompt": func() (oarlock.ControlOutcome, error) { return s.Prompt(ctx, "y", oarlock.PromptOptions{}) },
		"queue":  func() (oarlock.ControlOutcome, error) { return s.Queue(ctx, "y", oarlock.InputOptions{}) },
		"abort":  func() (oarlock.ControlOutcome, error) { return s.Abort(ctx) },
	} {
		if out, _ := call(); out.Kind != oarlock.ControlRejected || out.Code != oarlock.RuntimeExited {
			t.Fatalf("%s after exit = %+v, want rejected/runtime_exited", name, out)
		}
	}
	if err := s.Dispose(ctx); err != nil {
		t.Fatal(err)
	}

	h, _ := NewWithOptions(Options{AbortGrace: 80 * time.Millisecond}, Turn{IgnoreAbort(), Hang()})
	if out, _ := h.Prompt(ctx, "x", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v", out)
	}
	smokeWait(t, h, "running", smokeIs(fold.StatusRunning))
	start := time.Now()
	out, _ := h.Abort(ctx)
	if out.Kind != oarlock.ControlAccepted || time.Since(start) < 80*time.Millisecond {
		t.Fatalf("abort = %+v after %v, want accepted after the grace", out, time.Since(start))
	}
	smokeWait(t, h, "exited", smokeIs(fold.StatusExited))
	_ = h.Dispose(ctx)
}
