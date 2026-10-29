package voyage_test

// Issue #71's committed evidence: the tool turn in
// testdata/voyage/pi-1.4.0-tool-turn.jsonl was recorded from a real Pi 1.4.0
// session (bash `echo oarlock-tool-ok`) by TestLiveToolTurnRecordsAndReplays,
// with the live fold compared to the replayed fold before the file was written.
// These tests are the offline half: they replay the committed bytes, pin the
// numbers the live session reported, and assert the tool-call evidence and its
// order — with no runtime, no network and no process.

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
	"github.com/zzjcool/oarlock/internal/voyage"
)

const (
	turnFixture = "../../testdata/voyage/" + ltToolFixture + ".jsonl"
	// turnCallID and turnInput are the real ones the recorded session reported;
	// the model's tool call is part of the evidence, not a detail to normalise.
	turnCallID = "call_00_gcQEO43o77CvBExbFwqg2566|fc_18e3004be7c93fe600008e24"
	turnInput  = `{"command":"echo oarlock-tool-ok"}`
	turnOutput = "oarlock-tool-ok\n"
	turnAnswer = "oarlock-tool-ok"
)

func turnI64(v int64) *int64 { return &v }

func turnReplay(t *testing.T) []oarlock.Record {
	t.Helper()
	// A PATH holding only a tripwire `pi` proves the replay starts nothing; the
	// package's TestNoNetworkOrProcessImports proves it cannot dial either.
	return ltReplayNoProcess(t, turnFixture)
}

func TestCommittedToolTurnPin(t *testing.T) {
	recs := turnReplay(t)
	log, err := voyage.Read(turnFixture)
	if err != nil {
		t.Fatal(err)
	}
	if !log.Complete() || log.End.Reason != "disposed" {
		t.Fatalf("log complete=%v end=%+v, want a complete log ended by dispose", log.Complete(), log.End)
	}
	if log.Header.Runtime != "pi" || log.Header.Cwd != "." || log.Header.SessionID != "pi-live-"+ltToolFixture {
		t.Fatalf("header = %+v", log.Header)
	}
	if log.Header.StartedAt > recs[0].ReceivedAt.UnixMilli() || log.End.At < recs[len(recs)-1].ReceivedAt.UnixMilli() {
		t.Fatalf("clock ordering: startedAt %d, first %d, last %d, end %d",
			log.Header.StartedAt, recs[0].ReceivedAt.UnixMilli(), recs[len(recs)-1].ReceivedAt.UnixMilli(), log.End.At)
	}

	f := fold.Of(recs)
	st := f.Status().Value
	if st.Kind != fold.StatusExited {
		t.Fatalf("status = %s, want exited (the recording ends with the runtime exit)", st.Kind)
	}
	if st.LastTurnOutcome == nil || st.LastTurnOutcome.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("last turn outcome = %+v, want completed", st.LastTurnOutcome)
	}

	// The live session's own numbers: one assistant turn reporting usage
	// (message_end) and the post-settlement stats frame reporting context.
	wantTokens := &oarlock.TokenTotals{
		Input: 21583, Output: 75, Reasoning: turnI64(23), CacheRead: turnI64(13312), CacheWrite: turnI64(0),
	}
	if got := f.Usage().Value.Total; !reflect.DeepEqual(got, wantTokens) {
		t.Fatalf("usage total = %s, want input 21583 output 75 reasoning 23 cacheRead 13312 cacheWrite 0",
			goldenJSON(t, f.Usage().Value.Total))
	}
	c := f.ContextUsage().Value
	if c == nil || c.Usage.Tokens == nil || c.Usage.ContextWindow == nil || c.Usage.Percent == nil {
		t.Fatalf("context usage missing: %s", goldenJSON(t, c))
	}
	if *c.Usage.Tokens != 10843 || *c.Usage.ContextWindow != 1000000 || math.Abs(*c.Usage.Percent-1.0843) > 1e-9 {
		t.Fatalf("context = %s, want tokens 10843 of 1000000 = 1.0843%%", goldenJSON(t, c))
	}

	// Written once, read back: the incremental fold and the batch fold agree on
	// the committed bytes, exactly as they do for the pifixture traces.
	goldenEqual(t, "fold.Of vs Feed", goldenFoldAll(recs), goldenFeed(recs))

	// A tool turn, not a compaction: no compaction event may creep in.
	for _, ev := range ltEvents(recs) {
		if ev.Kind == oarlock.EventCompactionStarted || ev.Kind == oarlock.EventCompactionEnded {
			t.Fatalf("compaction event %s at seq %d in the tool-turn fixture", ev.Kind, ev.Seq)
		}
	}
}

func TestCommittedToolTurnCallAndTextOrder(t *testing.T) {
	recs := turnReplay(t)
	// The shared shape assertions: one bash call, started before ended, exit 0,
	// output text, the answer after the call, and the same order in the
	// conversation.
	ltToolTurn(t, recs)

	conv := fold.Of(recs).Conversation().Value
	var tool *fold.Message
	var answer, reasoning strings.Builder
	for i := range conv {
		m := &conv[i]
		switch m.Role {
		case fold.RoleTool:
			tool = m
		case fold.RoleAssistant:
			answer.WriteString(m.Text)
		case fold.RoleReasoning:
			reasoning.WriteString(m.Text)
		}
	}
	if tool == nil || tool.CallID != turnCallID || tool.Input != turnInput || tool.Output != turnOutput {
		t.Fatalf("tool message = %s, want call %q input %q output %q", goldenJSON(t, tool), turnCallID, turnInput, turnOutput)
	}
	if got := answer.String(); got != turnAnswer {
		t.Fatalf("assistant text = %q, want %q", got, turnAnswer)
	}
	// The deltas arrived in order: the reasoning that planned the call comes
	// before the call, the answer's text after it.
	if !strings.HasPrefix(reasoning.String(), "The user wants me to run") {
		t.Fatalf("reasoning text = %q", reasoning.String())
	}
	user := conv[0]
	if user.Role != fold.RoleUser || !strings.HasPrefix(user.Text, "Run this exact bash command") {
		t.Fatalf("first message = %s", goldenJSON(t, user))
	}
}
