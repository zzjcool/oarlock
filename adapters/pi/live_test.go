package pi

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/observe"
)

func TestLivePromptAndWait(t *testing.T) {
	if os.Getenv("OAR_PI_LIVE") != "1" {
		t.Skip("set OAR_PI_LIVE=1 to run PromptAndWait against real pi")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sess, err := Open(ctx, oarlock.Installation{}, oarlock.Options{Cwd: t.TempDir()})
	if err != nil {
		t.Skipf("pi could not be opened here: %v", err)
	}
	defer func() { _ = sess.Dispose(context.Background()) }()
	run, err := oarlock.PromptAndWait(ctx, sess, "Reply with the single word: ok", oarlock.WithTimeout(2*time.Minute))
	if err != nil {
		t.Fatalf("PromptAndWait: %v", err)
	}
	if run.Outcome != oarlock.RunCompleted || run.Text == "" || run.Turn == nil || run.Turn.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("run = %+v", run)
	}
	if st := sess.(*Session).Status(); st.Value.Kind != "idle" {
		t.Fatalf("status after the run = %+v, want idle", st)
	}
}

// TestLiveSteer is the opt-in, real-Pi proof of issue #45: a steer sent into a
// running turn is accepted by Pi, lands in that same turn (one turn_ended for
// the run, the steered text as a user message between the run's turn_active and
// its turn_ended, the run's final answer shaped by it), and a steer with no turn
// running is rejected no_active_turn without starting anything.
//
// The assertions are the contract's; the evidence for their shape is the raw
// frame pair on this host (pi 1.4.0, --mode rpc): Pi acknowledges a steer with
// {"command":"steer","success":true,"data":{"disposition":"queued"}}, drains
// "steering" at the next turn boundary of the same run and ends the run once.
func TestLiveSteer(t *testing.T) {
	if os.Getenv("OAR_PI_LIVE") != "1" {
		t.Skip("set OAR_PI_LIVE=1 to run Steer against real pi")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	opened, err := Open(ctx, oarlock.Installation{}, oarlock.Options{Cwd: t.TempDir()})
	if err != nil {
		t.Skipf("pi could not be opened here: %v", err)
	}
	sess := opened.(*Session)
	defer func() { _ = sess.Dispose(context.Background()) }()

	records := func() []oarlock.Record { return sess.Records(oarlock.Cursor{}) }
	deadline := time.Now().Add(3 * time.Minute)

	// A turn that stays busy long enough to steer into: the model runs bash
	// `sleep 12` before it answers.
	prompt, err := sess.Prompt(ctx, "Run this exact bash command and then reply with the single word: done. Command: sleep 12", oarlock.PromptOptions{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if prompt.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, want accepted", prompt)
	}
	liveWait(t, deadline, "the run to start", func() bool {
		return sess.Status().Value.Kind == observe.StatusRunning && countEvents(records(), oarlock.EventTurnActive) == 1
	})

	const steerText = "IMPORTANT: your final answer must include the exact word BANANA"
	steer, err := sess.Steer(ctx, steerText, oarlock.InputOptions{})
	if err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if steer.Kind != oarlock.ControlAccepted {
		t.Fatalf("steer into the running turn = %+v, want accepted", steer)
	}
	if steer.Request.Request.Action != oarlock.ActionSteer || steer.Request.Request.Input != steerText {
		t.Fatalf("recorded steer request = %+v, want the input on a steer request", steer.Request.Request)
	}

	liveWait(t, deadline, "the run to settle", func() bool {
		return countEvents(records(), oarlock.EventTurnEnded) == 1
	})
	liveWait(t, deadline, "the session to go idle", func() bool {
		return sess.Status().Value.Kind == observe.StatusIdle
	})

	run := records()
	var active, ended, steered *oarlock.Record
	var text strings.Builder
	for i := range run {
		r := &run[i]
		for _, ev := range r.Events {
			switch {
			case ev.Kind == oarlock.EventTurnActive && active == nil:
				active = r
			case ev.Kind == oarlock.EventTurnEnded && ended == nil:
				ended = r
			case ev.Kind == oarlock.EventUserMessage && strings.Contains(ev.Text, "BANANA") && steered == nil:
				steered = r
			}
			// The run's own assistant text, from its start to its end.
			if ev.Kind == oarlock.EventTextDelta && active != nil && ended == nil {
				text.WriteString(ev.Text)
			}
		}
	}
	if active == nil || ended == nil {
		t.Fatalf("the run has no turn_active/turn_ended pair: %v", kinds(run))
	}
	if steered == nil {
		t.Fatalf("no user_message record carries the steered text %q", steerText)
	}
	// (ii) inside the run, after its turn_active, before its turn_ended.
	if !(active.Seq < steered.Seq && steered.Seq < ended.Seq) {
		t.Fatalf("the steered user message is at seq %d, want after the run's turn_active (%d) and before its turn_ended (%d)",
			steered.Seq, active.Seq, ended.Seq)
	}
	// (iii) the run's final assistant text was shaped by it.
	if !strings.Contains(text.String(), "BANANA") {
		t.Fatalf("the run's assistant text = %q, want it to contain BANANA", text.String())
	}
	// (i) exactly one settlement for the run: the steer invents none.
	if n := countEvents(run, oarlock.EventTurnEnded); n != 1 {
		t.Fatalf("turn_ended events = %d, want exactly 1 for the run", n)
	}

	// A steer with nothing running is the trap the adapter refuses locally: Pi
	// would answer success and hold it for the next turn.
	late, err := sess.Steer(ctx, "this must never land", oarlock.InputOptions{})
	if err != nil {
		t.Fatalf("Steer while idle: %v", err)
	}
	if late.Kind != oarlock.ControlRejected || late.Code != oarlock.NoActiveTurn {
		t.Fatalf("steer while idle = %+v, want rejected/no_active_turn", late)
	}
	if late.Response.Response.Kind != oarlock.ResponseRejected || late.Response.Response.Reason == "" {
		t.Fatalf("recorded idle-steer response = %+v, want the rejection in the stream", late.Response.Response)
	}

	// Nothing starts and nothing lands: the session stays idle for a while.
	for until := time.Now().Add(5 * time.Second); time.Now().Before(until); {
		if st := sess.Status(); st.Value.Kind != observe.StatusIdle {
			t.Fatalf("status = %q after an idle steer, want idle: no turn may start", st.Value.Kind)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, r := range sess.Records(oarlock.Cursor{}) {
		for _, ev := range r.Events {
			if ev.Kind == oarlock.EventUserMessage && strings.Contains(ev.Text, "this must never land") {
				t.Fatalf("the refused idle steer reached the conversation at seq %d", r.Seq)
			}
		}
	}
	if n := countEvents(sess.Records(oarlock.Cursor{}), oarlock.EventTurnEnded); n != 1 {
		t.Fatalf("turn_ended events = %d, want exactly the run's 1", n)
	}
}

// liveWait polls cond until it holds or the deadline passes. A live run is
// model-paced, so the wait is time-bounded and never a fixed sleep.
func liveWait(t *testing.T, deadline time.Time, what string, cond func() bool) {
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
