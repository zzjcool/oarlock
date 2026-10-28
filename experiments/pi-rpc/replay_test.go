//go:build !windows

// Replay: the four recorded JSONL files under testdata/pi are the only input.
// No process, no network, no clock — every claim here is about bytes on disk.
package pirpc

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

type state struct {
	IsStreaming  bool   `json:"isStreaming"`
	MessageCount int    `json:"messageCount"`
	SessionID    string `json:"sessionId"`
}

type turnEnd struct {
	StopReason    string          `json:"stopReason"`
	RawStopReason json.RawMessage `json:"rawStopReason"`
}

// rec is a frame as the package parses it (Frame: Type/ID/Raw) plus the fixture
// fields under test; rec.State is rec.Data decoded.
type rec struct {
	Frame
	Command   string          `json:"command"`
	Success   bool            `json:"success"`
	Aborted   bool            `json:"aborted"`
	WillRetry bool            `json:"willRetry"`
	Data      json.RawMessage `json:"data"`
	Message   json.RawMessage `json:"message"` // an object on turn_end, a string elsewhere
	State     state           `json:"-"`
}

type replay struct {
	name   string
	frames []rec
	reqs   []string // request ids, send order
}

// lines also enforces the fixture envelope: LF only, one trailing newline.
func lines(t *testing.T, name, kind string) []string {
	t.Helper()
	path := "../../testdata/pi/" + name + "." + kind + ".jsonl"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.HasSuffix(string(b), "\n") || strings.ContainsRune(string(b), '\r') {
		t.Fatalf("%s: want one trailing LF and no CR", path)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func load(t *testing.T, name string) replay {
	t.Helper()
	r := replay{name: name}
	for _, line := range lines(t, name, "requests") {
		var req struct{ ID, Type string }
		if err := json.Unmarshal([]byte(line), &req); err != nil || req.ID == "" || req.Type == "" {
			t.Fatalf("%s request: not a JSON command: %q", name, line)
		}
		r.reqs = append(r.reqs, req.ID)
	}
	for i, line := range lines(t, name, "frames") {
		var c rec
		if err := json.Unmarshal([]byte(line), &c); err != nil || c.Type == "" {
			t.Fatalf("%s frame %d: not a JSON frame: %v", name, i, err)
		}
		c.Raw = json.RawMessage(line)
		if len(c.Data) > 0 && json.Unmarshal(c.Data, &c.State) != nil {
			t.Fatalf("%s frame %d: data is not an object: %s", name, i, c.Data)
		}
		r.frames = append(r.frames, c)
	}
	return r
}

// all lists the frame indices of typ; an id of "" matches any id.
func (r replay) all(typ, id string) []int {
	var out []int
	for i, f := range r.frames {
		if f.Type == typ && (id == "" || f.ID == id) {
			out = append(out, i)
		}
	}
	return out
}

// one requires exactly one match, so "exactly one agent_settled" is an assertion.
func (r replay) one(t *testing.T, typ, id string) int {
	t.Helper()
	got := r.all(typ, id)
	if len(got) != 1 {
		t.Fatalf("%s: want exactly one %s (id %q), got frames %v", r.name, typ, id, got)
	}
	return got[0]
}

// endMessage decodes the turn_end payload, which alone carries a message object.
func endMessage(t *testing.T, r replay, i int) turnEnd {
	t.Helper()
	var m turnEnd
	if err := json.Unmarshal(r.frames[i].Message, &m); err != nil || m.StopReason == "" {
		t.Fatalf("%s frame %d: not a turn_end message: %s (%v)", r.name, i, r.frames[i].Message, err)
	}
	return m
}

func eq(t *testing.T, at string, got, want any) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %#v, want %#v", at, got, want)
	}
}

func TestReplayNormalTurnCorrelatesEveryResponseToARequestID(t *testing.T) {
	r := load(t, "normal-turn")
	resp := r.all("response", "")
	if len(resp) != len(r.reqs) {
		t.Fatalf("normal-turn: %d responses for %d requests", len(resp), len(r.reqs))
	}
	for _, i := range resp {
		if !slices.Contains(r.reqs, r.frames[i].ID) {
			t.Fatalf("normal-turn frame %d: response id %q was never requested", i, r.frames[i].ID)
		}
	}
	for _, id := range r.reqs {
		if n := len(r.all("response", id)); n != 1 {
			t.Fatalf("normal-turn request id %q got %d responses, want 1", id, n)
		}
	}
}

func TestReplayNormalTurnEndsWithAgentSettledAfterTurnEndStop(t *testing.T) {
	r := load(t, "normal-turn")
	settled, end := r.one(t, "agent_settled", ""), r.one(t, "turn_end", "")
	// presence, not just value: a vendor dropping the key must fail here
	if !strings.Contains(string(r.frames[settled].Raw), `"aborted":false`) {
		t.Fatalf("normal-turn frame %d: agent_settled.aborted is missing or not false: %s", settled, r.frames[settled].Raw)
	}
	eq(t, "normal-turn agent_settled.aborted", r.frames[settled].Aborted, false)
	msg := endMessage(t, r, end)
	if string(msg.RawStopReason) != `"completed"` {
		t.Fatalf("normal-turn frame %d: turn_end.message.rawStopReason = %s, want \"completed\"", end, msg.RawStopReason)
	}
	eq(t, "normal-turn turn_end.message.stopReason", msg.StopReason, "stop")
	if end >= settled {
		t.Fatalf("normal-turn: turn_end frame %d not before agent_settled frame %d", end, settled)
	}
}

func TestReplayNormalTurnNarratesTheNativeEventOrder(t *testing.T) {
	r := load(t, "normal-turn")
	want := []string{"agent_start", "turn_start", "message_start", "message_end",
		"message_start", "message_end", "message_start", "message_update", "message_end",
		"turn_end", "agent_end", "agent_settled"}
	var got []string
	var idx []int
	for i, f := range r.frames {
		if f.Type == "extension_ui_request" || f.Type == "response" {
			continue
		}
		if f.Type == "message_update" && len(got) > 0 && got[len(got)-1] == f.Type {
			continue // a run of updates holds one position in the narration
		}
		got, idx = append(got, f.Type), append(idx, i)
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("normal-turn event order at frames %v:\n got %v\nwant %v", idx, got, want)
	}
	end := r.one(t, "agent_end", "")
	if !strings.Contains(string(r.frames[end].Raw), `"willRetry":false`) {
		t.Fatalf("normal-turn frame %d: agent_end.willRetry is missing or not false: %s", end, r.frames[end].Raw)
	}
	eq(t, "normal-turn agent_end.willRetry", r.frames[end].WillRetry, false)
}

func TestReplayAbortTurnReportsStopReasonAbortedAndSettledAborted(t *testing.T) {
	r := load(t, "abort-turn")
	end, settled := r.one(t, "turn_end", ""), r.one(t, "agent_settled", "")
	msg := endMessage(t, r, end)
	if len(msg.RawStopReason) != 0 {
		t.Fatalf("abort-turn frame %d: turn_end.message.rawStopReason = %s, want the key absent", end, msg.RawStopReason)
	}
	eq(t, "abort-turn turn_end.message.stopReason", msg.StopReason, "aborted")
	if !strings.Contains(string(r.frames[settled].Raw), `"aborted":true`) {
		t.Fatalf("abort-turn frame %d: agent_settled.aborted is missing or not true: %s", settled, r.frames[settled].Raw)
	}
	eq(t, "abort-turn agent_settled.aborted", r.frames[settled].Aborted, true)
}

func TestReplayAbortTurnAnswersAbortAfterAgentSettled(t *testing.T) {
	r := load(t, "abort-turn")
	settled, abort, probe4 := r.one(t, "agent_settled", ""), r.one(t, "response", "probe-3"), r.one(t, "response", "probe-4")
	ab := r.frames[abort]
	eq(t, "probe-3.command", ab.Command, "abort")
	eq(t, "probe-3.success", ab.Success, true)
	if len(ab.Data) != 0 {
		t.Fatalf("abort-turn frame %d: abort response carries data %s, want the key absent", abort, ab.Data)
	}
	eq(t, "probe-4.command", r.frames[probe4].Command, "get_state")
	eq(t, "probe-4.data.isStreaming", r.frames[probe4].State.IsStreaming, false)
	eq(t, "probe-4.data.messageCount", r.frames[probe4].State.MessageCount, 3)
	if abort <= settled || probe4 <= settled {
		t.Fatalf("abort-turn: abort frame %d / get_state frame %d not after agent_settled frame %d", abort, probe4, settled)
	}
}

func TestReplayAbortTurnRecoversWithGetStateAfterSettlement(t *testing.T) {
	r := load(t, "abort-turn")
	settled, abort, probe4 := r.one(t, "agent_settled", ""), r.one(t, "response", "probe-3"), r.one(t, "response", "probe-4")
	before, after := r.frames[r.one(t, "response", "probe-1")].State, r.frames[probe4].State
	if before.SessionID == "" || after.SessionID != before.SessionID {
		t.Fatalf("abort-turn: sessionId %q -> %q, want one unchanged non-empty id (a restart?)", before.SessionID, after.SessionID)
	}
	eq(t, "probe-4.success", r.frames[probe4].Success, true)
	eq(t, "probe-4.data.isStreaming", after.IsStreaming, false)
	if probe4 <= settled || probe4 <= abort {
		t.Fatalf("abort-turn: get_state frame %d not after agent_settled %d and abort %d", probe4, settled, abort)
	}
}

func TestReplayFramesContainOnlyKnownTypesAndTallyUnknown(t *testing.T) {
	known := map[string]bool{"response": true, "agent_start": true, "turn_start": true,
		"message_start": true, "message_update": true, "message_end": true, "turn_end": true,
		"agent_end": true, "agent_settled": true, "extension_ui_request": true, "extension_error": true}
	for name, want := range map[string]int{"normal-turn": 23, "abort-turn": 26} {
		r := load(t, name)
		if len(r.frames) != want {
			t.Fatalf("%s: %d frames, want the recorded %d (re-recorded fixture?)", name, len(r.frames), want)
		}
		tally, first := map[string]int{}, map[string]int{}
		for i, f := range r.frames {
			if _, ok := known[f.Type]; !ok {
				tally[f.Type]++
				if _, seen := first[f.Type]; !seen {
					first[f.Type] = i
				}
			}
		}
		if len(tally) != 0 {
			t.Fatalf("%s: unknown types counted %v (first frame each in %v)", name, tally, first)
		}
	}
}

var (
	// homePath catches a home-directory-shaped absolute path; the fixtures carry
	// the redacted $HOME/... form instead.
	homePath = regexp.MustCompile(`(^|[^A-Za-z0-9._-])/(root|home/[A-Za-z0-9._-]+|Users/[A-Za-z0-9._-]+)`)
	// leakyRedaction catches a [redacted] touching credential characters
	// (sk-[redacted], [redacted]xyz): the rest of the secret would stay visible.
	leakyRedaction = regexp.MustCompile(`[A-Za-z0-9_+.-]\[redacted\]|\[redacted\][A-Za-z0-9_+.-]`)
)

func TestReplayFixturesContainNoCredentialValuesOrHomePaths(t *testing.T) {
	if !leakyRedaction.MatchString("sk-[redacted]x") || leakyRedaction.MatchString(`"[redacted]"`) {
		t.Fatal("leakyRedaction cannot tell an adjacent leak from a bare marker")
	}
	for _, name := range []string{"normal-turn", "abort-turn"} {
		for _, kind := range []string{"frames", "requests"} {
			// lines() already failed any CR, a missing trailing LF or an empty file.
			for i, line := range lines(t, name, kind) {
				var v any
				if err := json.Unmarshal([]byte(line), &v); err != nil {
					t.Fatalf("%s.%s line %d: invalid JSON: %v", name, kind, i, err)
				}
				for _, bad := range []string{"/root", "sk-", "Bearer ", "apiKey", "password", "secret"} {
					if strings.Contains(line, bad) {
						t.Fatalf("%s.%s line %d: contains %q", name, kind, i, bad)
					}
				}
				if homePath.MatchString(line) || leakyRedaction.MatchString(line) {
					t.Fatalf("%s.%s line %d: home path or leaky [redacted]: %.160s", name, kind, i, line)
				}
			}
		}
	}
}
