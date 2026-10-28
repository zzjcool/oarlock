package pi

// The fake Pi is this very test binary re-executed in a helper mode: no sh, no
// Node, no network, and the same binary runs on every platform. Each mode
// speaks (or fails to speak) the JSONL dialect of docs/runtimes/pi.md, and the
// parent asserts on what the Conn makes of it.
//
// Every test arms a watchdog: a blocked call must fail loudly with a goroutine
// dump, not hang the package until `go test -timeout`.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	modeEnv   = "OARLOCK_PI_TEST_MODE"
	codeEnv   = "OARLOCK_PI_TEST_CODE"
	markerEnv = "OARLOCK_PI_TEST_MARKER"

	fiveMiB = 5 << 20
)

// helperModes maps a mode name to the child body; it returns the exit code.
// Platform files add their own modes from init.
var helperModes = map[string]func() int{
	"echo":                   helperEcho,
	"reverse":                helperReverse,
	"silent":                 helperSilent,
	"silent-grandchild":      helperSilentGrandchild,
	"record-pid":             helperRecordPID,
	"halfline":               helperHalfLine,
	"halfline-after-command": helperHalfLineAfterCommand,
	"bigevent":               helperBigEvent,
	"bigresponse":            helperBigResponse,
	"stderr-exit":            helperStderrExit,
	"stderr-flood":           helperStderrFlood,
	"unknown":                helperUnknown,
	"overlong":               helperOverlong,
	"flood":                  helperFlood,
}

// TestHelperProcess is not a test: it is the child body of the re-executed
// test binary, and always leaves through os.Exit so the testing framework never
// prints its PASS banner into the pipe the parent is framing.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(modeEnv)
	if mode == "" {
		return // an ordinary `go test` run
	}
	fn, ok := helperModes[mode]
	if !ok {
		fmt.Fprintf(os.Stderr, "helper: unknown mode %q\n", mode)
		os.Exit(97)
	}
	os.Exit(fn())
}

// ---- child side -----------------------------------------------------------

type wireCommand struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// readCommand reads one command line; ok is false at EOF.
func readCommand(r *bufio.Reader) (wireCommand, bool) {
	line, err := r.ReadBytes('\n')
	if len(line) == 0 && err != nil {
		return wireCommand{}, false
	}
	var c wireCommand
	if jerr := json.Unmarshal(line, &c); jerr != nil || c.ID == "" || c.Type == "" {
		fmt.Fprintf(os.Stderr, "helper: not a command line: %q (%v)\n", line, jerr)
		os.Exit(9)
	}
	return c, true
}

// emit writes one line to stdout in a single Write.
func emit(line string) { _, _ = os.Stdout.WriteString(line + "\n") }

func respond(c wireCommand, data string) {
	emit(fmt.Sprintf(`{"type":"response","id":%q,"command":%q,"success":true,"data":%s}`, c.ID, c.Type, data))
}

// drainStdin reads commands until EOF.
func drainStdin(r *bufio.Reader) {
	for {
		if _, ok := readCommand(r); !ok {
			return
		}
	}
}

func bigText() string { return strings.Repeat("0123456789abcdef", fiveMiB/16) }

func bigEventLine() []byte {
	return []byte(`{"type":"message_update","text":"` + bigText() + `"}`)
}

// lineOfLen builds an event line whose content is exactly n bytes.
func lineOfLen(n int) string {
	const head, tail = `{"type":"message_update","text":"`, `"}`
	return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
}

func helperEcho() int {
	r := bufio.NewReader(os.Stdin)
	for {
		c, ok := readCommand(r)
		if !ok {
			return 0
		}
		respond(c, fmt.Sprintf(`{"echo":%q}`, c.Type))
	}
}

// helperReverse collects three commands, then answers them in reverse order
// between a turn_start and a turn_end event.
func helperReverse() int {
	r := bufio.NewReader(os.Stdin)
	var got []wireCommand
	for len(got) < 3 {
		c, ok := readCommand(r)
		if !ok {
			return 0
		}
		got = append(got, c)
	}
	emit(`{"type":"turn_start"}`)
	for i := len(got) - 1; i >= 0; i-- {
		respond(got[i], fmt.Sprintf(`{"echo":%q,"received":%d}`, got[i].Type, i))
	}
	emit(`{"type":"turn_end"}`)
	drainStdin(r)
	return 0
}

// helperSilent reads commands and never answers one.
func helperSilent() int {
	drainStdin(bufio.NewReader(os.Stdin))
	time.Sleep(time.Minute)
	return 0
}

// helperSilentGrandchild is helperSilent plus a descendant in the same group.
func helperSilentGrandchild() int {
	exe, err := os.Executable()
	if err != nil {
		return 95
	}
	gc := osexec.Command(exe, "-test.run=TestHelperProcess$")
	gc.Env = append(os.Environ(), modeEnv+"=record-pid")
	if err := gc.Start(); err != nil {
		return 98
	}
	return helperSilent()
}

func helperRecordPID() int {
	if path := os.Getenv(markerEnv); path != "" {
		if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
			return 96
		}
	}
	time.Sleep(time.Minute)
	return 0
}

// helperHalfLine writes one complete event, then half a line, and exits 0.
func helperHalfLine() int {
	emit(`{"type":"agent_start"}`)
	_, _ = os.Stdout.WriteString(`{"type":"message_upd`)
	return 0
}

func helperHalfLineAfterCommand() int {
	if _, ok := readCommand(bufio.NewReader(os.Stdin)); !ok {
		return 0
	}
	return helperHalfLine()
}

func helperBigEvent() int {
	_, _ = os.Stdout.Write(append(bigEventLine(), '\n'))
	drainStdin(bufio.NewReader(os.Stdin))
	return 0
}

func helperBigResponse() int {
	r := bufio.NewReader(os.Stdin)
	c, ok := readCommand(r)
	if !ok {
		return 0
	}
	respond(c, `{"text":"`+bigText()+`"}`)
	drainStdin(r)
	return 0
}

func helperStderrExit() int {
	if _, ok := readCommand(bufio.NewReader(os.Stdin)); !ok {
		return 0
	}
	fmt.Fprintln(os.Stderr, "boom: model backend unreachable")
	code, _ := strconv.Atoi(os.Getenv(codeEnv))
	return code
}

func helperStderrFlood() int {
	if _, ok := readCommand(bufio.NewReader(os.Stdin)); !ok {
		return 0
	}
	fmt.Fprint(os.Stderr, "BEGIN\n"+strings.Repeat("x", 10<<10)+"\nEND-OF-STDERR\n")
	return 0
}

const (
	lineSep = "\u2028"
	paraSep = "\u2029"
)

// helperUnknown streams unknown, type-less, separator-bearing, CRLF-terminated
// and blank lines, then answers the first command.
func helperUnknown() int {
	r := bufio.NewReader(os.Stdin)
	c, ok := readCommand(r)
	if !ok {
		return 0
	}
	emit(`{"type":"future_event","n":1}`)
	emit(`{"n":2}`)
	emit(`{"type":"message_update","text":"a` + lineSep + `b` + paraSep + `c"}`)
	_, _ = os.Stdout.WriteString(`{"type":"turn_start"}` + "\r\n")
	emit("") // a blank line
	emit(`   `)
	emit(`{"type":"future_event","n":3}`)
	respond(c, `{}`)
	drainStdin(r)
	return 0
}

// helperOverlong writes a line of exactly 1000 bytes and then one of 1001.
func helperOverlong() int {
	r := bufio.NewReader(os.Stdin)
	if _, ok := readCommand(r); !ok {
		return 0
	}
	emit(lineOfLen(1000))
	emit(lineOfLen(1001))
	drainStdin(r)
	time.Sleep(time.Minute)
	return 0
}

func helperFlood() int {
	for i := 0; i < 2000; i++ {
		emit(fmt.Sprintf(`{"type":"message_update","n":%d}`, i))
	}
	drainStdin(bufio.NewReader(os.Stdin))
	return 0
}

// ---- parent side ----------------------------------------------------------

// watchdog kills the whole test binary, with a goroutine dump, if the test is
// still running after d. Use it as `defer watchdog(t, d)()`.
func watchdog(t *testing.T, d time.Duration) func() {
	timer := time.AfterFunc(d, func() {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		fmt.Fprintf(os.Stderr, "WATCHDOG: %s still running after %v\n%s\n", t.Name(), d, buf)
		os.Exit(3)
	})
	return func() { timer.Stop() }
}

// helperOptions points Options at the helper mode.
func helperOptions(mode string, env ...string) Options {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return Options{
		Command: exe,
		Args:    []string{"-test.run=TestHelperProcess$"},
		// GORACE only quiets the race runtime's one-second at-exit sleep.
		Env:       append(append(os.Environ(), "GORACE=atexit_sleep_ms=0", modeEnv+"="+mode), env...),
		KillGrace: time.Second,
		WaitDelay: 2 * time.Second,
	}
}

// launch starts the helper in mode, with opt's numeric options on top. A
// command timeout of 10s is the default so a bug fails instead of waiting 30s.
func launch(t *testing.T, mode string, opt Options, env ...string) *Process {
	t.Helper()
	base := helperOptions(mode, env...)
	opt.Command, opt.Args, opt.Env = base.Command, base.Args, base.Env
	if opt.KillGrace == 0 {
		opt.KillGrace = base.KillGrace
	}
	if opt.WaitDelay == 0 {
		opt.WaitDelay = base.WaitDelay
	}
	if opt.CommandTimeout == 0 {
		opt.CommandTimeout = 10 * time.Second
	}
	p, err := Launch(context.Background(), opt)
	if err != nil {
		t.Fatalf("Launch(%s) = %v", mode, err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// recv takes the next event or fails the test.
func recv(t *testing.T, p *Process) Message {
	t.Helper()
	select {
	case m, ok := <-p.Events():
		if !ok {
			t.Fatal("Events() closed, want another message")
		}
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("no event within 10s")
	}
	panic("unreachable")
}

// expectClosed fails the test unless Events() closes without another message.
func expectClosed(t *testing.T, p *Process) {
	t.Helper()
	select {
	case m, ok := <-p.Events():
		if ok {
			t.Fatalf("Events() delivered %q, want it closed", clip(m.Raw))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Events() did not close within 10s")
	}
}

// finish closes p and proves that none of the package's goroutines (nor
// internal/exec's) outlived it.
func finish(t *testing.T, p *Process) {
	t.Helper()
	if err := p.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
	noLeaks(t)
}

func noLeaks(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		stacks := leakedStacks()
		if len(stacks) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines of adapters/pi or internal/exec outlived the test:\n%s", strings.Join(stacks, "\n\n"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func leakedStacks() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var out []string
	for i, g := range strings.Split(string(buf), "\n\n") {
		if i == 0 {
			continue // the calling goroutine
		}
		if strings.Contains(g, "(*Process).") || strings.Contains(g, "oarlock/internal/exec.") {
			out = append(out, g)
		}
	}
	return out
}

// ---- tests ----------------------------------------------------------------

func TestDoCorrelatesByIDAndKeepsWireOrder(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	p := launch(t, "reverse", Options{})

	type echo struct {
		Echo string `json:"echo"`
	}
	var wg sync.WaitGroup
	outs := make([]echo, 3)
	errs := make([]error, 3)
	for i := range outs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = p.Do(context.Background(), Command{Type: "cmd" + strconv.Itoa(i)}, &outs[i])
		}()
	}
	wg.Wait()
	for i := range outs {
		if errs[i] != nil {
			t.Fatalf("Do(cmd%d) = %v", i, errs[i])
		}
		if want := "cmd" + strconv.Itoa(i); outs[i].Echo != want {
			t.Errorf("Do(cmd%d) got %q: the answer to another command", i, outs[i].Echo)
		}
	}

	// The child answered in reverse order; Events() keeps the wire order and
	// carries the responses too.
	var types []string
	var received []int
	for len(types) < 5 {
		m := recv(t, p)
		types = append(types, m.Type)
		if m.IsResponse() {
			var r struct {
				Data struct{ Received int } `json:"data"`
			}
			if err := json.Unmarshal(m.Raw, &r); err != nil {
				t.Fatalf("response %q: %v", m.Raw, err)
			}
			received = append(received, r.Data.Received)
		}
	}
	if got, want := strings.Join(types, ","), "turn_start,response,response,response,turn_end"; got != want {
		t.Errorf("Events() order = %s, want %s", got, want)
	}
	if !reflect.DeepEqual(received, []int{2, 1, 0}) {
		t.Errorf("responses reached Events() as receipt order %v, want the wire order [2 1 0]", received)
	}
	st := p.Stats()
	if st.Messages != 5 || st.Responses != 3 || st.Orphans != 0 || len(st.Unknown) != 0 {
		t.Errorf("Stats() = %+v, want 5 messages, 3 correlated responses, no orphans, nothing unknown", st)
	}
	finish(t, p)
}

func TestConcurrentDoWritesWholeLines(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	p := launch(t, "echo", Options{})
	go func() {
		for range p.Events() {
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out struct {
				Echo string `json:"echo"`
			}
			typ := "c" + strconv.Itoa(i)
			if err := p.Do(context.Background(), Command{Type: typ, Params: map[string]any{"n": i, "pad": strings.Repeat("p", 4096)}}, &out); err != nil {
				t.Errorf("Do(%s) = %v", typ, err)
			} else if out.Echo != typ {
				t.Errorf("Do(%s) decoded %q", typ, out.Echo)
			}
		}()
	}
	wg.Wait()
	finish(t, p)
}

// Acceptance: a child that never answers -> the command timeout -> every
// pending command fails and the process group is killed.
func TestSilentChildTimesOutAndFailsEveryPendingCommand(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	p := launch(t, "silent", Options{CommandTimeout: 300 * time.Millisecond})

	start := time.Now()
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = p.Do(context.Background(), Command{Type: "get_state"}, nil)
		}()
	}
	wg.Wait()
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond || elapsed > 8*time.Second {
		t.Errorf("pending commands failed after %v, want about the 300ms timeout", elapsed)
	}
	timeouts := 0
	for i, err := range errs {
		if !errors.Is(err, ErrTimeout) && !errors.Is(err, ErrExited) {
			t.Errorf("Do #%d = %v, want ErrTimeout or ErrExited", i, err)
		}
		var te *TimeoutError
		if errors.As(err, &te) {
			timeouts++
			if te.After != 300*time.Millisecond || te.Command != "get_state" || !strings.HasPrefix(te.ID, "oar-pi-") {
				t.Errorf("TimeoutError = %+v", te)
			}
		}
	}
	if timeouts == 0 {
		t.Errorf("no *TimeoutError among %v", errs)
	}

	st := p.Wait()
	if st.Cause != CauseTimeout {
		t.Errorf("ExitStatus.Cause = %q, want %q", st.Cause, CauseTimeout)
	}
	if runtime.GOOS != "windows" && st.Signal != os.Kill {
		t.Errorf("ExitStatus.Signal = %v, want the group SIGKILL", st.Signal)
	}
	select {
	case got, ok := <-p.Exit():
		if !ok || got.Cause != CauseTimeout {
			t.Errorf("Exit() = %+v, %v, want the timeout status", got, ok)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Exit() delivered nothing")
	}

	// A command after the kill fails at once, with the recorded cause.
	err := p.Do(context.Background(), Command{Type: "get_state"}, nil)
	var ee *ExitedError
	if !errors.Is(err, ErrExited) || !errors.As(err, &ee) || ee.Status.Cause != CauseTimeout {
		t.Errorf("Do after the kill = %v, want an *ExitedError with cause %q", err, CauseTimeout)
	}
	finish(t, p)
}

// A context that ends abandons one wait; it never kills the child.
func TestContextCancelAbandonsTheWaitWithoutKillingTheChild(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	p := launch(t, "silent", Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := p.Do(ctx, Command{Type: "get_state"}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do = %v, want context.DeadlineExceeded", err)
	}
	select {
	case st := <-p.Exit():
		t.Fatalf("the child was ended by a cancelled Do: %+v", st)
	case <-time.After(300 * time.Millisecond):
	}
	if err := p.Do(ctx, Command{Type: "get_state"}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do with a dead ctx = %v, want context.DeadlineExceeded", err)
	}
	finish(t, p)
}

// Acceptance: half a line and then the exit -> the parser reports it, nothing
// hangs, and the partial line is never delivered as a message.
func TestHalfLineThenExitFailsWithoutHanging(t *testing.T) {
	defer watchdog(t, 30*time.Second)()

	t.Run("idle", func(t *testing.T) {
		p := launch(t, "halfline", Options{})
		if m := recv(t, p); m.Type != "agent_start" {
			t.Fatalf("first event = %q, want the complete agent_start", m.Raw)
		}
		expectClosed(t, p)
		st := p.Wait()
		if !errors.Is(st.Err, ErrTruncatedLine) {
			t.Fatalf("ExitStatus.Err = %v, want ErrTruncatedLine", st.Err)
		}
		if !strings.Contains(st.Err.Error(), "message_upd") {
			t.Errorf("ExitStatus.Err = %v, want the quoted tail of the partial line", st.Err)
		}
		finish(t, p)
	})

	t.Run("pending command", func(t *testing.T) {
		p := launch(t, "halfline-after-command", Options{CommandTimeout: 20 * time.Second})
		start := time.Now()
		err := p.Do(context.Background(), Command{Type: "get_state"}, nil)
		if !errors.Is(err, ErrExited) || !errors.Is(err, ErrTruncatedLine) || errors.Is(err, ErrTimeout) {
			t.Fatalf("Do = %v, want ErrExited wrapping ErrTruncatedLine, not a timeout", err)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("Do failed after %v, want it prompt", elapsed)
		}
		finish(t, p)
	})
}

// Acceptance: a single 5 MiB JSON line reads intact.
func TestFiveMegabyteLinesAreReadIntact(t *testing.T) {
	defer watchdog(t, 60*time.Second)()

	t.Run("event", func(t *testing.T) {
		p := launch(t, "bigevent", Options{})
		m := recv(t, p)
		want := bigEventLine()
		if m.Type != "message_update" || !bytes.Equal(m.Raw, want) {
			t.Fatalf("event: type %q, %d bytes, want %d bytes byte for byte", m.Type, len(m.Raw), len(want))
		}
		finish(t, p)
	})

	t.Run("response", func(t *testing.T) {
		p := launch(t, "bigresponse", Options{})
		var out struct {
			Text string `json:"text"`
		}
		if err := p.Do(context.Background(), Command{Type: "get_state"}, &out); err != nil {
			t.Fatalf("Do = %v", err)
		}
		if out.Text != bigText() {
			t.Fatalf("decoded %d bytes, want the %d-byte text intact", len(out.Text), fiveMiB)
		}
		m := recv(t, p)
		if !m.IsResponse() || !bytes.Contains(m.Raw, []byte(bigText())) {
			t.Errorf("Events() copy of the response is %d bytes, want the intact line", len(m.Raw))
		}
		finish(t, p)
	})
}

// Acceptance: an over-long line is an error, never a silent truncation; a line
// of exactly the cap passes.
func TestOverlongLineIsAnErrorNotATruncation(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	p := launch(t, "overlong", Options{MaxLineBytes: 1000, CommandTimeout: 20 * time.Second})
	start := time.Now()
	err := p.Do(context.Background(), Command{Type: "get_state"}, nil)
	if !errors.Is(err, ErrExited) || !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("Do = %v, want ErrExited wrapping ErrLineTooLong", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Do failed after %v, want it prompt (not the command timeout)", elapsed)
	}
	if m := recv(t, p); len(m.Raw) != 1000 {
		t.Errorf("the at-cap line is %d bytes, want 1000 intact", len(m.Raw))
	}
	expectClosed(t, p)
	st := p.Wait()
	if st.Cause != CauseProtocol || !errors.Is(st.Err, ErrLineTooLong) {
		t.Errorf("ExitStatus = cause %q err %v, want protocol / ErrLineTooLong", st.Cause, st.Err)
	}
	if runtime.GOOS != "windows" && st.Signal != os.Kill {
		t.Errorf("ExitStatus.Signal = %v, want the group killed", st.Signal)
	}
	finish(t, p)
}

// Acceptance: exit code and stderr tail are the evidence of an exit.
func TestExitCodeAndStderrTailAreEvidence(t *testing.T) {
	defer watchdog(t, 30*time.Second)()

	t.Run("exit code", func(t *testing.T) {
		p := launch(t, "stderr-exit", Options{}, codeEnv+"=7")
		err := p.Do(context.Background(), Command{Type: "prompt", Params: map[string]any{"message": "hi"}}, nil)
		var ee *ExitedError
		if !errors.Is(err, ErrExited) || !errors.As(err, &ee) {
			t.Fatalf("Do = %v, want an *ExitedError", err)
		}
		if ee.Status.Code != 7 || ee.Status.Signal != nil || ee.Status.Cause != CauseExited || ee.Status.Err != nil {
			t.Errorf("Status = %+v, want code 7, no signal, cause exited, no error", ee.Status)
		}
		const tail = "boom: model backend unreachable"
		if !strings.Contains(ee.Status.StderrTail, tail) || !strings.Contains(err.Error(), tail) || !strings.Contains(err.Error(), "code 7") {
			t.Errorf("error %q / tail %q, want the exit code and the stderr tail", err, ee.Status.StderrTail)
		}
		st, ok := <-p.Exit()
		if !ok || st.Code != 7 || st.StderrTail != ee.Status.StderrTail {
			t.Errorf("Exit() = %+v, %v, want the same evidence", st, ok)
		}
		if _, ok := <-p.Exit(); ok {
			t.Error("Exit() delivered a second status")
		}
		expectClosed(t, p)
		finish(t, p)
	})

	t.Run("tail keeps the end", func(t *testing.T) {
		p := launch(t, "stderr-flood", Options{StderrTailBytes: 32})
		if err := p.Do(context.Background(), Command{Type: "get_state"}, nil); !errors.Is(err, ErrExited) {
			t.Fatalf("Do = %v, want ErrExited", err)
		}
		tail := p.Wait().StderrTail
		if len(tail) > 32 || !strings.HasSuffix(tail, "END-OF-STDERR\n") || strings.Contains(tail, "BEGIN") {
			t.Errorf("StderrTail = %q (%d bytes), want only the last <=32 bytes", tail, len(tail))
		}
		finish(t, p)
	})
}

// Unknown messages are delivered verbatim and counted; known types and
// responses are not tallied; LF is the only separator.
func TestUnknownMessagesAreVerbatimAndCounted(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	p := launch(t, "unknown", Options{})
	if err := p.Do(context.Background(), Command{Type: "get_state"}, nil); err != nil {
		t.Fatalf("Do = %v", err)
	}
	want := []string{
		`{"type":"future_event","n":1}`,
		`{"n":2}`,
		`{"type":"message_update","text":"a` + lineSep + `b` + paraSep + `c"}`,
		`{"type":"turn_start"}`, // the CR of the CRLF is gone
		`{"type":"future_event","n":3}`,
	}
	for i, w := range want {
		if m := recv(t, p); string(m.Raw) != w {
			t.Errorf("event %d Raw = %q, want %q", i, m.Raw, w)
		}
	}
	if m := recv(t, p); !m.IsResponse() {
		t.Errorf("last message = %q, want the response", m.Raw)
	}
	st := p.Stats()
	if st.Messages != 6 || st.Responses != 1 || st.Orphans != 0 || !reflect.DeepEqual(st.Unknown, map[string]int{"future_event": 2, "": 1}) {
		t.Errorf("Stats() = %+v, want 6 messages, 1 response and unknown {future_event:2, \"\":1}", st)
	}
	finish(t, p)
}

func TestCloseIsIdempotentAndReleasesEverything(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	p := launch(t, "echo", Options{})
	if err := p.Do(context.Background(), Command{Type: "get_state"}, nil); err != nil {
		t.Fatalf("Do = %v", err)
	}
	errs := make([]error, 3)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = p.Close() }()
	}
	wg.Wait()
	for i, err := range errs {
		if err != errs[0] {
			t.Errorf("Close #%d = %v, want the same result as #0 (%v)", i, err, errs[0])
		}
	}
	if err := p.Close(); err != errs[0] {
		t.Errorf("a later Close = %v, want %v", err, errs[0])
	}
	if st := p.Wait(); st.Cause != CauseClosed {
		t.Errorf("ExitStatus.Cause = %q, want %q", st.Cause, CauseClosed)
	}
	for range p.Events() { // drains the response, then must be closed
	}
	if err := p.Do(context.Background(), Command{Type: "get_state"}, nil); !errors.Is(err, ErrExited) {
		t.Errorf("Do after Close = %v, want ErrExited", err)
	}
	noLeaks(t)
}

// A consumer that never drains Events() must not make Close hang.
func TestCloseReleasesAReaderBlockedOnAFullEventsChannel(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	p := launch(t, "flood", Options{EventBuffer: 4})
	time.Sleep(300 * time.Millisecond) // let the reader fill the buffer and block
	done := make(chan error, 1)
	go func() { done <- p.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return while the events channel was full")
	}
	noLeaks(t)
}

func TestOptionsDefaultsAndValidation(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	if got := DefaultArgs(); !reflect.DeepEqual(got, []string{"--mode", "rpc"}) {
		t.Errorf("DefaultArgs() = %v, want [--mode rpc]", got)
	}
	a := DefaultArgs()
	a[0] = "mutated"
	if DefaultArgs()[0] != "--mode" {
		t.Error("DefaultArgs() shares its backing array between calls")
	}
	if DefaultExecutable != "pi" || DefaultCommandTimeout != 30*time.Second || DefaultMaxLineBytes != 16<<20 ||
		DefaultStderrTailBytes != 8<<10 || DefaultEventBuffer != 256 {
		t.Error("the documented defaults changed")
	}

	p, err := Launch(context.Background(), helperOptions("echo"))
	if err != nil {
		t.Fatal(err)
	}
	if p.timeout != 30*time.Second || p.maxLine != 16<<20 || len(p.tail.buf) != 8<<10 || cap(p.events) != 256 {
		t.Errorf("zero Options resolved to timeout %v, line %d, tail %d, events %d", p.timeout, p.maxLine, len(p.tail.buf), cap(p.events))
	}
	finish(t, p)

	o := helperOptions("echo")
	o.CommandTimeout, o.MaxLineBytes, o.StderrTailBytes, o.EventBuffer = 123*time.Millisecond, 77, 9, 3
	p, err = Launch(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if p.timeout != 123*time.Millisecond || p.maxLine != 77 || len(p.tail.buf) != 9 || cap(p.events) != 3 {
		t.Errorf("overrides not honoured: timeout %v, line %d, tail %d, events %d", p.timeout, p.maxLine, len(p.tail.buf), cap(p.events))
	}
	finish(t, p)

	for name, mutate := range map[string]func(*Options){
		"CommandTimeout":  func(o *Options) { o.CommandTimeout = -1 },
		"MaxLineBytes":    func(o *Options) { o.MaxLineBytes = -1 },
		"StderrTailBytes": func(o *Options) { o.StderrTailBytes = -1 },
		"EventBuffer":     func(o *Options) { o.EventBuffer = -1 },
	} {
		o := helperOptions("echo")
		mutate(&o)
		if p, err := Launch(context.Background(), o); err == nil {
			_ = p.Close()
			t.Errorf("negative %s was accepted", name)
		}
	}

	missing := Options{Command: filepath.Join(t.TempDir(), "no-such-pi")}
	if p, err := Launch(context.Background(), missing); err == nil {
		_ = p.Close()
		t.Error("Launch of a missing binary succeeded")
	}
	noLeaks(t)
}

func TestCommandEncodingRefusesWhatItCannotWriteAsOneLine(t *testing.T) {
	for name, cmd := range map[string]Command{
		"no type":       {},
		"params array":  {Type: "x", Params: []int{1}},
		"params scalar": {Type: "x", Params: 3},
		"params type":   {Type: "x", Params: map[string]any{"type": "other"}},
		"params id":     {Type: "x", Params: map[string]any{"id": "other"}},
		"unmarshalable": {Type: "x", Params: make(chan int)},
	} {
		if _, err := encodeCommand("oar-pi-1", cmd); err == nil {
			t.Errorf("%s: encodeCommand accepted %+v", name, cmd)
		}
	}
	line, err := encodeCommand("oar-pi-1", Command{Type: "prompt", Params: map[string]any{"message": "a\nb\u2028c"}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(line, []byte("\n")) != 1 || line[len(line)-1] != '\n' {
		t.Errorf("encoded %q, want exactly one LF at the end", line)
	}
	var back map[string]any
	if err := json.Unmarshal(line, &back); err != nil || back["id"] != "oar-pi-1" || back["type"] != "prompt" || back["message"] != "a\nb\u2028c" {
		t.Errorf("round trip = %v, %v", back, err)
	}
}
