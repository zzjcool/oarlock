package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

// ---- permission -------------------------------------------------------------

func TestPermissionPrefersAllowAlwaysAndIsRecorded(t *testing.T) {
	s := open(t, Kimi, "kimi", oarlock.Options{})
	prompt(t, s, "perm")
	waitIdle(t, s)
	recs := records(s)

	var req, ans *oarlock.Record
	for i := range recs {
		r := &recs[i]
		if r.Kind == oarlock.KindRequest && r.Direction == oarlock.ToApp {
			req = r
		}
		if r.Kind == oarlock.KindResponse && r.Response.Kind == oarlock.ResponseAnswered {
			ans = r
		}
	}
	if req == nil || ans == nil {
		t.Fatalf("permission request/answer not both recorded:\n%s", dump(recs))
	}
	if req.Request.Action != oarlock.ActionNative || req.Request.NativeType != "session/request_permission" {
		t.Fatalf("request = %+v", req.Request)
	}
	if ans.RequestID != req.ID {
		t.Fatalf("answer %q does not point at request %q", ans.RequestID, req.ID)
	}
	if !strings.Contains(string(ans.Response.Native), `"optionId":"always"`) {
		t.Fatalf("answer must select allow_always: %s", ans.Response.Native)
	}
	if ans.Seq < req.Seq {
		t.Fatal("answer before request")
	}
	// The tool call it gated is read from frames, with the runtime's exit code.
	ended := events(recs, oarlock.EventToolCallEnded)
	if len(ended) != 1 || ended[0].Result != oarlock.ToolOK || !ended[0].ExitCodeReported || ended[0].ExitCode == nil || *ended[0].ExitCode != 0 {
		t.Fatalf("tool_call_ended = %+v", ended)
	}
}

func TestChoosePermissionOrder(t *testing.T) {
	opts := func(kinds ...string) []sdkOption {
		var o []sdkOption
		for i, k := range kinds {
			o = append(o, sdkOption{id: string(rune('a' + i)), kind: k})
		}
		return o
	}
	cases := []struct {
		name string
		in   []sdkOption
		want string // option id, or "" for cancelled
	}{
		{"always beats once", opts("reject_once", "allow_once", "allow_always"), "c"},
		{"once when no always", opts("reject_once", "allow_once"), "b"},
		{"reject only cancels", opts("reject_once", "reject_always"), ""},
		{"empty cancels", nil, ""},
	}
	for _, c := range cases {
		got := choose(c.in)
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestPermissionRejectOnlyIsCancelled(t *testing.T) {
	s := open(t, Kimi, "kimi", oarlock.Options{})
	prompt(t, s, "perm-reject-only")
	waitIdle(t, s)
	for _, r := range records(s) {
		if r.Kind == oarlock.KindResponse && r.Response.Kind == oarlock.ResponseAnswered {
			if !strings.Contains(string(r.Response.Native), `"outcome":"cancelled"`) {
				t.Fatalf("answer = %s", r.Response.Native)
			}
			return
		}
	}
	t.Fatal("no answered response")
}

// ---- terminal ---------------------------------------------------------------

func TestTerminalLifecycle(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	s := open(t, Kimi, "kimi", oarlock.Options{})
	prompt(t, s, "term")
	waitIdle(t, s)
	recs := records(s)
	var methods []string
	for _, r := range recs {
		if r.Kind == oarlock.KindRequest && r.Direction == oarlock.ToApp {
			methods = append(methods, r.Request.NativeType)
		}
	}
	want := []string{"terminal/create", "terminal/wait_for_exit", "terminal/output", "terminal/release"}
	if strings.Join(methods, ",") != strings.Join(want, ",") {
		t.Fatalf("terminal requests = %v", methods)
	}
	answered := 0
	for _, r := range recs {
		if r.Kind == oarlock.KindResponse && r.Response.Kind == oarlock.ResponseAnswered {
			answered++
		}
	}
	if answered != 4 {
		t.Fatalf("answered = %d, want 4", answered)
	}
	out := text(recs)
	if !strings.Contains(out, "term-ok") || !strings.Contains(out, `exitCode\":3`) && !strings.Contains(out, `"exitCode":3`) {
		t.Fatalf("terminal result not seen by the agent: %q", out)
	}
}

func TestTerminalOutputIsTruncatedFromTheFront(t *testing.T) {
	h := newTerminals()
	tm := &terminal{limit: 4}
	tm.write([]byte("abcdef"))
	h.all["x"] = tm
	tm.done = make(chan struct{})
	r, err := h.output(sdkTerminalOutput("x"))
	if err != nil || r.Output != "cdef" || !r.Truncated {
		t.Fatalf("%+v %v", r, err)
	}
}

// ---- abort ------------------------------------------------------------------

func TestAbortThenAgentCancelledIsAborted(t *testing.T) {
	s := open(t, Kimi, "kimi", oarlock.Options{})
	prompt(t, s, "slow")
	waitFor(t, "running", func() bool { return strings.Contains(text(records(s)), "working") })
	out, _ := s.Abort(context.Background())
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("abort = %+v", out)
	}
	waitIdle(t, s)
	ended := events(records(s), oarlock.EventTurnEnded)
	if len(ended) != 1 || ended[0].Outcome.Kind != oarlock.OutcomeAborted {
		t.Fatalf("turn_ended = %+v", ended)
	}
	// Late abort: nothing is running, the turn keeps its own outcome.
	out, _ = s.Abort(context.Background())
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.NoActiveTurn {
		t.Fatalf("late abort = %+v", out)
	}
}

func TestAbortTakeOverKillsAndWritesNoTurnEnd(t *testing.T) {
	old := defaultGrace
	defaultGrace = 150 * time.Millisecond
	defer func() { defaultGrace = old }()
	s := open(t, Kimi, "kimi", oarlock.Options{})
	prompt(t, s, "stubborn")
	waitFor(t, "running", func() bool { return strings.Contains(text(records(s)), "deaf") })
	out, _ := s.Abort(context.Background())
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("abort = %+v", out)
	}
	waitFor(t, "exited", func() bool {
		for _, r := range records(s) {
			if r.Kind == oarlock.KindResponse && r.Response.Kind == oarlock.ResponseExited {
				return true
			}
		}
		return false
	})
	recs := records(s)
	if hasEvent(recs, oarlock.EventTurnEnded) {
		t.Fatalf("a taken-over turn must not get a synthetic turn_ended:\n%s", dump(recs))
	}
	if st := fold.StatusOf(recs).Value; st.Kind != fold.StatusExited {
		t.Fatalf("status = %+v", st)
	}
	// Reachability is read off the stream.
	if out, _ := s.Abort(context.Background()); out.Code != oarlock.RuntimeExited {
		t.Fatalf("abort after exit = %+v", out)
	}
}

func TestCrashMidTurnLeavesTheTurnOpenAndRecordsExit(t *testing.T) {
	s := open(t, Kimi, "kimi", oarlock.Options{})
	prompt(t, s, "crash")
	waitFor(t, "exited", func() bool { return fold.StatusOf(records(s)).Value.Kind == fold.StatusExited })
	recs := records(s)
	if hasEvent(recs, oarlock.EventTurnEnded) {
		t.Fatalf("no turn_ended may be invented for a crash:\n%s", dump(recs))
	}
	last := recs[len(recs)-1]
	if last.Kind != oarlock.KindResponse || last.Response.Kind != oarlock.ResponseExited || last.Response.ExitCode == nil || *last.Response.ExitCode != 7 {
		t.Fatalf("last = %+v", last)
	}
	assertDense(t, recs)
}

func TestDisposeIsIdempotentAndAnswersExited(t *testing.T) {
	s := open(t, Kimi, "kimi", oarlock.Options{})
	for i := 0; i < 2; i++ {
		if err := s.Dispose(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	recs := records(s)
	var disposes, exits int
	for _, r := range recs {
		if r.Kind == oarlock.KindRequest && r.Request != nil && r.Request.Action == oarlock.ActionDispose {
			disposes++
		}
		if r.Kind == oarlock.KindResponse && r.Response.Kind == oarlock.ResponseExited {
			exits++
		}
	}
	if disposes != 1 || exits != 1 {
		t.Fatalf("disposes=%d exits=%d\n%s", disposes, exits, dump(recs))
	}
	if out := prompt(t, s, "x"); out.Code != oarlock.Disposed && out.Code != oarlock.RuntimeExited {
		t.Fatalf("prompt after dispose = %+v", out)
	}
}

func TestQueueRunsAfterTheTurnEnds(t *testing.T) {
	s := open(t, Kimi, "kimi", oarlock.Options{})
	prompt(t, s, "slow")
	waitFor(t, "running", func() bool { return strings.Contains(text(records(s)), "working") })
	if out, _ := s.Queue(context.Background(), "queued-one", oarlock.InputOptions{}); out.Kind != oarlock.ControlAccepted {
		t.Fatal(out)
	}
	s.Abort(context.Background())
	waitFor(t, "queued turn", func() bool { return strings.Contains(text(records(s)), "echo:queued-one") })
	if s.Capabilities().Queue.Durable {
		t.Fatal("the adapter's queue is not durable")
	}
}

func TestEmptyPromptIsRefusedByTheKernelNotTheAgent(t *testing.T) {
	s := open(t, Kimi, "kimi", oarlock.Options{})
	if out := prompt(t, s, ""); out.Code != oarlock.Unsupported {
		t.Fatalf("empty prompt = %+v", out)
	}
}

// ---- resume -----------------------------------------------------------------

func TestResumeLoadReplaysHistoryWithoutEvents(t *testing.T) {
	s := open(t, Kimi, "kimi", oarlock.Options{Resume: "old-session"})
	recs := records(s)
	if recs[0].SessionID != "old-session" {
		t.Fatalf("stream instance must be named by the resumed session: %s", dump(recs))
	}
	var history bool
	for _, r := range recs {
		if strings.Contains(string(r.Native), "history") {
			history = true
			if len(r.Events) != 0 {
				t.Fatalf("replayed history must not become new facts: %+v", r.Events)
			}
		}
	}
	if !history {
		t.Fatalf("history frame lost (frames are never dropped):\n%s", dump(recs))
	}
	if hasEvent(recs, oarlock.EventTurnActive) || hasEvent(recs, oarlock.EventTurnEnded) {
		t.Fatal("resume synthesizes no turn")
	}
	if st := fold.StatusOf(recs).Value.Kind; st != fold.StatusIdle {
		t.Fatalf("status = %v", st)
	}
}

func TestResumeNotFoundIsTypedOnlyWhenTheVendorSaysSo(t *testing.T) {
	_, err := tryOpen(Grok, "grok", oarlock.Options{Resume: "missing"})
	var nf *oarlock.SessionNotFound
	if !errors.As(err, &nf) || nf.SessionID != "missing" {
		t.Fatalf("grok FS_NOT_FOUND must be SessionNotFound, got %v", err)
	}
	// kimi has no unique native code: the same refusal is a plain error.
	_, err = tryOpen(Kimi, "kimi", oarlock.Options{Resume: "missing"})
	if err == nil || errors.As(err, &nf) {
		t.Fatalf("kimi must not claim absence, got %v", err)
	}
}

func TestResumeWithoutAnyResumeMethodIsRefused(t *testing.T) {
	_, err := tryOpen(Kimi, "kimi", oarlock.Options{Resume: "x"}, "ACP_FAKE_NO_LOAD=1")
	var un *oarlock.UnsupportedOption
	if !errors.As(err, &un) {
		t.Fatalf("got %v", err)
	}
}

// ---- options refused before open -------------------------------------------

func TestRefusedOptionsFailBeforeAnyProcessStarts(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "launched")
	cases := []struct {
		prof Profile
		opt  oarlock.Options
		want oarlock.Option
	}{
		{Grok, oarlock.Options{Name: "n"}, oarlock.OptionName},
		{Grok, oarlock.Options{AdditionalDirectories: []string{"/x"}}, oarlock.OptionAdditionalDirectories},
		{Grok, oarlock.Options{DisallowedTools: []string{"Bash"}}, oarlock.OptionDisallowedTools},
		{Grok, oarlock.Options{SystemPrompt: "  "}, oarlock.OptionSystemPrompt},
		{Kimi, oarlock.Options{SystemPrompt: "s"}, oarlock.OptionSystemPrompt},
		{Kimi, oarlock.Options{OutputSchema: json.RawMessage(`{}`)}, oarlock.OptionOutputSchema},
		{OpenCodeV1, oarlock.Options{AdditionalDirectories: []string{"/x"}}, oarlock.OptionAdditionalDirectories},
		{OpenCodeV2, oarlock.Options{AppendSystemPrompt: "a"}, oarlock.OptionAppendSystemPrompt},
		{Qwen, oarlock.Options{ServiceTier: "fast"}, oarlock.OptionServiceTier},
		{Qoder, oarlock.Options{UserConfiguration: oarlock.UserConfigurationExclude}, oarlock.OptionUserConfiguration},
		{Antigravity, oarlock.Options{SystemPrompt: "s"}, oarlock.OptionSystemPrompt},
		{Antigravity, oarlock.Options{MCPServers: []oarlock.MCPServer{{Name: "m", Command: "c", Env: map[string]string{"K": "v"}}}}, oarlock.OptionMCPServerCredentials},
	}
	for _, c := range cases {
		_, err := tryOpen(c.prof, "x", c.opt, "ACP_FAKE_ARGS_FILE="+marker)
		var un *oarlock.UnsupportedOption
		if !errors.As(err, &un) || un.Option != c.want {
			t.Errorf("%s %v: err = %v, want option %s", c.prof.ID, c.opt, err, c.want)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a process was started for a refused option")
	}
	// The Runtime declaration is the table the open path checks.
	for id, rt := range Runtimes() {
		if len(rt.Refused) == 0 {
			t.Errorf("%s declares no refusals", id)
		}
		if err := rt.CheckOptions(oarlock.Options{}); err != nil {
			t.Errorf("%s: zero options must pass: %v", id, err)
		}
	}
}

func TestLaunchArgsPlacement(t *testing.T) {
	file := filepath.Join(t.TempDir(), "args")
	// Grok: options go before `stdio`. Others: appended.
	p := Grok
	p.Args = func(oarlock.Options) []string { return []string{"-test.run=^$", "stdio"} }
	exe, _ := os.Executable()
	t.Setenv("ACP_FAKE_AGENT", "grok")
	t.Setenv("ACP_FAKE_ARGS_FILE", file)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := p.Start(ctx, oarlock.Installation{Path: exe}, oarlock.Options{LaunchArgs: []string{"-test.v=false"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Dispose(context.Background())
	b, _ := os.ReadFile(file)
	if got := strings.Fields(string(b)); len(got) != 3 || got[1] != "-test.v=false" || got[2] != "stdio" {
		t.Fatalf("args = %q", got)
	}
}

func TestEnvValuesNeverEnterRecords(t *testing.T) {
	secret := "zk-live-9f8e7d6c5b4a39281706"
	s := open(t, Kimi, "kimi", oarlock.Options{Env: map[string]*string{"MY_API_KEY": &secret}})
	prompt(t, s, secret)
	waitIdle(t, s)
	if strings.Contains(dump(records(s)), secret) {
		t.Fatal("credential value leaked into a record")
	}
}

func TestMissingBinaryIsAnOpenError(t *testing.T) {
	_, err := Grok.Start(context.Background(), oarlock.Installation{Path: filepath.Join(t.TempDir(), "nope")}, oarlock.Options{})
	if err == nil {
		t.Fatal("expected a launch error")
	}
}
