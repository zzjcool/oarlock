package cursor

// Unit tests for the projection hooks: the assistant accumulator, the top-level
// thinking/tool_call frames, the result classifier and its usage. The recorded
// traces are pinned frame by frame in fixture_test.go; the cases here are the
// shapes those two traces do not happen to contain (a failure result, an
// absent usage count, a subtype nobody declared).

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

// foldLines folds raw frames, in order, through f.
func foldLines(t *testing.T, f *sj.Folder, lines ...string) []sj.Frame {
	t.Helper()
	out := make([]sj.Frame, 0, len(lines))
	for _, line := range lines {
		out = append(out, f.Fold([]byte(line)))
	}
	return out
}

// assistantFrame is one assistant frame stating one text block.
func assistantTextFrame(text string) string {
	return `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":` +
		strconv.Quote(text) + `}]}}`
}

func kindsOf(f sj.Frame) []oarlock.EventKind {
	var out []oarlock.EventKind
	for _, e := range f.Events {
		out = append(out, e.Kind)
	}
	return out
}

func textDeltas(f sj.Frame) []string {
	var out []string
	for _, e := range f.Events {
		if e.Kind == oarlock.EventTextDelta {
			out = append(out, e.Text)
		}
	}
	return out
}

func oneEvent(t *testing.T, f sj.Frame) oarlock.Event {
	t.Helper()
	if len(f.Events) != 1 {
		t.Fatalf("%s: events %v, want exactly one", f.Type, kindsOf(f))
	}
	return f.Events[0]
}

// ---- assistant text: the per-lane accumulator -----------------------------------

func TestAssistantTextAccumulator(t *testing.T) {
	p := newProfile()
	f := sj.NewFolder(p)
	frames := foldLines(t, f,
		assistantTextFrame("> preamble\n\n"),        // first snapshot of the turn
		assistantTextFrame("ok"),                    // a new, uncorrelated snapshot
		assistantTextFrame("> preamble\n\nok"),      // cumulative: repeats the turn
		assistantTextFrame("ok"),                    // the tail of what was emitted: nothing
		assistantTextFrame("> preamble"),            // retraction: only text already emitted
		assistantTextFrame("> preamble\n\nok more"), // extends the emitted text: suffix only
		assistantTextFrame("ok more"),               // the tail of what was emitted: nothing
		assistantTextFrame("a second message"),      // new text, uncorrelated: in full
		assistantTextFrame("a second message"),      // identical repeat: nothing
	)
	want := [][]string{
		{"> preamble\n\n"},
		{"ok"},
		nil,
		nil,
		nil,
		{" more"},
		nil,
		{"a second message"},
		nil,
	}
	for i := range want {
		if got := textDeltas(frames[i]); !reflect.DeepEqual(got, want[i]) {
			t.Fatalf("frame %d text = %#v, want %#v", i, got, want[i])
		}
	}

	// A result ends the turn: the next turn's first snapshot is emitted in
	// full even though it repeats the previous turn's text.
	f.Fold([]byte(`{"type":"result","subtype":"success","is_error":false}`))
	next := f.Fold([]byte(assistantTextFrame("> preamble\n\nok more")))
	if got := textDeltas(next); !reflect.DeepEqual(got, []string{"> preamble\n\nok more"}) {
		t.Fatalf("after result text = %#v, want the whole snapshot", got)
	}

	// A new system/init also starts from no emitted text.
	f.Fold([]byte(`{"type":"system","subtype":"init","model":"Auto Balance"}`))
	again := f.Fold([]byte(assistantTextFrame("> preamble\n\nok more")))
	if got := textDeltas(again); !reflect.DeepEqual(got, []string{"> preamble\n\nok more"}) {
		t.Fatalf("after init text = %#v, want the whole snapshot", got)
	}
}

func TestAssistantContentBlocks(t *testing.T) {
	f := sj.NewFolder(newProfile())
	frames := foldLines(t, f,
		// output_text is cursor's other name for assistant text.
		`{"type":"assistant","message":{"content":[{"type":"output_text","text":"hi"}]}}`,
		// A thinking block inside a frame is reasoning, stated as-is.
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"why"}]}}`,
		// A tool_use block opens a call with the compact JSON of its input.
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call-1","name":"shell","input":{"command":"echo hi"}}]}}`,
		// A block type nobody declared states no event and is not invented.
		`{"type":"assistant","message":{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}}`,
		// Blocks keep their order: reasoning before the text it precedes.
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"t"},{"type":"text","text":"answer"}]}}`,
	)
	if got := textDeltas(frames[0]); !reflect.DeepEqual(got, []string{"hi"}) {
		t.Fatalf("output_text = %#v", got)
	}
	if e := oneEvent(t, frames[1]); e.Kind != oarlock.EventReasoning || e.Text != "why" {
		t.Fatalf("thinking block = %+v", e)
	}
	if e := oneEvent(t, frames[2]); e.Kind != oarlock.EventToolCallStarted || e.CallID != "call-1" ||
		e.Tool != "shell" || e.Input != `{"command":"echo hi"}` {
		t.Fatalf("tool_use block = %+v", e)
	}
	if got := kindsOf(frames[3]); got != nil {
		t.Fatalf("undeclared block type produced %v", got)
	}
	if got := kindsOf(frames[4]); !reflect.DeepEqual(got, []oarlock.EventKind{oarlock.EventReasoning, oarlock.EventTextDelta}) {
		t.Fatalf("mixed block order = %v", got)
	}
}

// ---- top-level frames: thinking and tool_call -----------------------------------

func TestThinkingFrames(t *testing.T) {
	p := newProfile()
	f := sj.NewFolder(p)
	frames := foldLines(t, f,
		`{"type":"thinking","subtype":"delta","text":"The"}`,
		`{"type":"thinking","subtype":"completed"}`,
		`{"type":"thinking","subtype":"segment_text","text":"x"}`,
	)
	if e := oneEvent(t, frames[0]); e.Kind != oarlock.EventReasoning || e.Text != "The" {
		t.Fatalf("thinking/delta = %+v", e)
	}
	if got := kindsOf(frames[1]); got != nil {
		t.Fatalf("thinking/completed produced %v", got)
	}
	// An unknown subtype is never absorbed: no event, but counted.
	if got := kindsOf(frames[2]); got != nil {
		t.Fatalf("thinking/segment_text produced %v", got)
	}
	if got := p.unknownSubtypes(); !reflect.DeepEqual(got, map[string]int{"thinking/segment_text": 1}) {
		t.Fatalf("unknownSubtypes = %v", got)
	}
	// thinking is a declared vendor type: the folder does not tally it.
	if got := f.Unknown(); len(got) != 0 {
		t.Fatalf("folder tallied declared types: %v", got)
	}
}

func TestToolCallProjection(t *testing.T) {
	p := newProfile()
	f := sj.NewFolder(p)
	frames := foldLines(t, f,
		// started: the call id is the first line of the packed pair, the tool is
		// the payload key minus ToolCall, the input is the compact JSON of args.
		`{"type":"tool_call","subtype":"started","call_id":"call-ef8a-0\nfc_5e7a_0",
		  "tool_call":{"shellToolCall":{"args":{"command":"echo hi","timeout":30000},"description":"Print hi"},
		  "toolCallId":"call-ef8a-0\nfc_5e7a_0"}}`,
		// completed, success: the runtime's own ok, the command's output and the
		// exit code it reported.
		`{"type":"tool_call","subtype":"completed","call_id":"call-ef8a-0",
		  "tool_call":{"shellToolCall":{"args":{"command":"echo hi"},
		  "result":{"success":{"exitCode":0,"stdout":"hi\n","stderr":""},"isBackground":false}}}}`,
		// stdout and a non-empty stderr are two parts, in that order. The exit
		// code is a number, but `success` is still the runtime's ok: the class
		// comes from the key, never from the code.
		`{"type":"tool_call","subtype":"completed","call_id":"call-2",
		  "tool_call":{"shellToolCall":{"result":{"success":{"exitCode":1,"stdout":"out\n","stderr":"boom\n"}}}}}`,
		// no exit code reported: the pointer stays nil and reported stays false.
		`{"type":"tool_call","subtype":"completed","call_id":"call-3",
		  "tool_call":{"shellToolCall":{"result":{"success":{"stdout":"hi\n"}}}}}`,
		// the failure shape: `success` absent, another result key present.
		`{"type":"tool_call","subtype":"completed","call_id":"call-4",
		  "tool_call":{"shellToolCall":{"result":{"failure":{"errorMessage":"permission denied"}}}}}`,
		// no result at all: the call is closed with no result stated.
		`{"type":"tool_call","subtype":"completed","call_id":"call-5",
		  "tool_call":{"shellToolCall":{"args":{"command":"echo hi"}}}}`,
		// an unknown subtype is counted, not turned into a result.
		`{"type":"tool_call","subtype":"progress","call_id":"call-5"}`,
	)

	started := oneEvent(t, frames[0])
	if started.Kind != oarlock.EventToolCallStarted || started.CallID != "call-ef8a-0" ||
		started.Tool != "shell" || started.Input != `{"command":"echo hi","timeout":30000}` {
		t.Fatalf("started = %+v", started)
	}

	ok := oneEvent(t, frames[1])
	if ok.Kind != oarlock.EventToolCallEnded || ok.CallID != "call-ef8a-0" || ok.Tool != "shell" ||
		ok.Result != oarlock.ToolOK {
		t.Fatalf("completed = %+v", ok)
	}
	if code, reported := ok.ReportedExitCode(); !reported || code != 0 {
		t.Fatalf("exit code = %d reported=%v, want 0 reported", code, reported)
	}
	if !reflect.DeepEqual(ok.Content, []oarlock.ToolOutputPart{{Type: "text", Text: "hi\n"}}) {
		t.Fatalf("content = %+v", ok.Content)
	}

	two := oneEvent(t, frames[2])
	if two.Result != oarlock.ToolOK {
		t.Fatalf("success with exit code 1 = %+v, want the runtime's ok", two)
	}
	if code, reported := two.ReportedExitCode(); !reported || code != 1 {
		t.Fatalf("exit code = %d reported=%v, want 1 reported", code, reported)
	}
	wantParts := []oarlock.ToolOutputPart{{Type: "text", Text: "out\n"}, {Type: "text", Text: "boom\n"}}
	if !reflect.DeepEqual(two.Content, wantParts) {
		t.Fatalf("content = %+v, want %+v", two.Content, wantParts)
	}

	unreported := oneEvent(t, frames[3])
	if unreported.ExitCode != nil || unreported.ExitCodeReported {
		t.Fatalf("absent exit code = %+v", unreported)
	}

	failed := oneEvent(t, frames[4])
	if failed.Result != oarlock.ToolFailed || failed.ExitCode != nil || failed.ExitCodeReported || failed.Content != nil {
		t.Fatalf("failure shape = %+v", failed)
	}

	norel := oneEvent(t, frames[5])
	if norel.CallID != "call-5" || norel.Result != oarlock.ToolUnknown ||
		norel.ExitCodeReported || norel.Content != nil {
		t.Fatalf("no result = %+v", norel)
	}

	if got := kindsOf(frames[6]); got != nil {
		t.Fatalf("tool_call/progress produced %v", got)
	}
	if got := p.unknownSubtypes(); !reflect.DeepEqual(got, map[string]int{"tool_call/progress": 1}) {
		t.Fatalf("unknownSubtypes = %v", got)
	}
	if got := f.Unknown(); len(got) != 0 {
		t.Fatalf("folder tallied declared types: %v", got)
	}
}

// ---- result: outcome ------------------------------------------------------------

const successResult = `{"type":"result","subtype":"success","is_error":false,"result":"ok"}`

func TestResultOutcome(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		aborted bool
		kind    oarlock.OutcomeKind
		failure oarlock.FailureClass
		reason  string
		status  int
	}{
		{name: "success", line: successResult, kind: oarlock.OutcomeCompleted},
		{
			name: "is_error",
			line: `{"type":"result","subtype":"success","is_error":true,"api_error_status":401,` +
				`"result":"API Error: 401 invalid api key"}`,
			kind: oarlock.OutcomeFailed, failure: oarlock.FailureAuth,
			reason: "API Error: 401 invalid api key", status: 401,
		},
		{
			name: "subtype error",
			line: `{"type":"result","subtype":"error","is_error":false,"error":"agent crashed"}`,
			kind: oarlock.OutcomeFailed, failure: oarlock.FailureUnknown, reason: "agent crashed",
		},
		{name: "aborted wins", line: successResult, aborted: true, kind: oarlock.OutcomeAborted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := sj.NewFolder(newProfile())
			if c.aborted {
				f.AbortRequested()
			}
			frame := f.Fold([]byte(c.line))
			if !frame.TurnEnded {
				t.Fatalf("%s: TurnEnded = false", frame.Type)
			}
			ended := oneEvent(t, frame)
			if ended.Kind != oarlock.EventTurnEnded || ended.Outcome == nil {
				t.Fatalf("events = %v", kindsOf(frame))
			}
			if got := ended.Outcome.Kind; got != c.kind {
				t.Fatalf("kind = %q, want %q", got, c.kind)
			}
			if got := ended.Outcome.Failure; got != c.failure {
				t.Fatalf("failure = %q, want %q", got, c.failure)
			}
			if got := ended.Outcome.Reason; got != c.reason {
				t.Fatalf("reason = %q, want %q", got, c.reason)
			}
			if got := ended.Outcome.Status; got != c.status {
				t.Fatalf("status = %d, want %d", got, c.status)
			}
		})
	}
}

func TestResultFailureClassifier(t *testing.T) {
	cases := []struct {
		name string
		line string
		want oarlock.FailureClass
	}{
		{"401", `{"type":"result","subtype":"error","api_error_status":401}`, oarlock.FailureAuth},
		{"403", `{"type":"result","subtype":"error","api_error_status":403}`, oarlock.FailureAuth},
		{"402", `{"type":"result","subtype":"error","api_error_status":402}`, oarlock.FailureQuota},
		{"404", `{"type":"result","subtype":"error","api_error_status":404}`, oarlock.FailureModelUnavail},
		{"413", `{"type":"result","subtype":"error","api_error_status":413}`, oarlock.FailureInputTooLarge},
		{"429", `{"type":"result","subtype":"error","api_error_status":429}`, oarlock.FailureRateLimited},
		{"503", `{"type":"result","subtype":"error","api_error_status":503}`, oarlock.FailureOverloaded},
		{"529", `{"type":"result","subtype":"error","api_error_status":529}`, oarlock.FailureOverloaded},
		{"unnamed status", `{"type":"result","subtype":"error","api_error_status":418}`, oarlock.FailureUnknown},
		{"no status", `{"type":"result","subtype":"error","result":"something broke"}`, oarlock.FailureUnknown},
		{
			// Prose is never the branch word, however loud it is.
			"prose only", `{"type":"result","subtype":"error","result":"429 rate limit exceeded"}`,
			oarlock.FailureUnknown,
		},
		{
			"error_type", `{"type":"result","subtype":"error","error_type":"rate_limit_error"}`,
			oarlock.FailureRateLimited,
		},
		{
			"error object type", `{"type":"result","subtype":"error","error":{"type":"model_not_found"}}`,
			oarlock.FailureModelUnavail,
		},
		{
			"error object code", `{"type":"result","subtype":"error","error":{"code":"insufficient_quota"}}`,
			oarlock.FailureQuota,
		},
		{
			"unknown token", `{"type":"result","subtype":"error","error_type":"something_new"}`,
			oarlock.FailureUnknown,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			frame := sj.NewFolder(newProfile()).Fold([]byte(c.line))
			outcome := oneEvent(t, frame).Outcome
			if outcome == nil || outcome.Kind != oarlock.OutcomeFailed {
				t.Fatalf("outcome = %+v", outcome)
			}
			if outcome.Failure != c.want {
				t.Fatalf("failure = %q, want %q", outcome.Failure, c.want)
			}
		})
	}
}

// ---- result: usage --------------------------------------------------------------

func TestResultUsage(t *testing.T) {
	t.Run("all counts stated", func(t *testing.T) {
		frame := sj.NewFolder(newProfile()).Fold([]byte(
			`{"type":"result","subtype":"success","is_error":false,` +
				`"usage":{"inputTokens":6514,"outputTokens":28,"cacheReadTokens":6016,"cacheWriteTokens":0}}`))
		if got := kindsOf(frame); !reflect.DeepEqual(got, []oarlock.EventKind{oarlock.EventTurnEnded, oarlock.EventUsage}) {
			t.Fatalf("events = %v, want turn_ended then usage", got)
		}
		tok := frame.Events[1].Usage.Tokens
		if tok == nil || tok.Input != 6514 || tok.Output != 28 {
			t.Fatalf("tokens = %+v", tok)
		}
		// A reported zero stays a pointer to zero: the key was stated.
		if tok.CacheRead == nil || *tok.CacheRead != 6016 {
			t.Fatalf("cacheRead = %v", tok.CacheRead)
		}
		if tok.CacheWrite == nil || *tok.CacheWrite != 0 {
			t.Fatalf("cacheWrite = %v, want a pointer to 0", tok.CacheWrite)
		}
		if frame.Events[1].Usage.Total != nil || frame.Events[1].Usage.Context != nil {
			t.Fatalf("usage states more than the frame: %+v", frame.Events[1].Usage)
		}
	})

	t.Run("cache counts absent", func(t *testing.T) {
		frame := sj.NewFolder(newProfile()).Fold([]byte(
			`{"type":"result","subtype":"success","usage":{"inputTokens":10,"outputTokens":20}}`))
		tok := frame.Events[1].Usage.Tokens
		if tok.Input != 10 || tok.Output != 20 {
			t.Fatalf("tokens = %+v", tok)
		}
		if tok.CacheRead != nil || tok.CacheWrite != nil {
			t.Fatalf("absent cache counts = %v/%v, want nil", tok.CacheRead, tok.CacheWrite)
		}
	})

	t.Run("no usage", func(t *testing.T) {
		frame := sj.NewFolder(newProfile()).Fold([]byte(successResult))
		if got := kindsOf(frame); !reflect.DeepEqual(got, []oarlock.EventKind{oarlock.EventTurnEnded}) {
			t.Fatalf("events = %v, want no usage event", got)
		}
	})

	t.Run("empty usage object", func(t *testing.T) {
		frame := sj.NewFolder(newProfile()).Fold([]byte(
			`{"type":"result","subtype":"success","usage":{}}`))
		if got := kindsOf(frame); !reflect.DeepEqual(got, []oarlock.EventKind{oarlock.EventTurnEnded}) {
			t.Fatalf("events = %v, want no usage event", got)
		}
	})
}

// ---- the vendor type table ------------------------------------------------------

func TestVendorTypesAreDeclared(t *testing.T) {
	want := []string{"thinking", "tool_call"}
	if got := newProfile().Types(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Types = %v, want %v", got, want)
	}
	// A declared type is known to the fold: folding one must not tally it as an
	// unknown top-level type (that tally would make the fixture guard noisy).
	for _, typ := range want {
		f := sj.NewFolder(newProfile())
		frame := f.Fold([]byte(`{"type":"` + typ + `","subtype":"started"}`))
		if !strings.HasPrefix(frame.Type, typ) {
			t.Fatalf("%s: frame type = %q", typ, frame.Type)
		}
		if got := f.Unknown(); len(got) != 0 {
			t.Fatalf("%s is declared but the folder tallied it: %v", typ, got)
		}
	}
}
