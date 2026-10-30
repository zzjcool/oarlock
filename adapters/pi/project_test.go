package pi

// Tests for the Pi projection (issue #11): the pure fold in project.go.
//
// The golden tests replay the raw frames recorded from a real Pi 1.4.0 session
// (testdata/pi/*.frames.jsonl, docs/runtimes/pi.md §4) and assert the events
// frame by frame. The same frames are then pushed through a real Session and
// the retained records are folded again from their own Native bytes, which is
// the replay path: fold(live) must equal fold(replay).
//
// Seq and coalescing (TestCoalesceTextIsAReadingOnly): the stream is one record
// per native frame with a dense Seq; coalesceText returns a merged READING in a
// new slice and never touches a record. Merging N consecutive pieces turns N
// events into 1 (events_after = events_before - (N-1)); the record count and
// every Seq are unchanged, and a merged event spans FirstSeq..Seq.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
	"github.com/zzjcool/oarlock/internal/kernel"
)

// ---- helpers ---------------------------------------------------------------

// pjFrames returns the raw frames of a recorded fixture, one per line.
func pjFrames(t *testing.T, name string) []json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "pi", name+".frames.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []json.RawMessage
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			out = append(out, json.RawMessage(append([]byte(nil), line...)))
		}
	}
	return out
}

// pjFold folds every frame in order from the zero state.
func pjFold(raws []json.RawMessage) (piState, [][]oarlock.Event) {
	var st piState
	out := make([][]oarlock.Event, len(raws))
	for i, raw := range raws {
		st, out[i] = foldPiEvent(st, raw)
	}
	return st, out
}

func pjKinds(evs []oarlock.Event) []oarlock.EventKind {
	var out []oarlock.EventKind
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func pjOne(t *testing.T, st piState, raw string) (piState, oarlock.Event) {
	t.Helper()
	next, evs := foldPiEvent(st, json.RawMessage(raw))
	if len(evs) != 1 {
		t.Fatalf("%s -> %d events %v, want 1", raw, len(evs), pjKinds(evs))
	}
	return next, evs[0]
}

func pjNone(t *testing.T, st piState, raw string) piState {
	t.Helper()
	next, evs := foldPiEvent(st, json.RawMessage(raw))
	if len(evs) != 0 {
		t.Fatalf("%s -> events %v, want none", raw, pjKinds(evs))
	}
	return next
}

func pjI64(p *int64) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprint(*p)
}

func pjTokens(t *testing.T, ev oarlock.Event, input, output int64, reasoning, cacheRead, cacheWrite string) {
	t.Helper()
	if ev.Kind != oarlock.EventUsage || ev.Usage == nil || ev.Usage.Tokens == nil {
		t.Fatalf("event = %+v, want usage with Tokens", ev)
	}
	if ev.Usage.Total != nil || ev.Usage.Context != nil {
		t.Fatalf("usage = %+v: Pi has no total beyond its one agent and a message carries no context", ev.Usage)
	}
	tk := ev.Usage.Tokens
	got := fmt.Sprintf("%d/%d/%s/%s/%s", tk.Input, tk.Output, pjI64(tk.Reasoning), pjI64(tk.CacheRead), pjI64(tk.CacheWrite))
	want := fmt.Sprintf("%d/%d/%s/%s/%s", input, output, reasoning, cacheRead, cacheWrite)
	if got != want {
		t.Fatalf("tokens in/out/reasoning/cacheRead/cacheWrite = %s, want %s", got, want)
	}
}

// pjRecords retains the fold's events on a real kernel, one record per frame.
func pjRecords(raws []json.RawMessage) []oarlock.Record {
	k := kernel.New("s", nil)
	var st piState
	for _, raw := range raws {
		var evs []oarlock.Event
		st, evs = foldPiEvent(st, raw)
		k.Frame(kernel.FrameBody{Native: raw, Events: evs}, kernel.RecordAt{})
	}
	return k.Records(oarlock.Cursor{})
}

// ---- 1. golden: normal turn --------------------------------------------------

func TestGoldenNormalTurn(t *testing.T) {
	raws := pjFrames(t, "normal-turn")
	if len(raws) != 23 {
		t.Fatalf("fixture has %d frames, want 23", len(raws))
	}
	st, per := pjFold(raws)

	want := map[int][]oarlock.EventKind{
		4:  {oarlock.EventModel, oarlock.EventEffort}, // response(get_state)
		6:  {oarlock.EventTurnActive},                 // agent_start
		10: {oarlock.EventUserMessage},                // message_start user
		14: {oarlock.EventTextDelta},                  // text_delta "ready"
		16: {oarlock.EventUsage},                      // message_end assistant
		23: {oarlock.EventTurnEnded},                  // agent_settled
	}
	for i, evs := range per {
		if got, w := pjKinds(evs), want[i+1]; !reflect.DeepEqual(got, w) {
			t.Errorf("frame %d events = %v, want %v", i+1, got, w)
		}
	}

	if ev := per[3][0]; ev.Model != "any-endpoint/workbuddy/deepseek-v4.1-flash" {
		t.Errorf("model = %q", ev.Model)
	}
	if ev := per[3][1]; ev.Effort != "max" {
		t.Errorf("effort = %q", ev.Effort)
	}
	// response(prompt) is the control plane's: the pure fold reads nothing from it.
	if len(per[4]) != 0 {
		t.Errorf("response(prompt) produced %v", pjKinds(per[4]))
	}
	if ev := per[9][0]; ev.Text != "Reply with exactly one word: ready" || ev.Evidence != oarlock.EvidenceConversation {
		t.Errorf("user_message = %+v", ev)
	}
	if ev := per[13][0]; ev.Text != "ready" {
		t.Errorf("text_delta = %q", ev.Text)
	}
	// input 254 excludes the cache; cacheRead 256 stands as its own part and
	// counts back into Input; cacheWrite 0 and reasoning 0 were REPORTED.
	pjTokens(t, per[15][0], 510, 2, "0", "256", "0")
	out := per[22][0].Outcome
	if out == nil || out.Kind != oarlock.OutcomeCompleted || out.Failure != "" {
		t.Errorf("outcome = %+v, want completed", out)
	}
	if n := countKind(per, oarlock.EventTurnEnded); n != 1 {
		t.Errorf("turn_ended count = %d, want exactly 1 (turn_end and agent_end are not turn ends)", n)
	}
	if len(st.unknown) != 0 {
		t.Errorf("unknown tally = %v, want empty", st.unknown)
	}
}

func countKind(per [][]oarlock.Event, kind oarlock.EventKind) (n int) {
	for _, evs := range per {
		for _, e := range evs {
			if e.Kind == kind {
				n++
			}
		}
	}
	return n
}

// ---- 1. golden: abort turn ----------------------------------------------------

func TestGoldenAbortTurn(t *testing.T) {
	raws := pjFrames(t, "abort-turn")
	if len(raws) != 26 {
		t.Fatalf("fixture has %d frames, want 26", len(raws))
	}
	st, per := pjFold(raws)

	want := map[int][]oarlock.EventKind{
		4:  {oarlock.EventModel, oarlock.EventEffort},
		6:  {oarlock.EventTurnActive},
		10: {oarlock.EventUserMessage},
		14: {oarlock.EventReasoning},
		15: {oarlock.EventReasoning},
		16: {oarlock.EventReasoning},
		17: {oarlock.EventUsage},
		24: {oarlock.EventTurnEnded},
		26: {oarlock.EventModel, oarlock.EventEffort},
	}
	for i, evs := range per {
		if got, w := pjKinds(evs), want[i+1]; !reflect.DeepEqual(got, w) {
			t.Errorf("frame %d events = %v, want %v", i+1, got, w)
		}
	}
	var pieces []string
	for _, i := range []int{13, 14, 15} {
		pieces = append(pieces, per[i][0].Text)
	}
	if strings.Join(pieces, "|") != "We| need| answer" {
		t.Errorf("reasoning pieces = %q", pieces)
	}
	// All-zero usage was REPORTED and stays zero; the absent reasoning key stays nil.
	pjTokens(t, per[16][0], 0, 0, "nil", "0", "0")
	out := per[23][0].Outcome
	if out == nil || out.Kind != oarlock.OutcomeAborted || out.Failure != "" || out.Reason != "" {
		t.Errorf("outcome = %+v, want a bare aborted (the stopReason and errorMessage are the abort's own words)", out)
	}
	if len(per[24]) != 0 {
		t.Errorf("response(abort) produced %v: a late ack is a frame, never a second fact", pjKinds(per[24]))
	}
	if n := countKind(per, oarlock.EventTurnEnded); n != 1 {
		t.Errorf("turn_ended count = %d, want 1", n)
	}
	if len(st.unknown) != 0 {
		t.Errorf("unknown tally = %v, want empty", st.unknown)
	}
}

// ---- 2. live == replay ---------------------------------------------------------

func TestLiveEqualsReplay(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome oarlock.OutcomeKind
	}{
		{"normal-turn", oarlock.OutcomeCompleted},
		{"abort-turn", oarlock.OutcomeAborted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer watchdog(t, 30*time.Second)()
			raws := pjFrames(t, tc.name)
			sess, conn := newCoreSession(t, time.Minute)
			for _, raw := range raws {
				conn.push(string(raw))
			}
			coreWait(t, "every frame recorded", func() bool { return len(sess.Records(oarlock.Cursor{})) >= len(raws) })
			// A settlement asks Pi for its context; the fake Conn is unscripted,
			// so that read fails and nothing more enters the stream.
			time.Sleep(50 * time.Millisecond)
			records := sess.Records(oarlock.Cursor{})
			if len(records) != len(raws) {
				t.Fatalf("%d records for %d frames", len(records), len(raws))
			}
			assertDense(t, records)

			// Replay: the same function over the retained Native bytes.
			var st piState
			for i, r := range records {
				if !bytes.Equal(r.Native, raws[i]) {
					t.Fatalf("record %d Native is not the fixture line verbatim", i+1)
				}
				var replayed []oarlock.Event
				st, replayed = foldPiEvent(st, r.Native)
				if len(replayed) == 0 && len(r.Events) == 0 {
					continue
				}
				if !reflect.DeepEqual(replayed, r.Events) {
					t.Fatalf("record %d: replay %+v != live %+v", i+1, replayed, r.Events)
				}
			}

			status := fold.StatusOf(records).Value
			if status.Kind != fold.StatusIdle || status.LastTurnOutcome == nil || status.LastTurnOutcome.Kind != tc.outcome {
				t.Fatalf("status = %+v, want idle with outcome %s", status, tc.outcome)
			}
			usage := fold.UsageOf(records).Value
			if usage.Total == nil || usage.ByAgent != nil || usage.Unattributed != nil {
				t.Fatalf("usage = %+v: attribution none means Total from the root figure, no ByAgent, no Unattributed", usage)
			}
			if tc.name == "normal-turn" && (usage.Total.Input != 510 || usage.Total.Output != 2) {
				t.Fatalf("total = %+v, want 510 in / 2 out", *usage.Total)
			}
			if c := fold.ContextUsageOf(records).Value; c != nil {
				t.Fatalf("context = %+v, want nil: nothing reported it, and unreported is never zero", c)
			}
		})
	}
}

// ---- 3. context after the settlement ------------------------------------------

func TestContextAfterSettleComesFromGetSessionStats(t *testing.T) {
	run := func(t *testing.T, hook func() fakeReply) (*Session, *coreConn) {
		sess, conn := newCoreSession(t, time.Minute)
		conn.stats = hook
		conn.push(`{"type":"agent_start"}`)
		settle(conn, "stop", false)
		coreWait(t, "settled", func() bool { return countEvents(sess.Records(oarlock.Cursor{}), oarlock.EventTurnEnded) == 1 })
		return sess, conn
	}

	t.Run("reported", func(t *testing.T) {
		defer watchdog(t, 30*time.Second)()
		sess, conn := run(t, func() fakeReply {
			return fakeReply{success: true, data: `{"contextUsage":{"tokens":512,"contextWindow":1000000,"percent":0.0512}}`}
		})
		coreWait(t, "context", func() bool { return fold.ContextUsageOf(sess.Records(oarlock.Cursor{})).Value != nil })
		records := sess.Records(oarlock.Cursor{})
		c := fold.ContextUsageOf(records).Value
		if c.Usage.Tokens == nil || *c.Usage.Tokens != 512 || c.Usage.ContextWindow == nil || *c.Usage.ContextWindow != 1000000 ||
			c.Usage.Percent == nil || *c.Usage.Percent != 0.0512 {
			t.Fatalf("context = %+v", c.Usage)
		}
		var settledSeq uint64
		for _, r := range records {
			for _, e := range r.Events {
				if e.Kind == oarlock.EventTurnEnded {
					settledSeq = r.Seq
				}
			}
		}
		if c.Seq <= settledSeq {
			t.Fatalf("context came from Seq %d, want a frame after the agent_settled at %d", c.Seq, settledSeq)
		}
		if r := records[c.Seq-1]; r.Kind != oarlock.KindFrame || !strings.Contains(string(r.Native), `"get_session_stats"`) {
			t.Fatalf("the context event sits in %+v, want the response FRAME of get_session_stats", r.Kind)
		}
		if n := conn.countSent("get_session_stats"); n != 1 {
			t.Fatalf("get_session_stats sent %d times, want 1", n)
		}
		assertDense(t, records)
	})

	t.Run("null after compaction stays nil", func(t *testing.T) {
		defer watchdog(t, 30*time.Second)()
		sess, _ := run(t, func() fakeReply {
			return fakeReply{success: true, data: `{"contextUsage":{"tokens":null,"contextWindow":1000000,"percent":null}}`}
		})
		coreWait(t, "context", func() bool { return fold.ContextUsageOf(sess.Records(oarlock.Cursor{})).Value != nil })
		c := fold.ContextUsageOf(sess.Records(oarlock.Cursor{})).Value
		if c.Usage.Tokens != nil || c.Usage.Percent != nil || c.Usage.ContextWindow == nil || *c.Usage.ContextWindow != 1000000 {
			t.Fatalf("context = %+v, want tokens/percent nil (unknown), never 0", c.Usage)
		}
	})

	t.Run("transport failure leaves it nil", func(t *testing.T) {
		defer watchdog(t, 30*time.Second)()
		sess, conn := run(t, func() fakeReply { return fakeReply{err: errors.New("boom")} })
		coreWait(t, "the read was attempted", func() bool { return conn.countSent("get_session_stats") == 1 })
		time.Sleep(50 * time.Millisecond)
		records := sess.Records(oarlock.Cursor{})
		if c := fold.ContextUsageOf(records).Value; c != nil {
			t.Fatalf("context = %+v, want nil after a failed read", c)
		}
		if got := fold.StatusOf(records).Value.Kind; got != fold.StatusIdle {
			t.Fatalf("status = %s: a failed context read must not disturb the session", got)
		}
	})

	t.Run("refusal is a frame without events", func(t *testing.T) {
		defer watchdog(t, 30*time.Second)()
		sess, conn := run(t, func() fakeReply { return fakeReply{success: false} })
		coreWait(t, "the refusal recorded", func() bool {
			n := 0
			for _, r := range sess.Records(oarlock.Cursor{}) {
				if r.Kind == oarlock.KindFrame && strings.Contains(string(r.Native), `"get_session_stats"`) {
					n++
					if len(r.Events) != 0 {
						t.Fatalf("a refused stats read produced events %v", pjKinds(r.Events))
					}
				}
			}
			return n == 1
		})
		if c := fold.ContextUsageOf(sess.Records(oarlock.Cursor{})).Value; c != nil {
			t.Fatalf("context = %+v, want nil", c)
		}
		_ = conn
	})
}

// A held input must not reach the wire before the stats read: the figure would
// describe the next turn, not the settled one.
func TestQueueDrainWaitsForTheStatsFrame(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	release := make(chan struct{})
	conn.stats = func() fakeReply {
		<-release
		return fakeReply{success: true, data: `{"contextUsage":{"tokens":1,"contextWindow":10,"percent":10}}`}
	}
	startTurn(t, sess, conn, "A")
	if out, _ := sess.Queue(context.Background(), "B", oarlock.InputOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Queue = %+v", out)
	}
	settle(conn, "stop", false)
	coreWait(t, "stats asked", func() bool { return conn.countSent("get_session_stats") == 1 })
	time.Sleep(100 * time.Millisecond)
	if n := conn.countSent("prompt"); n != 1 {
		t.Fatalf("prompt sent %d times before the stats frame, want 1", n)
	}
	close(release)
	conn.next(t, "prompt").ok(`{"disposition":"started"}`)
	records := sess.Records(oarlock.Cursor{})
	var statsAt, promptAt uint64
	for _, r := range records {
		switch {
		case r.Kind == oarlock.KindFrame && strings.Contains(string(r.Native), `"get_session_stats"`):
			statsAt = r.Seq
		case r.Kind == oarlock.KindRequest && r.Request.Action == oarlock.ActionPrompt && r.Request.Input == "B":
			promptAt = r.Seq
		}
	}
	if statsAt == 0 || promptAt == 0 || statsAt > promptAt {
		t.Fatalf("stats frame at %d, queued prompt at %d: the context must precede the next turn's prompt", statsAt, promptAt)
	}
}

// ---- 4. unknown events ---------------------------------------------------------

func TestUnknownEventsAreCountedAndKept(t *testing.T) {
	var st piState
	st, _ = foldPiEvent(st, json.RawMessage(`{"type":"future_event","n":1}`))
	before := st
	beforeTally := map[string]int{}
	for k, v := range st.unknown {
		beforeTally[k] = v
	}
	for _, raw := range []string{
		`{"type":"future_event","n":2}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"brand_new"}}`,
		`[1]`,
		`{"n":2}`,
	} {
		var evs []oarlock.Event
		st, evs = foldPiEvent(st, json.RawMessage(raw))
		if len(evs) != 0 {
			t.Fatalf("%s produced events %v: an unknown frame is never turned into a plausible event", raw, pjKinds(evs))
		}
	}
	want := map[string]int{"future_event": 2, "message_update/brand_new": 1, "": 2}
	if !reflect.DeepEqual(st.unknown, want) {
		t.Fatalf("tally = %v, want %v", st.unknown, want)
	}
	if !reflect.DeepEqual(before.unknown, beforeTally) {
		t.Fatalf("folding mutated the earlier state's map: %v != %v", before.unknown, beforeTally)
	}

	t.Run("session keeps the frame and the order", func(t *testing.T) {
		defer watchdog(t, 30*time.Second)()
		sess, conn := newCoreSession(t, time.Minute)
		conn.push(`{"type":"future_event","n":1}`)
		conn.push(`{"type":"message_update","assistantMessageEvent":{"type":"brand_new"}}`)
		conn.push(`{"type":"agent_start"}`)
		coreWait(t, "three frames", func() bool { return len(sess.Records(oarlock.Cursor{})) == 3 })
		records := sess.Records(oarlock.Cursor{})
		if string(records[0].Native) != `{"type":"future_event","n":1}` || len(records[0].Events) != 0 {
			t.Fatalf("unknown frame = %s %v", records[0].Native, records[0].Events)
		}
		if len(records[1].Events) != 0 {
			t.Fatalf("unknown sub-type produced events %v", pjKinds(records[1].Events))
		}
		if got := pjKinds(records[2].Events); !reflect.DeepEqual(got, []oarlock.EventKind{oarlock.EventTurnActive}) {
			t.Fatalf("a known frame after unknown ones projects %v", got)
		}
		if got := sess.Unknown(); !reflect.DeepEqual(got, map[string]int{"future_event": 1, "message_update/brand_new": 1}) {
			t.Fatalf("Unknown() = %v", got)
		}
	})
}

// ---- 5. tools -------------------------------------------------------------------

func TestToolCallMapping(t *testing.T) {
	var st piState
	st, ev := pjOne(t, st, `{"type":"tool_execution_start","toolCallId":"c1","toolName":"bash","args":{ "command": "ls" }}`)
	if ev.Kind != oarlock.EventToolCallStarted || ev.CallID != "c1" || ev.Tool != "bash" || ev.Input != `{"command":"ls"}` {
		t.Fatalf("started = %+v", ev)
	}
	st, ev = pjOne(t, st, `{"type":"tool_execution_update","toolCallId":"c1","toolName":"bash","args":{},"partialResult":{"content":[{"type":"text","text":"a"}]}}`)
	if ev.Kind != oarlock.EventToolCallProgress || ev.CallID != "c1" || ev.Output != `{"content":[{"type":"text","text":"a"}]}` {
		t.Fatalf("progress = %+v", ev)
	}

	ended := func(result, isError string) oarlock.Event {
		raw := `{"type":"tool_execution_end","toolCallId":"c1","toolName":"bash","result":` + result + isError + `}`
		_, e := pjOne(t, st, raw)
		if e.Kind != oarlock.EventToolCallEnded || e.CallID != "c1" {
			t.Fatalf("ended = %+v", e)
		}
		return e
	}
	exit := func(e oarlock.Event) string {
		code, reported := e.ReportedExitCode()
		if !reported {
			return "unreported"
		}
		return fmt.Sprint(code)
	}

	e := ended(`{"content":[{"type":"text","text":"a\nb"}],"structuredContent":{"exit_code":3}}`, `,"isError":true`)
	if len(e.Content) != 1 || e.Content[0].Type != "text" || e.Content[0].Text != "a\nb" || e.Result != oarlock.ToolFailed || exit(e) != "3" {
		t.Fatalf("failed command = %+v", e)
	}
	e = ended(`{"content":[{"type":"text","text":"ok"}],"structuredContent":{"exit_code":0}}`, `,"isError":false`)
	if e.Result != oarlock.ToolOK || exit(e) != "0" || e.ExitCode == nil {
		t.Fatalf("a reported exit 0 must stay 0: %+v", e)
	}
	e = ended(`{"content":[{"type":"text","text":"x"}]}`, ``)
	if e.Result != oarlock.ToolUnknown || exit(e) != "unreported" || e.ExitCode != nil {
		t.Fatalf("no isError and no exit_code must stay unreported, not inferred: %+v", e)
	}
	if e = ended(`{"content":[],"structuredContent":{"exit_code":143}}`, `,"isError":true`); exit(e) != "143" || len(e.Content) != 0 {
		t.Fatalf("a signal exit (143) stays 143, an empty block list is no content: %+v", e)
	}
	for _, bad := range []string{`"3"`, `2.5`, `null`} {
		if e = ended(`{"content":[],"structuredContent":{"exit_code":`+bad+`}}`, ``); exit(e) != "unreported" {
			t.Fatalf("exit_code %s was reported as %s", bad, exit(e))
		}
	}
	if e = ended(`{"content":[],"structuredContent":{"exit_code":-1}}`, ``); exit(e) != "-1" {
		t.Fatalf("a negative exit_code is whatever the tool said: %s", exit(e))
	}
	if e = ended(`{"content":[{"type":"image","data":"AAA","mimeType":"image/png"}]}`, ``); len(e.Content) != 1 ||
		e.Content[0].Type != "image" || e.Content[0].MediaType != "image/png" || e.Content[0].Data != "AAA" {
		t.Fatalf("image part = %+v", e.Content)
	}
	if e = ended(`{"content":[{"type":"mystery","x":1}]}`, ``); len(e.Content) != 1 || e.Content[0].Type != "other" || string(e.Content[0].Value) != `{"type":"mystery","x":1}` {
		t.Fatalf("unrecognised block = %+v", e.Content)
	}
	if e = ended(`"plain"`, ``); len(e.Content) != 1 || e.Content[0].Text != "plain" {
		t.Fatalf("a string result is one text part: %+v", e.Content)
	}

	// The conversation fold sees the exit code and the explicit outcome.
	_, started := pjOne(t, piState{}, `{"type":"tool_execution_start","toolCallId":"c1","toolName":"bash","args":{"command":"false"}}`)
	_, finished := pjOne(t, piState{}, `{"type":"tool_execution_end","toolCallId":"c1","toolName":"bash","result":{"content":[{"type":"text","text":"boom"}],"structuredContent":{"exit_code":1}},"isError":true}`)
	k := kernel.New("s", nil)
	k.Frame(kernel.FrameBody{Native: json.RawMessage(`{}`), Events: []oarlock.Event{started}}, kernel.RecordAt{})
	k.Frame(kernel.FrameBody{Native: json.RawMessage(`{}`), Events: []oarlock.Event{finished}}, kernel.RecordAt{})
	msgs := fold.ConversationOf(k.Records(oarlock.Cursor{})).Value
	if len(msgs) != 1 || msgs[0].Role != fold.RoleTool || msgs[0].Tool != "bash" || msgs[0].Result != oarlock.ToolFailed ||
		!msgs[0].ExitCodeReported || msgs[0].ExitCode == nil || *msgs[0].ExitCode != 1 || msgs[0].Output != "boom" {
		t.Fatalf("conversation = %+v", msgs)
	}
}

// ---- 6. usage: nil, never zero ---------------------------------------------------

func TestUsageNilNeverZero(t *testing.T) {
	for _, raw := range []string{
		`{"type":"message_end","message":{"role":"assistant","content":[]}}`,
		`{"type":"message_end","message":{"role":"assistant","usage":{"input":5}}}`,
		`{"type":"message_end","message":{"role":"assistant","usage":{"output":5}}}`,
		`{"type":"message_end","message":{"role":"assistant","usage":{"input":"5","output":"2"}}}`,
		`{"type":"message_end","message":{"role":"assistant","usage":{"input":-1,"output":2}}}`,
		`{"type":"message_end","message":{"role":"user","usage":{"input":5,"output":2}}}`,
		`{"type":"message_end","message":{"role":"toolResult","usage":{"input":5,"output":2}}}`,
		`{"type":"message_end"}`,
	} {
		st := pjNone(t, piState{}, raw)
		if !reflect.DeepEqual(st.tokens, oarlock.TokenTotals{}) {
			t.Fatalf("%s changed the running total to %+v", raw, st.tokens)
		}
	}

	st, first := pjOne(t, piState{}, `{"type":"message_end","message":{"role":"assistant","usage":{"input":100,"output":10,"cacheRead":20,"cacheWrite":5,"cacheWrite1h":5}}}`)
	pjTokens(t, first, 125, 10, "nil", "20", "5") // reasoning never reported: nil
	st2, second := pjOne(t, st, `{"type":"message_end","message":{"role":"assistant","usage":{"input":10,"output":4,"reasoning":3}}}`)
	pjTokens(t, second, 135, 14, "3", "20", "5")
	// The first event and the first state must not alias the second fold.
	pjTokens(t, first, 125, 10, "nil", "20", "5")
	if got := st.tokens; got.Input != 125 || got.Reasoning != nil {
		t.Fatalf("the earlier state changed: %+v", got)
	}
	if st2.tokens.Input != 135 {
		t.Fatalf("state = %+v", st2.tokens)
	}

	records := pjRecords([]json.RawMessage{
		json.RawMessage(`{"type":"message_end","message":{"role":"assistant","usage":{"input":100,"output":10}}}`),
	})
	usage := fold.UsageOf(records).Value
	if usage.Total == nil || usage.Total.Input != 100 || usage.ByAgent != nil || usage.Unattributed != nil {
		t.Fatalf("usage = %+v", usage)
	}
	empty := fold.UsageOf(pjRecords([]json.RawMessage{json.RawMessage(`{"type":"agent_start"}`)})).Value
	if empty.Total != nil || empty.ByAgent != nil || empty.Unattributed != nil {
		t.Fatalf("no usage frame must be nil, never a zero total: %+v", empty)
	}
	if c := fold.ContextUsageOf(records).Value; c != nil {
		t.Fatalf("context = %+v", c)
	}
}

// ---- 7. outcomes and failure classes -----------------------------------------------

// settleRun feeds the frames of one run and returns the settlement's outcome.
func settleRun(t *testing.T, st piState, aborted bool, frames ...string) (piState, *oarlock.TurnOutcome) {
	t.Helper()
	for _, f := range frames {
		st, _ = foldPiEvent(st, json.RawMessage(f))
	}
	next, evs := foldPiEvent(st, json.RawMessage(fmt.Sprintf(`{"type":"agent_settled","aborted":%v}`, aborted)))
	if len(evs) != 1 || evs[0].Kind != oarlock.EventTurnEnded || evs[0].Outcome == nil {
		t.Fatalf("agent_settled -> %+v", evs)
	}
	return next, evs[0].Outcome
}

func failedTurn(message string) string {
	b, _ := json.Marshal(message)
	return `{"type":"turn_end","message":{"role":"assistant","stopReason":"error","errorMessage":` + string(b) + `}}`
}

func TestOutcomeAndFailureClasses(t *testing.T) {
	cases := []struct {
		name    string
		message string
		failure oarlock.FailureClass
		status  int
	}{
		{"auth (anthropic type)", `401 {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, oarlock.FailureAuth, 401},
		{"auth (status only)", `401 unauthorized`, oarlock.FailureAuth, 401},
		{"rate limit (openai code)", `429 {"error":{"code":"rate_limit_exceeded","message":"slow down"}}`, oarlock.FailureRateLimited, 429},
		{"rate limit (status only)", `429 slow down`, oarlock.FailureRateLimited, 429},
		{"context overflow (anthropic text)", `400 {"error":{"message":"prompt is too long: 213462 tokens > 200000 maximum"}}`, oarlock.FailureInputTooLarge, 400},
		{"context overflow (openai text, no status)", `Your input exceeds the context window of this model`, oarlock.FailureInputTooLarge, 0},
		{"context overflow (413)", `413 {"error":{"type":"request_too_large","message":"Request exceeds the maximum size"}}`, oarlock.FailureInputTooLarge, 413},
		{"unknown", `boom something odd happened`, oarlock.FailureUnknown, 0},
		{"overloaded", `503 upstream`, oarlock.FailureOverloaded, 503},
		{"provider", `500 oops`, oarlock.FailureProvider, 500},
		{"quota (code)", `429 {"error":{"code":"insufficient_quota"}}`, oarlock.FailureQuota, 429},
		{"billing (anthropic text)", `400 {"error":{"type":"invalid_request_error","message":"Your credit balance is too low"}}`, oarlock.FailureBilling, 400},
		{"throttling is not overflow", `Throttling error: Too many tokens, please wait before trying again.`, oarlock.FailureUnknown, 0},
		{"rate limit wins over overflow text", `429 rate limit exceeded: input exceeds the context window`, oarlock.FailureRateLimited, 429},
		{"too many tokens alone is an overflow pattern", `429 Too many tokens, please wait`, oarlock.FailureInputTooLarge, 429},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, out := settleRun(t, piState{}, false, failedTurn(tc.message))
			if out.Kind != oarlock.OutcomeFailed || out.Failure != tc.failure || out.Status != tc.status || out.Reason != tc.message {
				t.Fatalf("outcome = %+v, want failed/%s status %d with the message as reason", out, tc.failure, tc.status)
			}
		})
	}

	t.Run("a stream error carries the failure when the last turn_end has no text", func(t *testing.T) {
		_, out := settleRun(t, piState{}, false,
			`{"type":"message_update","assistantMessageEvent":{"type":"error","reason":"error","error":{"role":"assistant","stopReason":"error","errorMessage":"429 slow down"}}}`,
			`{"type":"turn_end","message":{"role":"assistant","stopReason":"error"}}`)
		if out.Kind != oarlock.OutcomeFailed || out.Failure != oarlock.FailureRateLimited || out.Reason != "429 slow down" {
			t.Fatalf("outcome = %+v", out)
		}
	})
	t.Run("a stream error alone classifies the same", func(t *testing.T) {
		_, out := settleRun(t, piState{}, false,
			`{"type":"message_update","assistantMessageEvent":{"type":"error","reason":"error","error":{"stopReason":"error","errorMessage":"prompt is too long: 9 tokens > 5 maximum"}}}`,
			`{"type":"turn_end","message":{"role":"assistant","stopReason":"error","errorMessage":"prompt is too long: 9 tokens > 5 maximum"}}`)
		if out.Failure != oarlock.FailureInputTooLarge {
			t.Fatalf("outcome = %+v", out)
		}
	})
	t.Run("no text at all is a failed turn with an honest unknown", func(t *testing.T) {
		_, out := settleRun(t, piState{}, false, `{"type":"turn_end","message":{"role":"assistant","stopReason":"error"}}`)
		if out.Kind != oarlock.OutcomeFailed || out.Failure != oarlock.FailureUnknown || out.Reason != "provider error" {
			t.Fatalf("outcome = %+v", out)
		}
	})
	t.Run("aborted wins over a failure", func(t *testing.T) {
		_, out := settleRun(t, piState{}, true, failedTurn("401 unauthorized"))
		if out.Kind != oarlock.OutcomeAborted || out.Failure != "" || out.Reason != "" {
			t.Fatalf("outcome = %+v", out)
		}
	})
	t.Run("stopReason aborted without the flag is aborted", func(t *testing.T) {
		_, out := settleRun(t, piState{}, false, `{"type":"turn_end","message":{"role":"assistant","stopReason":"aborted","errorMessage":"stream ended"}}`)
		if out.Kind != oarlock.OutcomeAborted {
			t.Fatalf("outcome = %+v", out)
		}
	})
	t.Run("stop is completed", func(t *testing.T) {
		_, out := settleRun(t, piState{}, false, `{"type":"turn_end","message":{"role":"assistant","stopReason":"stop"}}`)
		if out.Kind != oarlock.OutcomeCompleted || out.Failure != "" {
			t.Fatalf("outcome = %+v", out)
		}
	})
	t.Run("a retry that succeeded is completed", func(t *testing.T) {
		_, out := settleRun(t, piState{}, false, failedTurn("503 upstream"), `{"type":"turn_end","message":{"role":"assistant","stopReason":"stop"}}`)
		if out.Kind != oarlock.OutcomeCompleted {
			t.Fatalf("outcome = %+v", out)
		}
	})
	t.Run("a user or tool turn_end never fails a run", func(t *testing.T) {
		_, out := settleRun(t, piState{}, false, `{"type":"turn_end","message":{"role":"user","stopReason":"error","errorMessage":"x"}}`)
		if out.Kind != oarlock.OutcomeCompleted {
			t.Fatalf("outcome = %+v", out)
		}
	})
	t.Run("per-run state resets at the settlement, usage persists", func(t *testing.T) {
		st, out := settleRun(t, piState{}, false,
			`{"type":"message_end","message":{"role":"assistant","usage":{"input":7,"output":1}}}`, failedTurn("401 unauthorized"))
		if out.Kind != oarlock.OutcomeFailed {
			t.Fatalf("first run = %+v", out)
		}
		st, out = settleRun(t, st, false, `{"type":"turn_end","message":{"role":"assistant","stopReason":"stop"}}`)
		if out.Kind != oarlock.OutcomeCompleted {
			t.Fatalf("second run = %+v: the first run's failure leaked", out)
		}
		if st.tokens.Input != 7 || st.tokens.Output != 1 {
			t.Fatalf("running total = %+v, want it kept across runs", st.tokens)
		}
	})
	t.Run("isContextOverflow needs stopReason error", func(t *testing.T) {
		msg, _ := parseObj(json.RawMessage(`{"stopReason":"stop","errorMessage":"prompt is too long"}`))
		if isContextOverflow(msg) {
			t.Fatal("a successful message is not an overflow, whatever its text says")
		}
		msg, _ = parseObj(json.RawMessage(`{"stopReason":"error","provider":"cerebras","errorMessage":"400 status code (no body)"}`))
		if !isContextOverflow(msg) {
			t.Fatal("cerebras's bodyless 400 is pi-ai's overflow")
		}
	})
}

// ---- 8. message updates and lifecycle -------------------------------------------------

func TestMessageUpdateAndLifecycleMapping(t *testing.T) {
	var st piState
	if _, ev := pjOne(t, st, `{"type":"agent_start"}`); ev.Kind != oarlock.EventTurnActive {
		t.Fatalf("agent_start = %+v", ev)
	}
	for _, sub := range []string{"start", "text_start", "text_end", "toolcall_start", "toolcall_delta", "toolcall_end", "done"} {
		next := pjNone(t, st, `{"type":"message_update","assistantMessageEvent":{"type":"`+sub+`"}}`)
		if len(next.unknown) != 0 {
			t.Fatalf("%s was counted unknown: %v", sub, next.unknown)
		}
	}
	// An empty thinking block is one empty reasoning item; a block that streamed
	// text is not duplicated by its end; an empty delta says nothing.
	st = pjNone(t, st, `{"type":"message_update","assistantMessageEvent":{"type":"thinking_start"}}`)
	if _, ev := pjOne(t, st, `{"type":"message_update","assistantMessageEvent":{"type":"thinking_end"}}`); ev.Kind != oarlock.EventReasoning || ev.Text != "" {
		t.Fatalf("empty thinking = %+v", ev)
	}
	st = pjNone(t, st, `{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":""}}`)
	st, ev := pjOne(t, st, `{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":"hm"}}`)
	if ev.Kind != oarlock.EventReasoning || ev.Text != "hm" {
		t.Fatalf("thinking_delta = %+v", ev)
	}
	pjNone(t, st, `{"type":"message_update","assistantMessageEvent":{"type":"thinking_end"}}`)

	if _, ev := pjOne(t, piState{}, `{"type":"message_start","message":{"role":"user","content":"plain text"}}`); ev.Kind != oarlock.EventUserMessage || ev.Text != "plain text" {
		t.Fatalf("string user content = %+v", ev)
	}
	pjNone(t, piState{}, `{"type":"message_start","message":{"role":"assistant","content":[]}}`)
	pjNone(t, piState{}, `{"type":"message_start","message":{"role":"system","content":""}}`)

	if _, ev := pjOne(t, piState{}, `{"type":"compaction_start","reason":"threshold"}`); ev.Kind != oarlock.EventCompactionStarted || ev.Trigger != "threshold" {
		t.Fatalf("compaction_start = %+v", ev)
	}
	for raw, want := range map[string]oarlock.CompactionOutcome{
		`{"type":"compaction_end","reason":"manual","aborted":true,"willRetry":false}`:                         oarlock.CompactionAborted,
		`{"type":"compaction_end","reason":"overflow","aborted":false,"willRetry":true,"errorMessage":"nope"}`: oarlock.CompactionFailed,
		`{"type":"compaction_end","reason":"threshold","aborted":false,"willRetry":false}`:                     oarlock.CompactionCompleted,
	} {
		if _, ev := pjOne(t, piState{}, raw); ev.Kind != oarlock.EventCompactionEnded || ev.CompactionResult != want {
			t.Fatalf("%s -> %+v, want %s", raw, ev, want)
		}
	}
	if _, ev := pjOne(t, piState{}, `{"type":"thinking_level_changed","level":"high"}`); ev.Kind != oarlock.EventEffort || ev.Effort != "high" {
		t.Fatalf("effort = %+v", ev)
	}
	for _, typ := range []string{"auto_retry_start", "summarization_retry_scheduled"} {
		_, ev := pjOne(t, piState{}, `{"type":"`+typ+`","attempt":1,"maxAttempts":3,"delayMs":2000,"errorMessage":"x"}`)
		if ev.Kind != oarlock.EventRetry || ev.Attempt != 1 || ev.MaxAttempts != 3 || ev.DelayMS != 2000 || ev.Reason != "x" {
			t.Fatalf("%s = %+v", typ, ev)
		}
	}
	for _, typ := range []string{"turn_start", "agent_end", "queue_update", "entry_appended", "session_info_changed", "auto_retry_end",
		"summarization_retry_attempt_start", "summarization_retry_finished", "bash_execution_update", "extension_ui_request", "extension_error"} {
		next := pjNone(t, piState{}, `{"type":"`+typ+`"}`)
		if len(next.unknown) != 0 {
			t.Fatalf("known type %s was counted unknown: %v", typ, next.unknown)
		}
	}

	pjNone(t, piState{}, `{"type":"response","command":"get_state","success":false,"error":"no"}`)
	pjNone(t, piState{}, `{"type":"response","command":"get_session_stats","success":true,"data":{}}`)
	pjNone(t, piState{}, `{"type":"response","command":"get_session_stats","success":true,"data":{"contextUsage":{}}}`)
	pjNone(t, piState{}, `{"type":"response","command":"prompt","success":true,"data":{"disposition":"handled"}}`)
	if _, ev := pjOne(t, piState{}, `{"type":"response","command":"get_session_stats","success":true,"data":{"contextUsage":{"tokens":9,"contextWindow":100,"percent":9}}}`); ev.Usage == nil || ev.Usage.Context == nil ||
		*ev.Usage.Context.Tokens != 9 || *ev.Usage.Context.ContextWindow != 100 || *ev.Usage.Context.Percent != 9 {
		t.Fatalf("context = %+v", ev)
	}
}

// ---- 9. coalescing is a reading ----------------------------------------------------------

func TestCoalesceTextIsAReadingOnly(t *testing.T) {
	raws := pjFrames(t, "abort-turn")
	records := pjRecords(raws)
	snapshot, _ := json.Marshal(records)

	merged := coalesceText(records)
	after, _ := json.Marshal(records)
	if !bytes.Equal(snapshot, after) {
		t.Fatal("coalesceText changed the records")
	}
	if len(records) != 26 {
		t.Fatalf("%d records, want 26 (one per native frame)", len(records))
	}
	assertDense(t, records)

	var reasoning []readEvent
	eventsBefore := 0
	for _, r := range records {
		eventsBefore += len(r.Events)
	}
	for _, m := range merged {
		if m.Event.Kind == oarlock.EventReasoning {
			reasoning = append(reasoning, m)
		}
	}
	if len(reasoning) != 1 || reasoning[0].Event.Text != "We need answer" || reasoning[0].FirstSeq != 14 || reasoning[0].Seq != 16 {
		t.Fatalf("reasoning reading = %+v, want one event 'We need answer' spanning Seq 14..16", reasoning)
	}
	// 3 pieces became 1: two merged away.
	if len(merged) != eventsBefore-2 {
		t.Fatalf("%d events after merging, want %d - 2", len(merged), eventsBefore)
	}

	text := func(s string) oarlock.Event { return oarlock.Event{Kind: oarlock.EventTextDelta, Text: s} }
	k := kernel.New("s", nil)
	frame := func(evs ...oarlock.Event) {
		k.Frame(kernel.FrameBody{Native: json.RawMessage(`{}`), Events: evs}, kernel.RecordAt{})
	}
	frame(text("a"))
	frame()          // a frame that states nothing (text_end, a host notification) ...
	frame(text("b")) // ... does not end the run
	frame(oarlock.Event{Kind: oarlock.EventToolCallStarted, CallID: "c", Tool: "bash"})
	frame(text("c"))
	frame(oarlock.Event{Kind: oarlock.EventReasoning, Text: "r"}) // another kind does not merge
	frame(oarlock.Event{Kind: oarlock.EventReasoning})            // an empty reasoning item never merges
	got := coalesceText(k.Records(oarlock.Cursor{}))
	var texts []string
	for _, g := range got {
		texts = append(texts, fmt.Sprintf("%s:%q:%d-%d", g.Event.Kind, g.Event.Text, g.FirstSeq, g.Seq))
	}
	want := []string{`text_delta:"ab":1-3`, `tool_call_started:"":4-4`, `text_delta:"c":5-5`, `reasoning:"r":6-6`, `reasoning:"":7-7`}
	if !reflect.DeepEqual(texts, want) {
		t.Fatalf("reading = %v, want %v", texts, want)
	}
}

// ---- 10. purity ----------------------------------------------------------------------------

func TestFoldIsPure(t *testing.T) {
	var st piState
	for _, raw := range pjFrames(t, "normal-turn")[:16] {
		st, _ = foldPiEvent(st, raw)
	}
	st, _ = foldPiEvent(st, json.RawMessage(`{"type":"future_event"}`))
	frame := json.RawMessage(`{"type":"message_end","message":{"role":"assistant","usage":{"input":1,"output":1,"reasoning":1}}}`)

	snapshot := st
	snapshot.unknown = map[string]int{}
	for k, v := range st.unknown {
		snapshot.unknown[k] = v
	}
	tokens := cloneTokens(st.tokens)
	snapshot.tokens = tokens

	a, ea := foldPiEvent(st, frame)
	b, eb := foldPiEvent(st, frame)
	if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(ea, eb) {
		t.Fatal("the same state and frame folded to different results")
	}
	if !reflect.DeepEqual(st, snapshot) {
		t.Fatalf("the fold mutated the state it was given:\n got  %+v\n want %+v", st, snapshot)
	}
	// Unknown frames must not write into the shared map either.
	_, _ = foldPiEvent(st, json.RawMessage(`{"type":"another_future_event"}`))
	if !reflect.DeepEqual(st.unknown, snapshot.unknown) {
		t.Fatalf("the unknown map was mutated in place: %v", st.unknown)
	}
}

// TestCommittedRetryFixtureMapsToRetryEvents replays the native frames of the
// committed issue #88 retry recording (testdata/voyage/pi-1.4.0-retry-turn.jsonl,
// made by internal/voyage/live_retry_test.go from a real Pi 1.4.0 behind a
// local fault proxy) through this package's projection. The live run already
// proved the mapping once; this pins the recorded frame shape offline, so a Pi
// or projection change that stops projecting `auto_retry_start` fails here
// instead of only in an opt-in live run.
func TestCommittedRetryFixtureMapsToRetryEvents(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "voyage", "pi-1.4.0-retry-turn.jsonl"))
	if err != nil {
		t.Fatalf("read the retry fixture: %v", err)
	}
	var st piState
	var retries []oarlock.Event
	retryEnds := 0
	for i, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var envelope struct {
			Kind   string `json:"kind"`
			Record *struct {
				Kind   string          `json:"kind"`
				Native json.RawMessage `json:"native"`
			} `json:"record"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		if envelope.Kind != "record" || envelope.Record == nil || envelope.Record.Kind != "frame" {
			continue
		}
		next, evs := foldPiEvent(st, envelope.Record.Native)
		st = next
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(envelope.Record.Native, &head); err != nil {
			t.Fatalf("line %d native: %v", i+1, err)
		}
		if head.Type == "auto_retry_end" {
			retryEnds++
			if len(evs) != 0 {
				t.Fatalf("line %d: auto_retry_end produced %v, want none (deliberately silent)", i+1, pjKinds(evs))
			}
		}
		for _, ev := range evs {
			if ev.Kind == oarlock.EventRetry {
				retries = append(retries, ev)
			}
		}
	}
	if len(retries) != 2 {
		t.Fatalf("%d retry events from the committed fixture, want 2 (one per auto_retry_start)", len(retries))
	}
	for i, ev := range retries {
		if ev.Attempt != i+1 || ev.MaxAttempts != 3 {
			t.Fatalf("retry %d = %+v, want attempt %d of 3", i+1, ev, i+1)
		}
		if ev.DelayMS != int64(i+1) {
			t.Fatalf("retry %d delayMs = %d, want %d (Pi's 1 ms base backoff, doubled)", i+1, ev.DelayMS, i+1)
		}
		if !strings.Contains(ev.Reason, "500") {
			t.Fatalf("retry %d reason = %q, want the provider's 500", i+1, ev.Reason)
		}
	}
	if retryEnds != 1 {
		t.Fatalf("%d auto_retry_end frames, want 1", retryEnds)
	}
	for _, typ := range []string{"auto_retry_start", "auto_retry_end"} {
		if n := st.unknown[typ]; n != 0 {
			t.Fatalf("%q was tallied %d times as unknown; it has a case in the projection", typ, n)
		}
	}
}
