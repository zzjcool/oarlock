package acp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

// The end-to-end path on grok: open (initialize + authenticate + session/new
// + yolo meta), prompt, frames in wire order, the agent's own turn end.
func TestGrokEndToEnd(t *testing.T) {
	s := open(t, Grok, "grok", oarlock.Options{})
	if got := s.Capabilities(); got.Attribution != oarlock.AttributionNested || !got.Images {
		t.Fatalf("capabilities = %+v", got) // images: true despite initialize saying false (grok quirk)
	}
	out := prompt(t, s, "hello")
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v", out)
	}
	waitIdle(t, s)
	recs := records(s)
	assertDense(t, recs)

	// The handshake answers are frames, in wire order, before the prompt.
	if recs[0].Kind != oarlock.KindFrame || recs[3].Kind != oarlock.KindRequest || recs[3].Request.Action != oarlock.ActionPrompt {
		t.Fatalf("want 3 handshake frames then the prompt request:\n%s", dump(recs))
	}
	if recs[4].Kind != oarlock.KindResponse || recs[4].Response.Kind != oarlock.ResponseAccepted {
		t.Fatalf("prompt must be accepted right after the request:\n%s", dump(recs))
	}
	if got := text(recs); got != "echo:hello" {
		t.Fatalf("text = %q", got)
	}
	ended := events(recs, oarlock.EventTurnEnded)
	if len(ended) != 1 || ended[0].Outcome.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("turn_ended = %+v", ended)
	}
	// Every frame keeps its native bytes.
	for _, r := range recs {
		if r.Kind == oarlock.KindFrame && !json.Valid(r.Native) {
			t.Fatalf("frame without native: %+v", r)
		}
	}
	if n := s.(interface{ Unknown() map[string]int }).Unknown(); len(n) != 0 {
		t.Fatalf("unknown updates: %v", n)
	}
	if _, ok := s.(oarlock.Steerer); !ok {
		t.Fatal("grok must be a Steerer")
	}
}

func TestGrokUsageIsOneAuthoritativeView(t *testing.T) {
	s := open(t, Grok, "grok", oarlock.Options{})
	prompt(t, s, "usage")
	waitIdle(t, s)
	prompt(t, s, "usage")
	waitFor(t, "second turn", func() bool { return len(events(records(s), oarlock.EventTurnEnded)) == 2 })
	u := fold.UsageOf(records(s)).Value
	// Each prompt bills 100/7 on its own ledger; the session is the sum of the
	// root's prompt ledgers, never plus any second view.
	if u.Total == nil || u.Total.Input != 200 || u.Total.Output != 14 {
		t.Fatalf("usage total = %+v", u.Total)
	}
	if u.Total.CacheRead == nil || *u.Total.CacheRead != 128 {
		t.Fatalf("cache read = %v", u.Total.CacheRead)
	}
	if c := fold.ContextUsageOf(records(s)).Value; c == nil {
		t.Fatal("no context report")
	}
}

// A foreground child's spend is inside the root's prompt ledger: the child's
// own turn_completed must not be billed again, and the edge is stated.
func TestGrokChildNestedAndNotDoubleBilled(t *testing.T) {
	s := open(t, Grok, "grok", oarlock.Options{})
	prompt(t, s, "child-grok")
	waitIdle(t, s)
	recs := records(s)
	var childFrames int
	for _, r := range recs {
		if r.SessionID == "fake-child-grok" {
			childFrames++
		}
	}
	if childFrames == 0 {
		t.Fatalf("child frames must carry the child's session id:\n%s", dump(recs))
	}
	links := events(recs, oarlock.EventSessionLink)
	if len(links) != 1 || links[0].Parent != "fake-session" || links[0].Child != "fake-child-grok" {
		t.Fatalf("links = %+v", links)
	}
	if u := fold.UsageOf(recs).Value; u.Total == nil || u.Total.Input != 200 || u.Total.Output != 20 {
		t.Fatalf("usage = %+v (child ledger must not be added)", u.Total)
	}
	// A child's records never end the root's turn.
	if st := fold.StatusOf(recs).Value; st.Kind != fold.StatusIdle {
		t.Fatalf("status = %+v", st)
	}
}

func TestGrokIndependentWakeLedgerBilledOnce(t *testing.T) {
	s := open(t, Grok, "grok", oarlock.Options{})
	prompt(t, s, "child-wake")
	waitIdle(t, s)
	if u := fold.UsageOf(records(s)).Value; u.Total == nil || u.Total.Input != 15 || u.Total.Output != 3 {
		t.Fatalf("usage = %+v, want 15/3 (prompt 10/2 + one wake 5/1)", u.Total)
	}
}

func TestGrokSteerSendNowFoldsIntoOneTurn(t *testing.T) {
	s := open(t, Grok, "grok", oarlock.Options{})
	if out := prompt(t, s, "steerable"); out.Kind != oarlock.ControlAccepted {
		t.Fatal(out)
	}
	waitFor(t, "running", func() bool { return strings.Contains(text(records(s)), "working") })
	st := s.(oarlock.Steerer)
	out, err := st.Steer(context.Background(), "go left", oarlock.InputOptions{})
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("steer = %+v %v", out, err)
	}
	waitIdle(t, s)
	recs := records(s)
	if ended := events(recs, oarlock.EventTurnEnded); len(ended) != 1 || ended[0].Outcome.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("a send-now steer is one turn that completes, got %+v\n%s", ended, dump(recs))
	}
	// The steering prompt carried the vendor's own channel on the wire.
	var sawMeta bool
	for _, r := range recs {
		if r.Kind == oarlock.KindFrame {
			continue
		}
	}
	for _, r := range recs {
		if r.Request != nil && r.Request.Action == oarlock.ActionSteer {
			sawMeta = true
		}
	}
	if !sawMeta {
		t.Fatalf("no steer request recorded:\n%s", dump(recs))
	}
	// A send-now steer supersedes the running prompt: its cancelled answer is
	// still that prompt's spend, and the steer answer is its own. Both bill, so
	// the running total carries the cancelled call (16776/222) plus the steer
	// answer (34420/279) — a final-only rule would silently drop the former.
	var billed []*oarlock.TokenTotals
	for _, ev := range events(recs, oarlock.EventUsage) {
		if ev.Usage != nil && ev.Usage.Tokens != nil {
			billed = append(billed, ev.Usage.Tokens)
		}
	}
	if len(billed) != 2 {
		t.Fatalf("usage events carrying tokens = %d, want 2 (cancelled turn + steer)\n%s", len(billed), dump(recs))
	}
	if last := billed[1]; last.Input != 51196 || last.Output != 501 {
		t.Fatalf("cumulative tokens = %+v, want 51196/501", last)
	}
	// Steer with nothing running: rejected no_active_turn, input stays the caller's.
	out, _ = st.Steer(context.Background(), "late", oarlock.InputOptions{})
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.NoActiveTurn {
		t.Fatalf("late steer = %+v", out)
	}
}

func TestPromptWhileRunningIsBusy(t *testing.T) {
	s := open(t, Grok, "grok", oarlock.Options{})
	prompt(t, s, "slow")
	waitFor(t, "running", func() bool { return strings.Contains(text(records(s)), "working") })
	if out := prompt(t, s, "second"); out.Kind != oarlock.ControlRejected || out.Code != oarlock.Busy {
		t.Fatalf("second prompt = %+v", out)
	}
	if out, _ := s.Abort(context.Background()); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("abort = %+v", out)
	}
	waitIdle(t, s)
}

func TestAuthFailureEndsTurnFailed(t *testing.T) {
	s := open(t, Grok, "grok", oarlock.Options{})
	prompt(t, s, "fail-auth")
	waitIdle(t, s)
	ended := events(records(s), oarlock.EventTurnEnded)
	if len(ended) != 1 || ended[0].Outcome.Kind != oarlock.OutcomeFailed || ended[0].Outcome.Failure != oarlock.FailureAuth {
		t.Fatalf("turn_ended = %+v", ended)
	}
}
