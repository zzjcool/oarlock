package acp

// The terminal host: create / output / wait_for_exit / kill / release for the
// agent's shell tool, run through internal/exec so a killed terminal takes its
// whole process tree with it (POSIX group, Windows job).

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/zzjcool/oarlock/internal/exec"
)

const defaultOutputLimit = 1 << 20

type terminal struct {
	cmd *exec.Cmd

	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool

	done   chan struct{}
	status exec.ExitStatus
}

func (t *terminal) write(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.limit; over > 0 {
		t.buf = t.buf[over:] // keep the end, as the protocol asks
		t.truncated = true
	}
}

type terminals struct {
	mu   sync.Mutex
	next int
	all  map[string]*terminal
}

func newTerminals() *terminals { return &terminals{all: map[string]*terminal{}} }

type pipeWriter struct{ t *terminal }

func (w pipeWriter) Write(p []byte) (int, error) { w.t.write(p); return len(p), nil }

func (h *terminals) create(ctx context.Context, p sdk.CreateTerminalRequest, shellLine bool, defaultCwd string) (sdk.CreateTerminalResponse, error) {
	path, args := p.Command, p.Args
	if shellLine && len(args) == 0 && strings.ContainsAny(path, " \t") {
		// Some agents put a whole shell line in `command`.
		if runtime.GOOS == "windows" {
			path, args = "cmd.exe", []string{"/c", p.Command}
		} else {
			path, args = "/bin/sh", []string{"-c", p.Command}
		}
	}
	dir := defaultCwd
	if p.Cwd != nil && *p.Cwd != "" {
		dir = *p.Cwd
	}
	var env []string
	if len(p.Env) > 0 {
		env = envWith(p.Env)
	}
	c, err := exec.Start(context.WithoutCancel(ctx), &exec.Command{Path: path, Args: args, Dir: dir, Env: env, KillGrace: 2 * time.Second})
	if err != nil {
		return sdk.CreateTerminalResponse{}, sdk.NewInternalError(map[string]any{"error": err.Error()})
	}
	limit := defaultOutputLimit
	if p.OutputByteLimit != nil && *p.OutputByteLimit > 0 {
		limit = *p.OutputByteLimit
	}
	t := &terminal{cmd: c, limit: limit, done: make(chan struct{})}
	// A zero-length Read claims a stream so Wait waits for it instead of
	// draining it away.
	_, _ = c.Stdout().Read(nil)
	_, _ = c.Stderr().Read(nil)
	var copies sync.WaitGroup
	for _, r := range []interface{ Read([]byte) (int, error) }{c.Stdout(), c.Stderr()} {
		copies.Add(1)
		go func(r interface{ Read([]byte) (int, error) }) {
			defer copies.Done()
			b := make([]byte, 4096)
			for {
				n, err := r.Read(b)
				if n > 0 {
					t.write(b[:n])
				}
				if err != nil {
					return
				}
			}
		}(r)
	}
	go func() {
		st, _ := c.Wait(context.Background())
		copies.Wait()
		t.status = st
		close(t.done)
	}()
	h.mu.Lock()
	h.next++
	id := fmt.Sprintf("term-%d", h.next)
	h.all[id] = t
	h.mu.Unlock()
	return sdk.CreateTerminalResponse{TerminalId: id}, nil
}

func (h *terminals) get(id string) (*terminal, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.all[id]
	if t == nil {
		return nil, sdk.NewInvalidParams(map[string]any{"terminalId": id, "error": "unknown terminal"})
	}
	return t, nil
}

func exitStatusOf(st exec.ExitStatus) (code *int, signal *string) {
	if st.Signal != nil {
		s := st.Signal.String()
		return nil, &s
	}
	c := st.Code
	return &c, nil
}

func (h *terminals) output(p sdk.TerminalOutputRequest) (sdk.TerminalOutputResponse, error) {
	t, err := h.get(p.TerminalId)
	if err != nil {
		return sdk.TerminalOutputResponse{}, err
	}
	t.mu.Lock()
	resp := sdk.TerminalOutputResponse{Output: string(t.buf), Truncated: t.truncated}
	t.mu.Unlock()
	select {
	case <-t.done:
		code, sig := exitStatusOf(t.status)
		resp.ExitStatus = &sdk.TerminalExitStatus{ExitCode: code, Signal: sig}
	default:
	}
	return resp, nil
}

func (h *terminals) wait(ctx context.Context, p sdk.WaitForTerminalExitRequest) (sdk.WaitForTerminalExitResponse, error) {
	t, err := h.get(p.TerminalId)
	if err != nil {
		return sdk.WaitForTerminalExitResponse{}, err
	}
	select {
	case <-t.done:
		code, sig := exitStatusOf(t.status)
		return sdk.WaitForTerminalExitResponse{ExitCode: code, Signal: sig}, nil
	case <-ctx.Done():
		return sdk.WaitForTerminalExitResponse{}, sdk.NewRequestCancelled(nil)
	}
}

func (h *terminals) kill(p sdk.KillTerminalRequest) (sdk.KillTerminalResponse, error) {
	t, err := h.get(p.TerminalId)
	if err != nil {
		return sdk.KillTerminalResponse{}, err
	}
	_ = t.cmd.Close(context.Background())
	<-t.done
	return sdk.KillTerminalResponse{}, nil
}

// release frees the id; a still-running command is killed first.
func (h *terminals) release(p sdk.ReleaseTerminalRequest) (sdk.ReleaseTerminalResponse, error) {
	t, err := h.get(p.TerminalId)
	if err != nil {
		return sdk.ReleaseTerminalResponse{}, err
	}
	_ = t.cmd.Close(context.Background())
	<-t.done
	h.mu.Lock()
	delete(h.all, p.TerminalId)
	h.mu.Unlock()
	return sdk.ReleaseTerminalResponse{}, nil
}

// closeAll kills every terminal still alive: a disposed session leaves none.
func (h *terminals) closeAll() {
	h.mu.Lock()
	all := h.all
	h.all = map[string]*terminal{}
	h.mu.Unlock()
	for _, t := range all {
		_ = t.cmd.Close(context.Background())
	}
}
