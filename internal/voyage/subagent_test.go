package voyage_test

// The offline half of issue #83: replay the committed Codex sub-agent recording
// and pin what the stream states. Everything here runs by default — no codex, no
// network, no credential — so the sub-agent facts cannot rot silently.
//
// The recording is one real codex 0.165.0 turn against a third-party OpenAI
// Responses endpoint (see live_subagent_test.go for how it was made): the root
// thread called its own multi-agent tool (spawnAgent), the child thread ran its
// own turn, and the root waited for it. The pinned numbers are folded from the
// committed bytes, not from a live run on another machine.

import (
	"testing"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
	"github.com/zzjcool/oarlock/internal/voyage"
)

const (
	// cmSubagentFixture is the committed recording this file replays.
	cmSubagentFixture = "../../testdata/voyage/codex-0.165.0-subagent-turn.jsonl"
	// cmSubagentRoot and cmSubagentChild are the normalised session ids the
	// fixture carries.
	cmSubagentRoot  = "codex-live-codex-0.165.0-subagent-turn"
	cmSubagentChild = "codex-live-codex-0.165.0-subagent-turn-child"
)

// cmI64 is a pointer to a pinned optional part.
func cmI64(v int64) *int64 { return &v }

// TestCommittedCodexSubagentTurn pins issue #83's four criteria on the
// committed bytes:
//
//   - the session_linked edge exists exactly once, parent → child, and the child
//     session really has records (19 of the 69);
//   - every child record carries the child's SessionID and no AgentPath: a child
//     is a session of its own, never a path inside the root, and never the root
//     id;
//   - Usage.WithChildren is the root's Total plus the child session's total
//     exactly once (root 37822/212 + child 6977/3 = 44799/215), while Total,
//     ByAgent and Unattributed stay the root's own;
//   - the root turn ends once, on its own turn/completed (seq 66); the child's
//     turn/completed (seq 40) ends only the child.
func TestCommittedCodexSubagentTurn(t *testing.T) {
	recs, err := voyage.Replay(cmSubagentFixture)
	if err != nil {
		t.Fatalf("replay %s: %v", cmSubagentFixture, err)
	}
	if len(recs) != 69 {
		t.Fatalf("records = %d, want 69", len(recs))
	}

	// The header names the root session and the runtime that recorded it.
	log, err := voyage.Read(cmSubagentFixture)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if log.Header.Runtime != "codex" || log.Header.SessionID != cmSubagentRoot {
		t.Fatalf("header = %+v, want codex / %s", log.Header, cmSubagentRoot)
	}
	if log.End == nil || log.End.Reason != "disposed" {
		t.Fatalf("end = %+v, want disposed", log.End)
	}

	per := map[string]int{}
	for _, r := range recs {
		per[r.SessionID]++
	}
	if per[cmSubagentRoot] != 50 || per[cmSubagentChild] != 19 || len(per) != 2 {
		t.Fatalf("records per session = %v, want root 50 and child 19 only", per)
	}
	for _, r := range recs {
		if r.SessionID != cmSubagentRoot && r.SessionID != cmSubagentChild {
			t.Fatalf("record %d carries session %q: it is neither the root nor the linked child", r.Seq, r.SessionID)
		}
		if r.SessionID == cmSubagentChild && len(r.AgentPath) != 0 {
			t.Fatalf("child record %d carries AgentPath %v: the child is a session root, not an agent path", r.Seq, r.AgentPath)
		}
	}

	// The edge: one session_linked, stated once, at the spawn item.
	var links []uint64
	for _, r := range recs {
		for _, ev := range r.Events {
			if ev.Kind != oarlock.EventSessionLink {
				continue
			}
			links = append(links, r.Seq)
			if ev.Parent != cmSubagentRoot || ev.Child != cmSubagentChild {
				t.Fatalf("session_linked at seq %d = %q → %q, want %q → %q",
					r.Seq, ev.Parent, ev.Child, cmSubagentRoot, cmSubagentChild)
			}
		}
	}
	if len(links) != 1 || links[0] != 19 {
		t.Fatalf("session_linked seqs = %v, want exactly [19] (the spawn item; later wait/close items restate the edge, the graph holds it once)", links)
	}

	// Turn ends: one per session, the root's last.
	var rootEnds, childEnds []uint64
	for _, r := range recs {
		for _, ev := range r.Events {
			if ev.Kind != oarlock.EventTurnEnded {
				continue
			}
			switch r.SessionID {
			case cmSubagentRoot:
				rootEnds = append(rootEnds, r.Seq)
			case cmSubagentChild:
				childEnds = append(childEnds, r.Seq)
			}
			if ev.Outcome == nil || ev.Outcome.Kind != oarlock.OutcomeCompleted {
				t.Fatalf("turn_ended at seq %d = %+v, want completed", r.Seq, ev.Outcome)
			}
		}
	}
	if len(rootEnds) != 1 || rootEnds[0] != 66 {
		t.Fatalf("root turn_ended seqs = %v, want exactly [66]", rootEnds)
	}
	if len(childEnds) != 1 || childEnds[0] != 40 {
		t.Fatalf("child turn_ended seqs = %v, want exactly [40] (the child's own turn, not the root's)", childEnds)
	}

	f := fold.Of(recs)
	st := f.Status().Value
	if st.Kind != fold.StatusExited {
		t.Fatalf("status = %s, want exited (the recorder disposed the session)", st.Kind)
	}
	if st.LastTurnOutcome == nil || st.LastTurnOutcome.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("root last turn = %+v, want completed: the child's turn_end must not replace it", st.LastTurnOutcome)
	}

	// Usage, folded once: the root's own figure, the child's own figure, and
	// WithChildren = root + child exactly once.
	u := f.Usage().Value
	wantTotal := oarlock.TokenTotals{Input: 37822, Output: 212, Reasoning: cmI64(0), CacheRead: cmI64(36992), CacheWrite: cmI64(0)}
	if u.Total == nil || lvJSON(t, *u.Total) != lvJSON(t, wantTotal) {
		t.Fatalf("Usage.Total = %s, want %s", lvJSON(t, u.Total), lvJSON(t, wantTotal))
	}
	wantChildren := oarlock.TokenTotals{Input: 44799, Output: 215, Reasoning: cmI64(0), CacheRead: cmI64(43648), CacheWrite: cmI64(0)}
	if u.WithChildren == nil || lvJSON(t, *u.WithChildren) != lvJSON(t, wantChildren) {
		t.Fatalf("Usage.WithChildren = %s, want %s (root 37822/212 + child 6977/3, once)", lvJSON(t, u.WithChildren), lvJSON(t, wantChildren))
	}
	if u.WithChildren.Input != u.Total.Input+6977 || u.WithChildren.Output != u.Total.Output+3 {
		t.Fatalf("WithChildren = %+v, want Total plus the child session's total once", *u.WithChildren)
	}
	if u.ByAgent != nil || u.Unattributed != nil {
		t.Fatalf("ByAgent/Unattributed = %v/%+v, want nil: the child is a session, so the root's own agents lose nothing", u.ByAgent, u.Unattributed)
	}

	// The graph is the same edge, read through the kernel's own view.
	if g := fold.Of(recs).Usage().Value; g.WithChildren == nil {
		t.Fatal("a replay without the edge would report no WithChildren")
	}
}
