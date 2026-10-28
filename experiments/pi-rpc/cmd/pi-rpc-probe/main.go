package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zzjcool/oarlock/experiments/pi-rpc"
	"github.com/zzjcool/oarlock/internal/redact"
)

const argvLine = "pi --mode rpc --offline --no-session --no-tools --no-mcp --no-skills " +
	"--no-prompt-templates --no-context-files --model any-endpoint/workbuddy/deepseek-v4.1-flash"
const (
	msgNormal = "Reply with exactly one word: ready"
	msgAbort  = "Count from 1 to 300, one number per line, no other words."
)

var known = map[string]bool{"response": true, "agent_start": true, "turn_start": true,
	"message_start": true, "message_update": true, "message_end": true, "turn_end": true,
	"agent_end": true, "agent_settled": true, "extension_ui_request": true, "extension_error": true}

type rec struct {
	mu     sync.Mutex
	frames []pirpc.Frame
	sent   []string
}

func (r *rec) drain(c *pirpc.Client) {
	for f := range c.Frames() {
		r.mu.Lock()
		r.frames = append(r.frames, f)
		r.mu.Unlock()
	}
}
func (r *rec) note(line string) { r.mu.Lock(); r.sent = append(r.sent, line); r.mu.Unlock() }
func (r *rec) wait(pred func(pirpc.Frame) bool, d time.Duration) (int, pirpc.Frame) {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		r.mu.Lock()
		for i, f := range r.frames {
			if pred(f) {
				r.mu.Unlock()
				return i, f
			}
		}
		r.mu.Unlock()
	}
	return -1, pirpc.Frame{}
}
func (r *rec) all() ([]pirpc.Frame, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]pirpc.Frame(nil), r.frames...), append([]string(nil), r.sent...)
}
func isType(t string) func(pirpc.Frame) bool {
	return func(f pirpc.Frame) bool { return f.Type == t }
}
func isResp(id string) func(pirpc.Frame) bool {
	return func(f pirpc.Frame) bool { return f.Type == "response" && f.ID == id }
}
func get(raw json.RawMessage, path ...string) (v any, ok bool) {
	if json.Unmarshal(raw, &v) != nil {
		return nil, false
	}
	for _, k := range path {
		m, isMap := v.(map[string]any)
		if !isMap {
			return nil, false
		}
		if v, ok = m[k]; !ok {
			return nil, false
		}
	}
	return v, true
}
func want(raw json.RawMessage, value any, path ...string) bool {
	v, ok := get(raw, path...)
	return ok && fmt.Sprint(v) == fmt.Sprint(value)
}
func main() {
	scenario := flag.String("scenario", "normal", "normal|abort")
	out := flag.String("out", "testdata/pi", "fixture directory")
	flag.Parse()
	if os.Getenv("OAR_PI_LIVE") != "1" || (*scenario != "normal" && *scenario != "abort") {
		fmt.Fprintln(os.Stderr, "usage: OAR_PI_LIVE=1 go run ./experiments/pi-rpc/cmd/pi-rpc-probe -scenario normal|abort -out testdata/pi")
		os.Exit(2)
	}
	obs, bad, err := run(*scenario, *out)
	obs = append(obs, "error="+fmt.Sprint(err), "failed="+strings.Join(bad, ","))
	fmt.Println(strings.Join(obs, "\n"))
	if err != nil || len(bad) > 0 {
		os.Exit(1)
	}
}
func run(scenario, dir string) (obs, bad []string, err error) {
	ok := func(name string, cond bool, format string, args ...any) {
		obs = append(obs, fmt.Sprintf("%-22s ok=%-5v %s", name, cond, fmt.Sprintf(format, args...)))
		if !cond {
			bad = append(bad, name)
		}
	}
	r := &rec{}
	c, err := pirpc.Start(strings.Fields(argvLine), "")
	if err != nil {
		return obs, bad, err
	}
	go r.drain(c)
	ctx := context.Background()
	pgid, _ := syscall.Getpgid(c.Pid)
	ok("launch", c.Pid > 0 && pgid == c.Pid && c.Pgid == c.Pid, "pid=%d pgid=%d", c.Pid, pgid)
	req := func(line string) (pirpc.Frame, error) {
		r.note(line)
		return c.Request(ctx, line)
	}
	gs, err := req(`{"type":"get_state","id":"probe-1"}`)
	if err != nil {
		return obs, bad, err
	}
	ok("get_state", want(gs.Raw, "get_state", "command") && want(gs.Raw, true, "success"), "%s", gs.Raw)
	msg := msgNormal
	if scenario == "abort" {
		msg = msgAbort
	}
	if _, err = req(fmt.Sprintf(`{"type":"prompt","id":"probe-2","message":%q}`, msg)); err != nil {
		return obs, bad, err
	}
	if scenario == "abort" { // the fourth claim: stop a live turn, then recover without a restart
		if i, _ := r.wait(isType("agent_start"), 120*time.Second); i < 0 {
			return obs, bad, fmt.Errorf("no agent_start within 120s")
		}
		if i, _ := r.wait(isType("message_update"), 120*time.Second); i < 0 {
			return obs, bad, fmt.Errorf("no message_update within 120s")
		}
		r.note(`{"type":"abort","id":"probe-3"}`)
		if err = c.Send(`{"type":"abort","id":"probe-3"}`); err != nil {
			return obs, bad, err
		}
	}
	si, settled := r.wait(isType("agent_settled"), 180*time.Second)
	if si < 0 {
		return obs, bad, fmt.Errorf("no agent_settled within 180s")
	}
	ok("settled", want(settled.Raw, scenario == "abort", "aborted"), "%s", settled.Raw)
	ti, turn := r.wait(isType("turn_end"), time.Second)
	stop := "stop"
	if scenario == "abort" {
		stop = "aborted"
	}
	_, hasRaw := get(turn.Raw, "message", "rawStopReason")
	ok("turn_end", ti >= 0 && ti < si && want(turn.Raw, stop, "message", "stopReason") && hasRaw == (scenario == "normal"),
		"stopReason=%v rawStopReason=%v after_settled=%v", value(turn.Raw, "message", "stopReason"),
		value(turn.Raw, "message", "rawStopReason"), ti > si)
	if scenario == "abort" {
		ai, ab := r.wait(isResp("probe-3"), 30*time.Second)
		ok("abort_response", ai > si && want(ab.Raw, true, "success"), "after_settled=%v frame=%s", ai > si, ab.Raw)
		alive := syscall.Kill(c.Pid, 0) == nil
		ok("alive_after_settle", alive, "pid=%d", c.Pid)
		back, err := req(`{"type":"get_state","id":"probe-4"}`)
		if err != nil {
			return obs, bad, err
		}
		ok("get_state_after_abort", want(back.Raw, false, "data", "isStreaming"), "%s", back.Raw)
	}
	fr, sent := r.all()
	ids := map[string]bool{}
	for _, l := range sent {
		var cmd struct{ ID string }
		json.Unmarshal([]byte(l), &cmd)
		ids[cmd.ID] = true
	}
	got, unknown, maxLine := map[string]int{}, map[string]int{}, 0
	for _, f := range fr {
		if len(f.Raw) > maxLine {
			maxLine = len(f.Raw)
		}
		if f.Type == "response" {
			got[f.ID]++
		} else if !known[f.Type] {
			unknown[f.Type]++
		}
	}
	all := len(ids) > 0
	for id := range ids {
		all = all && got[id] == 1
	}
	for id := range got {
		all = all && ids[id]
	}
	ok("id_correlation", all, "requests=%v responses=%v", ids, got)
	ok("framing", len(fr) > 0 && maxLine < pirpc.MaxLineBytes,
		"frames=%d requests=%d max_line_bytes=%d unknown_types=%v", len(fr), len(sent), maxLine, unknown)
	code, sig, gone, derr := c.Dispose(ctx, 5*time.Second)
	ok("exit", code == 0 && sig == 0 && derr == nil, "code=%d signal=%d stderr=%q detail=%v", code, sig, c.Stderr(), derr)
	ok("group_gone", gone, "pgid=%d detail=%v", pgid, derr)
	obs = append(obs, write(dir, scenario, fr, sent))
	return obs, bad, nil
}
func value(raw json.RawMessage, path ...string) any { v, _ := get(raw, path...); return v }
func write(out, scenario string, fr []pirpc.Frame, sent []string) string {
	var fb, sb bytes.Buffer
	for _, f := range fr {
		fb.WriteString(scrub(f.Raw) + "\n")
	}
	for _, l := range sent {
		sb.WriteString(scrub([]byte(l)) + "\n")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "fixtures=failed " + err.Error()
	}
	frames := filepath.Join(out, scenario+"-turn.frames.jsonl")
	requests := filepath.Join(out, scenario+"-turn.requests.jsonl")
	if err := os.WriteFile(frames, fb.Bytes(), 0o644); err != nil {
		return "fixtures=failed " + err.Error()
	}
	if err := os.WriteFile(requests, sb.Bytes(), 0o644); err != nil {
		return "fixtures=failed " + err.Error()
	}
	return fmt.Sprintf("fixtures=%s frames=%d requests=%d bytes=%d", out, len(fr), len(sent), fb.Len()+sb.Len())
}
func scrub(line []byte) string {
	home, _ := os.UserHomeDir()
	b, _ := redact.New(nil).Bytes([]byte(strings.ReplaceAll(string(line), home, "$HOME")))
	return string(b)
}
