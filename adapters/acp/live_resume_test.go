package acp

// live_resume_test.go — the one claim only a real runtime with a real provider
// can prove: a resumed session is a new process and a new stream (Seq 1, no
// replayed turn), the runtime keeps the session id, and the model's next turn
// still holds what the previous turn said.
//
// Opt-in: OAR_ACP_LIVE=1 plus the OpenAI-compatible endpoint of live_test.go.
// opencode 1.19.5 advertises sessionCapabilities.resume and this open takes
// it, so what runs here is session/resume; session/load itself is NOT
// verified live (that branch stays source-derived — see open.go).

import (
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

func TestLiveOpenCodeResume(t *testing.T) {
	a := liveAgentOrSkip(t)
	ctx := liveCtx(t)
	dir := t.TempDir() // the same cwd for both opens

	// First open: one turn whose answer carries the phrase the resumed turn
	// has to recall.
	first := openLive(t, a, oarlock.Options{Cwd: dir})
	if out, err := first.Prompt(ctx, "Reply with exactly: resumable-42", oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	waitLiveStatus(t, first, fold.StatusIdle, 2*time.Minute)
	if got := text(records(first)); !strings.Contains(got, "resumable-42") {
		t.Fatalf("the first turn did not answer the marker phrase: %q", got)
	}
	id := first.ID()
	if id == "" {
		t.Fatal("the first session has no id to resume")
	}
	assertDense(t, records(first))
	if err := first.Dispose(ctx); err != nil {
		t.Fatalf("dispose the first session: %v", err)
	}

	// Second open, new process, same id and cwd. open.go's order picks
	// session/resume here (opencode advertises sessionCapabilities.resume), but
	// the method a client *sends* is not part of the record stream — requests
	// to the agent are not records, only its answers are — so this test proves
	// the resumed session, not which of resume/load carried it.
	second := openLive(t, a, oarlock.Options{Cwd: dir, Resume: id})
	if got := second.ID(); got != id {
		t.Fatalf("resumed session id = %q, want %q", got, id)
	}
	recs := records(second)
	assertDense(t, recs)
	if len(recs) == 0 || recs[0].Seq != 1 {
		t.Fatalf("the resumed stream does not start at Seq 1: %s", dump(recs))
	}
	// No history came back as new facts: no turn was active or ended by the
	// replay, and no prior message text was replayed as a delta.
	if ends := liveTurnEnds(recs); len(ends) != 0 {
		t.Fatalf("the resumed open replayed %d turn ends:\n%s", len(ends), dump(recs))
	}
	if starts := events(recs, oarlock.EventTurnActive); len(starts) != 0 {
		t.Fatalf("the resumed open replayed %d turn starts:\n%s", len(starts), dump(recs))
	}
	if strings.Contains(dump(recs), "agent_message_chunk") || strings.Contains(dump(recs), "user_message_chunk") {
		t.Fatalf("the resumed open replayed message history:\n%s", dump(recs))
	}
	if st := fold.StatusOf(recs).Value.Kind; st != fold.StatusIdle {
		t.Fatalf("the resumed session starts at %v, want idle", st)
	}

	// The context really was restored: the next turn can still name the phrase
	// it answered with before the dispose.
	if out, err := second.Prompt(ctx, "Repeat the exact phrase you replied with before.", oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt on the resumed session = %+v, %v", out, err)
	}
	waitLiveStatus(t, second, fold.StatusIdle, 2*time.Minute)
	recs = records(second)
	assertDense(t, recs)
	got := text(recs)
	if !strings.Contains(got, "resumable-42") {
		t.Fatalf("the resumed turn did not recall the prior phrase: %q\n%s", got, dump(recs))
	}
	if ends := liveTurnEnds(recs); len(ends) != 1 || ends[0].Outcome == nil || ends[0].Outcome.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("turn ends = %+v, want exactly one completed", ends)
	}
	t.Logf("resumed %s: %d records, first seq %d, recalled the prior phrase", id, len(recs), recs[0].Seq)
}
