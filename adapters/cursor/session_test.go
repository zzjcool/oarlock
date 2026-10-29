package cursor

// session_test.go drives the real adapter code over a fresh kernel with a fake
// cursor-agent (helper_test.go) — real processes, real pipes, real exit codes.
// The frame mapping itself (thinking, tool calls, text suffixes, usage) is
// project.go's and is pinned by project_test.go; here the subject is the
// control plane: one process per turn, the chat id, stdin, abort, dispose, the
// queue and the argv.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func waitRecords(t *testing.T, s *Session, what string, cond func([]oarlock.Record) bool) []oarlock.Record {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		rs := s.Records(oarlock.Cursor{})
		if cond(rs) {
			return rs
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; %d records", what, len(rs))
		}
		time.Sleep(3 * time.Millisecond)
	}
}

// waitLastExit waits until the session has observed a process exit.
func waitLastExit(t *testing.T, s *Session) sj.ExitStatus {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if st, ok := s.LastExit(); ok {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for an observed process exit")
		}
		time.Sleep(3 * time.Millisecond)
	}
}

func turnsEnded(n int) func([]oarlock.Record) bool {
	return func(rs []oarlock.Record) bool { return eventCount(rs, oarlock.EventTurnEnded) >= n }
}

func exited(n int) func([]oarlock.Record) bool {
	return func(rs []oarlock.Record) bool { return len(responses(rs, oarlock.ResponseExited)) >= n }
}

func both(conds ...func([]oarlock.Record) bool) func([]oarlock.Record) bool {
	return func(rs []oarlock.Record) bool {
		for _, c := range conds {
			if !c(rs) {
				return false
			}
		}
		return true
	}
}

func frames(rs []oarlock.Record) (out []oarlock.Record) {
	for _, r := range rs {
		if r.Kind == oarlock.KindFrame {
			out = append(out, r)
		}
	}
	return out
}

func responses(rs []oarlock.Record, kind oarlock.ResponseKind) (out []oarlock.Record) {
	for _, r := range rs {
		if r.Kind == oarlock.KindResponse && r.Response != nil && r.Response.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

func eventCount(rs []oarlock.Record, kind oarlock.EventKind) int {
	n := 0
	for _, r := range rs {
		for _, e := range r.Events {
			if e.Kind == kind {
				n++
			}
		}
	}
	return n
}

func firstEvent(rs []oarlock.Record, kind oarlock.EventKind) (oarlock.Event, bool) {
	for _, r := range rs {
		for _, e := range r.Events {
			if e.Kind == kind {
				return e, true
			}
		}
	}
	return oarlock.Event{}, false
}

func outcomes(rs []oarlock.Record) (out []oarlock.TurnOutcome) {
	for _, r := range rs {
		for _, e := range r.Events {
			if e.Kind == oarlock.EventTurnEnded && e.Outcome != nil {
				out = append(out, *e.Outcome)
			}
		}
	}
	return
}

func countAction(rs []oarlock.Record, action oarlock.Action) int {
	n := 0
	for _, r := range rs {
		if r.Kind == oarlock.KindRequest && r.Request != nil && r.Request.Action == action {
			n++
		}
	}
	return n
}

// ---- one turn ------------------------------------------------------------------

// A prompt starts one cursor-agent, the runtime's own system/init states the
// model and the chat id, result ends the turn, and the exit that follows is the
// turn's lifecycle: remembered in LastExit, deliberately not recorded as the
// session's outcome (the chat can still be resumed).
func TestPromptTurnRecordsInitModelAndExit(t *testing.T) {
	h := newHelper(t, "turn", oarlock.Options{}, SessionOptions{})
	out, err := h.sess.Prompt(ctxT(t), "say hi", oarlock.PromptOptions{InputOptions: oarlock.InputOptions{InputID: "in-1"}})
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Prompt = %+v, %v", out, err)
	}
	rs := waitRecords(t, h.sess, "turn end", turnsEnded(1))
	st := waitLastExit(t, h.sess)

	if got := outcomes(rs); len(got) != 1 || got[0].Kind != oarlock.OutcomeCompleted {
		t.Fatalf("outcomes = %+v, want exactly one completed", got)
	}
	if ev, ok := firstEvent(rs, oarlock.EventModel); !ok || ev.Model != "Auto Balance" {
		t.Fatalf("model event = %+v (found %v), want the model system/init reported", ev, ok)
	}
	if n := eventCount(rs, oarlock.EventTurnActive); n != 1 {
		t.Fatalf("turn_active events = %d, want 1 from system/init (Folder.Prompted must not be called)", n)
	}

	// The chat id is learned from system/init and stamped on the records.
	if id := h.sess.ChatID(); id != "chat-fresh" {
		t.Fatalf("ChatID() = %q, want chat-fresh", id)
	}
	fr := frames(rs)
	if len(fr) != 4 {
		t.Fatalf("frame records = %d, want 4 (init, user, assistant, result)", len(fr))
	}
	if !strings.Contains(string(fr[0].Native), `"subtype":"init"`) {
		t.Fatalf("first frame = %s, want system/init", fr[0].Native)
	}
	for _, r := range fr {
		if r.SessionID != "chat-fresh" {
			t.Fatalf("frame %d carries session %q, want the chat id learned from init", r.Seq, r.SessionID)
		}
	}

	// A clean turn's exit is not a session exit.
	if exits := responses(rs, oarlock.ResponseExited); len(exits) != 0 {
		t.Fatalf("exited responses = %d after a clean turn: the turn ended with result, and the chat is still resumable", len(exits))
	}
	if st.Code != 0 || st.Signal != nil || st.Cause != sj.CauseExited {
		t.Fatalf("LastExit = %+v, want code 0, cause exited", st)
	}

	argv := h.waitProcesses(1)[0]
	if _, ok := hasFlag(argv, "-p"); !ok {
		t.Fatalf("argv %v is missing -p", argv)
	}
	if v, ok := hasFlag(argv, "--output-format"); !ok || v != "stream-json" {
		t.Fatalf("argv %v must ask for stream-json", argv)
	}
	if _, ok := hasFlag(argv, "--resume"); ok {
		t.Fatalf("a fresh run must not pass --resume: %v", argv)
	}
}

// A process that dies without a result is the session's own exit: one exited
// response with the code and cause, no invented turn end, and the kernel then
// settles the session.
func TestExitWithoutResultIsTheSessionExit(t *testing.T) {
	h := newHelper(t, "die", oarlock.Options{}, SessionOptions{})
	if out, _ := h.sess.Prompt(ctxT(t), "this will not run", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v", out)
	}
	rs := waitRecords(t, h.sess, "process exit", exited(1))
	if n := eventCount(rs, oarlock.EventTurnEnded); n != 0 {
		t.Fatalf("turn_ended events = %d, want 0: no result was recorded, so no turn end may be invented", n)
	}
	exits := responses(rs, oarlock.ResponseExited)
	if exits[0].Response.ExitCode == nil || *exits[0].Response.ExitCode != 3 || !exits[0].Response.ExitCodeReported {
		t.Fatalf("exited response = %+v, want exit code 3 reported", exits[0].Response)
	}
	if !strings.Contains(string(exits[0].Response.Native), `"cause":"exited"`) {
		t.Fatalf("exited native = %s, want the cause", exits[0].Response.Native)
	}
	if !strings.Contains(exits[0].Response.Reason, "dying without a result") {
		t.Fatalf("exited reason = %q, want the stderr tail", exits[0].Response.Reason)
	}
	if out, _ := h.sess.Prompt(ctxT(t), "again", oarlock.PromptOptions{}); out.Kind != oarlock.ControlRejected || out.Code != oarlock.RuntimeExited {
		t.Fatalf("prompt after the session exit = %+v, want rejected runtime_exited", out)
	}
}

// A second prompt continues the chat the first turn reported: one process per
// turn, `--resume <chat id>` on the argv of the next one. A clean turn's exit
// is not a session exit, so the kernel still reaches the adapter.
func TestSecondPromptResumesTheChat(t *testing.T) {
	h := newHelper(t, "turn", oarlock.Options{}, SessionOptions{})
	if out, _ := h.sess.Prompt(ctxT(t), "first", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("first prompt = %+v", out)
	}
	waitRecords(t, h.sess, "first turn end", turnsEnded(1))
	waitLastExit(t, h.sess)
	chat := h.sess.ChatID()
	if chat == "" {
		t.Fatal("ChatID is empty after the first turn")
	}

	if out, _ := h.sess.Prompt(ctxT(t), "second", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("second prompt = %+v, want accepted: the chat is resumed in a new process", out)
	}
	rs := waitRecords(t, h.sess, "second turn end", turnsEnded(2))
	argv := h.waitProcesses(2)[1]
	got, ok := hasFlag(argv, "--resume")
	if !ok || got != chat {
		t.Fatalf("second process argv = %v, want --resume %s", argv, chat)
	}
	if n := len(responses(rs, oarlock.ResponseExited)); n != 0 {
		t.Fatalf("exited responses = %d: no turn here lost its result", n)
	}
}

// One megabyte of multi-line prompt goes over stdin verbatim: it is free text,
// never a framed line, so neither pipe can deadlock against the other.
func TestBigPromptOverStdinDoesNotDeadlock(t *testing.T) {
	h := newHelper(t, "big", oarlock.Options{}, SessionOptions{})
	prompt := strings.Repeat("a line of the prompt, with punctuation: it is text.\n", 40000)
	if len(prompt) < 1<<20 {
		t.Fatalf("prompt is only %d bytes", len(prompt))
	}
	out, err := h.sess.Prompt(ctxT(t), prompt, oarlock.PromptOptions{})
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Prompt(1MB) = %+v, %v", out, err)
	}
	waitRecords(t, h.sess, "turn end", turnsEnded(1))
	waitLastExit(t, h.sess)
	gotBytes, gotLines := h.stdinStats()
	if gotBytes != int64(len(prompt)) {
		t.Fatalf("the child read %d stdin bytes, want %d", gotBytes, len(prompt))
	}
	if want := int64(strings.Count(prompt, "\n")); gotLines != want {
		t.Fatalf("the child read %d newlines, want %d", gotLines, want)
	}
	if h.sess.ChatID() != "chat-fresh" {
		t.Fatalf("ChatID = %q after the big turn", h.sess.ChatID())
	}
}

// An undeclared top-level type is kept verbatim in Native, states nothing, and
// is counted (MUL-5231: never silently absorbed).
func TestUnknownFrameIsKeptAndCounted(t *testing.T) {
	const line = `{"type":"cursor_telemetry","payload":{"n":1}}`
	h := newHelper(t, "unknown", oarlock.Options{}, SessionOptions{})
	if out, _ := h.sess.Prompt(ctxT(t), "say hi", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v", out)
	}
	rs := waitRecords(t, h.sess, "turn end", turnsEnded(1))
	waitLastExit(t, h.sess)

	var found bool
	for _, r := range frames(rs) {
		if string(r.Native) != line {
			continue
		}
		found = true
		if len(r.Events) != 0 {
			t.Fatalf("an unknown frame stated events: %+v", r.Events)
		}
	}
	if !found {
		t.Fatal("the unknown frame was not retained in Native verbatim")
	}
	unknown := h.sess.Unknown()
	if unknown["cursor_telemetry"] != 1 {
		t.Fatalf("Unknown() = %v, want cursor_telemetry counted once", unknown)
	}
}

// ---- abort ---------------------------------------------------------------------

// An abort is accepted and closes the process group; the turn never reached a
// result, so its exit IS the session's and is recorded — with no invented
// turn_ended. The session is then settled: a second Abort is runtime_exited.
func TestAbortClosesTheProcessWithoutInventingATurnEnd(t *testing.T) {
	h := newHelper(t, "hang", oarlock.Options{}, SessionOptions{AbortGrace: 500 * time.Millisecond}, "ignoreterm")
	if out, _ := h.sess.Prompt(ctxT(t), "run forever", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v", out)
	}
	waitRecords(t, h.sess, "turn running", func(rs []oarlock.Record) bool {
		return eventCount(rs, oarlock.EventTurnActive) == 1
	})

	out, err := h.sess.Abort(ctxT(t))
	if err != nil || out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Abort = %+v, %v", out, err)
	}
	rs := waitRecords(t, h.sess, "process exit", exited(1))
	if n := eventCount(rs, oarlock.EventTurnEnded); n != 0 {
		t.Fatalf("turn_ended events = %d after an abort: the adapter must not invent a turn end", n)
	}
	exits := responses(rs, oarlock.ResponseExited)
	if len(exits) != 1 {
		t.Fatalf("exited responses = %d, want 1", len(exits))
	}
	if !exits[0].Response.ExitCodeReported {
		t.Fatalf("exited response = %+v, want an exit that was reported (killed by signal carries no code)", exits[0].Response)
	}
	if !strings.Contains(string(exits[0].Response.Native), `"cause":"closed"`) {
		t.Fatalf("exited native = %s, want cause closed (oarlock closed the group)", exits[0].Response.Native)
	}
	st, ok := h.sess.LastExit()
	if !ok || st.Cause != sj.CauseClosed {
		t.Fatalf("LastExit = %+v (%v), want the closed exit the adapter closed", st, ok)
	}

	out, _ = h.sess.Abort(ctxT(t))
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.RuntimeExited {
		t.Fatalf("second Abort = %+v, want rejected runtime_exited: the aborted session is settled", out)
	}
}

// ---- dispose -------------------------------------------------------------------

// Dispose twice is idempotent: one dispose request in the stream, one process
// closed, and controls afterwards are refused by the kernel. The turn had
// already ended with result, so its exit is not re-recorded as a session exit.
func TestDisposeTwiceIsIdempotent(t *testing.T) {
	h := newHelper(t, "turn", oarlock.Options{}, SessionOptions{})
	if out, _ := h.sess.Prompt(ctxT(t), "say hi", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v", out)
	}
	waitRecords(t, h.sess, "turn end", turnsEnded(1))
	waitLastExit(t, h.sess)

	if err := h.sess.Dispose(ctxT(t)); err != nil {
		t.Fatalf("first Dispose: %v", err)
	}
	if err := h.sess.Dispose(ctxT(t)); err != nil {
		t.Fatalf("second Dispose: %v", err)
	}
	rs := h.sess.Records(oarlock.Cursor{})
	if n := countAction(rs, oarlock.ActionDispose); n != 1 {
		t.Fatalf("dispose requests = %d, want exactly 1", n)
	}
	if n := len(responses(rs, oarlock.ResponseExited)); n != 0 {
		t.Fatalf("exited responses = %d: the one turn ended with result, so nothing exits the session", n)
	}
	out, _ := h.sess.Prompt(ctxT(t), "after dispose", oarlock.PromptOptions{})
	if out.Kind != oarlock.ControlRejected || (out.Code != oarlock.Disposed && out.Code != oarlock.RuntimeExited) {
		t.Fatalf("prompt after dispose = %+v, want rejected disposed/runtime_exited", out)
	}
}

// Dispose while a turn runs closes that process and waits for its exit.
func TestDisposeClosesTheRunningTurn(t *testing.T) {
	h := newHelper(t, "hang", oarlock.Options{}, SessionOptions{AbortGrace: 500 * time.Millisecond}, "ignoreterm")
	if out, _ := h.sess.Prompt(ctxT(t), "run forever", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v", out)
	}
	waitRecords(t, h.sess, "turn running", func(rs []oarlock.Record) bool {
		return eventCount(rs, oarlock.EventTurnActive) == 1
	})
	if err := h.sess.Dispose(ctxT(t)); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	rs := h.sess.Records(oarlock.Cursor{})
	if n := len(responses(rs, oarlock.ResponseExited)); n != 1 {
		t.Fatalf("exited responses = %d, want 1", n)
	}
	if n := countAction(rs, oarlock.ActionDispose); n != 1 {
		t.Fatalf("dispose requests = %d, want 1", n)
	}
}

// ---- queue ---------------------------------------------------------------------

// Queued input is held in the adapter and sent as an ordinary prompt once the
// running turn is over, which starts the next process on the same chat.
func TestQueueHeldThenDrainedIntoASecondProcess(t *testing.T) {
	h := newHelper(t, "gate", oarlock.Options{}, SessionOptions{})
	if out, _ := h.sess.Prompt(ctxT(t), "first", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v", out)
	}
	waitRecords(t, h.sess, "turn running", func(rs []oarlock.Record) bool {
		return eventCount(rs, oarlock.EventTurnActive) == 1
	})
	queued, err := h.sess.Queue(ctxT(t), "second", oarlock.InputOptions{InputID: "queued-1"})
	if err != nil || queued.Kind != oarlock.ControlAccepted {
		t.Fatalf("Queue = %+v, %v", queued, err)
	}
	// Held, not sent: the first process is still the only one.
	if argv := h.argv(); len(argv) != 1 {
		t.Fatalf("%d processes ran while the turn was open, want 1 (the queue must be held)", len(argv))
	}

	h.openGate()
	rs := waitRecords(t, h.sess, "second turn end", turnsEnded(2))
	argv := h.waitProcesses(2)
	if got, _ := hasFlag(argv[1], "--resume"); got != "chat-fresh" {
		t.Fatalf("drained process argv = %v, want --resume chat-fresh", argv[1])
	}
	var carried []string
	for _, r := range rs {
		if r.Kind == oarlock.KindRequest && r.Request != nil && r.Request.Action == oarlock.ActionPrompt && r.Request.Input == "second" {
			carried = r.Request.Carries
		}
	}
	if len(carried) != 1 || (carried[0] != "queued-1" && carried[0] != queued.RequestID) {
		t.Fatalf("the drained prompt carries %v, want the queued input (queued-1) or its request %s", carried, queued.RequestID)
	}
	if n := len(responses(rs, oarlock.ResponseExited)); n != 0 {
		t.Fatalf("exited responses = %d: both turns ended with result", n)
	}
}

// ---- refusals -------------------------------------------------------------------

func TestImagesAreRejected(t *testing.T) {
	h := newHelper(t, "turn", oarlock.Options{}, SessionOptions{})
	out, _ := h.sess.Prompt(ctxT(t), "look", oarlock.PromptOptions{InputOptions: oarlock.InputOptions{
		Images: []oarlock.InputImage{{Path: "shot.png", MediaType: "image/png"}},
	}})
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.Unsupported {
		t.Fatalf("Prompt with an image = %+v, want rejected unsupported", out)
	}
	if argv := h.argv(); len(argv) != 0 {
		t.Fatalf("an image prompt started a process: %v", argv)
	}
}

// Empty input never reaches the adapter: the kernel refuses it and the request
// stays in the stream.
func TestEmptyPromptIsRefusedByTheKernel(t *testing.T) {
	h := newHelper(t, "turn", oarlock.Options{}, SessionOptions{})
	out, _ := h.sess.Prompt(ctxT(t), "", oarlock.PromptOptions{})
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.Unsupported {
		t.Fatalf("Prompt(\"\") = %+v, want rejected unsupported", out)
	}
	rs := h.sess.Records(oarlock.Cursor{})
	if n := countAction(rs, oarlock.ActionPrompt); n != 1 {
		t.Fatalf("the refused prompt must stay in the stream, got %d prompt requests", n)
	}
	if argv := h.argv(); len(argv) != 0 {
		t.Fatalf("an empty prompt started a process: %v", argv)
	}
}

// ---- declarations ----------------------------------------------------------------

// No Steer method, no Withdrawer, and the capability table cursors promise.
func TestNoSteerNoWithdrawAndCapabilities(t *testing.T) {
	h := newHelper(t, "turn", oarlock.Options{}, SessionOptions{})
	if _, ok := any(h.sess).(oarlock.Steerer); ok {
		t.Fatal("a Cursor session must not implement oarlock.Steerer: the CLI has no mid-run input")
	}
	if _, ok := any(h.sess).(oarlock.Withdrawer); ok {
		t.Fatal("a Cursor session must not implement oarlock.Withdrawer")
	}
	caps := h.sess.Capabilities()
	if caps.Queue.Durable {
		t.Fatalf("Queue = %+v, want Durable false", caps.Queue)
	}
	if caps.Attribution != oarlock.AttributionOpaque {
		t.Fatalf("Attribution = %q, want opaque", caps.Attribution)
	}
	if caps.Images {
		t.Fatal("Images = true, want false")
	}
}

func TestArgsBaseLaunchAndRefusedTable(t *testing.T) {
	want := []string{"-p", "--output-format", "stream-json", "--stream-partial-output", "--force", "--trust", "--sandbox", "disabled"}
	if got := BaseArgs(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("BaseArgs = %v, want %v", got, want)
	}
	if got := LaunchArgs(oarlock.Options{}, ""); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("LaunchArgs(zero) = %v, want the base args", got)
	}
	got := LaunchArgs(oarlock.Options{Model: "auto"}, "chat-1")
	wantModel := append(append([]string{}, want...), "--model", "auto", "--resume", "chat-1")
	if strings.Join(got, " ") != strings.Join(wantModel, " ") {
		t.Fatalf("LaunchArgs = %v, want %v", got, wantModel)
	}

	refused := Refused()
	if _, ok := refused[oarlock.OptionEnv]; ok {
		t.Fatal("env is honoured and must not be refused")
	}
	for _, o := range oarlock.AllRefusableOptions {
		reason, ok := refused[o]
		if o == oarlock.OptionEnv {
			continue
		}
		if !ok || strings.TrimSpace(reason) == "" {
			t.Fatalf("option %q is not refused with a reason", o)
		}
	}
	// A copy, not the table itself.
	refused[oarlock.OptionEnv] = "mutated"
	if _, ok := Refused()[oarlock.OptionEnv]; ok {
		t.Fatal("Refused() handed out the table itself")
	}

	if err := CheckOptions(oarlock.Options{Env: map[string]*string{"A": nil}}); err != nil {
		t.Fatalf("env must be honoured: %v", err)
	}
	err := CheckOptions(oarlock.Options{Effort: "high"})
	var unsupported *oarlock.UnsupportedOption
	if !errors.As(err, &unsupported) || unsupported.Option != OptionEffort {
		t.Fatalf("CheckOptions(effort) = %v, want *UnsupportedOption for %q", err, OptionEffort)
	}
	if !strings.Contains(unsupported.Reason, "no effort flag") {
		t.Fatalf("effort reason = %q", unsupported.Reason)
	}
}

func TestRuntimeDeclaration(t *testing.T) {
	rt := Runtime()
	if rt.ID != "cursor" || rt.Session == nil {
		t.Fatalf("Runtime() = %+v", rt)
	}
	if len(rt.Refused) != len(Refused()) {
		t.Fatalf("Runtime().Refused has %d entries, Refused() %d", len(rt.Refused), len(Refused()))
	}
	for o, reason := range Refused() {
		if rt.Refused[o] != reason {
			t.Fatalf("Runtime().Refused[%q] = %q, want %q", o, rt.Refused[o], reason)
		}
	}
	if err := rt.CheckOptions(oarlock.Options{MCPServers: []oarlock.MCPServer{{Name: "x", Command: "y"}}}); err == nil {
		t.Fatal("Runtime().CheckOptions must refuse MCP servers")
	}
}

// Every refused option, and effort, is refused before anything starts.
func TestOpenRefusesBeforeAnythingStarts(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	spec := openSpec{
		launch:      sj.Options{Command: exe},
		extraPrefix: []string{"-test.run=TestHelperProcess$", "--"},
		extraEnv:    []string{modeEnv + "=turn", argsEnv + "=" + argsFile},
	}
	cases := []struct {
		name string
		opt  oarlock.Options
		want oarlock.Option
	}{
		{"launchArgs", oarlock.Options{LaunchArgs: []string{"--yolo"}}, oarlock.OptionLaunchArgs},
		{"systemPrompt", oarlock.Options{SystemPrompt: "be nice"}, oarlock.OptionSystemPrompt},
		{"mcp", oarlock.Options{MCPServers: []oarlock.MCPServer{{Name: "s", Command: "c"}}}, oarlock.OptionMCPServers},
		{"resume+systemPrompt", oarlock.Options{Resume: "chat-1", SystemPrompt: "x"}, oarlock.OptionSystemPrompt},
		{"effort", oarlock.Options{Effort: "high"}, OptionEffort},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := open(context.Background(), spec, c.opt)
			var unsupported *oarlock.UnsupportedOption
			if !errors.As(err, &unsupported) {
				t.Fatalf("open(%+v) = %v, want *UnsupportedOption", c.opt, err)
			}
			if unsupported.Option != c.want {
				t.Fatalf("refused %q, want %q", unsupported.Option, c.want)
			}
			if _, statErr := os.Stat(argsFile); !os.IsNotExist(statErr) {
				t.Fatalf("a refused open started a process (%v)", statErr)
			}
		})
	}
}

// A resumed session keeps the chat id it was opened on.
func TestOpenOnResumeKeepsTheChatID(t *testing.T) {
	h := newHelper(t, "turn", oarlock.Options{Resume: "chat-old"}, SessionOptions{})
	if got := h.sess.ChatID(); got != "chat-old" {
		t.Fatalf("ChatID() = %q, want the resumed chat id before any frame", got)
	}
	if out, _ := h.sess.Prompt(ctxT(t), "again", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v", out)
	}
	waitRecords(t, h.sess, "turn end", turnsEnded(1))
	waitLastExit(t, h.sess)
	argv := h.waitProcesses(1)[0]
	if got, ok := hasFlag(argv, "--resume"); !ok || got != "chat-old" {
		t.Fatalf("the first process argv = %v, want --resume chat-old", argv)
	}
}

// ---- the recorded fixtures, through the live path --------------------------------

// The recorded real cursor-agent stream goes through the same pump the live
// socket uses: one frame, one record, Native verbatim, and the frame types the
// profile declares are not tallied as unknown.
func TestRecordedFixtureThroughTheSession(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "cursor", "2026.09.18-auto-normal.stream-json.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 5 {
		t.Fatalf("fixture has %d lines", len(lines))
	}
	h := replayHelper(t, path)
	if out, _ := h.sess.Prompt(ctxT(t), "Reply with the single word: ok", oarlock.PromptOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v", out)
	}
	rs := waitRecords(t, h.sess, "the recorded turn", turnsEnded(1))
	waitLastExit(t, h.sess)
	if id := h.sess.ChatID(); id != "8bf53fd2-dd67-43f4-a867-b43832b4d59d" {
		t.Fatalf("ChatID() = %q, want the recorded session id", id)
	}
	if got := outcomes(rs); len(got) != 1 || got[0].Kind != oarlock.OutcomeCompleted {
		t.Fatalf("outcomes = %+v, want one completed", got)
	}
	fr := frames(rs)
	if len(fr) != len(lines) {
		t.Fatalf("frame records = %d, want %d (one per recorded line)", len(fr), len(lines))
	}
	for i, line := range lines {
		if string(fr[i].Native) != line {
			t.Fatalf("frame %d = %s, want the recorded line %s", i, fr[i].Native, line)
		}
	}
	if n := len(responses(rs, oarlock.ResponseExited)); n != 0 {
		t.Fatalf("exited responses = %d: the recorded result ended the turn, the exit is its lifecycle", n)
	}
	if unknown := h.sess.Unknown(); len(unknown) != 0 {
		t.Fatalf("Unknown() = %v: thinking/tool_call are declared types and must not be tallied", unknown)
	}
}
