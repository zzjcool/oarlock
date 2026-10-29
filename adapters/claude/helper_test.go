package claude

// A fake claude: the test binary re-executed with CLAUDE_HELPER_MODE. It speaks
// the stream-json dialect just enough to drive the session: user messages in,
// replayed user echoes, init / assistant / result frames out, interrupt and the
// private control requests answered. Behaviour switches are env vars so one
// body serves every test. It is NOT a recording: the real recordings are the
// testdata/ fixtures.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

const (
	modeEnv     = "CLAUDE_HELPER_MODE"
	argsFileEnv = "CLAUDE_HELPER_ARGS"
	flagsEnv    = "CLAUDE_HELPER_FLAGS" // comma list: noecho, ignoreabort, refuseabort, nobaseline, nobreakdown
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

var out = bufio.NewWriter(os.Stdout)

func emit(v any) {
	b, _ := json.Marshal(v)
	out.Write(b)
	out.WriteByte('\n')
	out.Flush()
}

const helperContextAnswer = `{"categories":[{"name":"System prompt","tokens":2375,"kind":"used"},{"name":"MCP tools","tokens":120,"kind":"used"},{"name":"MCP tools (deferred)","tokens":165,"kind":"deferred"},{"name":"Memory files","tokens":111,"kind":"used"},{"name":"Skills","tokens":556,"kind":"used"},{"name":"Autocompact buffer","tokens":33000,"kind":"buffer"},{"name":"Free space","tokens":963838,"kind":"free"},{"name":"Something new","tokens":5,"kind":"pinned"}],"totalTokens":3162,"maxTokens":1000000,"memoryFiles":[{"path":"/home/user/.claude/CLAUDE.md","tokens":111}],"mcpTools":[{"name":"mcp__echo__echo","tokens":120,"isLoaded":true},{"name":"mcp__docs__search","tokens":165,"isLoaded":false}],"agents":[],"skills":{"skillFrontmatter":[{"name":"dataviz","tokens":482},{"name":"frontend-design","tokens":74}]}}`

func runHelper(mode string) int {
	if f := os.Getenv(argsFileEnv); f != "" {
		_ = os.WriteFile(f, []byte(strings.Join(os.Args, "\x00")), 0o600)
	}
	if mode == "missing-resume" {
		emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true,
			"errors": []string{"No conversation found with session ID: sess-gone"}, "modelUsage": map[string]any{}})
		return 1
	}
	if mode == "replay" {
		// Wait for the first stdin line, then play the fixture verbatim.
		in := bufio.NewReader(os.Stdin)
		_, _ = in.ReadBytes('\n')
		f, err := os.Open(os.Getenv("CLAUDE_HELPER_REPLAY"))
		if err != nil {
			return 90
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			out.WriteString(sc.Text() + "\n")
		}
		out.Flush()
		return 0
	}
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	turn := 0
	offset := 0 // a resumed claude continues the previous process's running total
	for _, a := range os.Args {
		if a == "--resume" {
			offset = 100
		}
	}
	hanging := false
	var abortID string
	interrupted := make(chan struct{}, 1)
	_ = abortID
	for {
		line, err := in.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			if helperFlag("ignoreabort") {
				// A claude that ignores stdin EOF too: only a signal ends it.
				time.Sleep(time.Minute)
			}
			return 0
		}
		m := sj.ParseRec(line)
		switch sj.Str(m["type"]) {
		case "control_request":
			req := sj.AsRec(m["request"])
			id := sj.Str(m["request_id"])
			resp := func(body any) {
				emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": body}})
			}
			switch sj.Str(req["subtype"]) {
			case "interrupt":
				if helperFlag("ignoreabort") {
					continue
				}
				if helperFlag("refuseabort") {
					emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": "cannot interrupt"}})
					continue
				}
				resp(map[string]any{"still_queued": []any{}})
				if hanging {
					hanging = false
					emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true,
						"errors": []string{"[ede_diagnostic] aborted"}, "terminal_reason": "aborted_streaming"})
				}
			case "get_context_usage":
				if helperFlag("nobreakdown") {
					emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": "get_context_usage is not supported in this context"}})
					continue
				}
				var ans any
				_ = json.Unmarshal([]byte(helperContextAnswer), &ans)
				resp(ans)
			case "initialize":
				resp(map[string]any{})
			case "get_usage":
				if helperFlag("nobaseline") {
					emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": "no"}})
					continue
				}
				resp(map[string]any{"session": map[string]any{"model_usage": map[string]any{
					"claude-x": map[string]any{"inputTokens": 100, "outputTokens": 50, "cacheReadInputTokens": 10, "cacheCreationInputTokens": 0}}}})
			}
		case "user":
			text := ""
			for _, p := range sj.AsList(sj.AsRec(m["message"])["content"]) {
				text += sj.Str(sj.AsRec(p)["text"])
			}
			uuid := sj.Str(m["uuid"])
			echo := func() {
				if uuid != "" && !helperFlag("noecho") {
					emit(map[string]any{"type": "user", "isReplay": true, "uuid": uuid, "parent_tool_use_id": nil,
						"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}})
				}
			}
			if hanging {
				// A steer: a user message inside the running turn.
				echo()
				continue
			}
			turn++
			emit(map[string]any{"type": "system", "subtype": "init", "model": "claude-x", "fast_mode_state": "off"})
			echo()
			emit(map[string]any{"type": "assistant", "message": map[string]any{"id": fmt.Sprintf("msg_%d", turn), "model": "claude-x",
				"usage":   map[string]any{"input_tokens": 3, "cache_read_input_tokens": 10, "cache_creation_input_tokens": 0, "output_tokens": 1},
				"content": []any{map[string]any{"type": "text", "text": "working"}}}})
			if strings.Contains(text, "long") {
				hanging = true
				continue
			}
			emit(map[string]any{"type": "result", "subtype": "success", "is_error": false,
				"usage":      map[string]any{"input_tokens": 3, "cache_read_input_tokens": 10, "cache_creation_input_tokens": 0, "output_tokens": 7},
				"modelUsage": map[string]any{"claude-x": map[string]any{"inputTokens": 3*turn + offset, "outputTokens": 7*turn + offset/2, "cacheReadInputTokens": 10*turn + offset/10, "cacheCreationInputTokens": 0, "contextWindow": 200000}}})
		}
		_ = interrupted
	}
}

// helperSession opens a Session over the fake claude in mode, with flags.
func helperSession(t *testing.T, mode string, so SessionOptions, flags ...string) (*Session, string) {
	t.Helper()
	sess, argsFile, err := helperOpen(t, mode, oarlock.Options{}, so, flags...)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return sess, argsFile
}

func helperOpen(t *testing.T, mode string, opt oarlock.Options, so SessionOptions, flags ...string) (*Session, string, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argsFile := t.TempDir() + "/args"
	prefix := []string{"-test.run=TestHelperProcess$", "--"}
	spec := openSpec{
		launch: sj.Options{
			Command: exe, KillGrace: time.Second, WaitDelay: 2 * time.Second,
		},
		session:         so,
		readbackTimeout: 5 * time.Second,
	}
	spec.launch.Args = nil
	spec.extraPrefix = prefix
	spec.extraEnv = []string{
		"GORACE=atexit_sleep_ms=0", modeEnv + "=" + mode, argsFileEnv + "=" + argsFile, flagsEnv + "=" + strings.Join(flags, ","),
	}
	sess, err := open(context.Background(), spec, opt)
	if err != nil {
		return nil, argsFile, err
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = sess.Dispose(ctx)
	})
	return sess, argsFile, nil
}
