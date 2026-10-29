package claude

// Permission answers (issue #53): claude's control_response dialect, the
// answer-once gate and the record-only default.
//
// The fake claude pushes one real can_use_tool control_request (helper_test.go's
// pushperm flag) and logs every stdin line verbatim (CLAUDE_HELPER_STDIN). That
// log is the only evidence of the exact bytes that reached the wire, so every
// `answered` record asserted below is backed by a line in it — a record is never
// taken as proof that claude was told anything.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

// ---- the ladder and the reply shapes (pure) ----------------------------------

func TestChoosePermissionLadder(t *testing.T) {
	cases := []struct {
		name    string
		options []permissionOption
		want    permissionOption
		ok      bool
	}{
		{
			name:    "allow_always after allow_once still wins",
			options: []permissionOption{{kind: "allow_once", id: "o1"}, {kind: "allow_always", id: "a1"}},
			want:    permissionOption{kind: "allow_always", id: "a1"},
			ok:      true,
		},
		{
			name:    "allow_always before allow_once wins too",
			options: []permissionOption{{kind: "allow_always", id: "a1"}, {kind: "allow_once", id: "o1"}},
			want:    permissionOption{kind: "allow_always", id: "a1"},
			ok:      true,
		},
		{
			name:    "a single allow_once is the allow tier",
			options: []permissionOption{{kind: "reject", id: "r1"}, {kind: "allow_once", id: "o1"}},
			want:    permissionOption{kind: "allow_once", id: "o1"},
			ok:      true,
		},
		{
			name:    "the first of two allow_always wins",
			options: []permissionOption{{kind: "allow_always", id: "a1"}, {kind: "allow_always", id: "a2"}},
			want:    permissionOption{kind: "allow_always", id: "a1"},
			ok:      true,
		},
		{
			name:    "the first of two allow_once wins",
			options: []permissionOption{{kind: "allow_once", id: "o1"}, {kind: "allow_once", id: "o2"}},
			want:    permissionOption{kind: "allow_once", id: "o1"},
			ok:      true,
		},
		{
			name:    "only reject options: nothing to allow",
			options: []permissionOption{{kind: "reject", id: "r1"}, {kind: "reject_always", id: "r2"}},
			want:    permissionOption{},
			ok:      false,
		},
		{name: "no options at all", options: nil, want: permissionOption{}, ok: false},
		{name: "an empty list", options: []permissionOption{}, want: permissionOption{}, ok: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := choosePermission(c.options)
			if ok != c.ok || got != c.want {
				t.Fatalf("choosePermission(%v) = %+v, %v; want %+v, %v", c.options, got, ok, c.want, c.ok)
			}
		})
	}
}

func TestPermissionReplyShapes(t *testing.T) {
	input := map[string]any{"command": "echo hi"}
	cases := []struct {
		name    string
		request map[string]any
		want    string // the reply as JSON; "" means no answer
	}{
		{
			name: "an options list picks the broadest allow and names it back",
			request: map[string]any{"subtype": "can_use_tool", "input": input, "options": []any{
				map[string]any{"kind": "reject", "id": "r1"},
				map[string]any{"kind": "allow_once", "id": "o1"},
				map[string]any{"kind": "allow_always", "id": "a1"},
			}},
			want: `{"behavior":"allow","optionId":"a1","updatedInput":{"command":"echo hi"}}`,
		},
		{
			name: "an options list that offers only a reject is not answered",
			request: map[string]any{"subtype": "can_use_tool", "input": input, "options": []any{
				map[string]any{"kind": "reject", "id": "r1"},
			}},
			want: "",
		},
		{
			name:    "an empty options list offers nothing to allow",
			request: map[string]any{"subtype": "can_use_tool", "input": input, "options": []any{}},
			want:    "",
		},
		{
			name:    "claude's shape: no options list is the one-shot allow",
			request: map[string]any{"subtype": "can_use_tool", "input": input},
			want:    `{"behavior":"allow","updatedInput":{"command":"echo hi"}}`,
		},
		{
			name:    "no input at all: a bare allow",
			request: map[string]any{"subtype": "can_use_tool"},
			want:    `{"behavior":"allow"}`,
		},
		{
			name:    "a non-object input is not echoed back",
			request: map[string]any{"subtype": "can_use_tool", "input": "echo hi"},
			want:    `{"behavior":"allow"}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reply, ok := permissionReply(c.request)
			if c.want == "" {
				if ok {
					t.Fatalf("permissionReply = %v, true; want no answer (the cancelled tier)", reply)
				}
				return
			}
			if !ok {
				t.Fatalf("permissionReply = no answer; want %s", c.want)
			}
			if got := permJSON(t, reply); got != c.want {
				t.Fatalf("permissionReply = %s; want %s", got, c.want)
			}
		})
	}
}

// ---- against the fake claude: one real can_use_tool request -------------------

// permSession opens the fake claude with pushperm on and its stdin log on.
// Nothing is written before the first prompt, so the log starts with the prompt
// line and then holds exactly what an answer writes.
func permSession(t *testing.T, so SessionOptions) (*Session, string) {
	t.Helper()
	return helperStdin(t, "chat", so, "pushperm")
}

// pushPermission sends the prompt that makes the fake emit its one
// can_use_tool control_request (id permRequestID) and returns once the toApp
// request record is in the stream: the point where the adapter has registered
// it. The fake holds the turn open until the host answers.
func pushPermission(t *testing.T, s *Session) {
	t.Helper()
	if out, err := s.Prompt(ctxT(t), "run a quick turn", oarlock.PromptOptions{}); err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v, %v", out, err)
	}
	waitRecords(t, s, "the runtime's can_use_tool request", func(rs []oarlock.Record) bool {
		return permRequestRecord(rs) != nil
	})
}

// permRequestRecord is the runtime→app request record the fake's pushperm flag
// makes: a toApp native request for permRequestID.
func permRequestRecord(rs []oarlock.Record) *oarlock.Record {
	for i := range rs {
		r := &rs[i]
		if r.Kind == oarlock.KindRequest && r.ID == permRequestID && r.Direction == oarlock.ToApp &&
			r.Request != nil && r.Request.NativeType == "can_use_tool" {
			return r
		}
	}
	return nil
}

// permAnsweredRecords is every `answered` response recorded against id.
func permAnsweredRecords(rs []oarlock.Record, id string) []oarlock.Record {
	var out []oarlock.Record
	for _, r := range rs {
		if r.Kind == oarlock.KindResponse && r.RequestID == id && r.Response != nil && r.Response.Kind == oarlock.ResponseAnswered {
			out = append(out, r)
		}
	}
	return out
}

// permControlLines is the raw stdin lines that are control_response frames.
func permControlLines(lines []string) []string {
	var out []string
	for _, l := range lines {
		if m := sj.ParseRec([]byte(l)); sj.Str(m["type"]) == "control_response" {
			out = append(out, l)
		}
	}
	return out
}

// waitPermControlLines waits until the fake has received at least n
// control_response lines and returns them verbatim.
func waitPermControlLines(t *testing.T, stdinPath string, n int) []string {
	t.Helper()
	waitStdin(t, stdinPath, "the control_response line", func(frames []sj.Rec) bool {
		count := 0
		for _, m := range frames {
			if sj.Str(m["type"]) == "control_response" {
				count++
			}
		}
		return count >= n
	})
	return permControlLines(stdinRawLines(t, stdinPath))
}

// permJSON is a reply as JSON, for whole-shape assertions.
func permJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %v: %v", v, err)
	}
	return string(b)
}

// TestAnswerWritesTheLineAndRecordsOneAnswer is the core of §4a: Answer writes
// claude's own control_response line, and the record it writes afterwards carries
// exactly that reply.
func TestAnswerWritesTheLineAndRecordsOneAnswer(t *testing.T) {
	s, stdinPath := permSession(t, SessionOptions{SessionID: "sess-perm-answer"})
	pushPermission(t, s)

	result := map[string]any{"behavior": "allow", "updatedInput": map[string]any{"command": "echo hi"}}
	if err := s.Answer(ctxT(t), permRequestID, result); err != nil {
		t.Fatalf("Answer = %v", err)
	}

	want := `{"type":"control_response","response":{"subtype":"success","request_id":"perm-1","response":{"behavior":"allow","updatedInput":{"command":"echo hi"}}}}`
	lines := waitPermControlLines(t, stdinPath, 1)
	if len(lines) != 1 {
		t.Fatalf("control_response lines on stdin = %d, want exactly 1: %v", len(lines), lines)
	}
	if lines[0] != want {
		t.Fatalf("stdin line =\n%s\nwant\n%s", lines[0], want)
	}

	rs := s.Records(oarlock.Cursor{})
	answered := permAnsweredRecords(rs, permRequestID)
	if len(answered) != 1 {
		t.Fatalf("answered records = %d, want exactly 1", len(answered))
	}
	if got, wantNative := string(answered[0].Response.Native), permJSON(t, result); got != wantNative {
		t.Fatalf("answered Native = %s, want the marshalled result %s", got, wantNative)
	}
	if answered[0].RequestID != permRequestID {
		t.Fatalf("answered RequestID = %q, want %q", answered[0].RequestID, permRequestID)
	}
	req := permRequestRecord(rs)
	if req == nil {
		t.Fatal("the request record disappeared")
	}
	if req.Seq >= answered[0].Seq {
		t.Fatalf("the answered record (seq %d) must follow its request (seq %d)", answered[0].Seq, req.Seq)
	}
}

// A control request is answerable once: the second answer is refused before
// anything reaches the wire.
func TestAnswerTwiceIsAlreadyAnsweredAndWritesNothing(t *testing.T) {
	s, stdinPath := permSession(t, SessionOptions{SessionID: "sess-perm-twice"})
	pushPermission(t, s)
	if err := s.Answer(ctxT(t), permRequestID, map[string]any{"behavior": "allow"}); err != nil {
		t.Fatalf("first Answer = %v", err)
	}
	waitPermControlLines(t, stdinPath, 1)
	before := len(stdinRawLines(t, stdinPath))

	if err := s.Answer(ctxT(t), permRequestID, map[string]any{"behavior": "allow"}); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("second Answer = %v, want ErrAlreadyAnswered", err)
	}
	time.Sleep(100 * time.Millisecond) // nothing more may reach the wire
	if after := len(stdinRawLines(t, stdinPath)); after != before {
		t.Fatalf("the refused second answer wrote to stdin: %d -> %d lines", before, after)
	}
	if n := len(permAnsweredRecords(s.Records(oarlock.Cursor{}), permRequestID)); n != 1 {
		t.Fatalf("answered records = %d, want exactly 1", n)
	}
}

func TestAnswerUnknownRequestIsUnknownAndWritesNothing(t *testing.T) {
	if ErrUnknownRequest == ErrAlreadyAnswered {
		t.Fatal("the two sentinels must be distinct")
	}
	for want, err := range map[string]error{
		"claude: no unanswered runtime request with that id": ErrUnknownRequest,
		"claude: the runtime request is already answered":    ErrAlreadyAnswered,
	} {
		if err.Error() != want {
			t.Fatalf("sentinel = %q, want %q", err.Error(), want)
		}
	}

	s, stdinPath := permSession(t, SessionOptions{SessionID: "sess-perm-unknown"})
	pushPermission(t, s)
	if err := s.Answer(ctxT(t), "perm-nope", map[string]any{"behavior": "allow"}); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("Answer(unknown id) = %v, want ErrUnknownRequest", err)
	}
	if lines := permControlLines(stdinRawLines(t, stdinPath)); len(lines) != 0 {
		t.Fatalf("an unknown id wrote %d control_response lines: %v", len(lines), lines)
	}
	if n := len(permAnsweredRecords(s.Records(oarlock.Cursor{}), "perm-nope")); n != 0 {
		t.Fatalf("an unknown id recorded %d answers", n)
	}
	// A cancelled context is the first gate and writes nothing either.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Answer(cancelled, permRequestID, map[string]any{"behavior": "allow"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Answer(cancelled ctx) = %v, want context.Canceled", err)
	}
	if lines := permControlLines(stdinRawLines(t, stdinPath)); len(lines) != 0 {
		t.Fatalf("a cancelled answer wrote %d control_response lines: %v", len(lines), lines)
	}
	// ... and neither consumed the request: it is still answerable.
	if err := s.Answer(ctxT(t), permRequestID, map[string]any{"behavior": "allow"}); err != nil {
		t.Fatalf("the refused answers consumed the request: %v", err)
	}
}

func TestAnswerAfterDisposeErrorsAndRecordsNothing(t *testing.T) {
	s, stdinPath := permSession(t, SessionOptions{SessionID: "sess-perm-disposed"})
	pushPermission(t, s)
	if err := s.Dispose(ctxT(t)); err != nil {
		t.Fatalf("Dispose = %v", err)
	}
	before := len(stdinRawLines(t, stdinPath))
	err := s.Answer(ctxT(t), permRequestID, map[string]any{"behavior": "allow"})
	if err == nil {
		t.Fatal("Answer after Dispose = nil, want the kernel's unreachable error")
	}
	if errors.Is(err, ErrUnknownRequest) || errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("Answer after Dispose = %v, want the exit/dispose error, not a pending-request sentinel", err)
	}
	time.Sleep(100 * time.Millisecond)
	if after := len(stdinRawLines(t, stdinPath)); after != before {
		t.Fatalf("an answer after Dispose wrote to stdin: %d -> %d lines", before, after)
	}
	if n := len(permAnsweredRecords(s.Records(oarlock.Cursor{}), permRequestID)); n != 0 {
		t.Fatalf("an answer after Dispose recorded %d answers", n)
	}
}

// An answer that cannot reach the wire records nothing — and does not consume
// the request either: a marshal failure rolls the gate back.
func TestAnswerThatCannotReachTheWireRecordsNothing(t *testing.T) {
	s, stdinPath := permSession(t, SessionOptions{SessionID: "sess-perm-nowire"})
	pushPermission(t, s)

	if err := s.Answer(ctxT(t), permRequestID, make(chan int)); err == nil {
		t.Fatal("Answer with an unmarshalable result = nil, want a marshal error")
	}
	if lines := permControlLines(stdinRawLines(t, stdinPath)); len(lines) != 0 {
		t.Fatalf("a failed marshal wrote %d control_response lines: %v", len(lines), lines)
	}
	if n := len(permAnsweredRecords(s.Records(oarlock.Cursor{}), permRequestID)); n != 0 {
		t.Fatalf("an answer that never reached the wire recorded %d answers", n)
	}

	// The rollback leaves the request answerable, and then the line and the
	// record appear together.
	if err := s.Answer(ctxT(t), permRequestID, map[string]any{"behavior": "allow"}); err != nil {
		t.Fatalf("the failed answer consumed the request: %v", err)
	}
	if lines := waitPermControlLines(t, stdinPath, 1); len(lines) != 1 {
		t.Fatalf("control_response lines = %d, want exactly 1: %v", len(lines), lines)
	}
	if n := len(permAnsweredRecords(s.Records(oarlock.Cursor{}), permRequestID)); n != 1 {
		t.Fatalf("answered records = %d, want exactly 1", n)
	}
}

// The default policy answers nothing: the request stays a visible, unanswered
// toApp record and no control_response is invented for it.
func TestRecordOnlyLeavesThePermissionUnanswered(t *testing.T) {
	s, stdinPath := permSession(t, SessionOptions{SessionID: "sess-perm-record-only"}) // zero value = PermissionRecordOnly
	pushPermission(t, s)

	// The policy decision is made at ingress, next to the request record, so a
	// short settle is enough to see that no answer is coming.
	time.Sleep(200 * time.Millisecond)
	if n := len(permAnsweredRecords(s.Records(oarlock.Cursor{}), permRequestID)); n != 0 {
		t.Fatalf("the record-only default answered %d times", n)
	}
	if lines := permControlLines(stdinRawLines(t, stdinPath)); len(lines) != 0 {
		t.Fatalf("the record-only default wrote %d control_response lines: %v", len(lines), lines)
	}
	if permRequestRecord(s.Records(oarlock.Cursor{})) == nil {
		t.Fatal("the record-only request is not in the stream; it must stay visible")
	}

	// Silence is the honest cancelled tier, not a dropped request: the host can
	// still answer it, and that answer is the first thing claude hears.
	if err := s.Answer(ctxT(t), permRequestID, map[string]any{"behavior": "allow"}); err != nil {
		t.Fatalf("the default policy left the request unanswerable: %v", err)
	}
	if lines := waitPermControlLines(t, stdinPath, 1); len(lines) != 1 {
		t.Fatalf("control_response lines = %d, want exactly 1: %v", len(lines), lines)
	}
	if n := len(permAnsweredRecords(s.Records(oarlock.Cursor{}), permRequestID)); n != 1 {
		t.Fatalf("answered records after the host's answer = %d, want exactly 1", n)
	}
}

// PermissionAllow answers the request once, with the one-shot allow, through the
// same Answer path.
func TestPermissionAllowAnswersOnce(t *testing.T) {
	s, stdinPath := permSession(t, SessionOptions{SessionID: "sess-perm-allow", Permissions: PermissionAllow})
	pushPermission(t, s)

	rs := waitRecords(t, s, "the adapter's answer", func(rs []oarlock.Record) bool {
		return len(permAnsweredRecords(rs, permRequestID)) >= 1
	})
	answered := permAnsweredRecords(rs, permRequestID)
	if len(answered) != 1 {
		t.Fatalf("answered records = %d, want exactly 1", len(answered))
	}
	// claude's real shape carries no options list, so the allow tier is the
	// one-shot allow and the recorded Native is exactly what went out.
	if got, want := string(answered[0].Response.Native), `{"behavior":"allow","updatedInput":{"command":"echo hi"}}`; got != want {
		t.Fatalf("answered Native = %s, want %s", got, want)
	}
	want := `{"type":"control_response","response":{"subtype":"success","request_id":"perm-1","response":{"behavior":"allow","updatedInput":{"command":"echo hi"}}}}`
	if lines := waitPermControlLines(t, stdinPath, 1); len(lines) != 1 || lines[0] != want {
		t.Fatalf("stdin control_response lines = %v, want exactly [%s]", lines, want)
	}

	// The policy used the same gate: a host answer afterwards is refused.
	if err := s.Answer(ctxT(t), permRequestID, map[string]any{"behavior": "allow"}); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("Answer after the policy's answer = %v, want ErrAlreadyAnswered", err)
	}

	// The answered request released the turn the fake held open, and the answer
	// stayed single.
	rs = waitRecords(t, s, "the turn the answer released", turnsEnded(1))
	if n := len(permAnsweredRecords(rs, permRequestID)); n != 1 {
		t.Fatalf("answered records = %d, want exactly 1", n)
	}
	if lines := permControlLines(stdinRawLines(t, stdinPath)); len(lines) != 1 {
		t.Fatalf("control_response lines on stdin = %d, want exactly 1: %v", len(lines), lines)
	}
}

// The default policy is the zero value and the only other value is "allow": the
// rest is refused by NewSession, not silently treated as record-only.
func TestPermissionPolicyValues(t *testing.T) {
	if PermissionRecordOnly != PermissionPolicy("") {
		t.Fatalf("PermissionRecordOnly = %q, want the zero value", PermissionRecordOnly)
	}
	if PermissionAllow != PermissionPolicy("allow") {
		t.Fatalf("PermissionAllow = %q, want %q", PermissionAllow, "allow")
	}
}
