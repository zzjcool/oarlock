// Package pirpc is the throwaway Pi RPC client written for the issue #5 spike:
package pirpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	CommandTimeout = 30 * time.Second
	MaxLineBytes   = 16 << 20
)

var ErrExited = errors.New("pirpc: child exited")

type Frame struct {
	Type, ID string
	Raw      json.RawMessage
}
type Client struct {
	Pid, Pgid int
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	frames    chan Frame
	exited    chan struct{}
	mu        sync.Mutex
	pending   map[string]chan Frame
	code      int
	sig       syscall.Signal
	stderr    bytes.Buffer
	once      sync.Once
	gone      bool
	close     error
}

func Start(argv []string, dir string) (*Client, error) {
	if len(argv) == 0 {
		return nil, errors.New("pirpc: empty argv")
	}
	c := &Client{frames: make(chan Frame, 512), exited: make(chan struct{}), pending: map[string]chan Frame{}}
	c.cmd = exec.Command(argv[0], argv[1:]...)
	c.cmd.Dir = dir
	c.cmd.Stderr = &c.stderr // diagnostics only: stdout is pure JSONL
	c.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if c.stdin, err = c.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	if err = c.cmd.Start(); err != nil {
		return nil, err
	}
	c.Pid = c.cmd.Process.Pid
	if c.Pgid, err = syscall.Getpgid(c.Pid); err != nil {
		c.Pgid = c.Pid
	}
	go func() {
		_ = ReadFrames(stdout, MaxLineBytes, c.dispatch) // a broken line shows up as a missing frame
		close(c.frames)
		_ = c.cmd.Wait()
		code, sig := -1, syscall.Signal(0)
		if ps := c.cmd.ProcessState; ps != nil {
			code = ps.ExitCode()
			if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				sig, code = ws.Signal(), -1
			}
		}
		c.mu.Lock()
		c.code, c.sig = code, sig
		c.mu.Unlock()
		close(c.exited)
	}()
	return c, nil
}
func ReadFrames(r io.Reader, max int, emit func(Frame) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), max)
	for n := 0; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var head struct{ Type, ID string }
		if err := json.Unmarshal(line, &head); err != nil {
			return fmt.Errorf("pirpc: line %d is not JSON: %w: %.200s", n, err, line)
		}
		if err := emit(Frame{Type: head.Type, ID: head.ID, Raw: append(json.RawMessage(nil), line...)}); err != nil {
			return err
		}
	}
	return sc.Err()
}
func (c *Client) dispatch(f Frame) error {
	c.mu.Lock()
	wait := c.pending[f.ID]
	if wait != nil && f.Type == "response" {
		delete(c.pending, f.ID)
	}
	c.mu.Unlock()
	if wait != nil && f.Type == "response" {
		wait <- f // buffered 1: one response per id
	}
	c.frames <- f
	return nil
}
func (c *Client) Frames() <-chan Frame { return c.frames }

// Stderr is the child's diagnostics; stdout is the protocol.
func (c *Client) Stderr() string { return c.stderr.String() }
func (c *Client) Alive() bool {
	select {
	case <-c.exited:
		return false
	default:
		return true
	}
}
func (c *Client) Send(line string) error {
	c.mu.Lock()
	w := c.stdin
	c.mu.Unlock()
	if w == nil || !c.Alive() {
		return ErrExited
	}
	if _, err := io.WriteString(w, line+"\n"); err != nil {
		return fmt.Errorf("pirpc: write: %w", err)
	}
	return nil
}
func (c *Client) Request(ctx context.Context, line string) (Frame, error) {
	var cmd struct{ ID string }
	if err := json.Unmarshal([]byte(line), &cmd); err != nil || cmd.ID == "" {
		return Frame{}, fmt.Errorf("pirpc: request needs an id: %q", line)
	}
	wait := make(chan Frame, 1)
	c.mu.Lock()
	if _, busy := c.pending[cmd.ID]; busy {
		c.mu.Unlock()
		return Frame{}, fmt.Errorf("pirpc: id %q already pending", cmd.ID)
	}
	c.pending[cmd.ID] = wait
	c.mu.Unlock()
	fail := func(err error) (Frame, error) {
		c.mu.Lock()
		delete(c.pending, cmd.ID)
		c.mu.Unlock()
		return Frame{}, err
	}
	if err := c.Send(line); err != nil {
		return fail(err)
	}
	timer := time.NewTimer(CommandTimeout)
	defer timer.Stop()
	select {
	case f := <-wait:
		return f, nil
	case <-c.exited:
		return fail(fmt.Errorf("%w before %q replied", ErrExited, cmd.ID))
	case <-ctx.Done():
		return fail(ctx.Err())
	case <-timer.C:
		_ = c.signalGroup(syscall.SIGKILL)
		return fail(fmt.Errorf("pirpc: no reply to %q within %v; killed the group", cmd.ID, CommandTimeout))
	}
}
func (c *Client) Dispose(ctx context.Context, grace time.Duration) (int, syscall.Signal, bool, error) {
	c.once.Do(func() {
		c.mu.Lock()
		w := c.stdin
		c.stdin = nil
		c.mu.Unlock()
		if w != nil {
			_ = w.Close()
		}
		if !waitExit(ctx, c.exited, grace) {
			_ = c.signalGroup(syscall.SIGTERM)
			waitExit(ctx, c.exited, 500*time.Millisecond)
		}
		_ = c.signalGroup(syscall.SIGTERM) // descendants of a clean exit too
		if !c.pollGone(300 * time.Millisecond) {
			_ = c.signalGroup(syscall.SIGKILL)
		}
		waitExit(ctx, c.exited, 2*time.Second)
		c.gone = c.pollGone(3 * time.Second)
		if !c.gone {
			c.close = fmt.Errorf("pirpc: process group %d still has members", c.Pgid)
		}
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.code, c.sig, c.gone, c.close
}
func waitExit(ctx context.Context, exited <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-exited:
		return true
	case <-ctx.Done():
		return false
	case <-t.C:
		return false
	}
}
func (c *Client) signalGroup(sig syscall.Signal) error {
	if c.Pid == 0 || c.Pgid != c.Pid {
		return fmt.Errorf("pirpc: group %d is not ours (pid %d); refusing to signal", c.Pgid, c.Pid)
	}
	if err := syscall.Kill(-c.Pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("pirpc: %v group %d: %w", sig, c.Pgid, err)
	}
	return nil
}
func (c *Client) pollGone(d time.Duration) bool {
	if c.Pgid != c.Pid {
		return !c.Alive()
	}
	deadline := time.Now().Add(d)
	for !errors.Is(syscall.Kill(-c.Pgid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
	return true
}
