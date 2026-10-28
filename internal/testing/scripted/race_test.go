package scripted

// Interleaving tests (issue #18). Each asserts an invariant of the stream that
// must hold whatever the scheduler does, so a failure is a real bug and never
// a flake: the timings below only make the interesting interleavings likely.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

// A take-over must never land after a turn that already reported its own end:
// with one turn in the script, the stream holds a turn_ended or a standalone
// exited response, never both. The turn ends by itself at about the moment the
// abort grace expires, which is where the two used to be able to cross.
func TestTakeOverNeverFollowsACompletedTurn(t *testing.T) {
	defer faultWatchdog(t, 60*time.Second)()
	const grace = 3 * time.Millisecond
	for i := 0; i < 120; i++ {
		d := grace - 2*time.Millisecond + time.Duration(i%5)*time.Millisecond/2 // 1ms .. 3ms around the grace
		s, err := NewWithOptions(Options{AbortGrace: grace}, Turn{IgnoreAbort(), Delay(d)})
		if err != nil {
			t.Fatal(err)
		}
		if out, _ := s.Prompt(context.Background(), "go", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
			t.Fatalf("prompt = %+v", out)
		}
		out, _ := s.Abort(context.Background())
		// Either the turn ended first (abort accepted, or rejected no_active_turn
		// when it was already over) or the take-over won (accepted).
		if out.Kind == oarlock.ControlRejected && out.Code != oarlock.NoActiveTurn {
			t.Fatalf("iteration %d: abort = %+v", i, out)
		}
		_ = s.Dispose(context.Background())

		records := s.Records(oarlock.Cursor{})
		ended := faultCount(records, oarlock.EventTurnEnded)
		var standalone int
		for _, r := range faultExits(records) {
			if r.RequestID == "" {
				standalone++
			}
		}
		if ended > 0 && standalone > 0 {
			t.Fatalf("iteration %d: the stream holds both a turn_ended and a take-over exit:\n%s", i, describe(records))
		}
		if ended > 1 || standalone > 1 {
			t.Fatalf("iteration %d: turn_ended=%d take-over exits=%d", i, ended, standalone)
		}
		faultDense(t, records)
	}
}

// Under concurrent controls no scripted frame may follow the exited response,
// the stream stays dense, there is one dispose request and one exited response.
func TestNoFrameFollowsTheExitUnderConcurrentControls(t *testing.T) {
	defer faultWatchdog(t, 60*time.Second)()
	for round := 0; round < 25; round++ {
		script := make([]Turn, 12)
		for i := range script {
			script[i] = Turn{Say("x"), Delay(time.Millisecond), Say("y"), Hang()}
		}
		s, err := NewWithOptions(Options{AbortGrace: 5 * time.Millisecond}, script...)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for g := 0; g < 6; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				ctx := context.Background()
				for i := 0; i < 15; i++ {
					switch (g + i) % 5 {
					case 0:
						_, _ = s.Prompt(ctx, "p", oarlock.PromptOptions{})
					case 1:
						_, _ = s.Queue(ctx, "q", oarlock.InputOptions{})
					case 2, 3:
						_, _ = s.Abort(ctx)
					default:
						if g == 5 && i == 9 {
							_ = s.Dispose(ctx)
						}
					}
				}
			}(g)
		}
		wg.Wait()
		_ = s.Dispose(context.Background())

		records := s.Records(oarlock.Cursor{})
		faultDense(t, records)
		exits := faultExits(records)
		if len(exits) != 1 {
			t.Fatalf("round %d: exited responses = %d, want 1", round, len(exits))
		}
		for _, r := range records {
			if r.Kind == oarlock.KindFrame && r.Seq > exits[0].Seq {
				t.Fatalf("round %d: frame Seq %d follows the exited response (Seq %d)", round, r.Seq, exits[0].Seq)
			}
		}
		if n := faultRequests(records, oarlock.ActionDispose); n > 1 {
			t.Fatalf("round %d: dispose requests = %d, want at most 1", round, n)
		}
		// A turn_ended is the runtime's own: never more turn_ended than turn_active.
		if ended, active := faultCount(records, oarlock.EventTurnEnded), faultCount(records, oarlock.EventTurnActive); ended > active {
			t.Fatalf("round %d: %d turn_ended for %d turn_active", round, ended, active)
		}
		// Every request gets exactly one control answer. The one extra response a
		// request can have is the exited one that a dispose caused, and it must
		// name that dispose request (errors.md, "Process exit").
		action := map[string]oarlock.Action{}
		for _, r := range records {
			if r.Kind == oarlock.KindRequest {
				action[r.ID] = r.Request.Action
			}
		}
		answers := map[string]int{}
		for _, r := range records {
			if r.Kind != oarlock.KindResponse || r.RequestID == "" {
				continue
			}
			if r.Response.Kind == oarlock.ResponseExited {
				if action[r.RequestID] != oarlock.ActionDispose {
					t.Fatalf("round %d: an exited response answers %q (%s), want only a dispose", round, r.RequestID, action[r.RequestID])
				}
				continue
			}
			answers[r.RequestID]++
		}
		for id, n := range answers {
			if n != 1 {
				t.Fatalf("round %d: request %s has %d control answers, want 1", round, id, n)
			}
		}
		for id := range action {
			if answers[id] == 0 {
				t.Fatalf("round %d: request %s has no control answer", round, id)
			}
		}
		if st := fold.StatusOf(records).Value; st.Kind != fold.StatusExited {
			t.Fatalf("round %d: status = %q after dispose, want exited", round, st.Kind)
		}
	}
}

// describe is a compact stream dump for failure messages.
func describe(records []oarlock.Record) string {
	var b []byte
	for _, r := range records {
		line := string(r.Kind)
		switch r.Kind {
		case oarlock.KindRequest:
			line += " " + string(r.Request.Action)
		case oarlock.KindResponse:
			line += " " + string(r.Response.Kind) + " -> " + r.RequestID
		case oarlock.KindFrame:
			for _, e := range r.Events {
				line += " " + string(e.Kind)
			}
		}
		b = append(b, []byte(line+"\n")...)
	}
	return string(b)
}
