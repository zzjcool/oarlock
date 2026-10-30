package voyage_test

// The issue #82 live acceptance: record a real Pi 1.4.0 manual compaction and
// replay it with no process and no network.
//
// #71 tried the `/compact` slash command as an ordinary prompt and Pi ran a
// normal turn: the text reached the model and no compaction frame was ever
// sent. Pi's RPC command set has a real `compact` command, though
// (`{"type":"compact"}` → `session.compact()`, docs/runtimes/pi.md §3), and
// this test drives that one. It speaks the command on the session's own Conn —
// the only place it can be sent today, because oarlock's control surface has no
// compact action (issue #82's non-goal) and the Pi adapter sends Pi's RPC
// commands only for the actions it has. The command therefore leaves no
// oarlock request record; the stream carries Pi's own frames, and the
// `response(command:"compact")` frame is the wire evidence that the command ran.
//
// The recording needs the real pi, its login and its session directory, so it
// is opt-in (OAR_PI_LIVE=1). OAR_PI_RECORD=1 turns the normalised log into the
// committed fixture, exactly like the tool/breadth recordings.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/adapters/pi"
	"github.com/zzjcool/oarlock/internal/fold"
)

const (
	// lcFixture is the committed recording of the compaction.
	lcFixture = "pi-1.4.0-compaction-turn"
	// lcRecorder names the test in the fixture header.
	lcRecorder = "oarlock/voyage-live (pi 1.4.0, issue #82)"
	// lcPath is the committed fixture the offline half replays.
	lcPath = "../../testdata/voyage/" + lcFixture + ".jsonl"
	// lcKeepRecentTokens is written into the temp cwd's project settings.
	//
	// Pi's manual compaction summarizes what a cut point keeps out: it walks
	// the branch back from the end until keepRecentTokens is covered and cuts
	// there (DEFAULT_COMPACTION_SETTINGS.keepRecentTokens is 20000, tokens ×4
	// chars). A bounded live test cannot honestly produce a 20k-token
	// conversation, so the recording sets the documented project setting to 0
	// instead: the cut lands in the last turn, and the two short turns before
	// it are what gets summarized. The setting lives in the session's cwd, not
	// in the fixture; a Pi with its default keeps "Nothing to compact" for a
	// session this small, which is a fact about the test, not about compaction.
	lcKeepRecentTokens = "0"
	// lcPrompt asks for a one-word answer: the turns exist to give the
	// compaction something to summarize, not to be interesting.
	lcPrompt = "Reply with the single word: ok"
)

func TestLiveCompactionRecordsOrReportsUnverified(t *testing.T) {
	if os.Getenv("OAR_PI_LIVE") != "1" {
		t.Skip("set OAR_PI_LIVE=1 to record a real Pi compaction")
	}
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skipf("pi is not on PATH: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cwd := t.TempDir()
	agent, err := pi.AgentDir(os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(pi.SessionDir(cwd, agent)) })
	if err := os.MkdirAll(filepath.Join(cwd, ".pi"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := "{\n  \"compaction\": {\n    \"keepRecentTokens\": " + lcKeepRecentTokens + "\n  }\n}\n"
	if err := os.WriteFile(filepath.Join(cwd, ".pi", "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}

	// A raw Conn plus NewSession is this package's public launch path (what
	// pi.Open uses internally); it is the only way to speak a command the
	// Session's control surface does not send.
	proc, err := pi.Launch(ctx, pi.Options{
		Args:        []string{"--mode", "rpc", "--session-dir", pi.SessionDir(cwd, agent)},
		Dir:         cwd,
		EventBuffer: 4096,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	var state struct {
		SessionID string `json:"sessionId"`
	}
	if err := proc.Do(ctx, pi.Command{Type: "get_state"}, &state); err != nil {
		_ = proc.Close()
		t.Fatalf("get_state: %v", err)
	}
	session, err := pi.NewSession(proc, pi.SessionOptions{SessionID: state.SessionID})
	if err != nil {
		_ = proc.Close()
		t.Fatal(err)
	}
	sess := oarlock.Session(session)

	// Compaction needs something to compact: two real turns first.
	for range 2 {
		if out, err := sess.Prompt(ctx, lcPrompt, oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
			t.Fatalf("prompt = %+v, %v", out, err)
		}
		ltSettle(t, sess, time.Now().Add(3*time.Minute))
	}

	// Pi's own manual compaction command, on the wire as Pi declares it:
	// {"type":"compact"} with no customInstructions.
	var result struct {
		TokensBefore         int64 `json:"tokensBefore"`
		EstimatedTokensAfter int64 `json:"estimatedTokensAfter"`
	}
	lastSeq := sess.Records(oarlock.Cursor{})[len(sess.Records(oarlock.Cursor{}))-1].Seq
	compactErr := proc.Do(ctx, pi.Command{Type: "compact"}, &result)
	if compactErr != nil {
		t.Skipf("UNVERIFIED: pi refused the RPC compact command: %v; frames since seq %d: %s",
			compactErr, lastSeq, ltFrameTypes(sess.Records(oarlock.Cursor{}), lastSeq))
	}
	deadline := time.Now().Add(2 * time.Minute)
	for !lcCompacted(sess.Records(oarlock.Cursor{})) {
		if time.Now().After(deadline) {
			t.Skipf("UNVERIFIED: the compact command answered (%+v) but no compaction_start/compaction_end frame arrived; frames since seq %d: %s",
				result, lastSeq, ltFrameTypes(sess.Records(oarlock.Cursor{}), lastSeq))
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Pi's own post-compaction fullness, read the way the adapter reads it after
	// every settlement (get_session_stats): this is what shows the compaction
	// changed the context, not only the event stream. It is sent on the Conn
	// rather than as a third Prompt because a manual compaction leaves the
	// session's folded status running (docs/voyage.md, "Compaction"), and the
	// adapter then refuses the next Prompt locally as busy.
	recs := sess.Records(oarlock.Cursor{})
	mark := recs[len(recs)-1].Seq
	if err := proc.Do(ctx, pi.Command{Type: "get_session_stats"}, nil); err != nil {
		t.Fatalf("get_session_stats after the compaction: %v", err)
	}
	for !lcStatsAfter(sess.Records(oarlock.Cursor{}), mark) {
		if time.Now().After(deadline) {
			break // an honest "Pi reported no post-compaction context" is still comparable
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := sess.Dispose(ctx); err != nil {
		t.Fatal(err)
	}

	records := sess.Records(oarlock.Cursor{})
	if len(records) == 0 {
		t.Fatal("the session retained no record")
	}
	lcCompaction(t, records)
	live := ltFold(records)
	if live.Status.Value.Kind != fold.StatusExited {
		t.Fatalf("live status = %s, want exited after dispose", live.Status.Value.Kind)
	}
	norm := ltNormaliseLive(t, records, "pi-live-"+lcFixture, cwd)
	ltEqual(t, "live vs normalised", live, ltFold(norm))

	path := ltWriteAs(t, lcFixture, norm, "disposed", lcRecorder)
	replayed := ltReplayNoProcess(t, path)
	ltEqual(t, "live vs replayed", live, ltFold(replayed))
	lcCompaction(t, replayed)
	t.Logf("recorded a manual compaction: tokensBefore %d -> estimated %d, %d records (%d bytes), %s",
		result.TokensBefore, result.EstimatedTokensAfter, len(replayed), fileSize(t, path), path)
}

// lcCompacted reports whether the stream carried a finished compaction.
func lcCompacted(recs []oarlock.Record) bool {
	for _, ev := range ltEvents(recs) {
		if ev.Kind == oarlock.EventCompactionEnded {
			return true
		}
	}
	return false
}

// lcStatsAfter reports whether a get_session_stats response was recorded after
// seq: Pi's own post-settlement context read.
func lcStatsAfter(recs []oarlock.Record, seq uint64) bool {
	for _, r := range recs {
		if r.Kind != oarlock.KindFrame || r.Seq <= seq {
			continue
		}
		var head struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		}
		if json.Unmarshal(r.Native, &head) != nil {
			continue
		}
		if head.Type == "response" && head.Command == "get_session_stats" {
			return true
		}
	}
	return false
}

// lcCompaction pins what a compaction recording has to show, in the event
// stream and in the native frames: exactly one started/ended pair in that
// order, both manual, the end reported as completed, the `compact` command's
// own response frame — no oarlock request record exists for it, so the frame is
// the only wire evidence — the compaction's own token figures, usage untouched
// by the compaction, Pi's post-compaction context read, and the disposal that
// ends the stream. Every assertion is a fact of the bytes, so the live run and
// the offline replay are checked by the same function.
func lcCompaction(t *testing.T, recs []oarlock.Record) {
	t.Helper()
	evs := ltEvents(recs)
	var started, ended *ltEvent
	for i := range evs {
		ev := evs[i]
		switch ev.Kind {
		case oarlock.EventCompactionStarted:
			if started != nil {
				t.Fatalf("two compactions started: %+v and %+v", *started, ev)
			}
			started = &ev
		case oarlock.EventCompactionEnded:
			if ended != nil {
				t.Fatalf("two compactions ended: %+v and %+v", *ended, ev)
			}
			ended = &ev
		}
	}
	if started == nil || ended == nil {
		t.Fatalf("compaction evidence missing: started=%v ended=%v", started, ended)
	}
	if started.Seq >= ended.Seq {
		t.Fatalf("compaction order = started@%d ended@%d, want started < ended", started.Seq, ended.Seq)
	}
	if started.Trigger != "manual" || ended.Trigger != "manual" {
		t.Fatalf("compaction triggers = %q/%q, want manual/manual", started.Trigger, ended.Trigger)
	}
	if ended.CompactionResult != oarlock.CompactionCompleted {
		t.Fatalf("compaction result = %q, want %q", ended.CompactionResult, oarlock.CompactionCompleted)
	}

	// The command's own answer and the compaction's own numbers, from the native
	// frames: no oarlock request record exists for a compact command, so this is
	// the wire evidence, and Pi's tokensBefore/estimatedTokensAfter pair is what
	// says the context really shrank.
	var answered, numbered bool
	for _, r := range recs {
		if r.Kind != oarlock.KindFrame || r.Seq <= ended.Seq {
			continue
		}
		var res struct {
			Type    string `json:"type"`
			Command string `json:"command"`
			Success bool   `json:"success"`
			Data    struct {
				TokensBefore         int64 `json:"tokensBefore"`
				EstimatedTokensAfter int64 `json:"estimatedTokensAfter"`
			} `json:"data"`
		}
		if json.Unmarshal(r.Native, &res) != nil {
			continue
		}
		if res.Type != "response" || res.Command != "compact" {
			continue
		}
		if !res.Success || res.Data.TokensBefore <= 0 || res.Data.EstimatedTokensAfter <= 0 ||
			res.Data.EstimatedTokensAfter >= res.Data.TokensBefore {
			t.Fatalf("compact response at seq %d = %s, want success and tokensBefore > estimatedTokensAfter > 0", r.Seq, r.Native)
		}
		numbered = true
		answered = true
	}
	if !answered || !numbered {
		t.Fatal("no successful response(command:\"compact\") frame carrying the compaction's token figures")
	}

	// Pi's own post-compaction context read, the one the adapter sends after
	// every settlement.
	if !lcStatsAfter(recs, ended.Seq) {
		t.Fatal("no get_session_stats response after the compaction: the recording does not show the post-compaction context")
	}

	// Usage is untouched by the compaction: the fold before started.Seq and the
	// fold just after ended.Seq report the same ledger. (The QueryResult's own
	// Seq differs — it is the last record consumed — so the ledger is what is
	// compared, not the wrapper.)
	before := ltFold(lcPrefix(recs, started.Seq))
	after := ltFold(lcPrefix(recs, ended.Seq+1))
	if ltJSON(t, before.Usage.Value) != ltJSON(t, after.Usage.Value) {
		t.Fatalf("compaction moved usage:\n before=%s\n after=%s", ltJSON(t, before.Usage.Value), ltJSON(t, after.Usage.Value))
	}
	if before.Status.Value.Kind != fold.StatusIdle {
		t.Fatalf("status before the compaction = %s, want idle (the turns had settled)", before.Status.Value.Kind)
	}
	// The folded status after the compaction is the measured one, not the
	// desired one: the fold turns compaction_started into a running/compacting
	// span (internal/fold/status.go), no turn ends, and so it stays running.
	// Pinned so a fold fix shows up here; docs/voyage.md states the gap.
	if after.Status.Value.Kind != fold.StatusRunning || after.Status.Value.Phase.Kind != fold.PhaseWaitingModel {
		t.Fatalf("status after the compaction = %s/%s, want the measured running/waiting_model",
			after.Status.Value.Kind, after.Status.Value.Phase.Kind)
	}

	// Pi's context count is unknown after a compaction — its own rule: the last
	// assistant usage is pre-compaction, so tokens stay null until a response
	// that followed the compaction. The fold keeps the window and drops the
	// count; a fabricated number here would be the lie this test exists to stop.
	final := ltFold(recs)
	if final.Context.Value == nil || final.Context.Value.Usage.Tokens != nil {
		t.Fatalf("context after the compaction = %s, want a report with no token count", ltJSON(t, final.Context))
	}
	if w := final.Context.Value.Usage.ContextWindow; w == nil || *w <= 0 {
		t.Fatalf("context window = %v, want Pi's model window", w)
	}
	if final.Status.Value.Kind != fold.StatusExited {
		t.Fatalf("status = %s, want exited after the disposal", final.Status.Value.Kind)
	}
}

// lcPrefix returns the records that start before seq: the stream a consumer had
// seen when the record at seq arrived.
func lcPrefix(recs []oarlock.Record, seq uint64) []oarlock.Record {
	var out []oarlock.Record
	for _, r := range recs {
		if r.Seq < seq {
			out = append(out, r)
		}
	}
	return out
}

// TestCommittedCompactionPin replays the committed compaction recording and
// asserts the compaction evidence again, offline: the same order, trigger,
// result, wire evidence and unchanged usage, the observed Status, and the
// numbers the live session reported.
func TestCommittedCompactionPin(t *testing.T) {
	records := ltReplayNoProcess(t, lcPath)
	lcCompaction(t, records)
	// The pinned numbers are the recorded session's, not a claim that another
	// machine would report the same: they are the regression check on the fold.
	t.Logf("usage=%s", ltJSON(t, ltFold(records).Usage.Value))
}
