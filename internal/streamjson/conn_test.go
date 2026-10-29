package streamjson

import (
	"bufio"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

const modeEnv = "STREAMJSON_HELPER_MODE"

func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(modeEnv)
	if mode == "" {
		return
	}
	switch mode {
	case "echo": // every stdin line comes back as {"type":"echo","line":<n>,"len":<bytes>}
		in := bufio.NewReaderSize(os.Stdin, 1<<20)
		n := 0
		for {
			line, err := in.ReadBytes('\n')
			if len(line) == 0 && err != nil {
				os.Exit(0)
			}
			n++
			os.Stdout.WriteString(`{"type":"echo","n":` + itoa(n) + `,"len":` + itoa(len(line)) + "}\n")
		}
	case "flood-then-read": // writes a lot of stdout BEFORE reading stdin: a coupled transport deadlocks
		big := strings.Repeat("x", 1<<16)
		for i := 0; i < 64; i++ {
			os.Stdout.WriteString(`{"type":"noise","pad":"` + big + "\"}\n")
		}
		in := bufio.NewReaderSize(os.Stdin, 1<<20)
		line, _ := in.ReadBytes('\n')
		os.Stdout.WriteString(`{"type":"got","len":` + itoa(len(line)) + "}\n")
		os.Exit(0)
	case "framing":
		os.Stdout.WriteString("{\"type\":\"a\",\"s\":\"x\u2028y\u0085z\"}\r\n\n   \n{\"type\":\"b\"}\n")
		os.Exit(0)
	case "garbage":
		os.Stdout.WriteString("{\"type\":\"a\"}\nnot json\n{\"type\":\"never\"}\n")
		time.Sleep(time.Minute)
	case "halfline":
		os.Stdout.WriteString("{\"type\":\"a\"}\n{\"type\":\"b")
		os.Exit(0)
	case "deaf": // never reads stdin
		time.Sleep(time.Minute)
	case "exit3":
		os.Stderr.WriteString("boom on stderr\n")
		os.Exit(3)
	}
	os.Exit(97)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func launchHelper(t *testing.T, mode string, opt Options) *Process {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	opt.Command, opt.Args = exe, []string{"-test.run=TestHelperProcess$"}
	opt.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0", modeEnv+"="+mode)
	if opt.KillGrace == 0 {
		opt.KillGrace = time.Second
	}
	if opt.WaitDelay == 0 {
		opt.WaitDelay = 2 * time.Second
	}
	p, err := Launch(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func recv(t *testing.T, p *Process) (Message, bool) {
	t.Helper()
	select {
	case m, ok := <-p.Events():
		return m, ok
	case <-time.After(15 * time.Second):
		t.Fatal("no event within 15s")
	}
	return Message{}, false
}

func TestEchoRoundTripKeepsLinesWholeAndOrdered(t *testing.T) {
	p := launchHelper(t, "echo", Options{})
	for i := 1; i <= 5; i++ {
		if err := p.Write(context.Background(), []byte(`{"type":"user","i":`+itoa(i)+"}\n")); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 5; i++ {
		m, ok := recv(t, p)
		if !ok || m.Type != "echo" || !strings.Contains(string(m.Raw), `"n":`+itoa(i)+",") {
			t.Fatalf("event %d = %s", i, m.Raw)
		}
	}
}

func TestWriteWantsExactlyOneLine(t *testing.T) {
	p := launchHelper(t, "echo", Options{})
	for _, bad := range []string{"", "no newline", "two\nlines\n", "\n\n"} {
		if err := p.Write(context.Background(), []byte(bad)); err == nil {
			t.Errorf("Write(%q) accepted", bad)
		}
	}
}

// Reading and writing are decoupled: the child fills its stdout pipe (4 MiB)
// before it reads a line of stdin, while we write a 1 MiB line. A transport that
// writes inline with the reader parked would deadlock; this one finishes.
func TestWriteAndReadAreDecoupled(t *testing.T) {
	p := launchHelper(t, "flood-then-read", Options{EventBuffer: 4})
	big := `{"type":"user","pad":"` + strings.Repeat("y", 1<<20) + "\"}\n"
	errc := make(chan error, 1)
	go func() { errc <- p.Write(context.Background(), []byte(big)) }()
	var got bool
	for m := range p.Events() {
		if m.Type == "got" {
			got = true
			if !strings.Contains(string(m.Raw), `"len":`+itoa(len(big))) {
				t.Fatalf("the child saw a different line: %s", m.Raw)
			}
		}
	}
	if err := <-errc; err != nil {
		t.Fatalf("Write = %v", err)
	}
	if !got {
		t.Fatal("the child never got the line")
	}
}

func TestFramingIsLFOnlyAndVerbatim(t *testing.T) {
	p := launchHelper(t, "framing", Options{})
	a, _ := recv(t, p)
	if a.Type != "a" || !strings.Contains(string(a.Raw), "x\u2028y\u0085z") || strings.HasSuffix(string(a.Raw), "\r") {
		t.Fatalf("a = %q", a.Raw)
	}
	b, _ := recv(t, p)
	if b.Type != "b" {
		t.Fatalf("b = %q: blank lines must be skipped", b.Raw)
	}
	if _, ok := recv(t, p); ok {
		t.Fatal("expected the stream to end")
	}
}

func TestGarbageLineFailsClosedAndKillsTheGroup(t *testing.T) {
	p := launchHelper(t, "garbage", Options{})
	if m, _ := recv(t, p); m.Type != "a" {
		t.Fatalf("first = %s", m.Raw)
	}
	for m := range p.Events() {
		if m.Type == "never" {
			t.Fatal("a frame after a garbled line was delivered")
		}
	}
	st := p.Wait()
	if !errors.Is(st.Err, ErrBadFrame) || st.Cause != CauseProtocol {
		t.Fatalf("status = %+v", st)
	}
}

func TestTruncatedTailIsAnErrorNotAFrame(t *testing.T) {
	p := launchHelper(t, "halfline", Options{})
	if m, _ := recv(t, p); m.Type != "a" {
		t.Fatalf("first = %s", m.Raw)
	}
	for m := range p.Events() {
		t.Fatalf("a partial line was delivered: %s", m.Raw)
	}
	if st := p.Wait(); !errors.Is(st.Err, ErrTruncatedLine) {
		t.Fatalf("status = %+v", st)
	}
}

func TestOverlongLineIsFatal(t *testing.T) {
	p := launchHelper(t, "flood-then-read", Options{MaxLineBytes: 1024})
	for range p.Events() {
	}
	if st := p.Wait(); !errors.Is(st.Err, ErrLineTooLong) {
		t.Fatalf("status = %+v", st)
	}
}

func TestExitIsDataWithStderrTail(t *testing.T) {
	p := launchHelper(t, "exit3", Options{})
	st := p.Wait()
	if st.Code != 3 || st.Cause != CauseExited || !strings.Contains(st.StderrTail, "boom on stderr") {
		t.Fatalf("status = %+v", st)
	}
	err := p.Write(context.Background(), []byte("{}\n"))
	var ee *ExitedError
	if !errors.As(err, &ee) || !errors.Is(err, ErrExited) {
		t.Fatalf("Write after exit = %v", err)
	}
}

// A child that stops reading stdin: the write times out and the group is
// SIGKILLed instead of hanging the caller.
func TestWriteTimeoutKillsTheGroup(t *testing.T) {
	p := launchHelper(t, "deaf", Options{WriteTimeout: 300 * time.Millisecond})
	big := []byte(`{"pad":"` + strings.Repeat("z", 4<<20) + "\"}\n")
	err := p.Write(context.Background(), big)
	if !errors.Is(err, ErrWriteTimeout) {
		t.Fatalf("Write = %v, want ErrWriteTimeout", err)
	}
	if st := p.Wait(); st.Cause != CauseTimeout {
		t.Fatalf("status = %+v", st)
	}
}

func TestCtxAbandonsOnlyTheWait(t *testing.T) {
	p := launchHelper(t, "deaf", Options{WriteTimeout: time.Minute})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	big := []byte(`{"pad":"` + strings.Repeat("z", 4<<20) + "\"}\n")
	if err := p.Write(ctx, big); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Write = %v", err)
	}
	select {
	case <-p.done:
		t.Fatal("a cancelled ctx must not kill the child")
	default:
	}
}

func TestCloseIsIdempotentAndEndsEverything(t *testing.T) {
	p := launchHelper(t, "deaf", Options{})
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if st := p.Wait(); st.Cause != CauseClosed {
		t.Fatalf("status = %+v", st)
	}
}

func TestLaunchValidates(t *testing.T) {
	for _, o := range []Options{{}, {Command: "x", WriteTimeout: -1}, {Command: "x", MaxLineBytes: -1}, {Command: "x", EventBuffer: -1}, {Command: "x", StderrTailBytes: -1}} {
		if _, err := Launch(context.Background(), o); err == nil {
			t.Errorf("Launch(%+v) accepted", o)
		}
	}
}
