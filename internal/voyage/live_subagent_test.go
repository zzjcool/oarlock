package voyage_test

// The issue #83 live acceptance: a real Codex session that spawns a child
// thread (the runtime's own multi-agent tool), recorded into a voyage log and
// replayed with no process and no network. The replayed stream must keep the
// facts the issue asks about:
//
//   - a session_linked edge parent → child that matches the two threads'
//     records, once in the graph however often the runtime names it;
//   - the child's records carrying the child thread as their own SessionID
//     (never the root's), so a child never impersonates the root;
//   - Usage.WithChildren adding the child session's total exactly once, while
//     the root's Total and ByAgent stay the root's own;
//   - the root turn ending on its own turn/completed only — the child's
//     turn/completed ends the child, and the folded root status is idle after
//     both.
//
// It needs a real `codex` inside the version window the open gate enforces, a
// CODEX_HOME whose config.toml names the model and provider, and the token in
// the variable that file names. Everywhere else it skips as NOT VERIFIED.
//
//   sh: OARLOCK_LIVE_CODEX=1 CODEX_HOME=… OPENAI_API_KEY=… \
//         go test ./internal/voyage -run TestLiveSubagentRecordsAndReplays -v
//
// The recording stays in a temp dir unless OAR_CODEX_RECORD=1, which is how the
// committed fixture was made; recording is never a build step.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/adapters/codex"
	"github.com/zzjcool/oarlock/internal/fold"
	"github.com/zzjcool/oarlock/internal/voyage"
)

const (
	// lvFixture is the committed fixture this file records and replays.
	lvFixture = "codex-0.165.0-subagent-turn"
	// lvRecordGate is the recording switch: without it the live log stays in a
	// temp dir and the committed fixture is untouched.
	lvRecordGate = "OAR_CODEX_RECORD"
	// lvFixtureDir is where the committed fixtures live.
	lvFixtureDir = "../../testdata/voyage"
	// lvRecorder names the test in the fixture header.
	lvRecorder = "oarlock/voyage-live (codex 0.165.0, issue #83)"
	// lvPrompt asks for the spawn explicitly: the model must delegate, ask the
	// child for one word, and answer with one word of its own.
	lvPrompt = "You must delegate with your sub-agent tool (spawn_agent): spawn one new agent and ask it to reply with the single word banana. Wait for it, then reply with the single word: done"
	// lvChildAnswer is what the child was asked for; it is logged, never
	// asserted as the model's answer (the child's own turn is the evidence).
	lvChildAnswer = "banana"
	// lvFixedRoot and lvFixedChild are the deterministic session ids a committed
	// recording carries.
	lvFixedRoot  = "codex-live-" + lvFixture
	lvFixedChild = "codex-live-" + lvFixture + "-child"
)

// lvAnswers is the fold state the sub-agent criteria are read from.
type lvAnswers struct {
	Status fold.QueryResult[fold.Status]
	Usage  fold.QueryResult[fold.Usage]
	Conv   fold.QueryResult[[]fold.Message]
}

func lvFold(recs []oarlock.Record) lvAnswers {
	f := fold.Of(recs)
	return lvAnswers{Status: f.Status(), Usage: f.Usage(), Conv: f.Conversation()}
}

func lvJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// lvEvent is one event with the record that stated it.
type lvEvent struct {
	Seq  uint64
	Rec  oarlock.Record
	Kind oarlock.EventKind
	oarlock.Event
}

func lvEvents(recs []oarlock.Record) []lvEvent {
	var out []lvEvent
	for _, r := range recs {
		for _, ev := range r.Events {
			out = append(out, lvEvent{Seq: r.Seq, Rec: r, Kind: ev.Kind, Event: ev})
		}
	}
	return out
}

// lvSubagentFacts is the evidence issue #83 asks for, read off a record stream
// (live or replayed: the same function, so the two cannot drift).
type lvSubagentFacts struct {
	Root      string        // the root session id
	Child     string        // the derived child session id
	Edge      oarlock.Event // the session_linked event
	Links     int           // how many session_linked events the stream carries
	EdgeSeq   uint64        // where the edge is stated
	TaskStart oarlock.TaskEvent
	TaskEnd   oarlock.TaskEvent
	RootEnds  int // turn_ended records of the root session
	ChildEnds int // turn_ended records of the child session
	ChildSeqs []uint64
	Answers   lvAnswers
}

// lvReadSubagent reads the recorded sub-agent facts and fails the test when the
// stream does not carry the shape. Every check states what it measured.
func lvReadSubagent(t *testing.T, recs []oarlock.Record) lvSubagentFacts {
	t.Helper()
	if len(recs) == 0 {
		t.Fatal("no record")
	}
	facts := lvSubagentFacts{Root: recs[0].SessionID, Answers: lvFold(recs)}
	if facts.Root == "" {
		t.Fatal("record 1 has no SessionID: the root session is unnamed")
	}
	for _, ev := range lvEvents(recs) {
		switch ev.Kind {
		case oarlock.EventSessionLink:
			facts.Links++
			facts.Edge = ev.Event
			facts.EdgeSeq = ev.Seq
		case oarlock.EventTaskStarted:
			if ev.Task != nil && ev.Task.TaskType == "agent" {
				facts.TaskStart = *ev.Task
			}
		case oarlock.EventTaskEnded:
			if ev.Task != nil && ev.Task.TaskType == "agent" {
				facts.TaskEnd = *ev.Task
			}
		case oarlock.EventTurnEnded:
			switch {
			case ev.Rec.SessionID == facts.Root && len(ev.Rec.AgentPath) == 0:
				facts.RootEnds++
			default:
				facts.ChildEnds++
				facts.ChildSeqs = append(facts.ChildSeqs, ev.Seq)
			}
		}
	}
	if facts.Links != 1 {
		t.Fatalf("session_linked events = %d, want exactly 1 (the runtime may repeat an edge; the graph holds it once): %s",
			facts.Links, lvLinkList(recs))
	}
	if facts.Edge.Parent != facts.Root {
		t.Fatalf("session_linked parent = %q, want the root session %q", facts.Edge.Parent, facts.Root)
	}
	facts.Child = facts.Edge.Child
	if facts.Child == "" || facts.Child == facts.Root {
		t.Fatalf("session_linked child = %q, want a derived child session", facts.Child)
	}

	// The child's records are the child's: their SessionID is the child thread,
	// never the root's, and a nested child is its own session root (no AgentPath).
	childRecords := 0
	for _, r := range recs {
		if r.SessionID != facts.Child {
			continue
		}
		childRecords++
		if len(r.AgentPath) != 0 {
			t.Fatalf("child record %d carries AgentPath %v: a nested child is a session root, not a path inside the root", r.Seq, r.AgentPath)
		}
	}
	if childRecords == 0 {
		t.Fatalf("no record carries the child session id %q: the edge names a session the log does not contain", facts.Child)
	}
	for _, r := range recs {
		if r.SessionID == facts.Root && r.Kind == oarlock.KindFrame && len(r.AgentPath) != 0 {
			t.Fatalf("root session record %d carries AgentPath %v: codex attributes by session, not by path", r.Seq, r.AgentPath)
		}
	}

	// The delegation itself. codex states a spawn either as a subAgentActivity
	// item (which the projection also reads as task_started/task_ended) or as a
	// collab tool call (which it reads as the session_linked edge alone). Both
	// are the runtime's own statement; when the task events are present they must
	// agree with the edge, and when they are not the edge is the delegation.
	if facts.TaskStart.TaskID != "" {
		if facts.TaskStart.ChildSessionID != facts.Child {
			t.Fatalf("task_started ChildSessionID = %q, want the linked child %q", facts.TaskStart.ChildSessionID, facts.Child)
		}
		if facts.TaskEnd.TaskID != facts.TaskStart.TaskID || facts.TaskEnd.Status != oarlock.TaskCompleted {
			t.Fatalf("task_ended = %+v, want %s completed", facts.TaskEnd, facts.TaskStart.TaskID)
		}
	}

	// The root turn ends once, on its own turn/completed. The child's is the
	// child's: one turn each, and the root status is idle after both.
	if facts.RootEnds != 1 {
		t.Fatalf("root turn_ended = %d, want exactly 1", facts.RootEnds)
	}
	if facts.ChildEnds != 1 {
		t.Fatalf("child turn_ended = %d, want exactly 1 (the child ran one turn)", facts.ChildEnds)
	}
	// Settled, never left running by the child's turn_end: idle in a live stream,
	// exited once the recorder disposed the session. A child turn_end that ended
	// the root would show as a second root end; one that did not reach it would
	// leave the root turn active.
	switch kind := facts.Answers.Status.Value.Kind; kind {
	case fold.StatusIdle, fold.StatusExited:
	default:
		t.Fatalf("folded status = %s, want idle or exited after both turns (the child's turn_end must not leave the root unfinished)", kind)
	}
	if o := facts.Answers.Status.Value.LastTurnOutcome; o == nil || o.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("root last turn outcome = %+v, want completed", o)
	}

	// Usage: the root's Total is the root session's own figure; WithChildren adds
	// the child's total exactly once.
	usage := facts.Answers.Usage.Value
	if usage.Total == nil {
		t.Fatalf("root Usage.Total = nil: codex reported no token usage\n%s", lvFrameTypes(recs))
	}
	if usage.WithChildren == nil {
		t.Fatalf("Usage.WithChildren = nil, want the child session's total added once (edge %s → %s)", facts.Root, facts.Child)
	}
	return facts
}

// lvLinkList renders every session_linked event the stream carries.
func lvLinkList(recs []oarlock.Record) string {
	var parts []string
	for _, ev := range lvEvents(recs) {
		if ev.Kind == oarlock.EventSessionLink {
			parts = append(parts, fmt.Sprintf("seq %d %q → %q", ev.Seq, ev.Parent, ev.Child))
		}
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, ", ")
}

// lvFrameTypes tallies the native method of every frame in first-appearance
// order, so a failure names what the runtime did send instead of the shape the
// test expected.
func lvFrameTypes(recs []oarlock.Record) string {
	var order []string
	count := map[string]int{}
	for _, r := range recs {
		if r.Kind != oarlock.KindFrame {
			continue
		}
		var head struct {
			Method string `json:"method"`
		}
		method := "<unreadable>"
		if json.Unmarshal(r.Native, &head) == nil && head.Method != "" {
			method = head.Method
		}
		if count[method] == 0 {
			order = append(order, method)
		}
		count[method]++
	}
	sort.Strings(order)
	parts := make([]string, len(order))
	for i, m := range order {
		parts[i] = fmt.Sprintf("%s x%d", m, count[m])
	}
	return "frames: " + strings.Join(parts, ", ")
}

// lvWait blocks until cond holds or the deadline passes.
func lvWait(t *testing.T, deadline time.Time, what string, cond func() bool) {
	t.Helper()
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// lvNormalise rewrites the nondeterministic parts of a live recording: the wall
// clock becomes ltBase+Seq, the two thread ids fixed ids, the home directory
// "$HOME" and the cwd ".". Everything the fold answers with must stay what it
// was; the caller checks that instead of trusting this comment.
func lvNormalise(t *testing.T, recs []oarlock.Record, pairs [][2]string) []oarlock.Record {
	t.Helper()
	out := make([]oarlock.Record, len(recs))
	for i, rec := range recs {
		body := []byte(lvJSON(t, rec))
		at := ltBase.Add(time.Duration(rec.Seq) * time.Millisecond)
		old, err := json.Marshal(rec.ReceivedAt)
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(at)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(body, old) {
			t.Fatalf("record %d: receivedAt %s is not in its own JSON", rec.Seq, old)
		}
		body = bytes.ReplaceAll(body, old, want)
		for _, pair := range pairs {
			if pair[0] != "" && pair[0] != pair[1] {
				body = bytes.ReplaceAll(body, []byte(pair[0]), []byte(pair[1]))
			}
		}
		var got oarlock.Record
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("record %d after normalisation: %v", rec.Seq, err)
		}
		if got.Seq != rec.Seq || got.Kind != rec.Kind || !got.ReceivedAt.Equal(at) {
			t.Fatalf("record %d lost its identity in normalisation: seq %d kind %s at %s", rec.Seq, got.Seq, got.Kind, got.ReceivedAt)
		}
		out[i] = got
	}
	return out
}

// lvNormaliseLive is lvNormalise with this host's values filled in: the two
// session ids, the home directory and the throwaway cwd.
func lvNormaliseLive(t *testing.T, recs []oarlock.Record, root, child, cwd string) []oarlock.Record {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return lvNormalise(t, recs, [][2]string{
		{root, lvFixedRoot},
		{child, lvFixedChild},
		{cwd, "."},
		{home, "$HOME"},
	})
}

// lvWrite writes recs as a voyage log. With OAR_CODEX_RECORD=1 the log replaces
// the committed fixture, which is the only way testdata/voyage/<name>.jsonl is
// produced or updated.
func lvWrite(t *testing.T, name string, recs []oarlock.Record, endReason string) string {
	t.Helper()
	dir := t.TempDir()
	if os.Getenv(lvRecordGate) == "1" {
		dir = lvFixtureDir
		t.Logf("recording the committed fixture %s/%s.jsonl", lvFixtureDir, name)
	}
	path := filepath.Join(dir, name+".jsonl")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	w, err := voyage.Create(path, voyage.Header{
		Runtime:   "codex",
		Model:     lvModel(recs),
		Cwd:       ".",
		SessionID: recs[0].SessionID,
		StartedAt: ltBase.UnixMilli(),
		Recorder:  lvRecorder,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if err := w.Append(rec); err != nil {
			t.Fatalf("append seq %d: %v", rec.Seq, err)
		}
	}
	if err := w.EndAt(endReason, recs[len(recs)-1].ReceivedAt); err != nil {
		t.Fatal(err)
	}
	return path
}

// lvModel reads the model codex reported for the root thread, "" when the
// stream says nothing: the header never guesses one.
func lvModel(recs []oarlock.Record) string {
	for _, r := range recs {
		if r.Kind != oarlock.KindFrame || r.SessionID != recs[0].SessionID {
			continue
		}
		var head struct {
			Params struct {
				Model string `json:"model"`
			} `json:"params"`
		}
		if json.Unmarshal(r.Native, &head) == nil && head.Params.Model != "" {
			return head.Params.Model
		}
	}
	return ""
}

// lvReplayNoProcess replays path with a PATH whose only entry is a tripwire
// `codex` script. A replay that starts a process, or needs the runtime, fails.
func lvReplayNoProcess(t *testing.T, path string) []oarlock.Record {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	script := "#!/bin/sh\necho ran > " + marker + "\n"
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	recs, err := voyage.Replay(path)
	if err != nil {
		t.Fatalf("replay with a poisoned PATH: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replay started a process (%s exists)", marker)
	}
	return recs
}

// TestLiveSubagentRecordsAndReplays is the issue #83 recording: one real codex
// turn that spawns a child thread, recorded, normalised and replayed offline.
func TestLiveSubagentRecordsAndReplays(t *testing.T) {
	if os.Getenv("OARLOCK_LIVE_CODEX") != "1" {
		t.Skip("NOT VERIFIED: set OARLOCK_LIVE_CODEX=1 to record a codex sub-agent turn")
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		t.Skipf("NOT VERIFIED: no codex binary on PATH: %v", err)
	}
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		t.Skip("NOT VERIFIED: set CODEX_HOME to a directory whose config.toml names the model, model_provider and env_key")
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); err != nil {
		t.Skipf("NOT VERIFIED: CODEX_HOME/config.toml is not readable: %v", err)
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("NOT VERIFIED: set OPENAI_API_KEY to the token CODEX_HOME/config.toml names as env_key")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cwd := t.TempDir()
	sess, err := codex.Open(ctx, oarlock.Installation{Available: true, Path: path}, oarlock.Options{Cwd: cwd})
	if err != nil {
		t.Fatalf("open codex: %v", err)
	}
	if out, err := sess.Prompt(ctx, lvPrompt, oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	// Both turns must end: the root's turn/completed alone would be a stream
	// where the child never finished, and the child's is half the evidence.
	deadline := time.Now().Add(4 * time.Minute)
	lvWait(t, deadline, "the root and child turns", func() bool {
		recs := sess.Records(oarlock.Cursor{})
		roots, children := lvTurnEndCounts(recs)
		return roots >= 1 && children >= 1
	})
	lvWait(t, deadline, "idle", func() bool {
		return fold.StatusOf(sess.Records(oarlock.Cursor{})).Value.Kind == fold.StatusIdle
	})
	if err := sess.Dispose(ctx); err != nil {
		t.Fatal(err)
	}
	records := sess.Records(oarlock.Cursor{})
	live := lvReadSubagent(t, records)
	if live.Answers.Status.Value.Kind != fold.StatusExited {
		t.Fatalf("live status = %s, want exited after dispose", live.Answers.Status.Value.Kind)
	}
	if live.Answers.Status.Value.LastTurnOutcome == nil || live.Answers.Status.Value.LastTurnOutcome.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("root last turn = %+v, want completed", live.Answers.Status.Value.LastTurnOutcome)
	}

	norm := lvNormaliseLive(t, records, live.Root, live.Child, cwd)
	// Normalisation must not touch a fold answer, or the fixture stops being
	// evidence.
	if got, want := lvJSON(t, lvFold(norm)), lvJSON(t, live.Answers); got != want {
		t.Fatalf("normalisation changed the fold:\n live=%s\n norm=%s", want, got)
	}
	normFacts := lvReadSubagent(t, norm)
	if normFacts.Edge.Parent != lvFixedRoot || normFacts.Edge.Child != lvFixedChild {
		t.Fatalf("normalised edge = %q → %q, want %q → %q", normFacts.Edge.Parent, normFacts.Edge.Child, lvFixedRoot, lvFixedChild)
	}

	logPath := lvWrite(t, lvFixture, norm, "disposed")
	replayed := lvReplayNoProcess(t, logPath)
	if got, want := lvJSON(t, lvFold(replayed)), lvJSON(t, live.Answers); got != want {
		t.Fatalf("replay differs from live:\n live=%s\n replay=%s", want, got)
	}
	lvReadSubagent(t, replayed)
	log, err := voyage.Read(logPath)
	if err != nil || !log.Complete() {
		t.Fatalf("read %s: %v (complete %v)", logPath, err, log != nil && log.Complete())
	}
	if log.Header.SessionID != lvFixedRoot {
		t.Fatalf("header session %q, want %q", log.Header.SessionID, lvFixedRoot)
	}
	t.Logf("delegation items: %s", lvDelegationItems(records))
	t.Logf("recorded %d records (%d bytes) to %s: edge %s → %s, %d child records, WithChildren %s",
		len(replayed), fileSize(t, logPath), logPath, normFacts.Edge.Parent, normFacts.Edge.Child,
		lvChildRecordCount(replayed, normFacts.Edge.Child), lvJSON(t, normFacts.Answers.Usage.Value.WithChildren))
}

// lvTurnEndCounts counts the root's and the derived sessions' turn ends.
func lvTurnEndCounts(recs []oarlock.Record) (root, children int) {
	if len(recs) == 0 {
		return 0, 0
	}
	rootSession := recs[0].SessionID
	for _, ev := range lvEvents(recs) {
		if ev.Kind != oarlock.EventTurnEnded {
			continue
		}
		if ev.Rec.SessionID == rootSession && len(ev.Rec.AgentPath) == 0 {
			root++
		} else {
			children++
		}
	}
	return root, children
}

// lvChildRecordCount counts the records that carry the child session id.
func lvChildRecordCount(recs []oarlock.Record, child string) int {
	n := 0
	for _, r := range recs {
		if r.SessionID == child {
			n++
		}
	}
	return n
}

// lvDelegationItems names every native item that mentions another thread, so the
// report says how this codex build stated the spawn (a subAgentActivity item, a
// collab tool call, or both).
func lvDelegationItems(recs []oarlock.Record) string {
	var parts []string
	for _, r := range recs {
		if r.Kind != oarlock.KindFrame {
			continue
		}
		var head struct {
			Method string `json:"method"`
			Params struct {
				Item struct {
					Type      string   `json:"type"`
					Tool      string   `json:"tool"`
					Kind      string   `json:"kind"`
					AgentID   string   `json:"agentThreadId"`
					Receivers []string `json:"receiverThreadIds"`
				} `json:"item"`
			} `json:"params"`
		}
		if json.Unmarshal(r.Native, &head) != nil {
			continue
		}
		it := head.Params.Item
		if it.Type == "" || (it.AgentID == "" && len(it.Receivers) == 0) {
			continue
		}
		parts = append(parts, fmt.Sprintf("seq %d %s item=%s tool=%s kind=%s child=%s", r.Seq, head.Method, it.Type, it.Tool, it.Kind, firstNonEmpty(it.AgentID, strings.Join(it.Receivers, ","))))
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, "; ")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
