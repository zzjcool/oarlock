package cursor

// A fake cursor-agent: the test binary re-executed with CURSOR_HELPER_MODE.
// It behaves like the CLI in the one way the session depends on — the prompt is
// free text on stdin and the run starts at EOF — and then emits stream-json
// frames. Behaviour switches are environment variables, so one body serves
// every test. It is NOT a recording: the real recordings are the
// testdata/cursor fixtures (mode "replay" plays one back verbatim).
//
// Every process appends its own argv and its stdin statistics to files in the
// test's temp directory, so a test can assert what the adapter launched
// (for example `--resume <chat id>`) and how many processes ran.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

const (
	modeEnv   = "CURSOR_HELPER_MODE"
	argsEnv   = "CURSOR_HELPER_ARGS"
	stdinEnv  = "CURSOR_HELPER_STDIN"
	gateEnv   = "CURSOR_HELPER_GATE"
	replayEnv = "CURSOR_HELPER_REPLAY"
	flagsEnv  = "CURSOR_HELPER_FLAGS" // comma list: ignoreterm
	chatEnv   = "CURSOR_HELPER_CHAT"  // the chat id a fresh run assigns
)

func helperFlag(name string) bool {
	for _, f := range strings.Split(os.Getenv(flagsEnv), ",") {
		if f == name {
			return true
		}
	}
	return false
}

func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(modeEnv)
	if mode == "" {
		return
	}
	os.Exit(runHelper(mode))
}

var helperOut = bufio.NewWriter(os.Stdout)

func emit(v any) {
	b, _ := json.Marshal(v)
	helperOut.Write(b)
	helperOut.WriteByte('\n')
	helperOut.Flush()
}

func emitRaw(line string) {
	helperOut.WriteString(line)
	helperOut.WriteByte('\n')
	helperOut.Flush()
}

func runHelper(mode string) int {
	if f := os.Getenv(argsEnv); f != "" {
		// Append: one line per process, so a test can count processes.
		h, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = h.WriteString(strings.Join(os.Args, "\x00") + "\n")
			_ = h.Close()
		}
	}
	if helperFlag("ignoreterm") {
		// A child that survives SIGTERM: only the kill grace ends it.
		signal.Ignore(syscall.SIGTERM)
	}
	chat := resumedChatID(os.Args)
	fresh := os.Getenv(chatEnv)
	if fresh == "" {
		fresh = "chat-fresh"
	}

	if mode == "replay" {
		// The prompt is still read to EOF first, then the recording is played.
		n, lines, _ := readStdin()
		recordStdin(n, lines)
		return replayFixture()
	}

	// Every mode but "big" reads the prompt to EOF first, like the CLI.
	var prompt []byte
	n, lines := int64(0), int64(0)
	if mode == "big" {
		// The deadlock case: both pipes in use at once. The init frame goes out
		// before stdin is drained.
		emit(initFrame(fresh))
		n, lines, prompt = readStdin()
	} else {
		n, lines, prompt = readStdin()
	}
	recordStdin(n, lines)
	if chat == "" {
		chat = fresh
	}

	switch mode {
	case "turn", "big", "unknown":
		if mode != "big" {
			emit(initFrame(chat))
		}
		emit(userEcho(chat, head(prompt, 200)))
		emit(assistantFrame(chat, "working"))
		if mode == "unknown" {
			emitRaw(`{"type":"cursor_telemetry","payload":{"n":1}}`)
		}
		emit(resultFrame(chat, "ok"))
	case "gate":
		// Hold the turn open until the test creates the gate file, so a queued
		// input is provably held rather than raced.
		emit(initFrame(chat))
		emit(userEcho(chat, head(prompt, 200)))
		emit(assistantFrame(chat, "working"))
		if !waitGate(30 * time.Second) {
			return 91
		}
		emit(resultFrame(chat, "ok"))
	case "die":
		// A process that dies without a result: the session's own exit.
		fmt.Fprintln(os.Stderr, "fake cursor-agent: dying without a result")
		return 3
	case "hang":
		emit(initFrame(chat))
		emit(userEcho(chat, head(prompt, 200)))
		emit(assistantFrame(chat, "working"))
		time.Sleep(time.Minute) // until killed
		return 92
	default:
		fmt.Fprintln(os.Stderr, "fake cursor-agent: unknown mode "+mode)
		return 90
	}
	return 0
}

func initFrame(chat string) map[string]any {
	return map[string]any{
		"type": "system", "subtype": "init", "apiKeySource": "login",
		"session_id": chat, "model": "Auto Balance", "permissionMode": "default",
	}
}

func userEcho(chat, text string) map[string]any {
	return map[string]any{
		"type": "user", "session_id": chat,
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}},
	}
}

func assistantFrame(chat, text string) map[string]any {
	return map[string]any{
		"type": "assistant", "session_id": chat,
		"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}},
	}
}

func resultFrame(chat, text string) map[string]any {
	return map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "result": text,
		"session_id": chat, "duration_ms": 5,
		"usage": map[string]any{"inputTokens": 10, "outputTokens": 2, "cacheReadTokens": 0, "cacheWriteTokens": 0},
	}
}

// resumedChatID is the chat the adapter asked to continue, from argv.
func resumedChatID(args []string) string {
	for i, a := range args {
		if a == "--resume" && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--resume="); ok {
			return v
		}
	}
	return ""
}

// readStdin drains the prompt to EOF, returning its byte and line counts and a
// copy of it (the fake needs the text only for the echo).
func readStdin() (int64, int64, []byte) {
	var buf bytes.Buffer
	n, _ := io.Copy(&buf, os.Stdin)
	data := buf.Bytes()
	return n, int64(bytes.Count(data, []byte("\n"))), data
}

func recordStdin(n, lines int64) {
	if f := os.Getenv(stdinEnv); f != "" {
		_ = os.WriteFile(f, []byte(fmt.Sprintf("%d %d", n, lines)), 0o600)
	}
}

func waitGate(timeout time.Duration) bool {
	path := os.Getenv(gateEnv)
	if path == "" {
		return true
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

func replayFixture() int {
	f, err := os.Open(os.Getenv(replayEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake cursor-agent: replay:", err)
		return 90
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		emitRaw(sc.Text())
	}
	return 0
}

func head(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}

// ---- harness ------------------------------------------------------------------

// helper is one fake cursor-agent session: the session under test plus the
// files the fake writes.
type helper struct {
	t     *testing.T
	sess  *Session
	dir   string
	args  string
	stdin string
	gate  string
}

// newHelper opens a Session over the fake cursor-agent in mode.
func newHelper(t *testing.T, mode string, opt oarlock.Options, so SessionOptions, flags ...string) *helper {
	t.Helper()
	return openHelper(t, mode, opt, so, "", flags...)
}

// replayHelper opens a session whose fake cursor-agent plays a fixture back.
func replayHelper(t *testing.T, fixture string) *helper {
	t.Helper()
	return openHelper(t, "replay", oarlock.Options{}, SessionOptions{}, fixture)
}

// openHelper wires open() to the test binary: -test.run selects
// TestHelperProcess, everything after -- is the argv the adapter built.
func openHelper(t *testing.T, mode string, opt oarlock.Options, so SessionOptions, fixture string, flags ...string) *helper {
	t.Helper()
	h := &helper{t: t, dir: t.TempDir()}
	h.args = filepath.Join(h.dir, "args")
	h.stdin = filepath.Join(h.dir, "stdin")
	h.gate = filepath.Join(h.dir, "gate")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := []string{
		"GORACE=atexit_sleep_ms=0",
		modeEnv + "=" + mode,
		argsEnv + "=" + h.args,
		stdinEnv + "=" + h.stdin,
		gateEnv + "=" + h.gate,
		flagsEnv + "=" + strings.Join(flags, ","),
		chatEnv + "=chat-fresh",
	}
	if fixture != "" {
		env = append(env, replayEnv+"="+fixture)
	}
	spec := openSpec{
		launch:      sj.Options{Command: exe},
		session:     so,
		extraPrefix: []string{"-test.run=TestHelperProcess$", "--"},
		extraEnv:    env,
	}
	sess, err := open(context.Background(), spec, opt)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	h.sess = sess
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = sess.Dispose(ctx)
	})
	return h
}

// argv is the argument vector of every process the fake ran, oldest first.
func (h *helper) argv() [][]string {
	h.t.Helper()
	data, err := os.ReadFile(h.args)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		h.t.Fatalf("read args: %v", err)
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		out = append(out, strings.Split(line, "\x00"))
	}
	return out
}

// waitProcesses waits until the fake has run at least n processes.
func (h *helper) waitProcesses(n int) [][]string {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		argv := h.argv()
		if len(argv) >= n {
			return argv
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %d fake processes, saw %d", n, len(argv))
		}
		time.Sleep(3 * time.Millisecond)
	}
}

// stdinStats is the byte and newline count the last fake process read.
func (h *helper) stdinStats() (bytes, lines int64) {
	h.t.Helper()
	data, err := os.ReadFile(h.stdin)
	if err != nil {
		h.t.Fatalf("read stdin stats: %v", err)
	}
	if _, err := fmt.Sscanf(string(data), "%d %d", &bytes, &lines); err != nil {
		h.t.Fatalf("parse stdin stats %q: %v", data, err)
	}
	return bytes, lines
}

// openGate lets a "gate" fake finish its turn.
func (h *helper) openGate() {
	h.t.Helper()
	if err := os.WriteFile(h.gate, []byte("go"), 0o600); err != nil {
		h.t.Fatalf("open gate: %v", err)
	}
}

// hasFlag reports whether argv contains flag, and the value after it.
func hasFlag(argv []string, flag string) (value string, ok bool) {
	for i, a := range argv {
		if a == flag {
			if i+1 < len(argv) {
				return argv[i+1], true
			}
			return "", true
		}
		if v, cut := strings.CutPrefix(a, flag+"="); cut {
			return v, true
		}
	}
	return "", false
}
