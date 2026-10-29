package codex

// Offline replay of OAR's recorded raw app-server notifications
// (testdata/codex-*.raw.jsonl, recorded on codex 0.149-0.165 and scrubbed to the
// consumed fields; the .projected.txt files are OAR's own expected tables). Two
// layers: the pure fold against the table, and the whole session fed by the fake
// app-server. No network, no codex binary.

import (
	"bufio"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

func readLines(t *testing.T, name string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			out = append(out, sc.Text())
		}
	}
	return out
}

func describe(method string, cmd frameCmd) string {
	where := ""
	if cmd.SessionID != "" {
		where = " @thread:" + cmd.SessionID
	}
	var evs []string
	for _, ev := range cmd.Events {
		switch ev.Kind {
		case oarlock.EventToolCallStarted:
			evs = append(evs, "tool_call_started "+ev.Tool)
		case oarlock.EventReasoning:
			k := "empty"
			switch {
			case ev.Text != "":
				k = "text"
			case ev.Redacted:
				k = "redacted"
			}
			evs = append(evs, "reasoning "+k)
		case oarlock.EventTurnEnded:
			evs = append(evs, "turn_ended "+string(ev.Outcome.Kind))
		default:
			evs = append(evs, string(ev.Kind))
		}
	}
	row := "event" + where
	if len(evs) > 0 {
		row += " → " + strings.Join(evs, ", ")
	}
	for _, l := range cmd.Links {
		row += ", link " + l.Parent + " → " + l.Child + " (tool_call)"
	}
	return row
}

func foldFixture(t *testing.T, name string) (string, codexState, []frameCmd) {
	t.Helper()
	st := initialState(rootThread, false)
	var rows []string
	var cmds []frameCmd
	for _, line := range readLines(t, name) {
		m := parseObj([]byte(line))
		method, ok := m.str("method")
		if !ok {
			continue
		}
		var cmd frameCmd
		st, cmd = foldNotification(st, method, m)
		rows = append(rows, method+strings.Repeat(" ", max(0, 28-len(method)))+" │ "+describe(method, cmd))
		cmds = append(cmds, cmd)
	}
	return strings.Join(rows, "\n") + "\n", st, cmds
}

// The recorded notification streams fold to OAR's expected tables, line for line
// (event kinds, child-session attribution, collaboration links).
func TestRecordedStreamsFoldToTheReferenceTables(t *testing.T) {
	for _, name := range []string{"tool-round", "subagent", "file-change", "mcp-echo"} {
		t.Run(name, func(t *testing.T) {
			got, st, _ := foldFixture(t, "codex-"+name+".raw.jsonl")
			wantRaw, err := os.ReadFile(filepath.Join("testdata", "codex-"+name+".projected.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if got != string(wantRaw) {
				gl, wl := strings.Split(got, "\n"), strings.Split(string(wantRaw), "\n")
				for i := range wl {
					if i >= len(gl) || gl[i] != wl[i] {
						g := ""
						if i < len(gl) {
							g = gl[i]
						}
						t.Fatalf("line %d:\n got  %q\n want %q", i+1, g, wl[i])
					}
				}
				t.Fatalf("got %d lines, want %d", len(gl), len(wl))
			}
			if len(st.unknown) != 0 {
				t.Errorf("unknown methods in a recorded stream: %v", st.unknown)
			}
		})
	}
}

// Peers recorded on 0.158.0: root starts alpha and beta, alpha messages beta
// and starts gamma, gamma messages the root. Each reports on its own thread, and
// only an item about the reporting thread's own child is a task change or edge.
func TestPeersChangeOnlyTheirOwnChildrensTasksAndEdges(t *testing.T) {
	_, _, cmds := foldFixture(t, "codex-peers.raw.jsonl")
	var tasks, edges []string
	seen := map[string]bool{}
	for _, c := range cmds {
		who := c.SessionID
		if who == "" {
			who = rootThread
		}
		for _, ev := range c.Events {
			switch ev.Kind {
			case oarlock.EventTaskStarted, oarlock.EventTaskUpdated, oarlock.EventTaskEnded:
				tasks = append(tasks, who+": "+string(ev.Kind)+" "+ev.Task.TaskID)
			}
		}
		for _, l := range c.Links {
			k := l.Parent + " → " + l.Child
			if !seen[k] {
				seen[k] = true
				edges = append(edges, k)
			}
		}
	}
	wantTasks := []string{
		"thread-root: task_started thread-alpha",
		"thread-root: task_started thread-beta",
		"thread-alpha: task_started thread-gamma",
		"thread-root: task_ended thread-beta",
		"thread-alpha: task_ended thread-gamma",
		"thread-root: task_ended thread-alpha",
	}
	wantEdges := []string{"thread-root → thread-alpha", "thread-root → thread-beta", "thread-alpha → thread-gamma"}
	if !reflect.DeepEqual(tasks, wantTasks) {
		t.Errorf("tasks:\n got  %q\n want %q", tasks, wantTasks)
	}
	if !reflect.DeepEqual(edges, wantEdges) {
		t.Errorf("edges:\n got  %q\n want %q", edges, wantEdges)
	}
}

// After a resume no `started` item names the old children: the agentPath decides
// whose child an unstarted thread is.
func TestPeersAfterAResumeAreDecidedByAgentPath(t *testing.T) {
	st := initialState(rootThread, true)
	activity := func(thread, kind, child, path string) (frameCmd, codexState) {
		var cmd frameCmd
		var next codexState
		next, cmd = foldNotification(st, "item/completed", obj{"threadId": thread, "item": map[string]any{
			"type": "subAgentActivity", "id": thread + "-" + kind, "kind": kind, "agentThreadId": child, "agentPath": path}})
		return cmd, next
	}
	var got []string
	for _, a := range [][4]string{
		{"thread-alpha", "interacted", "thread-beta", "/root/beta"},
		{"thread-alpha", "completed", "thread-gamma", "/root/alpha/gamma"},
		{rootThread, "completed", "thread-beta", "/root/beta"},
		{rootThread, "interacted", "thread-gamma", "/root/alpha/gamma"},
	} {
		cmd, next := activity(a[0], a[1], a[2], a[3])
		st = next
		for _, ev := range cmd.Events {
			if ev.Kind == oarlock.EventTaskEnded || ev.Kind == oarlock.EventTaskUpdated {
				got = append(got, a[0]+": "+string(ev.Kind)+" "+ev.Task.TaskID)
			}
		}
		for _, l := range cmd.Links {
			got = append(got, l.Parent+" → "+l.Child)
		}
	}
	want := []string{"thread-alpha: task_ended thread-gamma", "thread-alpha → thread-gamma", "thread-root: task_ended thread-beta", "thread-root → thread-beta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// The whole session, fed a recorded stream by the fake app-server: one prompt,
// the notifications as codex wrote them, the runtime's own turn end.
func replaySession(t *testing.T, fixture string) *Session {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	e := newFake(t, "", fixtureEnv, abs)
	s := e.mustOpen(oarlock.Options{}, Config{})
	if out := prompt(t, s, "replay", ""); out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt = %+v", out)
	}
	return s
}

func TestReplayToolRoundThroughTheSession(t *testing.T) {
	s := replaySession(t, "codex-tool-round.raw.jsonl")
	waitIdle(t, s)
	recs := s.Records(oarlock.Cursor{})
	if got := len(events(recs, oarlock.EventTextDelta)); got < 10 {
		t.Errorf("text deltas = %d", got)
	}
	ts, te := events(recs, oarlock.EventToolCallStarted), events(recs, oarlock.EventToolCallEnded)
	if len(ts) != 1 || len(te) != 1 || ts[0].CallID != te[0].CallID || ts[0].Tool != "commandExecution" {
		t.Fatalf("tool events = %+v / %+v", ts, te)
	}
	// The recorded item carries no status: the result stays unreported, never inferred.
	if te[0].Result != oarlock.ToolUnknown {
		t.Errorf("tool result = %q, want unreported", te[0].Result)
	}
	end := events(recs, oarlock.EventTurnEnded)
	if len(end) != 1 || end[0].Outcome.Kind != oarlock.OutcomeCompleted {
		t.Errorf("turn_ended = %+v", end)
	}
	if s.Unknown() != nil && len(s.Unknown()) != 0 {
		t.Errorf("unknown = %v", s.Unknown())
	}
	// Every native frame is retained verbatim (a JSON-RPC notification line).
	for _, r := range recs {
		if r.Kind == oarlock.KindFrame && r.Native == nil {
			t.Errorf("record %d has no Native", r.Seq)
		}
	}
}

func TestReplayFileChangeAndMCPKeepToolDetail(t *testing.T) {
	s := replaySession(t, "codex-mcp-echo.raw.jsonl")
	waitIdle(t, s)
	recs := s.Records(oarlock.Cursor{})
	te := events(recs, oarlock.EventToolCallEnded)
	if len(te) != 2 || te[0].Result != oarlock.ToolOK || len(te[0].Content) != 1 || !strings.Contains(te[0].Content[0].Text, "echo:stdio-call") ||
		!strings.Contains(te[1].Content[0].Text, "echo:http-call") {
		t.Fatalf("mcp tool_call_ended = %+v", te)
	}
	ts := events(recs, oarlock.EventToolCallStarted)
	if len(ts) != 2 || ts[0].Input != `{"text":"stdio-call"}` {
		t.Errorf("mcp tool_call_started = %+v", ts)
	}
	if r := events(recs, oarlock.EventReasoning); len(r) == 0 {
		t.Error("reasoning events missing")
	} else if !r[len(r)-1].Redacted {
		t.Errorf("the encrypted reasoning item must read as redacted: %+v", r)
	}
	// The recorded usage is all zeros: a reported zero stays zero.
	u := events(recs, oarlock.EventUsage)
	if len(u) == 0 || u[0].Usage.Tokens == nil || u[0].Usage.Tokens.Input != 0 || u[0].Usage.Tokens.CacheRead == nil {
		t.Errorf("zero usage = %+v", u)
	}
}

// Acceptance 3 on a recorded stream: the child's notifications are records of
// the child session, the edge is reported once, and the root's turn ends once.
func TestReplaySubagentThroughTheSession(t *testing.T) {
	s := replaySession(t, "codex-subagent.raw.jsonl")
	waitFor(t, "the recorded stream", func() bool { return hasEvent(s.Records(oarlock.Cursor{}), oarlock.EventTaskEnded) })
	waitFor(t, "idle", func() bool { return s.status().Kind == fold.StatusIdle })
	recs := s.Records(oarlock.Cursor{})

	sessions := map[string]int{}
	for _, r := range recs {
		if r.Kind == oarlock.KindFrame {
			sessions[r.SessionID]++
			if len(r.AgentPath) != 0 {
				t.Errorf("record %d AgentPath = %v", r.Seq, r.AgentPath)
			}
		}
	}
	if sessions["thread-child"] != 7 || sessions[rootThread] != 18 {
		t.Errorf("frames per session = %v, want child 7 (3 status, turn/started, 2 items, turn/completed) and root 18 (+ the open replies)", sessions)
	}
	links := events(recs, oarlock.EventSessionLink)
	if len(links) != 1 || links[0].Parent != rootThread || links[0].Child != "thread-child" {
		t.Errorf("session_linked = %+v (the edge is named by two items and a third later; once in the graph)", links)
	}
	if g := s.Graph(); len(g.Edges) != 1 {
		t.Errorf("graph = %+v", g)
	}
	var kinds []string
	for _, ev := range append(events(recs, oarlock.EventTaskStarted), events(recs, oarlock.EventTaskEnded)...) {
		kinds = append(kinds, string(ev.Kind)+" "+ev.Task.TaskID)
	}
	sort.Strings(kinds)
	if !reflect.DeepEqual(kinds, []string{"task_ended thread-child", "task_started thread-child"}) {
		t.Errorf("task events = %q", kinds)
	}
	// Two turn_ended: the root's and the child's; the root fold sees only its own.
	if n := len(events(recs, oarlock.EventTurnEnded)); n != 2 {
		t.Errorf("turn_ended count = %d", n)
	}
}

// A resumed thread's token totals count from codex's own re-report: the
// re-report reads zero, the next total is the difference.
func TestResumedUsageCountsFromTheReReport(t *testing.T) {
	st := initialState(rootThread, true)
	total := func(in, out int) obj {
		return obj{"threadId": rootThread, "tokenUsage": map[string]any{"total": map[string]any{"inputTokens": float64(in), "outputTokens": float64(out), "reasoningOutputTokens": float64(in / 10)}}}
	}
	var tokens []oarlock.TokenTotals
	for _, step := range []struct {
		method string
		p      obj
	}{
		{"thread/tokenUsage/updated", total(100, 50)},
		{"turn/started", obj{"threadId": rootThread, "turn": map[string]any{"id": "next"}}},
		{"thread/tokenUsage/updated", total(300, 90)},
	} {
		var cmd frameCmd
		st, cmd = foldNotification(st, step.method, step.p)
		for _, ev := range cmd.Events {
			if ev.Kind == oarlock.EventUsage && ev.Usage.Tokens != nil {
				tokens = append(tokens, *ev.Usage.Tokens)
			}
		}
	}
	if len(tokens) != 2 || tokens[0].Input != 0 || tokens[0].Output != 0 || tokens[1].Input != 200 || tokens[1].Output != 40 ||
		*tokens[1].Reasoning != 20 {
		t.Errorf("tokens = %+v", tokens)
	}
	// A resumed thread whose first turn starts without a re-report has no known
	// start: no tokens at all, never a guess.
	st = initialState(rootThread, true)
	st, _ = foldNotification(st, "turn/started", obj{"threadId": rootThread, "turn": map[string]any{"id": "t"}})
	_, cmd := foldNotification(st, "thread/tokenUsage/updated", total(300, 90))
	for _, ev := range cmd.Events {
		if ev.Kind == oarlock.EventUsage && ev.Usage.Tokens != nil {
			t.Errorf("tokens reported without a baseline: %+v", ev.Usage.Tokens)
		}
	}
}

func TestContextFullnessIsTheLastCallAgainstTheWindow(t *testing.T) {
	st := initialState(rootThread, false)
	_, cmd := foldNotification(st, "thread/tokenUsage/updated", obj{"threadId": rootThread, "turnId": "t3", "tokenUsage": map[string]any{
		"total":              map[string]any{"totalTokens": 44201.0, "inputTokens": 44166.0, "cachedInputTokens": 32512.0, "outputTokens": 35.0, "reasoningOutputTokens": 0.0},
		"last":               map[string]any{"totalTokens": 15774.0, "inputTokens": 15762.0, "outputTokens": 12.0},
		"modelContextWindow": 121600.0,
	}})
	u := cmd.Events[0].Usage
	if *u.Context.Tokens != 15774 || *u.Context.ContextWindow != 121600 || *u.Context.Percent != 13 {
		t.Errorf("context = %+v", u.Context)
	}
	if u.Tokens.Input != 44166 || u.Tokens.Output != 35 || *u.Tokens.CacheRead != 32512 || u.Tokens.CacheWrite != nil {
		t.Errorf("tokens = %+v", u.Tokens)
	}
	// No `last`: occupancy is unknown; the cumulative total is never read against the window.
	_, cmd = foldNotification(st, "thread/tokenUsage/updated", obj{"threadId": rootThread, "tokenUsage": map[string]any{
		"total": map[string]any{"inputTokens": 200.0, "outputTokens": 5.0}, "modelContextWindow": 121600.0}})
	c := cmd.Events[0].Usage.Context
	if *c.Tokens != 200 || c.ContextWindow != nil || c.Percent != nil {
		t.Errorf("context without last = %+v", c)
	}
}

func TestCompactionCompletionIsReportedOnce(t *testing.T) {
	st := initialState(rootThread, false)
	var kinds []string
	for _, step := range []struct {
		method string
		p      obj
	}{
		{"item/started", obj{"threadId": rootThread, "item": map[string]any{"id": "cc_1", "type": "contextCompaction"}}},
		{"item/completed", obj{"threadId": rootThread, "item": map[string]any{"id": "cc_1", "type": "contextCompaction"}}},
		{"thread/compacted", obj{"threadId": rootThread}},
		{"item/commandExecution/outputDelta", obj{"threadId": rootThread, "itemId": "call_9", "delta": "hello\n"}},
	} {
		var cmd frameCmd
		st, cmd = foldNotification(st, step.method, step.p)
		var ks []string
		for _, ev := range cmd.Events {
			ks = append(ks, string(ev.Kind))
		}
		kinds = append(kinds, strings.Join(ks, ","))
	}
	want := []string{"compaction_started", "compaction_ended", "", "tool_call_progress"}
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("got %q want %q", kinds, want)
	}
}

func TestToolCompletionMapsAnExplicitStatusOnly(t *testing.T) {
	st := initialState(rootThread, false)
	_, failed := foldNotification(st, "item/completed", obj{"threadId": rootThread, "item": map[string]any{
		"type": "commandExecution", "id": "exec-fail", "status": "failed", "aggregatedOutput": "boom", "exitCode": 2.0}})
	ev := failed.Events[0]
	if ev.Result != oarlock.ToolFailed || ev.Content[0].Text != "boom" || ev.ExitCode == nil || *ev.ExitCode != 2 || !ev.ExitCodeReported {
		t.Errorf("failed = %+v", ev)
	}
	_, unknown := foldNotification(st, "item/completed", obj{"threadId": rootThread, "item": map[string]any{
		"type": "commandExecution", "id": "exec-unknown", "aggregatedOutput": "?"}})
	ev = unknown.Events[0]
	if ev.Result != oarlock.ToolUnknown || ev.ExitCodeReported || ev.ExitCode != nil {
		t.Errorf("unknown = %+v", ev)
	}
	_, sig := foldNotification(st, "item/completed", obj{"threadId": rootThread, "item": map[string]any{
		"type": "commandExecution", "id": "exec-sig", "status": "failed", "exitCode": nil}})
	ev = sig.Events[0]
	if !ev.ExitCodeReported || ev.ExitCode != nil {
		t.Errorf("signal exit = %+v", ev)
	}
}

func TestFailureClassComesFromCodexErrorInfo(t *testing.T) {
	cases := []struct {
		err  any
		want oarlock.FailureClass
		stat int
	}{
		{map[string]any{"codexErrorInfo": "usageLimitExceeded"}, oarlock.FailureQuota, 0},
		{map[string]any{"codexErrorInfo": "modelNotFound"}, oarlock.FailureModelUnavail, 0},
		{map[string]any{"codexErrorInfo": "contextWindowExceeded"}, oarlock.FailureInputTooLarge, 0},
		{map[string]any{"codexErrorInfo": map[string]any{"httpConnectionFailed": map[string]any{"httpStatusCode": 429.0}}}, oarlock.FailureRateLimited, 429},
		{map[string]any{"codexErrorInfo": "other", "message": `{"error":{"code":"insufficient_quota"}}`}, oarlock.FailureQuota, 0},
		{map[string]any{"codexErrorInfo": "unauthorized"}, oarlock.FailureUnknown, 0},
		{nil, oarlock.FailureUnknown, 0},
	}
	for _, c := range cases {
		o := codexFailure("failed", c.err)
		if o.Kind != oarlock.OutcomeFailed || o.Failure != c.want || o.Status != c.stat {
			t.Errorf("%v → %+v", c.err, o)
		}
	}
}
