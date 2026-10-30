package cli

// Black-box-ish integration tests for `oarlock run`: the App seams are built
// directly (fake registry, fake runtime, captured streams, injected interrupt
// channel), the session is the scripted runtime from internal/testing/scripted,
// and nothing talks to a real vendor CLI.
//
// Every helper here is prefixed "it" so it cannot collide with the other test
// files in this package. No test calls t.Parallel: the signal tests own the
// process-wide os.Interrupt notion through an injected channel, and every wait
// is bounded by itDeadline so a regression fails instead of hanging.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/testing/scripted"
	"github.com/zzjcool/oarlock/internal/voyage"
	"github.com/zzjcool/oarlock/observe"
)

const (
	// itDeadline bounds every wait in this file.
	itDeadline = 10 * time.Second
	// itForcedBound is how fast the second SIGINT must return. It is far below
	// itAbortGrace, so a command that waited for the runtime's own turn end
	// could not pass.
	itForcedBound = 2 * time.Second
	// itAbortGrace is the scripted take-over grace of the stubborn turn: a
	// runtime that ignores abort and never ends on its own.
	itAbortGrace = 30 * time.Second
	// itRuntimeID is the one runtime every test registers.
	itRuntimeID = "it-fake"
)

// itBuffer is the App's stdout/stderr. The run writes from more than one
// goroutine (progress callbacks append, dispose waits), so the sink carries its
// own mutex: bytes.Buffer alone would be a data race under -race.
type itBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *itBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *itBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// itProbe is the fake InstallationProbe. The zero value is "not probed"; the
// constructor below installs an available one.
type itProbe struct {
	inst oarlock.Installation
	err  error
}

func (p itProbe) Installation(context.Context) (oarlock.Installation, error) { return p.inst, p.err }

// itPrompt is one Prompt call the CLI made on the scripted session.
type itPrompt struct {
	input string
	opts  oarlock.PromptOptions
}

// itSession wraps the scripted session and records what Prompt was handed. The
// scripted session itself refuses images unless asked, so Options.Images is the
// only capability the image test has to declare.
type itSession struct {
	*scripted.Session
	mu      sync.Mutex
	prompts []itPrompt
}

func (s *itSession) Prompt(ctx context.Context, input string, opt oarlock.PromptOptions) (oarlock.ControlOutcome, error) {
	s.mu.Lock()
	s.prompts = append(s.prompts, itPrompt{input: input, opts: opt})
	s.mu.Unlock()
	return s.Session.Prompt(ctx, input, opt)
}

func (s *itSession) itPromptCalls() []itPrompt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]itPrompt(nil), s.prompts...)
}

// itRuntime is a fake oarlock.Runtime: one probe answer, one session factory and
// the facts the test needs afterwards (was the session constructed at all, what
// Options it was opened with).
type itRuntime struct {
	id      string
	script  []scripted.Turn
	sessOpt scripted.Options
	probe   itProbe

	mu      sync.Mutex
	opens   int
	options []oarlock.Options
	sess    *itSession
}

func itNewRuntime(id string, turns ...scripted.Turn) *itRuntime {
	return &itRuntime{
		id:     id,
		script: turns,
		probe: itProbe{inst: oarlock.Installation{
			Available: true,
			Version:   "it-1",
			Path:      "/fake/" + id,
			Source:    "test",
		}},
	}
}

// itNewImageRuntime is itNewRuntime with a session that accepts images.
func itNewImageRuntime(id string, turns ...scripted.Turn) *itRuntime {
	rt := itNewRuntime(id, turns...)
	rt.sessOpt = scripted.Options{Images: true}
	return rt
}

// itNewStubbornRuntime is itNewRuntime whose turns ignore abort and are taken
// over only after itAbortGrace.
func itNewStubbornRuntime(id string, turns ...scripted.Turn) *itRuntime {
	rt := itNewRuntime(id, turns...)
	rt.sessOpt = scripted.Options{AbortGrace: itAbortGrace}
	return rt
}

// itRuntimeValue is the oarlock.Runtime the App's Lookup hands back. The closure
// counts opens, so a refused --record can prove no session was ever constructed.
func (f *itRuntime) itRuntimeValue() oarlock.Runtime {
	return oarlock.Runtime{
		ID: f.id,
		Session: func(_ context.Context, _ oarlock.Installation, opt oarlock.Options) (oarlock.Session, error) {
			f.mu.Lock()
			f.opens++
			f.options = append(f.options, opt)
			f.mu.Unlock()

			sess, err := scripted.NewWithOptions(f.sessOpt, f.script...)
			if err != nil {
				return nil, err
			}
			wrapped := &itSession{Session: sess}
			f.mu.Lock()
			f.sess = wrapped
			f.mu.Unlock()
			return wrapped, nil
		},
		Probe: f.probe,
	}
}

func (f *itRuntime) itOpenCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens
}

func (f *itRuntime) itOpenedOptions() []oarlock.Options {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]oarlock.Options(nil), f.options...)
}

// itOpenedSession returns the session the fake runtime constructed, failing when
// it never opened one.
func (f *itRuntime) itOpenedSession(t *testing.T) *itSession {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sess == nil {
		t.Fatalf("the fake runtime never opened a session")
	}
	return f.sess
}

// itNewApp builds the App seams directly: the fake registry, the fake runtime,
// the captured streams and (when non-nil) the injected interrupt channel.
func itNewApp(t *testing.T, rt *itRuntime, signals <-chan os.Signal) (*App, *itBuffer, *itBuffer) {
	t.Helper()
	out, errOut := &itBuffer{}, &itBuffer{}
	app := &App{
		Stdout: out,
		Stderr: errOut,
		Lookup: func(id string) (oarlock.Runtime, error) {
			if id != rt.id {
				return oarlock.Runtime{}, fmt.Errorf("runtime %q is not registered", id)
			}
			return rt.itRuntimeValue(), nil
		},
		Registered: func() []string { return []string{rt.id} },
		Signals:    signals,
	}
	return app, out, errOut
}

// itRunAsync starts one App.Run on its own goroutine and returns its exit code
// channel. The caller must await it with itAwaitExit: every wait stays bounded.
func itRunAsync(t *testing.T, app *App, args ...string) <-chan int {
	t.Helper()
	done := make(chan int, 1)
	go func() { done <- app.Run(args) }()
	return done
}

// itAwaitExit waits for a started run, failing the test when it outlives
// itDeadline.
func itAwaitExit(t *testing.T, done <-chan int, what string) int {
	t.Helper()
	select {
	case code := <-done:
		return code
	case <-time.After(itDeadline):
		t.Fatalf("%s: App.Run did not return within %v", what, itDeadline)
		return -1
	}
}

// itRunBounded runs one command line synchronously, bounded by itDeadline.
func itRunBounded(t *testing.T, app *App, args ...string) int {
	t.Helper()
	return itAwaitExit(t, itRunAsync(t, app, args...), fmt.Sprintf("App.Run(%q)", args))
}

// itPoll waits, bounded by itDeadline, until cond reports true.
func itPoll(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(itDeadline)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", itDeadline, what)
		}
		time.Sleep(time.Millisecond)
	}
}

// itTurnActive reports whether a turn started in the stream: the scripted
// session writes turn_active once the prompt was accepted.
func itTurnActive(records []oarlock.Record) bool {
	for _, rec := range records {
		for _, ev := range rec.Events {
			if ev.Kind == oarlock.EventTurnActive {
				return true
			}
		}
	}
	return false
}

// itRunningSession waits until the fake runtime opened its session and that
// session's own stream says a turn is running, then returns it. This is the
// synchronisation point the signal tests signal after: the prompt is accepted
// and the turn is live, so the interrupt cannot race the prompt.
func itRunningSession(t *testing.T, rt *itRuntime) *itSession {
	t.Helper()
	var sess *itSession
	itPoll(t, "a scripted session with a running turn", func() bool {
		rt.mu.Lock()
		sess = rt.sess
		rt.mu.Unlock()
		if sess == nil {
			return false
		}
		records := sess.Records(oarlock.Cursor{})
		return observe.StatusOf(records).Value.Kind == observe.StatusRunning && itTurnActive(records)
	})
	return sess
}

// itParseRunDTO decodes the one JSON document a --json run must write: exactly
// one line on stdout, nothing else.
func itParseRunDTO(t *testing.T, out *itBuffer) RunDTO {
	t.Helper()
	raw := out.String()
	if strings.TrimSpace(raw) == "" {
		t.Fatalf("stdout is empty, want one JSON document")
	}
	if lines := strings.Count(strings.TrimSuffix(raw, "\n"), "\n"); lines != 0 {
		t.Fatalf("stdout must be exactly one JSON line, got %d lines:\n%s", lines+1, raw)
	}
	var dto RunDTO
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &dto); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, raw)
	}
	return dto
}

// itJSONOf is the record's JSON encoding without HTML escaping, the same form
// the voyage log writes; comparing it is comparing every field a replay reads.
func itJSONOf(rec oarlock.Record) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rec); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// itCompareRecords checks that the recorded log is the session's own stream:
// same length, same dense Seq order, same content.
func itCompareRecords(recorded, stream []oarlock.Record) error {
	if len(recorded) != len(stream) {
		return fmt.Errorf("record count = %d, want the session's %d", len(recorded), len(stream))
	}
	for i := range stream {
		if recorded[i].Seq != stream[i].Seq {
			return fmt.Errorf("record %d: seq = %d, want %d", i, recorded[i].Seq, stream[i].Seq)
		}
		got, err := itJSONOf(recorded[i])
		if err != nil {
			return fmt.Errorf("encoding recorded record %d: %w", i, err)
		}
		want, err := itJSONOf(stream[i])
		if err != nil {
			return fmt.Errorf("encoding session record %d: %w", i, err)
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("record %d (seq %d) differs:\n recorded %s\n session  %s", i, stream[i].Seq, got, want)
		}
	}
	return nil
}

// itHasDispose reports whether the stream recorded the dispose request: the
// session was released, not abandoned.
func itHasDispose(records []oarlock.Record) bool {
	for _, rec := range records {
		if rec.Kind == oarlock.KindRequest && rec.Request != nil && rec.Request.Action == oarlock.ActionDispose {
			return true
		}
	}
	return false
}

// TestITRunJSONCompleted pins the happy path of `run --json`: one DTO, exit 0,
// and every flag arriving where the contract says.
func TestITRunJSONCompleted(t *testing.T) {
	dir := t.TempDir()
	rt := itNewRuntime(itRuntimeID, scripted.Turn{scripted.Say("ok")})
	app, out, errOut := itNewApp(t, rt, nil)

	code := itRunBounded(t, app,
		"run", itRuntimeID, "say hello",
		"--json",
		"--cwd", dir,
		"--model", "it-model",
		"--effort", "it-effort",
		"--service-tier", "it-tier",
		"--resume", "it-native-session",
	)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, ExitOK, errOut.String())
	}

	dto := itParseRunDTO(t, out)
	if dto.Schema != Schema {
		t.Errorf("schema = %q, want %q", dto.Schema, Schema)
	}
	if dto.Command != "run" {
		t.Errorf("command = %q, want %q", dto.Command, "run")
	}
	if dto.Runtime != itRuntimeID {
		t.Errorf("runtime = %q, want %q", dto.Runtime, itRuntimeID)
	}
	if dto.Outcome != string(oarlock.RunCompleted) {
		t.Errorf("outcome = %q, want %q", dto.Outcome, oarlock.RunCompleted)
	}
	if dto.Text != "ok" {
		t.Errorf("text = %q, want %q", dto.Text, "ok")
	}
	if dto.SessionID == "" {
		t.Errorf("sessionId is empty, want the scripted stream's session id")
	}

	opens := rt.itOpenedOptions()
	if len(opens) != 1 {
		t.Fatalf("the runtime's Session was constructed %d times, want 1", len(opens))
	}
	opened := opens[0]
	if !filepath.IsAbs(opened.Cwd) {
		t.Errorf("Options.Cwd = %q, want an absolute path", opened.Cwd)
	}
	if opened.Cwd != dir {
		t.Errorf("Options.Cwd = %q, want the --cwd %q", opened.Cwd, dir)
	}
	if opened.Model != "it-model" {
		t.Errorf("Options.Model = %q, want %q", opened.Model, "it-model")
	}
	if opened.Effort != "it-effort" {
		t.Errorf("Options.Effort = %q, want %q", opened.Effort, "it-effort")
	}
	if opened.ServiceTier != "it-tier" {
		t.Errorf("Options.ServiceTier = %q, want %q", opened.ServiceTier, "it-tier")
	}
	if opened.Resume != "it-native-session" {
		t.Errorf("Options.Resume = %q, want %q", opened.Resume, "it-native-session")
	}

	calls := rt.itOpenedSession(t).itPromptCalls()
	if len(calls) != 1 {
		t.Fatalf("Prompt was called %d times, want 1", len(calls))
	}
	if calls[0].input != "say hello" {
		t.Errorf("prompt text = %q, want %q", calls[0].input, "say hello")
	}
	if len(calls[0].opts.Images) != 0 {
		t.Errorf("PromptOptions.Images = %v, want none", calls[0].opts.Images)
	}
}

// TestITRunImagesAreAbsoluteInOrder pins the repeatable --image flag: every path
// reaches PromptOptions.InputOptions.Images, absolute and in argument order.
func TestITRunImagesAreAbsoluteInOrder(t *testing.T) {
	dir := t.TempDir()
	rt := itNewImageRuntime(itRuntimeID, scripted.Turn{scripted.Say("ok")})
	app, out, errOut := itNewApp(t, rt, nil)

	code := itRunBounded(t, app,
		"run", itRuntimeID, "look at these",
		"--json",
		"--cwd", dir,
		"--image", "it-one.png",
		"--image", filepath.Join("it-sub", "it-two.png"),
	)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, ExitOK, errOut.String())
	}
	if dto := itParseRunDTO(t, out); dto.Outcome != string(oarlock.RunCompleted) {
		t.Fatalf("outcome = %q, want %q", dto.Outcome, oarlock.RunCompleted)
	}

	calls := rt.itOpenedSession(t).itPromptCalls()
	if len(calls) != 1 {
		t.Fatalf("Prompt was called %d times, want 1", len(calls))
	}
	images := calls[0].opts.Images
	if len(images) != 2 {
		t.Fatalf("PromptOptions.InputOptions.Images = %v, want 2 images", images)
	}
	// --image resolves against the process cwd, not --cwd (which is a different
	// temp directory here), and the argument order is preserved.
	want := []string{filepath.Join(".", "it-one.png"), filepath.Join("it-sub", "it-two.png")}
	for i, rel := range want {
		abs, err := filepath.Abs(rel)
		if err != nil {
			t.Fatalf("filepath.Abs(%q): %v", rel, err)
		}
		if !filepath.IsAbs(images[i].Path) {
			t.Errorf("image %d path = %q, want an absolute path", i, images[i].Path)
		}
		if images[i].Path != abs {
			t.Errorf("image %d path = %q, want %q", i, images[i].Path, abs)
		}
	}
}

// TestITRunOutputModes pins the two output modes: human progress plus the
// outcome line, and a JSON mode that emits no progress text at all.
func TestITRunOutputModes(t *testing.T) {
	t.Run("human", func(t *testing.T) {
		rt := itNewRuntime(itRuntimeID, scripted.Turn{scripted.Say("ok")})
		app, out, errOut := itNewApp(t, rt, nil)

		code := itRunBounded(t, app, "run", itRuntimeID, "hello", "--cwd", t.TempDir())
		if code != ExitOK {
			t.Fatalf("exit = %d, want %d (stderr: %s)", code, ExitOK, errOut.String())
		}
		human := out.String()
		for _, want := range []string{"[runtime " + itRuntimeID + "]", "[text] ok", "[turn] completed", "outcome: completed"} {
			if !strings.Contains(human, want) {
				t.Errorf("human stdout is missing %q:\n%s", want, human)
			}
		}
	})

	t.Run("json", func(t *testing.T) {
		rt := itNewRuntime(itRuntimeID, scripted.Turn{scripted.Say("ok")})
		app, out, errOut := itNewApp(t, rt, nil)

		code := itRunBounded(t, app, "run", itRuntimeID, "hello", "--json", "--cwd", t.TempDir())
		if code != ExitOK {
			t.Fatalf("exit = %d, want %d (stderr: %s)", code, ExitOK, errOut.String())
		}
		raw := out.String()
		for _, banned := range []string{"[tool]", "[text]", "outcome:"} {
			if strings.Contains(raw, banned) {
				t.Errorf("json stdout contains the progress marker %q:\n%s", banned, raw)
			}
		}
		if dto := itParseRunDTO(t, out); dto.Outcome != string(oarlock.RunCompleted) {
			t.Errorf("outcome = %q, want %q", dto.Outcome, oarlock.RunCompleted)
		}
	})
}

// TestITRunRecordWritesCompleteVoyage pins --record's happy path: a new file
// becomes a complete voyage whose header names what was requested and whose
// records are the session's whole stream, dispose included.
func TestITRunRecordWritesCompleteVoyage(t *testing.T) {
	cwd := t.TempDir()
	path := filepath.Join(t.TempDir(), "it-run.oar")
	rt := itNewRuntime(itRuntimeID, scripted.Turn{scripted.Say("ok")})
	app, out, errOut := itNewApp(t, rt, nil)

	code := itRunBounded(t, app,
		"run", itRuntimeID, "hello",
		"--json",
		"--cwd", cwd,
		"--model", "it-model",
		"--effort", "it-effort",
		"--service-tier", "it-tier",
		"--record", path,
	)
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, ExitOK, errOut.String())
	}
	dto := itParseRunDTO(t, out)

	log, err := voyage.Read(path)
	if err != nil {
		t.Fatalf("voyage.Read(%s): %v", path, err)
	}
	if !log.Complete() {
		t.Fatalf("record %s has no end line (truncated)", path)
	}

	header := log.Header
	if header.Runtime != itRuntimeID {
		t.Errorf("header runtime = %q, want %q", header.Runtime, itRuntimeID)
	}
	if header.Model != "it-model" {
		t.Errorf("header model = %q, want the requested %q", header.Model, "it-model")
	}
	if header.Effort != "it-effort" {
		t.Errorf("header effort = %q, want the requested %q", header.Effort, "it-effort")
	}
	if header.ServiceTier != "it-tier" {
		t.Errorf("header serviceTier = %q, want the requested %q", header.ServiceTier, "it-tier")
	}
	if header.Cwd != cwd {
		t.Errorf("header cwd = %q, want %q", header.Cwd, cwd)
	}
	if header.SessionID == "" {
		t.Errorf("header sessionId is empty, want the run's session id")
	}
	if header.SessionID != dto.SessionID {
		t.Errorf("header sessionId = %q, want the DTO's %q", header.SessionID, dto.SessionID)
	}
	if header.StartedAt <= 0 {
		t.Errorf("header startedAt = %d, want Unix milliseconds after the epoch", header.StartedAt)
	}
	if !strings.HasPrefix(header.Recorder, "oarlock-cli/") {
		t.Errorf("header recorder = %q, want the oarlock-cli/ prefix", header.Recorder)
	}
	if log.End.Reason != string(oarlock.RunCompleted) {
		t.Errorf("end reason = %q, want %q", log.End.Reason, oarlock.RunCompleted)
	}

	stream := rt.itOpenedSession(t).Records(oarlock.Cursor{})
	if err := itCompareRecords(log.Records, stream); err != nil {
		t.Fatalf("the record is not the session's stream: %v", err)
	}
	for i, rec := range log.Records {
		if rec.Seq != uint64(i+1) {
			t.Fatalf("record %d has seq %d, want the dense value %d", i, rec.Seq, i+1)
		}
	}
	if !itHasDispose(log.Records) {
		t.Fatalf("the record has %d records but no dispose request", len(log.Records))
	}
}

// TestITRunRecordRefusesExistingPath pins the no-overwrite rule: an existing
// target fails before any session opens and leaves the file untouched.
func TestITRunRecordRefusesExistingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "it-taken.oar")
	const keep = "do not overwrite me\n"
	if err := os.WriteFile(path, []byte(keep), 0o644); err != nil {
		t.Fatalf("pre-creating %s: %v", path, err)
	}

	rt := itNewRuntime(itRuntimeID, scripted.Turn{scripted.Say("ok")})
	app, out, errOut := itNewApp(t, rt, nil)

	code := itRunBounded(t, app, "run", itRuntimeID, "hello", "--json", "--cwd", t.TempDir(), "--record", path)
	if code != ExitError {
		t.Fatalf("exit = %d, want %d (stdout: %s, stderr: %s)", code, ExitError, out.String(), errOut.String())
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s after the refused run: %v", path, err)
	}
	if string(got) != keep {
		t.Fatalf("the existing record file changed:\n got %q\nwant %q", got, keep)
	}
	if opens := rt.itOpenCount(); opens != 0 {
		t.Fatalf("the runtime's Session was constructed %d times, want 0 (no session before the record check)", opens)
	}

	dto := itParseRunDTO(t, out)
	if dto.Error == "" {
		t.Fatalf("--json error is empty, want the record refusal: %+v", dto)
	}
	if dto.Schema != Schema || dto.Command != "run" {
		t.Errorf("error DTO = {schema:%q command:%q}, want {%q run}", dto.Schema, dto.Command, Schema)
	}
	if dto.Outcome != "" {
		t.Errorf("outcome = %q, want it absent on a pre-run error", dto.Outcome)
	}
}

// TestITRunTimeoutIsExitOne pins the deadline: a timed-out turn reports
// interrupted and exits 1, never the signal-driven 130.
func TestITRunTimeoutIsExitOne(t *testing.T) {
	rt := itNewRuntime(itRuntimeID, scripted.Turn{scripted.Hang()})
	app, out, errOut := itNewApp(t, rt, nil)

	code := itRunBounded(t, app,
		"run", itRuntimeID, "hello",
		"--json",
		"--cwd", t.TempDir(),
		"--timeout", "50ms",
	)
	if code == ExitInterrupt {
		t.Fatalf("exit = %d: a timeout is not a signal-driven interrupt", code)
	}
	if code != ExitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, ExitError, errOut.String())
	}
	dto := itParseRunDTO(t, out)
	if dto.Outcome != string(oarlock.RunInterrupted) {
		t.Fatalf("outcome = %q, want %q", dto.Outcome, oarlock.RunInterrupted)
	}
}

// TestITRunFirstSIGINTExits130 pins the first interrupt: it cancels the turn,
// the run reports interrupted and the command exits 130.
func TestITRunFirstSIGINTExits130(t *testing.T) {
	signals := make(chan os.Signal, 2)
	rt := itNewRuntime(itRuntimeID, scripted.Turn{scripted.Hang()})
	app, out, _ := itNewApp(t, rt, signals)

	done := itRunAsync(t, app, "run", itRuntimeID, "hello", "--json", "--cwd", t.TempDir())
	itRunningSession(t, rt)
	signals <- os.Interrupt

	code := itAwaitExit(t, done, "first SIGINT")
	if code != ExitInterrupt {
		t.Fatalf("exit = %d, want %d; stdout %s", code, ExitInterrupt, out.String())
	}
	dto := itParseRunDTO(t, out)
	if dto.Outcome != string(oarlock.RunInterrupted) {
		t.Fatalf("outcome = %q, want %q", dto.Outcome, oarlock.RunInterrupted)
	}
}

// TestITRunSecondSIGINTDisposesAndExits130 pins the second interrupt: the turn
// ignores abort and would outlive a 30s take-over grace, yet the command must
// dispose and leave at once.
func TestITRunSecondSIGINTDisposesAndExits130(t *testing.T) {
	signals := make(chan os.Signal, 2)
	rt := itNewStubbornRuntime(itRuntimeID, scripted.Turn{scripted.IgnoreAbort(), scripted.Hang()})
	app, out, _ := itNewApp(t, rt, signals)

	done := itRunAsync(t, app, "run", itRuntimeID, "hello", "--json", "--cwd", t.TempDir())
	sess := itRunningSession(t, rt)

	signals <- os.Interrupt
	signals <- os.Interrupt
	start := time.Now()
	code := itAwaitExit(t, done, "second SIGINT")
	elapsed := time.Since(start)

	if code != ExitInterrupt {
		t.Fatalf("exit = %d, want %d; stdout %s", code, ExitInterrupt, out.String())
	}
	if elapsed >= itForcedBound {
		t.Fatalf("second SIGINT returned after %v (bound %v): it waited for the turn instead of disposing", elapsed, itForcedBound)
	}

	records := sess.Records(oarlock.Cursor{})
	if !itHasDispose(records) {
		t.Fatalf("the session was not disposed: no dispose request in %d records", len(records))
	}
	if got := observe.StatusOf(records).Value.Kind; got != observe.StatusExited {
		t.Fatalf("session status = %q, want %q after the forced dispose", got, observe.StatusExited)
	}
	dto := itParseRunDTO(t, out)
	if dto.Outcome != string(oarlock.RunInterrupted) {
		t.Fatalf("outcome = %q, want %q", dto.Outcome, oarlock.RunInterrupted)
	}
}
