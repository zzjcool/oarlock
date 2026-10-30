package voyage_test

// The issue #71 live acceptance: record a real Pi 1.4.0 turn that runs a bash
// tool call (and, second, a manual /compact) into a voyage log, replay it with
// no process and no network, and prove the folded Status / Usage / Context /
// Conversation and the tool-call evidence — started, ended, exit code, text —
// survive the round trip.
//
// Both tests need the real pi, its login and its session directory, so they are
// opt-in. The recorded log stays in a temp dir, except when OAR_PI_RECORD=1:
// then the normalised log is (re)written as the committed fixture under
// testdata/voyage, which is how the committed live fixtures were made.
// Recording is never a build step: `go test ./...` and CI skip both tests.
//
// A recording is normalised before it is committed (see ltNormalise): the wall
// clock becomes a fixed one, the session id a fixed one, and the host's home
// directory and cwd become "$HOME" and ".". ltFold proves per run that the
// normalisation changes nothing the fold can see, and the replay comparison is
// against the pre-write live fold, so redaction, write, read and folding are
// all covered.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/adapters/pi"
	"github.com/zzjcool/oarlock/internal/fold"
	"github.com/zzjcool/oarlock/internal/voyage"
)

const (
	ltToolFixture = "pi-1.4.0-tool-turn"
	ltRecordGate  = "OAR_PI_RECORD"
	ltFixtureDir  = "../../testdata/voyage"
	// ltToolText is the output of the recorded bash call: what the replay has
	// to keep is "the runtime ran this command and got this back".
	ltToolText = "oarlock-tool-ok"
	// ltToolPrompt asks for exactly one bash call and a one-line answer.
	ltToolPrompt = "Run this exact bash command, then reply with the command's output and nothing else. Command: echo " + ltToolText
	ltRecorder   = "oarlock/voyage-live (pi 1.4.0, issue #71)"
)

// ltBase is the fixed clock of a recorded fixture: record Seq is received at
// ltBase+Seq ms, the same rule the pifixture stand-in uses.
var ltBase = time.Date(2026, time.November, 3, 0, 0, 0, 0, time.UTC)

// ltAnswers is the four documented fold queries issue #71 asks to compare.
type ltAnswers struct {
	Status  fold.QueryResult[fold.Status]
	Usage   fold.QueryResult[fold.Usage]
	Context fold.QueryResult[*fold.ContextReport]
	Conv    fold.QueryResult[[]fold.Message]
}

func ltFold(recs []oarlock.Record) ltAnswers {
	f := fold.Of(recs)
	return ltAnswers{Status: f.Status(), Usage: f.Usage(), Context: f.ContextUsage(), Conv: f.Conversation()}
}

func ltJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func ltEqual(t *testing.T, what string, a, b ltAnswers) {
	t.Helper()
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("%s differ:\n recorded=%s\n replayed=%s", what, ltJSON(t, a), ltJSON(t, b))
	}
}

// ltWait blocks until cond holds or the deadline passes.
func ltWait(t *testing.T, deadline time.Time, what string, cond func() bool) {
	t.Helper()
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ltSession opens a real Pi session in a throwaway cwd and retires its session
// file afterwards. It skips (not fails) when Pi cannot be opened here, so a
// machine without a login reports "unverified" instead of a red build.
func ltSession(t *testing.T, ctx context.Context) (oarlock.Session, string) {
	t.Helper()
	cwd := t.TempDir()
	agent, err := pi.AgentDir(os.Environ())
	if err != nil {
		t.Skipf("cannot locate the pi agent directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(pi.SessionDir(cwd, agent)) })
	sess, err := pi.Open(ctx, oarlock.Installation{}, oarlock.Options{Cwd: cwd})
	if err != nil {
		t.Skipf("pi could not be opened here (login or model missing?): %v", err)
	}
	return sess, cwd
}

// ltSettle waits for the turn to end and, best effort, for the post-settlement
// stats frame that carries the context figure. An honest "Pi reported no
// context" stays comparable; only the turn end is required.
func ltSettle(t *testing.T, sess oarlock.Session, deadline time.Time) {
	t.Helper()
	recs := func() []oarlock.Record { return sess.Records(oarlock.Cursor{}) }
	ltWait(t, deadline, "idle after the turn", func() bool {
		return fold.StatusOf(recs()).Value.Kind == fold.StatusIdle
	})
	for fold.ContextUsageOf(recs()).Value == nil {
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ltNormalisation is what a committed live recording is allowed to remember.
type ltNormalisation struct {
	Session   string // the runtime's session id, as recorded
	SessionTo string // the fixed id it becomes
	Home      string // the recording host's home directory
	Cwd       string // the recording host's cwd
}

// ltNormalise rewrites the nondeterministic parts of a live recording: the wall
// clock becomes ltBase+Seq, the session id a fixed id, the home directory
// "$HOME" and the cwd ".". Everything the fold answers with stays what it was;
// the caller checks that with ltFold instead of trusting this comment.
func ltNormalise(t *testing.T, recs []oarlock.Record, n ltNormalisation) []oarlock.Record {
	t.Helper()
	out := make([]oarlock.Record, len(recs))
	for i, rec := range recs {
		body := []byte(ltJSON(t, rec))
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
		for _, pair := range [][2]string{
			{n.Session, n.SessionTo},
			{n.Home, "$HOME"},
			{n.Cwd, "."},
		} {
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

// ltNormaliseLive is ltNormalise with the three host values a live recording
// carries filled in.
func ltNormaliseLive(t *testing.T, recs []oarlock.Record, fixedID, cwd string) []oarlock.Record {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return ltNormalise(t, recs, ltNormalisation{Session: recs[0].SessionID, SessionTo: fixedID, Home: home, Cwd: cwd})
}

// ltWrite writes recs as a voyage log and returns its path. With
// OAR_PI_RECORD=1 the log replaces the committed fixture, which is the only way
// testdata/voyage/<name>.jsonl is produced or updated.
func ltWrite(t *testing.T, name string, recs []oarlock.Record, endReason string) string {
	t.Helper()
	return ltWriteAs(t, name, recs, endReason, ltRecorder)
}

// ltWriteAs is ltWrite with the header's recorder line chosen by the caller, so
// each issue's fixtures name the test that produced them.
func ltWriteAs(t *testing.T, name string, recs []oarlock.Record, endReason, recorder string) string {
	t.Helper()
	dir := t.TempDir()
	if os.Getenv(ltRecordGate) == "1" {
		dir = ltFixtureDir
		t.Logf("recording the committed fixture %s/%s.jsonl", ltFixtureDir, name)
	}
	path := filepath.Join(dir, name+".jsonl")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	w, err := voyage.Create(path, voyage.Header{
		Runtime:   "pi",
		Cwd:       ".",
		SessionID: recs[0].SessionID,
		StartedAt: ltBase.UnixMilli(),
		Recorder:  recorder,
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

// ltReplayNoProcess replays path with a PATH whose only entry is a tripwire
// `pi` script that records itself if anything runs it. A replay that starts a
// process, or needs the runtime, fails here.
func ltReplayNoProcess(t *testing.T, path string) []oarlock.Record {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	script := "#!/bin/sh\necho ran > " + marker + "\n"
	if err := os.WriteFile(filepath.Join(dir, "pi"), []byte(script), 0o755); err != nil {
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

// ltEvents numbers every event of the stream, in Seq order, so an order
// assertion names what it compared.
type ltEvent struct {
	Seq uint64
	oarlock.Event
}

func ltEvents(recs []oarlock.Record) []ltEvent {
	var out []ltEvent
	for _, r := range recs {
		for _, ev := range r.Events {
			out = append(out, ltEvent{Seq: r.Seq, Event: ev})
		}
	}
	return out
}

// ltToolTurn pins the tool-call evidence of one stream: exactly one bash call,
// started before it ended, its reported exit code 0, the command's output as
// the end report's text, and the assistant's answer after the call — the order
// the live stream had, in the events and in the conversation.
func ltToolTurn(t *testing.T, recs []oarlock.Record) {
	t.Helper()
	evs := ltEvents(recs)
	var started, ended, answer *ltEvent
	for i := range evs {
		ev := evs[i]
		switch ev.Kind {
		case oarlock.EventToolCallStarted:
			if started != nil {
				t.Fatalf("two tool calls started: %+v and %+v", *started, ev)
			}
			started = &ev
		case oarlock.EventToolCallEnded:
			if ended != nil {
				t.Fatalf("two tool calls ended: %+v and %+v", *ended, ev)
			}
			ended = &ev
		case oarlock.EventTextDelta:
			if started != nil {
				answer = &ev
			}
		}
	}
	if started == nil || ended == nil || answer == nil {
		t.Fatalf("tool turn evidence missing: started=%v ended=%v answer=%v", started, ended, answer)
	}
	if started.Tool != "bash" || started.CallID == "" || ended.CallID != started.CallID {
		t.Fatalf("call = %q started as %q, ended as %q", started.CallID, started.Tool, ended.CallID)
	}
	if !(started.Seq < ended.Seq && ended.Seq <= answer.Seq) {
		t.Fatalf("order = started@%d ended@%d answer@%d, want started < ended <= answer", started.Seq, ended.Seq, answer.Seq)
	}
	if code, reported := ended.ReportedExitCode(); !reported || code != 0 {
		t.Fatalf("exit code = %d (reported %v), want a reported 0", code, reported)
	}
	if ended.Result != oarlock.ToolOK {
		t.Fatalf("tool result = %q, want %q", ended.Result, oarlock.ToolOK)
	}
	var out strings.Builder
	for _, part := range ended.Content {
		out.WriteString(part.Text)
	}
	if !strings.Contains(out.String(), ltToolText) {
		t.Fatalf("tool content = %q, want it to contain %q", out.String(), ltToolText)
	}

	// The same order in the folded conversation: the user's prompt, the tool
	// call, then the assistant's answer.
	var user, tool, last *fold.Message
	conv := fold.Of(recs).Conversation().Value
	for i := range conv {
		m := &conv[i]
		switch m.Role {
		case fold.RoleUser:
			if user == nil {
				user = m
			}
		case fold.RoleTool:
			if tool != nil {
				t.Fatalf("two tool messages in the conversation: %+v and %+v", *tool, *m)
			}
			tool = m
		case fold.RoleAssistant:
			if m.Text != "" {
				last = m
			}
		}
	}
	if user == nil || tool == nil || last == nil {
		t.Fatalf("conversation evidence missing: user=%v tool=%v assistant=%v", user, tool, last)
	}
	if !(user.Seq <= tool.Seq && tool.Seq <= last.Seq) {
		t.Fatalf("conversation order = user@%d tool@%d assistant@%d", user.Seq, tool.Seq, last.Seq)
	}
	if tool.CallID != started.CallID || tool.Tool != "bash" {
		t.Fatalf("tool message = %+v, want call %q on bash", tool, started.CallID)
	}
	if tool.Result != oarlock.ToolOK || !tool.ExitCodeReported || tool.ExitCode == nil || *tool.ExitCode != 0 {
		t.Fatalf("tool message result/exit = %q/%v/%v", tool.Result, tool.ExitCode, tool.ExitCodeReported)
	}
	if !strings.Contains(tool.Output, ltToolText) {
		t.Fatalf("tool message output = %q, want it to contain %q", tool.Output, ltToolText)
	}
}

func TestLiveToolTurnRecordsAndReplays(t *testing.T) {
	if os.Getenv("OAR_PI_LIVE") != "1" {
		t.Skip("set OAR_PI_LIVE=1 to record a real Pi tool turn")
	}
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skipf("pi is not on PATH: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	sess, cwd := ltSession(t, ctx)
	if out, err := sess.Prompt(ctx, ltToolPrompt, oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	ltSettle(t, sess, time.Now().Add(3*time.Minute))
	if err := sess.Dispose(ctx); err != nil {
		t.Fatal(err)
	}
	records := sess.Records(oarlock.Cursor{})
	if len(records) == 0 {
		t.Fatal("the session retained no record")
	}

	// The live answer, from the records the kernel retained, and the tool
	// evidence in them: without a tool call there is nothing to compare.
	ltToolTurn(t, records)
	live := ltFold(records)
	if live.Status.Value.Kind != fold.StatusExited {
		t.Fatalf("live status = %s, want exited after dispose", live.Status.Value.Kind)
	}

	norm := ltNormaliseLive(t, records, "pi-live-"+ltToolFixture, cwd)
	// Normalisation must not touch a fold answer; otherwise the fixture would
	// stop being evidence.
	ltEqual(t, "live vs normalised", live, ltFold(norm))

	path := ltWrite(t, ltToolFixture, norm, "disposed")
	replayed := ltReplayNoProcess(t, path)
	ltEqual(t, "live vs replayed", live, ltFold(replayed))
	ltToolTurn(t, replayed)
	log, err := voyage.Read(path)
	if err != nil || !log.Complete() {
		t.Fatalf("read %s: %v (complete %v)", path, err, log != nil && log.Complete())
	}
	if log.Header.SessionID != norm[0].SessionID {
		t.Fatalf("header session %q, records %q", log.Header.SessionID, norm[0].SessionID)
	}
	t.Logf("recorded %d records (%d bytes) to %s", len(replayed), fileSize(t, path), path)
}

// ltFrameTypes tallies the native "type" of every frame after seq, in first
// appearance order, so a "compaction did not happen" report names what Pi did
// send instead.
func ltFrameTypes(recs []oarlock.Record, after uint64) string {
	var order []string
	count := map[string]int{}
	for _, r := range recs {
		if r.Kind != oarlock.KindFrame || r.Seq <= after {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(r.Native, &head); err != nil || head.Type == "" {
			head.Type = "<unreadable>"
		}
		if count[head.Type] == 0 {
			order = append(order, head.Type)
		}
		count[head.Type]++
	}
	if len(order) == 0 {
		return "(no frame)"
	}
	parts := make([]string, len(order))
	for i, typ := range order {
		parts[i] = fmt.Sprintf("%s x%d", typ, count[typ])
	}
	return strings.Join(parts, ", ")
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}
