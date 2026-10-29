package codex

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

var bg = context.Background()

func prompt(t *testing.T, s *Session, text string, id string) oarlock.ControlOutcome {
	t.Helper()
	out, err := s.Prompt(bg, text, oarlock.PromptOptions{InputOptions: oarlock.InputOptions{InputID: id}})
	if err != nil {
		t.Fatalf("Prompt(%q): %v", text, err)
	}
	return out
}

// ---- handshake -------------------------------------------------------------------

// Acceptance 1 (handshake): version probe, process, initialize with the client
// identity and experimentalApi, initialized notification, thread/start, in that
// order; the open frame carries the model the thread reply reported.
func TestHandshakeThenThreadStart(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{Model: "gpt-5.5", Effort: ""}, Config{})

	got := e.methods()
	want := []string{"PROBE", "START", "initialize", "initialized", "thread/start"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wire order = %v, want %v", got, want)
	}
	start := e.wire()[1]
	if !strings.Contains(start, "app-server") || !strings.Contains(start, "--listen stdio://") {
		t.Errorf("launch line %q must run `app-server ... --listen stdio://`", start)
	}
	init := e.sent("initialize", 0)
	ci, _ := init["clientInfo"].(map[string]any)
	caps, _ := init["capabilities"].(map[string]any)
	if ci["name"] != ClientName || caps["experimentalApi"] != true {
		t.Errorf("initialize params = %v", init)
	}
	ts := e.sent("thread/start", 0)
	if ts["approvalPolicy"] != "never" || ts["experimentalRawEvents"] != true || ts["model"] != "gpt-5.5" {
		t.Errorf("thread/start params = %v", ts)
	}
	if s.ThreadID() != rootThread || s.CodexVersion() == "" {
		t.Errorf("thread %q version %q", s.ThreadID(), s.CodexVersion())
	}
	recs := s.Records(oarlock.Cursor{})
	// The runtime's own words: the initialize reply (it names the build; the
	// version is in the stream, not only in the accessor) and the thread reply.
	if len(recs) != 2 || recs[0].Kind != oarlock.KindFrame || recs[1].Kind != oarlock.KindFrame || recs[1].SessionID != rootThread {
		t.Fatalf("records after open = %v", skeleton(recs))
	}
	if !strings.Contains(string(recs[0].Native), "codex-cli/0.160.1") || len(recs[0].Events) != 0 {
		t.Errorf("initialize frame = %s %+v", recs[0].Native, recs[0].Events)
	}
	if ev := events(recs, oarlock.EventModel); len(ev) != 1 || ev[0].Model != "gpt-5.5" {
		t.Errorf("open frame events = %+v", recs[1].Events)
	}
	if c := s.Capabilities(); c.Attribution != oarlock.AttributionNested || !c.Queue.Durable || !c.Images {
		t.Errorf("capabilities = %+v", c)
	}
}

// ---- one complete turn ------------------------------------------------------------

// Acceptance 1 (complete turn): prompt request, accepted response written at the
// reply's wire position (before turn/started), the frames, and the runtime's own
// turn/completed as the only turn end; span is codex's turn id.
func TestOneCompleteTurn(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{}, Config{})
	out := prompt(t, s, "hi", "in-1")
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v", out)
	}
	waitIdle(t, s)

	want := []string{
		"frame",         // initialize reply
		"frame → model", // thread/start reply
		"toRuntime prompt",
		"response accepted",
		"frame turn/started span=turn-1 → turn_active",
		"frame item/started span=turn-1 → user_message",
		"frame item/agentMessage/delta span=turn-1 → text_delta",
		"frame thread/tokenUsage/updated span=turn-1 → usage",
		"frame turn/completed span=turn-1 → turn_ended",
	}
	got := skeleton(s.Records(oarlock.Cursor{}))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stream:\n got  %q\n want %q", got, want)
	}
	st := fold.StatusOf(s.Records(oarlock.Cursor{})).Value
	if st.Kind != fold.StatusIdle || st.LastTurnOutcome == nil || st.LastTurnOutcome.Kind != oarlock.OutcomeCompleted {
		t.Errorf("status = %+v", st)
	}
	um := events(s.Records(oarlock.Cursor{}), oarlock.EventUserMessage)
	if len(um) != 1 || um[0].Text != "hi" || um[0].InputID != "in-1" || um[0].Evidence != oarlock.EvidenceTurnItem {
		t.Errorf("user_message = %+v", um)
	}
	sent := e.sent("turn/start", 0)
	if sent["threadId"] != rootThread || sent["clientUserMessageId"] != "in-1" {
		t.Errorf("turn/start params = %v", sent)
	}
	u := fold.UsageOf(s.Records(oarlock.Cursor{})).Value
	if u.Total == nil || u.Total.Input != 120 || u.Total.Output != 30 {
		t.Errorf("usage = %+v", u.Total)
	}
	// Dense seq.
	for i, r := range s.Records(oarlock.Cursor{}) {
		if r.Seq != uint64(i+1) {
			t.Fatalf("seq gap at %d: %d", i, r.Seq)
		}
	}
}

func TestPromptWhileRunningIsBusyAndEmptyIsRefused(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{}, Config{})
	if out := prompt(t, s, "hold", ""); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("hold = %+v", out)
	}
	waitFor(t, "running", func() bool { return s.status().Kind == fold.StatusRunning })
	if out := prompt(t, s, "again", ""); out.Kind != oarlock.ControlRejected || out.Code != oarlock.Busy {
		t.Errorf("second prompt = %+v, want busy", out)
	}
	if out := prompt(t, s, "", ""); out.Kind != oarlock.ControlRejected || out.Code != oarlock.Unsupported {
		t.Errorf("empty prompt = %+v, want unsupported", out)
	}
	if n := len(e.methodsOf("turn/start")); n != 1 {
		t.Errorf("turn/start sent %d times, want 1 (a busy/empty prompt is never sent)", n)
	}
}

func (e *fakeEnv) methodsOf(m string) []string {
	var out []string
	for _, x := range e.methods() {
		if x == m {
			out = append(out, x)
		}
	}
	return out
}

func TestFailedTurnIsClassifiedFromCodexErrorInfo(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{}, Config{})
	prompt(t, s, "fail", "")
	waitIdle(t, s)
	end := events(s.Records(oarlock.Cursor{}), oarlock.EventTurnEnded)
	if len(end) != 1 || end[0].Outcome.Kind != oarlock.OutcomeFailed || end[0].Outcome.Failure != oarlock.FailureQuota ||
		end[0].Outcome.Reason != "failed: boom: quota" {
		t.Fatalf("turn_ended = %+v", end)
	}
}

func TestUnknownNotificationIsRetainedAndCounted(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{}, Config{})
	prompt(t, s, "unknown", "")
	waitIdle(t, s)
	var found bool
	for _, r := range s.Records(oarlock.Cursor{}) {
		if strings.Contains(string(r.Native), "mystery/event") {
			found = true
			if len(r.Events) != 0 {
				t.Errorf("an unknown notification must produce no event: %+v", r.Events)
			}
		}
	}
	if !found || s.Unknown()["mystery/event"] != 1 {
		t.Errorf("found=%v unknown=%v", found, s.Unknown())
	}
}

// ---- steer ----------------------------------------------------------------------

// Acceptance 2: a steer whose expectedTurnId is not the active turn is rejected
// explicitly, never silently; the wrong id is never sent when the adapter knows
// better, and when codex itself refuses the answer is runtime_refused with
// codex's words.
func TestSteerExpectedTurnIDMismatchIsRejectedNotSilent(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{}, Config{})
	prompt(t, s, "hold", "")
	waitFor(t, "running", func() bool { return s.status().Kind == fold.StatusRunning })

	out, err := s.SteerTurn(bg, "more", oarlock.InputOptions{}, "turn-WRONG")
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.NoActiveTurn {
		t.Fatalf("pinned mismatch = %+v, want rejected no_active_turn", out)
	}
	if !strings.Contains(out.Reason, "turn-WRONG") || !strings.Contains(out.Reason, "turn-1") {
		t.Errorf("reason must name both ids: %q", out.Reason)
	}
	if n := len(e.methodsOf("turn/steer")); n != 0 {
		t.Errorf("a mismatching steer was sent to codex %d times", n)
	}
	// The request is on the record and has a rejected response.
	sk := skeleton(s.Records(oarlock.Cursor{}))
	if !contains(sk, "toRuntime steer") || !contains(sk, "response rejected:no_active_turn") {
		t.Errorf("stream = %q", sk)
	}

	// The pinned correct id goes out with expectedTurnId and is accepted.
	ok, _ := s.SteerTurn(bg, "more", oarlock.InputOptions{}, "turn-1")
	if ok.Kind != oarlock.ControlAccepted {
		t.Fatalf("pinned steer = %+v", ok)
	}
	if p := e.sent("turn/steer", 0); p["expectedTurnId"] != "turn-1" || p["threadId"] != rootThread {
		t.Errorf("turn/steer params = %v", p)
	}
}

func TestSteerRefusedByCodexIsRuntimeRefused(t *testing.T) {
	e := newFake(t, "steer-mismatch")
	s := e.mustOpen(oarlock.Options{}, Config{})
	prompt(t, s, "hold", "")
	waitFor(t, "running", func() bool { return s.status().Kind == fold.StatusRunning })
	out, _ := s.Steer(bg, "more", oarlock.InputOptions{})
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.RuntimeRefused || !strings.Contains(out.Reason, "does not match the active turn") {
		t.Fatalf("steer = %+v", out)
	}
}

func TestSteerWithNoTurnIsNoActiveTurn(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{}, Config{})
	out, _ := s.Steer(bg, "x", oarlock.InputOptions{})
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.NoActiveTurn {
		t.Fatalf("steer on idle = %+v", out)
	}
	if n := len(e.methodsOf("turn/steer")); n != 0 {
		t.Errorf("steer sent with no turn: %d", n)
	}
}

// An accepted steer that the interrupt then drops is reported as input_dropped
// (turn_interrupted); one codex echoed as a user message is not.
func TestInterruptDropsUnechoedSteer(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{}, Config{})
	prompt(t, s, "hold", "")
	waitFor(t, "running", func() bool { return s.status().Kind == fold.StatusRunning })
	if o, _ := s.Steer(bg, "echoed", oarlock.InputOptions{InputID: "echo-1"}); o.Kind != oarlock.ControlAccepted {
		t.Fatalf("steer = %+v", o)
	}
	if o, _ := s.Steer(bg, "lost", oarlock.InputOptions{InputID: "lost-1"}); o.Kind != oarlock.ControlAccepted {
		t.Fatalf("steer = %+v", o)
	}
	ab, _ := s.Abort(bg)
	if ab.Kind != oarlock.ControlAccepted {
		t.Fatalf("abort = %+v", ab)
	}
	waitIdle(t, s)
	drops := events(s.Records(oarlock.Cursor{}), oarlock.EventInputDropped)
	if len(drops) != 1 || drops[0].InputID != "lost-1" || drops[0].Dropped != oarlock.DroppedTurnInterrupted {
		t.Fatalf("input_dropped = %+v", drops)
	}
	end := events(s.Records(oarlock.Cursor{}), oarlock.EventTurnEnded)
	if len(end) != 1 || end[0].Outcome.Kind != oarlock.OutcomeAborted {
		t.Errorf("turn_ended = %+v", end)
	}
	if p := e.sent("turn/interrupt", 0); p["turnId"] != "turn-1" || p["threadId"] != rootThread {
		t.Errorf("turn/interrupt params = %v", p)
	}
}

// ---- queue and withdraw ----------------------------------------------------------

// Acceptance (queue): the native queue; Withdraw is not implemented (the
// interface is absent, not a flag), Steer is.
func TestNativeQueueAndNoWithdrawer(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{}, Config{})
	var sess oarlock.Session = s
	if _, ok := sess.(oarlock.Withdrawer); ok {
		t.Fatal("*Session must not implement oarlock.Withdrawer (thread/queue/delete is not verified)")
	}
	if _, ok := sess.(oarlock.Steerer); !ok {
		t.Fatal("*Session must implement oarlock.Steerer")
	}
	out, _ := s.Queue(bg, "later", oarlock.InputOptions{InputID: "q-1"})
	if out.Kind != oarlock.ControlAccepted || !strings.Contains(string(out.Response.Response.Native), "sub-9") {
		t.Fatalf("queue = %+v", out)
	}
	p := e.sent("thread/queue/add", 0)
	if p["threadId"] != rootThread || p["clientUserMessageId"] != "q-1" {
		t.Errorf("thread/queue/add params = %v", p)
	}
	// Codex starts the queued turn by itself; the stream adopts it off its own
	// turn/started and ends it on its own turn/completed.
	waitIdle(t, s)
	if n := len(e.methodsOf("turn/start")); n != 0 {
		t.Errorf("a queued input must not be sent as turn/start (%d)", n)
	}
	if empty, _ := s.Queue(bg, "", oarlock.InputOptions{}); empty.Code != oarlock.Unsupported {
		t.Errorf("empty queue = %+v", empty)
	}
}

// ---- abort -------------------------------------------------------------------------

func TestAbortWithoutTurnIsNoActiveTurn(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{}, Config{})
	out, _ := s.Abort(bg)
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.NoActiveTurn {
		t.Fatalf("abort = %+v", out)
	}
}

func TestAbortRefusedByCodexKillsNothing(t *testing.T) {
	e := newFake(t, "interrupt-refuse")
	s := e.mustOpen(oarlock.Options{}, Config{AbortGrace: 200 * time.Millisecond})
	prompt(t, s, "hold", "")
	waitFor(t, "running", func() bool { return s.status().Kind == fold.StatusRunning })
	out, _ := s.Abort(bg)
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.RuntimeRefused || out.Reason != "turn already finished" {
		t.Fatalf("abort = %+v", out)
	}
	time.Sleep(600 * time.Millisecond) // past the grace: the refusal withdrew its timer
	if s.k.Unreachable() != nil {
		t.Fatal("a refused interrupt must not kill the runtime")
	}
}

// The turn does not stop within the grace: oarlock takes over (accepted), kills
// the tree, records the exit and invents no turn_ended.
func TestAbortTakeOverAfterGrace(t *testing.T) {
	e := newFake(t, "interrupt-ack-only")
	s := e.mustOpen(oarlock.Options{}, Config{AbortGrace: 300 * time.Millisecond})
	prompt(t, s, "hold", "")
	waitFor(t, "running", func() bool { return s.status().Kind == fold.StatusRunning })
	out, _ := s.Abort(bg)
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("abort = %+v", out)
	}
	waitFor(t, "exited response", func() bool { return s.k.Unreachable() != nil })
	if hasEvent(s.Records(oarlock.Cursor{}), oarlock.EventTurnEnded) {
		t.Error("a turn_ended was invented for the take-over")
	}
	if o := prompt(t, s, "x", ""); o.Code != oarlock.RuntimeExited {
		t.Errorf("prompt after exit = %+v", o)
	}
}

func TestRuntimeCrashMidTurn(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{}, Config{})
	prompt(t, s, "crash", "")
	waitFor(t, "exit", func() bool { return s.k.Unreachable() != nil })
	recs := s.Records(oarlock.Cursor{})
	last := recs[len(recs)-1]
	if last.Kind != oarlock.KindResponse || last.Response.Kind != oarlock.ResponseExited || last.RequestID != "" ||
		last.Response.ExitCode == nil || *last.Response.ExitCode != 9 {
		t.Fatalf("last record = %+v", last)
	}
	if hasEvent(recs, oarlock.EventTurnEnded) {
		t.Error("turn_ended invented for a crash")
	}
}

func TestDisposeIsIdempotentAndAnswersWithExit(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{}, Config{})
	for i := 0; i < 2; i++ {
		if err := s.Dispose(bg); err != nil {
			t.Fatalf("dispose %d: %v", i, err)
		}
	}
	sk := skeleton(s.Records(oarlock.Cursor{}))
	want := []string{"frame", "frame → model", "toRuntime dispose", "response accepted", "response exited"}
	if !reflect.DeepEqual(sk, want) {
		t.Fatalf("stream = %q", sk)
	}
	recs := s.Records(oarlock.Cursor{})
	if recs[4].RequestID != recs[2].ID {
		t.Errorf("exited response must answer the dispose request: %+v", recs[4])
	}
	if o := prompt(t, s, "x", ""); o.Code != oarlock.Disposed && o.Code != oarlock.RuntimeExited {
		t.Errorf("prompt after dispose = %+v", o)
	}
}

// ---- children ----------------------------------------------------------------------

// Acceptance 3: a notification of another thread is a record of a derived child
// session: SessionID is the child thread, AgentPath stays empty (a nested child
// is the root agent of its own session), the edge is reported once by the
// collaboration item, and the child's turn end does not end the root's turn.
func TestChildThreadNotificationsAreChildSessionRecords(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{}, Config{})
	prompt(t, s, "child", "")
	waitIdle(t, s)
	recs := s.Records(oarlock.Cursor{})

	var childFrames int
	for _, r := range recs {
		if r.Kind != oarlock.KindFrame || r.SessionID != "thread-child" {
			continue
		}
		childFrames++
		if len(r.AgentPath) != 0 {
			t.Errorf("child frame has AgentPath %v; nested children are sessions, not agent paths", r.AgentPath)
		}
	}
	if childFrames != 4 {
		t.Errorf("child frames = %d, want 4 (turn/started, delta, usage, turn/completed; the approval is a request record)", childFrames)
	}
	// Root records stay on the root session.
	for _, r := range recs {
		if r.SessionID == "thread-child" {
			continue
		}
		if r.SessionID != rootThread {
			t.Errorf("record %d session %q", r.Seq, r.SessionID)
		}
	}

	// The edge, once, from the runtime's own item.
	links := events(recs, oarlock.EventSessionLink)
	if len(links) != 1 || links[0].Parent != rootThread || links[0].Child != "thread-child" || links[0].Via != "tool_call" {
		t.Fatalf("session_linked = %+v", links)
	}
	g := s.Graph()
	if len(g.Edges) != 1 || g.Edges[0].Parent != rootThread || g.Edges[0].Child != "thread-child" {
		t.Errorf("graph edges = %+v", g.Edges)
	}
	nodes := map[string]bool{}
	for _, n := range g.Nodes {
		nodes[n.ID] = true
	}
	if !nodes[rootThread] || !nodes["thread-child"] {
		t.Errorf("graph nodes = %+v", g.Nodes)
	}

	// Task facts come from the root thread's own subAgentActivity.
	ts := events(recs, oarlock.EventTaskStarted)
	if len(ts) != 1 || ts[0].Task.TaskID != "thread-child" || ts[0].Task.ChildSessionID != "thread-child" || ts[0].Task.Description != "/root/helper" {
		t.Errorf("task_started = %+v", ts)
	}

	// The child's turn/completed did not end the root's turn: the root fold sees
	// exactly one turn_ended, from the root session.
	st := fold.StatusOf(recs).Value
	if st.Kind != fold.StatusIdle || st.LastTurnOutcome == nil {
		t.Errorf("status = %+v", st)
	}
	// ... and spans are the child's own turn id.
	var childSpan bool
	for _, r := range recs {
		if r.SessionID == "thread-child" && r.SpanID == "turn-child" {
			childSpan = true
		}
	}
	if !childSpan {
		t.Error("child frames must carry the child's turn id as SpanID")
	}
}

// Acceptance 6: parent and child each report their own usage; the parent's Total
// counts only itself; WithChildren adds the child once.
func TestUsageParentAndChildReportSeparately(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{}, Config{})
	prompt(t, s, "child", "")
	waitIdle(t, s)
	u := fold.UsageOf(s.Records(oarlock.Cursor{})).Value
	if u.Total == nil || u.Total.Input != 100 || u.Total.Output != 10 {
		t.Fatalf("parent Total = %+v, want 100/10 (own only)", u.Total)
	}
	if u.WithChildren == nil || u.WithChildren.Input != 130 || u.WithChildren.Output != 13 {
		t.Fatalf("WithChildren = %+v, want 130/13", u.WithChildren)
	}
	if len(u.ByAgent) != 0 || u.Unattributed != nil {
		t.Errorf("a nested child must not appear as an agent of the parent: %+v / %+v", u.ByAgent, u.Unattributed)
	}
	// The child's tokens sit on the child session's own usage frame.
	var seen bool
	for _, r := range s.Records(oarlock.Cursor{}) {
		for _, ev := range r.Events {
			if ev.Kind == oarlock.EventUsage && r.SessionID == "thread-child" && ev.Usage.Tokens.Input == 30 {
				seen = true
			}
		}
	}
	if !seen {
		t.Error("child usage frame missing")
	}
}

// ---- approvals ------------------------------------------------------------------------

// Acceptance 4: an approval is a toApp request record (verbatim, open
// vocabulary) the host can answer; the answer reaches codex under the request's
// own JSON-RPC id and is recorded as an answered response.
func TestApprovalIsAToAppRequestTheHostAnswers(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{}, Config{ApprovalPolicy: "on-request"})
	if p := e.sent("thread/start", 0); p["approvalPolicy"] != "on-request" {
		t.Fatalf("approvalPolicy = %v", p["approvalPolicy"])
	}
	prompt(t, s, "ask", "")
	waitFor(t, "toApp request", func() bool {
		for _, r := range s.Records(oarlock.Cursor{}) {
			if r.Kind == oarlock.KindRequest && r.Direction == oarlock.ToApp {
				return true
			}
		}
		return false
	})
	var req oarlock.Record
	for _, r := range s.Records(oarlock.Cursor{}) {
		if r.Kind == oarlock.KindRequest && r.Direction == oarlock.ToApp {
			req = r
		}
	}
	if req.ID != "srv-7" || req.Request.Action != oarlock.ActionNative || req.Request.NativeType != "item/commandExecution/requestApproval" ||
		!strings.Contains(string(req.Request.Native), "rm -rf build") || req.SpanID != "turn-1" {
		t.Fatalf("toApp request = %+v", req)
	}
	// The status stays running: nothing answered it for the host.
	if s.status().Kind != fold.StatusRunning {
		t.Errorf("status = %+v", s.status())
	}

	if err := s.Answer(bg, "nope", map[string]any{}); !errors.Is(err, ErrUnknownRequest) {
		t.Errorf("unknown id: %v", err)
	}
	if err := s.Answer(bg, "srv-7", map[string]any{"decision": "accept"}); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if err := s.Answer(bg, "srv-7", map[string]any{"decision": "accept"}); !errors.Is(err, ErrAlreadyAnswered) {
		t.Errorf("second answer: %v", err)
	}
	waitIdle(t, s)

	// Codex received a JSON-RPC response with the same id and our result.
	var sentBack bool
	for _, l := range e.wire() {
		if strings.Contains(l, `"id":"srv-7"`) && strings.Contains(l, `"result"`) && strings.Contains(l, `"decision":"accept"`) {
			sentBack = true
		}
	}
	if !sentBack {
		t.Errorf("codex never saw the answer: %q", e.wire())
	}
	recs := s.Records(oarlock.Cursor{})
	var answered *oarlock.Record
	for i := range recs {
		if recs[i].Kind == oarlock.KindResponse && recs[i].RequestID == "srv-7" {
			answered = &recs[i]
		}
	}
	if answered == nil || answered.Response.Kind != oarlock.ResponseAnswered {
		t.Fatalf("no answered response: %q", skeleton(recs))
	}
	// codex's own serverRequest/resolved after our answer is not a cancellation.
	if c := events(recs, oarlock.EventAppRequestCancelled); len(c) != 0 {
		t.Errorf("app_request_cancelled after our own answer: %+v", c)
	}
}

// A request the runtime cancels itself stays on the record, unanswered, with an
// app_request_cancelled event.
func TestRuntimeCancelledApprovalStaysUnanswered(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{}, Config{ApprovalPolicy: "on-request"})
	prompt(t, s, "ask-cancel", "")
	waitIdle(t, s)
	recs := s.Records(oarlock.Cursor{})
	c := events(recs, oarlock.EventAppRequestCancelled)
	if len(c) != 1 || c[0].RequestID != "srv-8" {
		t.Fatalf("app_request_cancelled = %+v", c)
	}
	for _, r := range recs {
		if r.Kind == oarlock.KindResponse && r.RequestID == "srv-8" {
			t.Errorf("a cancelled request got a response: %+v", r)
		}
	}
	if err := s.Answer(bg, "srv-8", map[string]any{}); !errors.Is(err, ErrUnknownRequest) {
		t.Errorf("answering a cancelled request: %v", err)
	}
}

// A child thread's approval is a toApp request recorded on the child session.
func TestChildApprovalIsRecordedOnTheChildSession(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{}, Config{})
	prompt(t, s, "child", "")
	waitIdle(t, s)
	for _, r := range s.Records(oarlock.Cursor{}) {
		if r.Kind == oarlock.KindRequest && r.Direction == oarlock.ToApp {
			if r.ID != "srv-child" || r.SessionID != "thread-child" {
				t.Errorf("child approval = id %q session %q", r.ID, r.SessionID)
			}
			return
		}
	}
	t.Fatal("child approval missing")
}

// ---- compaction ------------------------------------------------------------------------

// Acceptance 5: the contextCompaction item is compaction_started/ended; the
// deprecated thread/compacted that says the same thing closes nothing twice.
func TestCompactionMapsToStartedAndEndedOnce(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{}, Config{})
	prompt(t, s, "compact", "")
	waitIdle(t, s)
	recs := s.Records(oarlock.Cursor{})
	st, en := events(recs, oarlock.EventCompactionStarted), events(recs, oarlock.EventCompactionEnded)
	if len(st) != 1 || len(en) != 1 || en[0].CompactionResult != oarlock.CompactionCompleted {
		t.Fatalf("compaction events started=%+v ended=%+v", st, en)
	}
	// The status fold saw compacting between them.
	var mid fold.Status
	for i := range recs {
		for _, ev := range recs[i].Events {
			if ev.Kind == oarlock.EventCompactionStarted {
				mid = fold.StatusOf(recs[:i+1]).Value
			}
		}
	}
	if mid.Phase.Kind != fold.PhaseCompacting {
		t.Errorf("phase after compaction_started = %+v", mid.Phase)
	}
}

func TestLegacyThreadCompactedAloneEndsAnOpenCompaction(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{}, Config{})
	prompt(t, s, "compact-legacy", "")
	waitIdle(t, s)
	recs := s.Records(oarlock.Cursor{})
	if len(events(recs, oarlock.EventCompactionStarted)) != 1 || len(events(recs, oarlock.EventCompactionEnded)) != 1 {
		t.Fatalf("stream = %q", skeleton(recs))
	}
}

// ---- version window and refusals --------------------------------------------------------

// Acceptance 6: outside the window the open is refused before any session
// process starts: the log holds the version probe and nothing else.
func TestVersionOutsideWindowIsRefusedBeforeOpen(t *testing.T) {
	for _, v := range []string{"codex-cli 0.148.9", "codex-cli 0.166.0", "codex-cli 1.0.0", "not a version"} {
		e := newFake(t, "", versionEnv, v)
		s, err := e.open(oarlock.Options{}, Config{})
		if s != nil {
			_ = s.Dispose(bg)
			t.Fatalf("version %q: a session was opened", v)
		}
		var ve *VersionError
		if !errors.As(err, &ve) || !errors.Is(err, ErrVersionOutOfWindow) {
			t.Fatalf("version %q: err = %v, want *VersionError", v, err)
		}
		if ve.Min != MinVersion || ve.Max != MaxVersionExclusive || !strings.Contains(err.Error(), v) {
			t.Errorf("version %q: %+v / %v", v, ve, err)
		}
		if got := e.methods(); !reflect.DeepEqual(got, []string{"PROBE"}) {
			t.Errorf("version %q: wire = %v; only the --version probe may have run", v, got)
		}
	}
}

func TestVersionInsideWindowOpens(t *testing.T) {
	for _, v := range []string{"codex-cli 0.149.0", "codex-cli 0.165.9", "0.160.1\n"} {
		e := newFake(t, "", versionEnv, v)
		s := e.mustOpen(oarlock.Options{}, Config{})
		if !strings.Contains(s.CodexVersion(), strings.TrimSpace(v)) {
			t.Errorf("CodexVersion = %q for %q", s.CodexVersion(), v)
		}
	}
}

func TestVersionProbeFailuresAreTyped(t *testing.T) {
	for _, v := range []string{"fail", "empty"} {
		e := newFake(t, "", versionEnv, v)
		_, err := e.open(oarlock.Options{}, Config{})
		var pe *VersionProbeError
		if !errors.As(err, &pe) || !errors.Is(err, ErrVersionProbe) {
			t.Fatalf("%s: err = %v", v, err)
		}
		if v == "fail" && (pe.ExitCode != 3 || !strings.Contains(pe.StderrTail, "broken install")) {
			t.Errorf("probe error = %+v", pe)
		}
		if got := e.methods(); !reflect.DeepEqual(got, []string{"PROBE"}) {
			t.Errorf("%s: wire = %v", v, got)
		}
	}
}

func TestVersionProbeHangIsKilled(t *testing.T) {
	e := newFake(t, "", versionEnv, "hang")
	start := time.Now()
	_, err := e.open(oarlock.Options{}, Config{VersionTimeout: 300 * time.Millisecond})
	var pe *VersionProbeError
	if !errors.As(err, &pe) || !pe.Timeout {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 15*time.Second {
		t.Errorf("hung probe took %v", time.Since(start))
	}
}

// The running build's own userAgent is checked too: a wrapper that answers
// --version in the window but runs another build is refused, and killed.
func TestInitializeUserAgentOutsideWindowIsRefused(t *testing.T) {
	e := newFake(t, "", uaEnv, "codex-cli/0.170.0")
	_, err := e.open(oarlock.Options{}, Config{})
	if !errors.Is(err, ErrVersionOutOfWindow) {
		t.Fatalf("err = %v", err)
	}
	if got := e.methods(); contains(got, "thread/start") {
		t.Errorf("thread/start was sent to a build outside the window: %v", got)
	}
}

func TestSchemaMismatchFailsTheOpen(t *testing.T) {
	e := newFake(t, "no-thread")
	_, err := e.open(oarlock.Options{}, Config{})
	var se *SchemaError
	if !errors.As(err, &se) || !errors.Is(err, ErrSchema) || se.Method != "thread/start" {
		t.Fatalf("err = %v", err)
	}
}

func TestRefusedOptionsFailBeforeAnythingStarts(t *testing.T) {
	e := newFake(t, "")
	table := Refused()
	for _, o := range oarlock.AllRefusableOptions {
		opt := optionFor(t, o)
		sess, err := e.open(opt, Config{})
		if sess != nil {
			_ = sess.Dispose(context.Background())
		}
		reason, refused := table[o]
		var un *oarlock.UnsupportedOption
		if refused {
			if o == oarlock.OptionMCPServerCredentials {
				// derived from mcpServers: Check names the first refused option
				if !errors.As(err, &un) {
					t.Errorf("option %s: err = %v", o, err)
				}
				continue
			}
			if !errors.As(err, &un) || un.Option != o || un.Reason != reason {
				t.Errorf("option %s: err = %v, want *UnsupportedOption with the table's reason", o, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("option %s is honoured but open failed: %v", o, err)
		}
	}
	// Refused ones never cost a probe: the log only has probes of honoured opens.
	for _, l := range e.wire() {
		if strings.Contains(l, "mcp") {
			t.Errorf("a refused option reached the wire: %s", l)
		}
	}
}

func optionFor(t *testing.T, o oarlock.Option) oarlock.Options {
	t.Helper()
	dir := t.TempDir()
	v := "x"
	opt := oarlock.Options{Cwd: dir}
	switch o {
	case oarlock.OptionSystemPrompt:
		opt.SystemPrompt = "be brief"
	case oarlock.OptionAppendSystemPrompt:
		opt.AppendSystemPrompt = "be kind"
	case oarlock.OptionEnv:
		opt.Env = map[string]*string{"FOO": &v}
	case oarlock.OptionMCPServers, oarlock.OptionMCPServerCredentials:
		opt.MCPServers = []oarlock.MCPServer{{Name: "echo", Command: "echo", Env: map[string]string{"TOKEN": "t"}}}
	case oarlock.OptionDisallowedTools:
		opt.DisallowedTools = []string{"mcp__a__b"}
	case oarlock.OptionLaunchArgs:
		opt.LaunchArgs = []string{"-c", "foo=1"}
	case oarlock.OptionServiceTier:
		opt.ServiceTier = ""
	case oarlock.OptionAdditionalDirectories:
		opt.AdditionalDirectories = nil
	case oarlock.OptionOutputSchema:
		opt.OutputSchema = nil
	case oarlock.OptionName:
		opt.Name = ""
	case oarlock.OptionUserConfiguration:
		opt.UserConfiguration = oarlock.UserConfigurationExclude
	}
	return opt
}

func TestRefusedTableCoversEveryFrozenOption(t *testing.T) {
	table := Refused()
	for _, o := range oarlock.AllRefusableOptions {
		_, refused := table[o]
		honoured := false
		for _, h := range honouredOptions {
			honoured = honoured || h == o
		}
		if refused == honoured {
			t.Errorf("option %s: refused=%v honoured=%v; exactly one", o, refused, honoured)
		}
	}
	if len(table) != len(oarlock.AllRefusableOptions)-len(honouredOptions) {
		t.Errorf("table size %d", len(table))
	}
	rt := Runtime()
	if rt.ID != "codex" || rt.Session == nil || len(rt.Refused) != len(table) {
		t.Errorf("Runtime() = %+v", rt)
	}
	// Mutating the copy does not change the table Open checks.
	delete(table, oarlock.OptionMCPServers)
	if _, ok := Refused()[oarlock.OptionMCPServers]; !ok {
		t.Error("Refused() must hand out a copy")
	}
}

func TestOptionsReachThreadStart(t *testing.T) {
	e := newFake(t, "")
	dir := t.TempDir()
	extra := t.TempDir()
	v := "1"
	e.mustOpen(oarlock.Options{
		Cwd: dir, Model: "gpt-5.5", SystemPrompt: "sys", AppendSystemPrompt: "dev",
		AdditionalDirectories: []string{extra}, Env: map[string]*string{"CODEX_X": &v},
	}, Config{Sandbox: "danger-full-access"})
	p := e.sent("thread/start", 0)
	if p["baseInstructions"] != "sys" || p["developerInstructions"] != "dev" || p["model"] != "gpt-5.5" {
		t.Errorf("thread/start = %v", p)
	}
	roots, _ := p["runtimeWorkspaceRoots"].([]any)
	if len(roots) != 2 || roots[0] != dir || roots[1] != extra {
		t.Errorf("runtimeWorkspaceRoots = %v", roots)
	}
	if line := e.wire()[1]; !strings.Contains(line, `-c sandbox_mode="danger-full-access"`) {
		t.Errorf("launch line = %q", line)
	}
}

func TestModelKeptByCodexRefusesTheOpen(t *testing.T) {
	e := newFake(t, "model-kept")
	_, err := e.open(oarlock.Options{Model: "gpt-5.5"}, Config{})
	if err == nil || !strings.Contains(err.Error(), "kept model gpt-other although gpt-5.5 was requested") {
		t.Fatalf("err = %v", err)
	}
}

func TestUnconfirmedWorkspaceRootsRefuseTheOpen(t *testing.T) {
	e := newFake(t, "roots-unconfirmed")
	_, err := e.open(oarlock.Options{AdditionalDirectories: []string{t.TempDir()}}, Config{})
	if err == nil || !strings.Contains(err.Error(), "runtimeWorkspaceRoots") {
		t.Fatalf("err = %v", err)
	}
}

// ---- resume --------------------------------------------------------------------------------

func TestResumeUsesThreadResumeAndAFreshStream(t *testing.T) {
	e := newFake(t, "")
	s := e.mustOpen(oarlock.Options{Resume: "thread-old"}, Config{})
	if s.ThreadID() != "thread-old" {
		t.Fatalf("thread = %q", s.ThreadID())
	}
	p := e.sent("thread/resume", 0)
	if p["threadId"] != "thread-old" || p["excludeTurns"] != true {
		t.Errorf("thread/resume = %v", p)
	}
	if _, ok := p["experimentalRawEvents"]; ok {
		t.Error("experimentalRawEvents belongs to thread/start only")
	}
	recs := s.Records(oarlock.Cursor{})
	if len(recs) != 2 || recs[0].Seq != 1 || recs[1].SessionID != "thread-old" {
		t.Errorf("a resume is a new stream instance: %q", skeleton(recs))
	}
	if _, err := e.open(oarlock.Options{Resume: "thread-old", AppendSystemPrompt: "x"}, Config{}); err == nil {
		t.Error("appendSystemPrompt on resume must be refused")
	} else {
		var un *oarlock.UnsupportedOption
		if !errors.As(err, &un) || un.Option != oarlock.OptionAppendSystemPrompt {
			t.Errorf("err = %v", err)
		}
	}
}

func TestResumeOfMissingThreadIsSessionNotFound(t *testing.T) {
	e := newFake(t, "norollout")
	_, err := e.open(oarlock.Options{Resume: "thread-gone"}, Config{})
	var nf *oarlock.SessionNotFound
	if !errors.As(err, &nf) || nf.SessionID != "thread-gone" {
		t.Fatalf("err = %v", err)
	}
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != -32600 {
		t.Errorf("the native cause must stay reachable: %v", err)
	}
	e2 := newFake(t, "archived")
	_, err = e2.open(oarlock.Options{Resume: "x"}, Config{})
	if err == nil || errors.As(err, &nf) || !strings.Contains(err.Error(), "thread/resume failed") {
		t.Errorf("an archived thread is not 'not found': %v", err)
	}
}

func TestProcessExitedDuringOpenIsAnError(t *testing.T) {
	e := newFake(t, "exit-after-open")
	s, err := e.open(oarlock.Options{}, Config{})
	if err != nil {
		// the exit raced the open: also acceptable, but must be typed
		if !errors.Is(err, ErrExited) {
			t.Fatalf("err = %v", err)
		}
		return
	}
	defer s.Dispose(bg)
	waitFor(t, "exit", func() bool { return s.k.Unreachable() != nil })
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}
