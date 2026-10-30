package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/testing/scripted"
)

// The centurion's unit tests: DTO shape, rendering, exit codes, flag parsing
// and the pre-run record check. The run path itself is covered by
// run_integration_test.go.

func ctPtr(v int64) *int64 { return &v }

// ctProbe is a Runtime.InstallationProbe built from a function.
type ctProbe func(context.Context) (oarlock.Installation, error)

func (f ctProbe) Installation(ctx context.Context) (oarlock.Installation, error) { return f(ctx) }

// ctModels is a Runtime.ModelLister built from a function.
type ctModels func(context.Context) ([]oarlock.Model, error)

func (f ctModels) ListModels(ctx context.Context) ([]oarlock.Model, error) { return f(ctx) }

// ctApp builds an App over a fixed registry and captures its streams.
func ctApp(rts ...oarlock.Runtime) (*App, *bytes.Buffer, *bytes.Buffer) {
	byID := make(map[string]oarlock.Runtime, len(rts))
	for _, rt := range rts {
		byID[rt.ID] = rt
	}
	var stdout, stderr bytes.Buffer
	app := &App{
		Stdout: &stdout,
		Stderr: &stderr,
		Lookup: func(id string) (oarlock.Runtime, error) {
			rt, ok := byID[id]
			if !ok {
				return oarlock.Runtime{}, errors.Join(oarlock.ErrRuntimeNotFound, errors.New(id+" not in the test registry"))
			}
			return rt, nil
		},
		Registered: func() []string {
			ids := make([]string, 0, len(byID))
			for id := range byID {
				ids = append(ids, id)
			}
			return ids
		},
		Signals: make(chan os.Signal),
	}
	return app, &stdout, &stderr
}

// ctRuntime is the smallest runtime the list/probe/models commands need.
func ctRuntime(id string) oarlock.Runtime {
	return oarlock.Runtime{
		ID:      id,
		Session: func(context.Context, oarlock.Installation, oarlock.Options) (oarlock.Session, error) { return nil, nil },
	}
}

func ctRun(t *testing.T, app *App, args ...string) (int, string, string) {
	t.Helper()
	code := app.Run(args)
	return code, app.Stdout.(*bytes.Buffer).String(), app.Stderr.(*bytes.Buffer).String()
}

func ctDecode(t *testing.T, out string, v any) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly one JSON line, got %d:\n%s", len(lines), out)
	}
	if err := json.Unmarshal([]byte(lines[0]), v); err != nil {
		t.Fatalf("invalid JSON DTO: %v\n%s", err, lines[0])
	}
}

func TestParseInterspersedAcceptsFlagsAfterPositionals(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "json")
	cwd := fs.String("cwd", "", "cwd")
	args, err := parseInterspersed(fs, []string{"fake", "hello", "--json", "--cwd", "/tmp", "tail"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(args, "|"), "fake|hello|tail"; got != want {
		t.Fatalf("positional = %q, want %q", got, want)
	}
	if !*jsonOut || *cwd != "/tmp" {
		t.Fatalf("flags = json:%v cwd:%q", *jsonOut, *cwd)
	}
}

func TestParseInterspersedDoubleDash(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "json")
	args, err := parseInterspersed(fs, []string{"--json", "run", "--", "--not-a-flag"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(args, "|"), "run|--not-a-flag"; got != want {
		t.Fatalf("positional = %q, want %q", got, want)
	}
	if !*jsonOut {
		t.Fatal("--json before -- was not parsed")
	}
}

func TestListJSONDTOShape(t *testing.T) {
	a := ctRuntime("alpha")
	a.Probe = ctProbe(func(context.Context) (oarlock.Installation, error) { return oarlock.Installation{}, nil })
	a.Models = ctModels(func(context.Context) ([]oarlock.Model, error) { return nil, nil })
	b := ctRuntime("beta")
	app, _, _ := ctApp(b, a)

	code, out, _ := ctRun(t, app, "list", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var raw map[string]json.RawMessage
	ctDecode(t, out, &raw)
	for _, key := range []string{"schema", "command", "runtimes"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("missing top-level key %q in %s", key, out)
		}
	}
	var dto ListDTO
	ctDecode(t, out, &dto)
	if dto.Schema != Schema || dto.Command != "list" {
		t.Fatalf("schema/command = %q/%q", dto.Schema, dto.Command)
	}
	if len(dto.Runtimes) != 2 || dto.Runtimes[0].ID != "alpha" || dto.Runtimes[1].ID != "beta" {
		t.Fatalf("runtimes = %+v, want sorted alpha,beta", dto.Runtimes)
	}
	c := dto.Runtimes[0].Capabilities
	if !c.Session || !c.Probe || !c.Models || c.Install || c.Login || c.Usage || c.Upgrade || c.Inventory {
		t.Fatalf("alpha capabilities = %+v", c)
	}
	if !dto.Runtimes[1].Capabilities.Session || dto.Runtimes[1].Capabilities.Probe {
		t.Fatalf("beta capabilities = %+v", dto.Runtimes[1].Capabilities)
	}
}

func TestListHumanTable(t *testing.T) {
	a := ctRuntime("alpha")
	a.Probe = ctProbe(func(context.Context) (oarlock.Installation, error) { return oarlock.Installation{}, nil })
	app, _, _ := ctApp(a)
	code, out, _ := ctRun(t, app, "list")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"RUNTIME", "SESSION", "INVENTORY", "alpha", "yes", "no"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q:\n%s", want, out)
		}
	}
}

func TestProbeIsolationOrderAndExit(t *testing.T) {
	bad := ctRuntime("bad")
	bad.Probe = ctProbe(func(context.Context) (oarlock.Installation, error) {
		// Slow enough that completion order differs from registry order: the
		// result must still be bad, missing, ok.
		time.Sleep(30 * time.Millisecond)
		return oarlock.Installation{}, errors.New("probe exploded")
	})
	missing := ctRuntime("missing")
	missing.Probe = ctProbe(func(context.Context) (oarlock.Installation, error) {
		return oarlock.Installation{Available: false, Source: "path"}, nil
	})
	ok := ctRuntime("ok")
	ok.Probe = ctProbe(func(context.Context) (oarlock.Installation, error) {
		return oarlock.Installation{Available: true, Version: "1.2.3", Path: "/bin/ok", Source: "path"}, nil
	})
	app, _, _ := ctApp(ok, bad, missing)

	code, out, _ := ctRun(t, app, "probe", "all", "--json")
	if code != ExitError {
		t.Fatalf("exit = %d, want 1 because one probe failed", code)
	}
	var dto ProbeDTO
	ctDecode(t, out, &dto)
	if dto.Schema != Schema || dto.Command != "probe" {
		t.Fatalf("schema/command = %q/%q", dto.Schema, dto.Command)
	}
	if len(dto.Results) != 3 {
		t.Fatalf("results = %d", len(dto.Results))
	}
	if got := []string{dto.Results[0].Runtime, dto.Results[1].Runtime, dto.Results[2].Runtime}; got[0] != "bad" || got[1] != "missing" || got[2] != "ok" {
		t.Fatalf("order = %v, want bad,missing,ok", got)
	}
	if dto.Results[0].Error == "" {
		t.Fatal("bad probe has no error")
	}
	if dto.Results[1].Error != "" || dto.Results[1].Installation == nil || dto.Results[1].Installation.Available {
		t.Fatalf("unavailable probe = %+v", dto.Results[1])
	}
	if dto.Results[2].Error != "" || dto.Results[2].Installation == nil || dto.Results[2].Installation.Version != "1.2.3" {
		t.Fatalf("ok probe = %+v", dto.Results[2])
	}
}

func TestProbeUnavailableIsNotAnError(t *testing.T) {
	a := ctRuntime("a")
	a.Probe = ctProbe(func(context.Context) (oarlock.Installation, error) {
		return oarlock.Installation{Available: false}, nil
	})
	app, out, _ := ctApp(a)
	code, _, _ := ctRun(t, app, "probe", "a", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), `"available":false`) {
		t.Fatalf("json = %s", out)
	}
}

func TestProbeUnknownRuntimeIsAnError(t *testing.T) {
	app, out, _ := ctApp()
	code, _, _ := ctRun(t, app, "probe", "nope", "--json")
	if code != ExitError {
		t.Fatalf("exit = %d, want 1", code)
	}
	var dto ProbeDTO
	ctDecode(t, out.String(), &dto)
	if len(dto.Results) != 1 || dto.Results[0].Error == "" {
		t.Fatalf("dto = %+v", dto)
	}
}

func TestModelsUnsupportedEmptyAndError(t *testing.T) {
	unimplemented := ctRuntime("unimplemented")

	empty := ctRuntime("empty")
	empty.Models = ctModels(func(context.Context) ([]oarlock.Model, error) { return nil, nil })

	unsupported := ctRuntime("unsupported")
	unsupported.Models = ctModels(func(context.Context) ([]oarlock.Model, error) {
		return nil, &oarlock.UnsupportedOperation{Operation: "models", Reason: "not signed in"}
	})

	ok := ctRuntime("ok")
	ok.Models = ctModels(func(context.Context) ([]oarlock.Model, error) {
		return []oarlock.Model{{Provider: "p", ID: "m1", Name: "Model One", EffortLevels: []string{"low", "high"}, ServiceTiers: []string{"default"}}}, nil
	})

	bad := ctRuntime("bad")
	bad.Models = ctModels(func(context.Context) ([]oarlock.Model, error) {
		return nil, errors.New("list exploded")
	})

	app, out, _ := ctApp(ok, unimplemented, empty, unsupported, bad)
	code, _, _ := ctRun(t, app, "models", "all", "--json")
	if code != ExitError {
		t.Fatalf("exit = %d, want 1 because one runtime errored", code)
	}
	var dto ModelsDTO
	ctDecode(t, out.String(), &dto)
	if dto.Schema != Schema || dto.Command != "models" {
		t.Fatalf("schema/command = %q/%q", dto.Schema, dto.Command)
	}
	if len(dto.Results) != 5 {
		t.Fatalf("results = %d", len(dto.Results))
	}
	byID := map[string]ModelsResultDTO{}
	for _, r := range dto.Results {
		byID[r.Runtime] = r
	}
	if byID["unimplemented"].Unsupported == "" || byID["unimplemented"].Error != "" {
		t.Fatalf("unimplemented = %+v", byID["unimplemented"])
	}
	if byID["empty"].Unsupported != "" || byID["empty"].Error != "" || len(byID["empty"].Models) != 0 {
		t.Fatalf("empty = %+v", byID["empty"])
	}
	if byID["unsupported"].Unsupported == "" || byID["unsupported"].Error != "" {
		t.Fatalf("unsupported = %+v", byID["unsupported"])
	}
	if byID["bad"].Error == "" || byID["bad"].Unsupported != "" {
		t.Fatalf("bad = %+v", byID["bad"])
	}
	if len(byID["ok"].Models) != 1 || byID["ok"].Models[0].ID != "m1" || len(byID["ok"].Models[0].EffortLevels) != 2 {
		t.Fatalf("ok = %+v", byID["ok"])
	}
}

func TestModelsUnknownRuntimeIsUnsupported(t *testing.T) {
	app, out, _ := ctApp()
	code, _, _ := ctRun(t, app, "models", "nope", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 (unsupported is not a failure)", code)
	}
	var dto ModelsDTO
	ctDecode(t, out.String(), &dto)
	if len(dto.Results) != 1 || dto.Results[0].Unsupported == "" || dto.Results[0].Error != "" {
		t.Fatalf("dto = %+v", dto.Results)
	}
}

func TestModelsHumanTable(t *testing.T) {
	ok := ctRuntime("ok")
	ok.Models = ctModels(func(context.Context) ([]oarlock.Model, error) {
		return []oarlock.Model{{ID: "m1", Name: "One", EffortLevels: []string{"low"}, ServiceTiers: []string{"default"}}}, nil
	})
	app, out, _ := ctApp(ok)
	code, _, _ := ctRun(t, app, "models", "ok")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"RUNTIME", "MODEL", "EFFORT", "SERVICE TIERS", "m1", "low", "default"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("table missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunFlagParseErrorIsExit2(t *testing.T) {
	app, _, stderr := ctApp()
	code, _, _ := ctRun(t, app, "run", "--not-a-flag")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
	if stderr.Len() == 0 {
		t.Fatal("no usage diagnostic on stderr")
	}
}

func TestRunMissingArgumentsIsExit2(t *testing.T) {
	app, _, _ := ctApp()
	for _, args := range [][]string{
		{"run"},
		{"run", "only-runtime"},
		{"run", "fake", "prompt", "extra"},
		{"probe", "a", "b"},
		{"models", "a", "b"},
		{"list", "extra"},
	} {
		if code := app.Run(args); code != ExitUsage {
			t.Fatalf("%v exit = %d, want 2", args, code)
		}
	}
}

func TestRunNegativeTimeoutIsUsageError(t *testing.T) {
	app, _, _ := ctApp(ctRuntime("fake"))
	code, _, _ := ctRun(t, app, "run", "fake", "hi", "--timeout", "-1s")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
}

func TestRunProbeUnavailableIsPreRunError(t *testing.T) {
	opened := 0
	rt := oarlock.Runtime{
		ID: "fake",
		Session: func(context.Context, oarlock.Installation, oarlock.Options) (oarlock.Session, error) {
			opened++
			return nil, errors.New("must not open")
		},
		Probe: ctProbe(func(context.Context) (oarlock.Installation, error) {
			return oarlock.Installation{Available: false}, nil
		}),
	}
	app, out, _ := ctApp(rt)
	code, _, _ := ctRun(t, app, "run", "fake", "hi", "--json")
	if code != ExitError {
		t.Fatalf("exit = %d, want 1", code)
	}
	if opened != 0 {
		t.Fatalf("session opened %d times, want 0", opened)
	}
	var dto RunDTO
	ctDecode(t, out.String(), &dto)
	if !strings.Contains(dto.Error, "not installed") {
		t.Fatalf("error = %q", dto.Error)
	}
}

func TestRunRecordWriteFailureIsExit1(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "record-dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The pre-run check sees the directory; the session removes it before the
	// record write, so Create fails after a completed turn.
	rt := oarlock.Runtime{
		ID: "fake",
		Session: func(context.Context, oarlock.Installation, oarlock.Options) (oarlock.Session, error) {
			if err := os.Remove(dir); err != nil {
				return nil, err
			}
			return scripted.New(scripted.Turn{scripted.Say("ok")}), nil
		},
		Probe: ctProbe(func(context.Context) (oarlock.Installation, error) {
			return oarlock.Installation{Available: true}, nil
		}),
	}
	app, out, _ := ctApp(rt)
	code, _, _ := ctRun(t, app, "run", "fake", "hi", "--json", "--record", filepath.Join(dir, "out.voyage"))
	if code != ExitError {
		t.Fatalf("exit = %d, want 1", code)
	}
	var dto RunDTO
	ctDecode(t, out.String(), &dto)
	if dto.Outcome != "completed" {
		t.Fatalf("outcome = %q, want completed", dto.Outcome)
	}
	if !strings.Contains(dto.Error, "writing record") {
		t.Fatalf("error = %q", dto.Error)
	}
}

func TestRunPreRunRecordExistsFailsBeforeSession(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "voyage.jsonl")
	if err := os.WriteFile(record, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	opened := 0
	rt := oarlock.Runtime{
		ID: "fake",
		Session: func(context.Context, oarlock.Installation, oarlock.Options) (oarlock.Session, error) {
			opened++
			return nil, errors.New("must not open")
		},
		Probe: ctProbe(func(context.Context) (oarlock.Installation, error) {
			return oarlock.Installation{Available: true}, nil
		}),
	}
	app, out, _ := ctApp(rt)
	code, _, _ := ctRun(t, app, "run", "fake", "hi", "--json", "--record", record)
	if code != ExitError {
		t.Fatalf("exit = %d, want 1", code)
	}
	if opened != 0 {
		t.Fatalf("session opened %d times, want 0", opened)
	}
	var dto RunDTO
	ctDecode(t, out.String(), &dto)
	if dto.Error == "" || dto.Command != "run" || dto.Schema != Schema {
		t.Fatalf("dto = %+v", dto)
	}
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "keep me" {
		t.Fatalf("record changed to %q", body)
	}
}

func TestRunUnknownRuntimeJSON(t *testing.T) {
	app, out, _ := ctApp()
	code, _, _ := ctRun(t, app, "run", "nope", "hi", "--json")
	if code != ExitError {
		t.Fatalf("exit = %d, want 1", code)
	}
	var dto RunDTO
	ctDecode(t, out.String(), &dto)
	if !strings.Contains(dto.Error, "not registered") {
		t.Fatalf("error = %q", dto.Error)
	}
}

func TestRunCwdMissingIsPreRunError(t *testing.T) {
	rt := ctRuntime("fake")
	app, out, _ := ctApp(rt)
	code, _, _ := ctRun(t, app, "run", "fake", "hi", "--json", "--cwd", filepath.Join(t.TempDir(), "absent"))
	if code != ExitError {
		t.Fatalf("exit = %d, want 1", code)
	}
	var dto RunDTO
	ctDecode(t, out.String(), &dto)
	if dto.Error == "" {
		t.Fatal("no error in DTO")
	}
}

func TestRunDTOFromRecords(t *testing.T) {
	records := []oarlock.Record{
		{Seq: 1, Kind: oarlock.KindRequest, SessionID: "sess-1", ID: "req-1", Direction: oarlock.ToRuntime,
			Request: &oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: "hi"}},
		{Seq: 2, Kind: oarlock.KindFrame, SessionID: "sess-1", Events: []oarlock.Event{
			{Kind: oarlock.EventModel, Model: "reported-model"},
			{Kind: oarlock.EventEffort, Effort: "reported-effort"},
			{Kind: oarlock.EventServiceTier, ServiceTier: "reported-tier"},
		}},
		{Seq: 3, Kind: oarlock.KindFrame, SessionID: "sess-1", Events: []oarlock.Event{
			{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{
				Tokens:  &oarlock.TokenTotals{Input: 11, Output: 7},
				Context: &oarlock.ContextUsage{Tokens: ctPtr(120), ContextWindow: ctPtr(1000)},
			}},
		}},
		{Seq: 4, Kind: oarlock.KindFrame, SessionID: "sess-1", Events: []oarlock.Event{
			{Kind: oarlock.EventTurnEnded, Outcome: &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted}},
		}},
	}
	res := oarlock.RunResult{
		Outcome:   oarlock.RunCompleted,
		Text:      "hello",
		Turn:      &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted},
		RequestID: "req-1",
		Seq:       4,
	}
	dto := runDTO("fake", records, res, "requested-model", "requested-effort", "requested-tier")
	if dto.Schema != Schema || dto.Command != "run" || dto.Runtime != "fake" {
		t.Fatalf("envelope = %+v", dto)
	}
	if dto.SessionID != "sess-1" || dto.Outcome != "completed" || dto.Text != "hello" {
		t.Fatalf("dto = %+v", dto)
	}
	if dto.Turn == nil || dto.Turn.Kind != "completed" {
		t.Fatalf("turn = %+v", dto.Turn)
	}
	if dto.Status == nil || dto.Status.Kind != "idle" {
		t.Fatalf("status = %+v", dto.Status)
	}
	if dto.Usage == nil || dto.Usage.Input != 11 || dto.Usage.Output != 7 {
		t.Fatalf("usage = %+v", dto.Usage)
	}
	if dto.Context == nil || dto.Context.Tokens == nil || *dto.Context.Tokens != 120 || dto.Context.Window == nil || *dto.Context.Window != 1000 {
		t.Fatalf("context = %+v", dto.Context)
	}
	if dto.Model != "reported-model" || dto.Effort != "reported-effort" || dto.ServiceTier != "reported-tier" {
		t.Fatalf("effective model/effort/tier = %q/%q/%q", dto.Model, dto.Effort, dto.ServiceTier)
	}
	if dto.RequestID != "req-1" || dto.Seq != 4 {
		t.Fatalf("request = %+v", dto)
	}
}

func TestRunDTOFallsBackToRequestedValues(t *testing.T) {
	dto := runDTO("fake", nil, oarlock.RunResult{Outcome: oarlock.RunInterrupted}, "m", "high", "default")
	if dto.Model != "m" || dto.Effort != "high" || dto.ServiceTier != "default" {
		t.Fatalf("fallback = %+v", dto)
	}
	if dto.Usage != nil || dto.Context != nil {
		t.Fatalf("unreported numbers must stay absent: %+v", dto)
	}
	if dto.Status == nil || dto.Status.Kind != "idle" {
		t.Fatalf("zero stream status = %+v", dto.Status)
	}
}

func TestCheckRecordPath(t *testing.T) {
	dir := t.TempDir()
	if err := checkRecordPath(filepath.Join(dir, "new.jsonl")); err != nil {
		t.Fatalf("new path: %v", err)
	}
	existing := filepath.Join(dir, "existing.jsonl")
	if err := os.WriteFile(existing, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkRecordPath(existing); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing path error = %v", err)
	}
	if err := checkRecordPath(filepath.Join(dir, "absent", "x.jsonl")); err == nil {
		t.Fatal("missing parent directory was accepted")
	}
}

func TestAbsoluteDirAndImages(t *testing.T) {
	dir := t.TempDir()
	got, err := absoluteDir(dir)
	if err != nil || !filepath.IsAbs(got) {
		t.Fatalf("absoluteDir(%q) = %q, %v", dir, got, err)
	}
	if _, err := absoluteDir(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("missing directory was accepted")
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	images, err := absoluteImages([]string{"a.png", "sub/b.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 || images[0].Path != filepath.Join(wd, "a.png") || !filepath.IsAbs(images[1].Path) {
		t.Fatalf("images = %+v", images)
	}
	if _, err := absoluteImages([]string{""}); err == nil {
		t.Fatal("empty image path was accepted")
	}
}

func TestRunExitCodes(t *testing.T) {
	completed := oarlock.RunResult{Outcome: oarlock.RunCompleted}
	if code := runExit(completed, nil, &signalGuard{}); code != ExitOK {
		t.Fatalf("completed = %d", code)
	}
	if code := runExit(oarlock.RunResult{Outcome: oarlock.RunFailed}, nil, &signalGuard{}); code != ExitError {
		t.Fatalf("failed = %d", code)
	}
	if code := runExit(oarlock.RunResult{Outcome: oarlock.RunInterrupted}, nil, &signalGuard{}); code != ExitError {
		t.Fatalf("timeout interrupted = %d", code)
	}
	guard := &signalGuard{}
	guard.fired.Store(true)
	if code := runExit(oarlock.RunResult{Outcome: oarlock.RunInterrupted}, context.Canceled, guard); code != ExitInterrupt {
		t.Fatalf("signal interrupted = %d", code)
	}
	if code := runExit(completed, nil, guard); code != ExitOK {
		t.Fatalf("completion racing a signal = %d", code)
	}
	if code := runExit(completed, context.Canceled, guard); code != ExitOK {
		t.Fatalf("completion racing a cancelled context = %d", code)
	}
}

func TestHelpIsExit0(t *testing.T) {
	app, out, _ := ctApp()
	if code := app.Run([]string{"help"}); code != ExitOK {
		t.Fatalf("help exit = %d", code)
	}
	if !strings.Contains(out.String(), "oarlock list") {
		t.Fatalf("help output = %q", out.String())
	}
	if code := app.Run(nil); code != ExitUsage {
		t.Fatalf("no args exit = %d", code)
	}
	if code := app.Run([]string{"wat"}); code != ExitUsage {
		t.Fatalf("unknown command exit = %d", code)
	}
}
