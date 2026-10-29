package cursor

// Replay of the two REAL cursor-agent traces in testdata/cursor (recorded
// 2026-10-28, cursor-agent 2026.09.18-9a7762b). Every frame's events are
// asserted, and the event count per fixture is pinned: a silent upstream
// addition — a new event type, a renamed subtype, a tool call folded away —
// changes the count and fails loudly instead of quietly shrinking the
// transcript. That silence is the MUL-5231 bug this file exists to prevent.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

const (
	normalFixture = "2026.09.18-auto-normal.stream-json.jsonl"
	shellFixture  = "2026.09.18-auto-shell-tool.stream-json.jsonl"

	// shellCallID is the shell fixture's packed call id: cursor puts two ids in
	// one field separated by a newline, and the projection keeps the first line.
	shellCallID = "call-ef8a46bc-a4b0-4281-a63f-03e3bb746f57-0"

	// The preamble cursor-agent emits on some auto-routed models.
	routedPreamble = "> Auto routed to Grok 4.7\n\n"
)

func loadFixtureFrames(t *testing.T, name string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "cursor", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
}

func countEvents(frames []sj.Frame) int {
	n := 0
	for _, f := range frames {
		n += len(f.Events)
	}
	return n
}

// checkKinds pins the exact event kinds of one frame, in order.
func checkKinds(t *testing.T, i int, f sj.Frame, want ...oarlock.EventKind) {
	t.Helper()
	got := kindsOf(f)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("frame %d (%s): events %v, want %v", i, f.Type, got, want)
	}
}

// allText is every text_delta of the replay, concatenated: the assistant text
// the record ends up carrying.
func allText(frames []sj.Frame) string {
	var b strings.Builder
	for _, f := range frames {
		for _, e := range f.Events {
			if e.Kind == oarlock.EventTextDelta {
				b.WriteString(e.Text)
			}
		}
	}
	return b.String()
}

// allReasoning is every reasoning event's text, in order.
func allReasoning(frames []sj.Frame) []string {
	var out []string
	for _, f := range frames {
		for _, e := range f.Events {
			if e.Kind == oarlock.EventReasoning {
				out = append(out, e.Text)
			}
		}
	}
	return out
}

// checkNoUnknown proves the fixture states nothing the adapters had to absorb:
// no undeclared top-level type (the folder's tally) and no unread subtype of a
// declared one (the profile's tally).
func checkNoUnknown(t *testing.T, name string, frames [][]byte) {
	t.Helper()
	p := newProfile()
	f := sj.NewFolder(p)
	direct := make([]sj.Frame, 0, len(frames))
	for _, raw := range frames {
		direct = append(direct, f.Fold(raw))
	}
	if got := f.Unknown(); len(got) != 0 {
		t.Fatalf("%s: unknown top-level types %v", name, got)
	}
	if got := p.unknownSubtypes(); len(got) != 0 {
		t.Fatalf("%s: unknown subtypes %v", name, got)
	}
	// Project folds through the same Folder: a live session and this replay
	// cannot disagree about the trace.
	if project := Project(frames); !reflect.DeepEqual(project, direct) {
		t.Fatalf("%s: Project != a direct Folder fold", name)
	}
}

func TestReplayNormalFixture(t *testing.T) {
	raw := loadFixtureFrames(t, normalFixture)
	fr := Project(raw)
	if len(fr) != 10 {
		t.Fatalf("frames = %d, want 10", len(fr))
	}
	// 2 init + 1 preamble + 3 reasoning + 1 "ok" + 2 result; the cumulative
	// frame and thinking/completed state nothing.
	if n := countEvents(fr); n != 9 {
		t.Fatalf("events = %d, want 9; got %v", n, eventKinds(fr))
	}

	checkKinds(t, 0, fr[0], oarlock.EventTurnActive, oarlock.EventModel)
	if got := fr[0].Events[1].Model; got != "Auto Balance" {
		t.Fatalf("model = %q, want Auto Balance", got)
	}
	checkKinds(t, 1, fr[1]) // the prompt echo states no turn fact here
	checkKinds(t, 2, fr[2], oarlock.EventTextDelta)
	if got := fr[2].Events[0].Text; got != routedPreamble {
		t.Fatalf("preamble = %q", got)
	}
	if got := allReasoning(fr); !reflect.DeepEqual(got, []string{
		"The", " user wants", ` me to reply with the single word "ok".`,
	}) {
		t.Fatalf("reasoning = %#v", got)
	}
	checkKinds(t, 7, fr[7], oarlock.EventTextDelta)
	if got := fr[7].Events[0].Text; got != "ok" {
		t.Fatalf("answer = %q", got)
	}
	checkKinds(t, 8, fr[8]) // the cumulative snapshot repeats the turn: nothing

	checkKinds(t, 9, fr[9], oarlock.EventTurnEnded, oarlock.EventUsage)
	if !fr[9].TurnEnded {
		t.Fatal("result frame did not end the turn")
	}
	outcome := fr[9].Events[0].Outcome
	if outcome == nil || outcome.Kind != oarlock.OutcomeCompleted || outcome.Failure != "" {
		t.Fatalf("outcome = %+v, want completed with no failure", outcome)
	}
	checkTokens(t, fr[9].Events[1], 6514, 28, 6016, 0)

	// The turn's text is the preamble plus the answer, stated once: a second
	// copy of the cumulative frame would show up as a doubled preamble.
	if got := allText(fr); got != routedPreamble+"ok" {
		t.Fatalf("text = %q", got)
	}
	if got := strings.Count(allText(fr), "Auto routed"); got != 1 {
		t.Fatalf("preamble stated %d times, want 1", got)
	}

	checkNoUnknown(t, normalFixture, raw)
}

func TestReplayShellToolFixture(t *testing.T) {
	raw := loadFixtureFrames(t, shellFixture)
	fr := Project(raw)
	if len(fr) != 21 {
		t.Fatalf("frames = %d, want 21", len(fr))
	}
	// 2 init + 1 preamble + 2 reasoning + 6 pieces + started + ended
	// + 2 reasoning + "done" + 2 result. The cumulative frame, both
	// thinking/completed frames and the repeated "done" state nothing.
	if n := countEvents(fr); n != 18 {
		t.Fatalf("events = %d, want 18; got %v", n, eventKinds(fr))
	}

	checkKinds(t, 0, fr[0], oarlock.EventTurnActive, oarlock.EventModel)
	if got := fr[0].Events[1].Model; got != "Auto Balance" {
		t.Fatalf("model = %q, want Auto Balance", got)
	}
	checkKinds(t, 1, fr[1])
	checkKinds(t, 2, fr[2], oarlock.EventTextDelta)
	if got := fr[2].Events[0].Text; got != routedPreamble {
		t.Fatalf("preamble = %q", got)
	}

	// The six pieces of the first message, then the cumulative frame with
	// nothing new: the turn's text is not repeated.
	wantPieces := []string{"I'll", " run", " that", " command", " now", "."}
	for i, want := range wantPieces {
		checkKinds(t, 6+i, fr[6+i], oarlock.EventTextDelta)
		if got := fr[6+i].Events[0].Text; got != want {
			t.Fatalf("piece %d = %q, want %q", i, got, want)
		}
	}
	checkKinds(t, 12, fr[12])

	// The call: top-level tool_call frames, a single-line id, the tool name
	// from the payload key, and the command it ran.
	checkKinds(t, 13, fr[13], oarlock.EventToolCallStarted)
	started := fr[13].Events[0]
	if started.CallID != shellCallID {
		t.Fatalf("started call id = %q, want %q", started.CallID, shellCallID)
	}
	if started.Tool != "shell" {
		t.Fatalf("started tool = %q, want shell", started.Tool)
	}
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(started.Input), &args); err != nil {
		t.Fatalf("input is not JSON (%q): %v", started.Input, err)
	}
	if args.Command != "echo hi" {
		t.Fatalf("input command = %q, want echo hi", args.Command)
	}
	if strings.Contains(started.Input, "\n") {
		t.Fatalf("input is not compact: %q", started.Input)
	}

	checkKinds(t, 14, fr[14], oarlock.EventToolCallEnded)
	ended := fr[14].Events[0]
	if ended.CallID != shellCallID || ended.Tool != "shell" {
		t.Fatalf("ended = %+v", ended)
	}
	if ended.Result != oarlock.ToolOK {
		t.Fatalf("ended result = %q, want ok", ended.Result)
	}
	if code, reported := ended.ReportedExitCode(); !reported || code != 0 {
		t.Fatalf("exit code = %d reported=%v, want 0 reported", code, reported)
	}
	if want := []oarlock.ToolOutputPart{{Type: "text", Text: "hi\n"}}; !reflect.DeepEqual(ended.Content, want) {
		t.Fatalf("content = %+v, want %+v", ended.Content, want)
	}

	// The reasoning after the tool call, then "done" once — the second,
	// identical "done" frame states nothing new.
	if got := allReasoning(fr); !reflect.DeepEqual(got, []string{
		"The",
		` user wants me to use the shell tool to run exactly "echo hi" and then reply "done".`,
		"The",
		` command ran successfully and output "hi".`,
	}) {
		t.Fatalf("reasoning = %#v", got)
	}
	checkKinds(t, 18, fr[18], oarlock.EventTextDelta)
	if got := fr[18].Events[0].Text; got != "done" {
		t.Fatalf("answer = %q", got)
	}
	checkKinds(t, 19, fr[19]) // the duplicate frame

	checkKinds(t, 20, fr[20], oarlock.EventTurnEnded, oarlock.EventUsage)
	if !fr[20].TurnEnded {
		t.Fatal("result frame did not end the turn")
	}
	outcome := fr[20].Events[0].Outcome
	if outcome == nil || outcome.Kind != oarlock.OutcomeCompleted || outcome.Failure != "" {
		t.Fatalf("outcome = %+v, want completed with no failure", outcome)
	}
	// The shell fixture's own recorded counts (the normal fixture's are
	// 6514/28/6016/0).
	checkTokens(t, fr[20].Events[1], 6746, 70, 18432, 0)

	if got, want := allText(fr), routedPreamble+"I'll run that command now.done"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
	if got := strings.Count(allText(fr), "Auto routed"); got != 1 {
		t.Fatalf("preamble stated %d times, want 1", got)
	}
	if got := strings.Count(allText(fr), "done"); got != 1 {
		t.Fatalf("done stated %d times, want 1", got)
	}

	checkNoUnknown(t, shellFixture, raw)
}

// checkTokens pins a usage event's Tokens: the four counts the result stated,
// with the cache counts present as pointers (a reported zero is not absent).
func checkTokens(t *testing.T, ev oarlock.Event, input, output, cacheRead, cacheWrite int64) {
	t.Helper()
	if ev.Kind != oarlock.EventUsage || ev.Usage == nil || ev.Usage.Tokens == nil {
		t.Fatalf("usage event = %+v", ev)
	}
	tok := ev.Usage.Tokens
	if tok.Input != input || tok.Output != output {
		t.Fatalf("tokens = input %d output %d, want %d/%d", tok.Input, tok.Output, input, output)
	}
	if tok.CacheRead == nil || *tok.CacheRead != cacheRead {
		t.Fatalf("cacheRead = %v, want %d", tok.CacheRead, cacheRead)
	}
	if tok.CacheWrite == nil || *tok.CacheWrite != cacheWrite {
		t.Fatalf("cacheWrite = %v, want %d", tok.CacheWrite, cacheWrite)
	}
}

// TestUndeclaredTopLevelType is the MUL-5231 guard: a top-level type nobody
// declared is counted by the shared folder (not absorbed by the projection) and
// stays in the frame, which is what the record writes as Native.
func TestUndeclaredTopLevelType(t *testing.T) {
	p := newProfile()
	f := sj.NewFolder(p)
	for _, raw := range loadFixtureFrames(t, normalFixture) {
		f.Fold(raw)
	}
	if got := f.Unknown(); len(got) != 0 {
		t.Fatalf("the recorded trace already has unknown types: %v", got)
	}

	fr := f.Fold([]byte(`{"type":"mystery_event","seq":7,"note":"not a cursor frame"}`))
	if got := kindsOf(fr); got != nil {
		t.Fatalf("undeclared type produced events %v", got)
	}
	if got := f.Unknown(); !reflect.DeepEqual(got, map[string]int{"mystery_event": 1}) {
		t.Fatalf("Unknown = %v, want mystery_event=1", got)
	}
	if len(p.unknownSubtypes()) != 0 {
		t.Fatalf("the profile counted a type that is not its own: %v", p.unknownSubtypes())
	}
	// Kept: the frame (the record's Native) still holds the vendor's payload.
	if fr.Rec == nil || sj.Str(fr.Rec["type"]) != "mystery_event" || sj.Str(fr.Rec["note"]) != "not a cursor frame" {
		t.Fatalf("frame not kept: %+v", fr.Rec)
	}
	if seq, ok := sj.AsNum(fr.Rec["seq"]); !ok || seq != 7 {
		t.Fatalf("frame payload lost: %+v", fr.Rec)
	}
}

// eventKinds is a one-line rendering of a whole replay's event kinds, for a
// failure message that shows what the new event was.
func eventKinds(frames []sj.Frame) []string {
	var out []string
	for _, f := range frames {
		for _, k := range kindsOf(f) {
			out = append(out, string(k))
		}
	}
	return out
}
