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
	// stdinFileEnv names a file every stdin line is appended to, verbatim: the
	// only way a test can assert the exact wire text the adapter wrote.
	stdinFileEnv = "CLAUDE_HELPER_STDIN"
	// flagsEnv is a comma list: noecho, ignoreabort, refuseabort, nobaseline,
	// nobreakdown, lifecycle, defersteer, noinitcaps, pushperm, cancelqueued.
	flagsEnv = "CLAUDE_HELPER_FLAGS"
	// permRequestID is the one runtime→app request the pushperm flag emits.
	permRequestID = "perm-1"
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
	// The stdin log keeps the wire text the adapter wrote, appended verbatim per
	// line and unbuffered, so the test process reads the same file while this
	// one is still running.
	var stdinLog *os.File
	if p := os.Getenv(stdinFileEnv); p != "" {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return 91
		}
		defer f.Close()
		stdinLog = f
	}
	captureStdin := func(line []byte) {
		if stdinLog == nil || len(line) == 0 {
			return
		}
		_, _ = stdinLog.Write(line)
		if line[len(line)-1] != '\n' {
			_, _ = stdinLog.Write([]byte{'\n'})
		}
	}
	if mode == "missing-resume" {
		emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true,
			"errors": []string{"No conversation found with session ID: sess-gone"}, "modelUsage": map[string]any{}})
		return 1
	}
	if mode == "replay" {
		// Wait for the first stdin line, then play the fixture verbatim.
		in := bufio.NewReader(os.Stdin)
		line, _ := in.ReadBytes('\n')
		captureStdin(line)
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
	sid := ""   // claude's own session id, echoed in its lifecycle frames
	for i, a := range os.Args {
		if a == "--resume" {
			offset = 100
		}
		if a == "--session-id" && i+1 < len(os.Args) {
			sid = os.Args[i+1]
		}
	}
	hanging := false
	current := "" // the uuid of the input whose turn is running
	// cancelqueued reproduces the shape a cancel_queued interrupt exists for: an
	// input claude queued and never started. queuedU is that input's uuid and
	// queuedOnce keeps the shape to the first prompt, so a later one runs a turn
	// of its own.
	queuedU := ""
	queuedOnce := false
	lifecycleSeq := 0
	// emitLifecycle states claude's own receipt of an input (msg_lifecycle_v1),
	// the frame the adapter reads ownership off when no echo arrives. Off unless
	// the lifecycle flag is set, so the other tests keep a fake that never says
	// it received anything; cancelqueued needs exactly these receipts, so it
	// turns them on too.
	emitLifecycle := func(uuid, state string) {
		if (!helperFlag("lifecycle") && !helperFlag("cancelqueued")) || uuid == "" {
			return
		}
		lifecycleSeq++
		emit(map[string]any{"type": "command_lifecycle", "command_uuid": uuid, "state": state,
			"uuid": fmt.Sprintf("lifecycle-%d", lifecycleSeq), "session_id": sid})
	}
	var abortID string
	interrupted := make(chan struct{}, 1)
	_ = abortID
	// The init frame is the 2.1.312 recording's shape: the adapter negotiates
	// cancel_queued off this capability list, and permissionMode is what a
	// headless run says. noinitcaps withholds the one capability so the
	// unnegotiated path is exercised too; the rest of the frame stays.
	initCaps := []string{"interrupt_receipt_v1", "interrupt_cancel_queued_v1", "interrupt_send_now_v1", "msg_lifecycle_v1"}
	if helperFlag("noinitcaps") {
		initCaps = []string{"interrupt_receipt_v1", "interrupt_send_now_v1", "msg_lifecycle_v1"}
	}
	emitInit := func() {
		emit(map[string]any{"type": "system", "subtype": "init", "model": "claude-x", "fast_mode_state": "off",
			"permissionMode": "bypassPermissions", "capabilities": initCaps})
	}
	// result is claude's own end of the running turn. The pushperm turn ends here
	// only after the host has answered, so both paths share it.
	result := func() {
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false,
			"usage":      map[string]any{"input_tokens": 3, "cache_read_input_tokens": 10, "cache_creation_input_tokens": 0, "output_tokens": 7},
			"modelUsage": map[string]any{"claude-x": map[string]any{"inputTokens": 3*turn + offset, "outputTokens": 7*turn + offset/2, "cacheReadInputTokens": 10*turn + offset/10, "cacheCreationInputTokens": 0, "contextWindow": 200000}}})
	}
	// pushperm emits one runtime→app permission request per process. The fake
	// never answers it: only the host's control_response does, and that answer
	// ends the turn.
	permPushed, pendingPerm := false, ""
	for {
		line, err := in.ReadBytes('\n')
		captureStdin(line)
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
				if queuedU != "" {
					// The queued input never started: an interrupt carrying
					// cancel_queued drops it — nothing left in still_queued, a
					// cancelled receipt, no turn and so no result frame. A bare one
					// leaves it queued, which is exactly what still_queued names.
					cancelQueued, _ := req["cancel_queued"].(bool)
					if !cancelQueued {
						resp(map[string]any{"still_queued": []any{queuedU}})
						continue
					}
					resp(map[string]any{"still_queued": []any{}})
					emitLifecycle(queuedU, "cancelled")
					queuedU = ""
					continue
				}
				resp(map[string]any{"still_queued": []any{}})
				if hanging {
					hanging = false
					emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true,
						"errors": []string{"[ede_diagnostic] aborted"}, "terminal_reason": "aborted_streaming"})
					emitLifecycle(current, "cancelled")
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
		case "control_response":
			// The host answering our permission request (the stdin log already
			// recorded the answer verbatim): the turn can now end normally.
			if pendingPerm != "" && sj.Str(sj.AsRec(m["response"])["request_id"]) == pendingPerm {
				pendingPerm = ""
				emitLifecycle(current, "completed")
				result()
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
			if helperFlag("cancelqueued") && !queuedOnce && uuid != "" {
				// cancelqueued: claude queued this input and did not start it, so the
				// queued receipt and the init frame (where the capability comes from)
				// are all the stream says — no echo, no result. Only an interrupt can
				// take the input back now.
				queuedOnce, queuedU = true, uuid
				emitLifecycle(uuid, "queued")
				emitInit()
				continue
			}
			if hanging {
				// A steer: a user message inside the running turn.
				emitLifecycle(uuid, "queued")
				if helperFlag("defersteer") {
					// claude 2.1.312 on a turn with no request boundary: the input
					// is queued, the turn ends without echoing it, and applying it as
					// a follow-up turn is claude's own decision after the result.
					hanging = false
					emitLifecycle(current, "completed")
					result()
					continue
				}
				echo()
				emitLifecycle(uuid, "started")
				continue
			}
			turn++
			current = uuid
			emitLifecycle(uuid, "queued")
			emitLifecycle(uuid, "started")
			emitInit()
			echo()
			emit(map[string]any{"type": "assistant", "message": map[string]any{"id": fmt.Sprintf("msg_%d", turn), "model": "claude-x",
				"usage":   map[string]any{"input_tokens": 3, "cache_read_input_tokens": 10, "cache_creation_input_tokens": 0, "output_tokens": 1},
				"content": []any{map[string]any{"type": "text", "text": "working"}}}})
			if helperFlag("pushperm") && !permPushed {
				permPushed, pendingPerm = true, permRequestID
				emit(map[string]any{"type": "control_request", "request_id": permRequestID,
					"request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "echo hi"}}})
				continue
			}
			if strings.Contains(text, "long") {
				hanging = true
				continue
			}
			emitLifecycle(uuid, "completed")
			result()
		}
		_ = interrupted
	}
}

// helperSession opens a Session over the fake claude in mode, with flags.
func helperSession(t *testing.T, mode string, so SessionOptions, flags ...string) (*Session, string) {
	t.Helper()
	sess, argsFile, err := helperOpenEnv(t, mode, oarlock.Options{}, so, nil, flags...)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return sess, argsFile
}

func helperOpen(t *testing.T, mode string, opt oarlock.Options, so SessionOptions, flags ...string) (*Session, string, error) {
	t.Helper()
	return helperOpenEnv(t, mode, opt, so, nil, flags...)
}

// helperOpenEnv is helperOpen with extra environment for the fake process (the
// stdin capture path, for instance).
func helperOpenEnv(t *testing.T, mode string, opt oarlock.Options, so SessionOptions, env []string, flags ...string) (*Session, string, error) {
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
	spec.extraEnv = append(spec.extraEnv, env...)
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
