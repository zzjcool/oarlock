// Package codex is the Codex adapter: JSON-RPC over the stdio of
// `codex app-server --listen stdio://` (issue #20).
//
// rpc.go is the transport only. One JSON object per LF-terminated line in each
// direction, requests correlated to replies by a numeric id, everything else
// delivered in wire order on one feed:
//
//   - a notification (method, no id),
//   - a server request (method and id: approvals, user input; the client does
//     not answer these by itself),
//   - a reply to one of our requests (id, no method).
//
// The one ordering rule the layers above rely on: a reply is handed to the
// consumer on the same feed as the notifications around it, and Call does not
// return until the consumer has run the reply's hook (Frame.Settle), so a
// control's response record can be written at the reply's wire position, before
// any notification codex wrote after it.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/zzjcool/oarlock/internal/exec"
)

const (
	// DefaultCallTimeout bounds one Call (write + reply). A timeout does not
	// kill the app-server: a slow codex is not a dead codex, and a late reply
	// still reaches the feed.
	DefaultCallTimeout = 30 * time.Second
	// DefaultMaxLineBytes caps one stdout line.
	DefaultMaxLineBytes = 16 << 20
	// DefaultStderrTailBytes is how much of the end of stderr is kept.
	DefaultStderrTailBytes = 8 << 10
	// DefaultFrameBuffer is the capacity of the Frames channel.
	DefaultFrameBuffer = 4096

	readChunk  = 64 << 10
	quoteBytes = 200
)

var (
	// ErrExited is the sentinel of ExitedError.
	ErrExited = errors.New("codex: app-server exited")
	// ErrTimeout is the sentinel of TimeoutError.
	ErrTimeout = errors.New("codex: call timed out")
	// ErrLineTooLong reports a stdout line longer than the cap.
	ErrLineTooLong = errors.New("codex: stdout line exceeds MaxLineBytes")
	// ErrTruncatedLine reports that stdout ended inside a line.
	ErrTruncatedLine = errors.New("codex: stdout ended inside an unterminated line")
)

// FrameKind says which of the three inbound shapes a line is.
type FrameKind int

const (
	FrameNotification FrameKind = iota + 1
	FrameServerRequest
	FrameResponse
)

// RPCError is a JSON-RPC error object returned for one of our requests.
type RPCError struct {
	Method  string
	Code    int
	Message string
	Data    json.RawMessage
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("codex: %s failed (code %d): %s", e.Method, e.Code, e.Message)
}

// ExitedError is the error of a call the app-server could not answer because
// it ended first.
type ExitedError struct{ Status ExitStatus }

func (e *ExitedError) Error() string {
	s := fmt.Sprintf("codex: app-server exited (code %d)", e.Status.Code)
	if e.Status.StderrTail != "" {
		s += ": " + e.Status.StderrTail
	}
	return s
}
func (e *ExitedError) Is(target error) bool { return target == ErrExited }
func (e *ExitedError) Unwrap() error        { return e.Status.Err }

// TimeoutError is the error of a call that got no reply in time.
type TimeoutError struct {
	Method string
	After  time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("codex: %s got no reply within %v", e.Method, e.After)
}
func (e *TimeoutError) Is(target error) bool { return target == ErrTimeout }

// ExitStatus is how the process ended. A non-zero code or a signal is data.
type ExitStatus struct {
	Code       int
	Signal     os.Signal
	StderrTail string
	Err        error
}

// Reply is the answer to one request.
type Reply struct {
	ID     int64
	Result json.RawMessage
	Err    *RPCError
	Raw    json.RawMessage
}

// Frame is one inbound line.
type Frame struct {
	Kind   FrameKind
	Raw    json.RawMessage // the line verbatim
	Method string          // notification and server request
	ID     json.RawMessage // server request id as sent; reply id for a response
	Params json.RawMessage
	Reply  *Reply // FrameResponse only
	call   *call
}

// Direct reports whether this is the reply to a call made without a hook
// (the handshake and the open): the consumer owns what it means.
func (f Frame) Direct() bool { return f.call != nil && f.call.hook == nil }

// Matched reports whether the reply belongs to a call that is still known.
func (f Frame) Matched() bool { return f.call != nil }

// Settle runs the hook of the call this reply answers, then releases the
// waiting Call. It is a no-op for any other frame and idempotent.
func (f Frame) Settle() {
	if f.call != nil {
		f.call.settle()
	}
}

type call struct {
	method  string
	hook    func(Reply)
	reply   Reply
	got     chan struct{}
	settled chan struct{}
	once    sync.Once
}

func (c *call) settle() {
	c.once.Do(func() {
		if c.hook != nil {
			c.hook(c.reply)
		}
		close(c.settled)
	})
}

// Conn is the surface the session needs from an app-server process.
type Conn interface {
	// Call sends a request and waits for its reply. With a hook, the hook runs
	// on the feed's consumer in wire order and Call returns after it ran.
	Call(ctx context.Context, method string, params any, hook func(Reply)) (Reply, error)
	// Notify sends a notification (no reply).
	Notify(method string, params any) error
	// RespondResult answers a server request with a result.
	RespondResult(id json.RawMessage, result any) error
	// Frames is the ordered feed of every inbound line.
	Frames() <-chan Frame
	// Exit delivers the ExitStatus once, then closes.
	Exit() <-chan ExitStatus
	// Close ends the process tree.
	Close() error
}

// Options configures Launch.
type Options struct {
	Command         string
	Args            []string
	Dir             string
	Env             []string
	CallTimeout     time.Duration
	MaxLineBytes    int
	StderrTailBytes int
	FrameBuffer     int
	KillGrace       time.Duration
	WaitDelay       time.Duration
}

// Stats is a snapshot of what the process has written.
type Stats struct {
	Lines    uint64
	BadLines uint64 // lines that were not a JSON-RPC object; skipped and counted
}

// Process is a running app-server. It implements Conn.
type Process struct {
	cmd     *exec.Cmd
	ctx     context.Context
	timeout time.Duration
	maxLine int

	frames chan Frame
	exit   chan ExitStatus
	done   chan struct{}
	stop   chan struct{}

	readerDone chan struct{}
	stderrDone chan struct{}

	writeMu sync.Mutex
	nextID  atomic.Int64

	tailMu  sync.Mutex
	tail    []byte
	tailCap int

	mu       sync.Mutex
	pending  map[int64]*call
	exited   bool
	status   ExitStatus
	frameErr error
	lines    uint64
	bad      uint64

	closeOnce sync.Once
	closeErr  error
}

var _ Conn = (*Process)(nil)

// Launch starts the process. ctx owns the lifetime of the whole tree.
func Launch(ctx context.Context, opt Options) (*Process, error) {
	if ctx == nil {
		return nil, errors.New("codex: Launch(nil context)")
	}
	if opt.Command == "" {
		return nil, errors.New("codex: Options.Command is empty")
	}
	timeout := opt.CallTimeout
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	maxLine := opt.MaxLineBytes
	if maxLine <= 0 {
		maxLine = DefaultMaxLineBytes
	}
	tailCap := opt.StderrTailBytes
	if tailCap <= 0 {
		tailCap = DefaultStderrTailBytes
	}
	buffer := opt.FrameBuffer
	if buffer <= 0 {
		buffer = DefaultFrameBuffer
	}
	cmd, err := exec.Start(ctx, &exec.Command{
		Path: opt.Command, Args: append([]string(nil), opt.Args...), Dir: opt.Dir, Env: opt.Env,
		KillGrace: opt.KillGrace, WaitDelay: opt.WaitDelay,
	})
	if err != nil {
		return nil, err
	}
	p := &Process{
		cmd: cmd, ctx: ctx, timeout: timeout, maxLine: maxLine, tailCap: tailCap,
		frames:     make(chan Frame, buffer),
		exit:       make(chan ExitStatus, 1),
		done:       make(chan struct{}),
		stop:       make(chan struct{}),
		readerDone: make(chan struct{}),
		stderrDone: make(chan struct{}),
		pending:    make(map[int64]*call),
	}
	// A zero-length Read claims a stream so Wait cannot drain output of a child
	// that is gone before the readers are scheduled (see adapters/pi).
	_, _ = cmd.Stdout().Read(nil)
	_, _ = cmd.Stderr().Read(nil)
	go p.readStdout()
	go p.readStderr()
	go p.reap()
	return p, nil
}

// Frames implements Conn.
func (p *Process) Frames() <-chan Frame { return p.frames }

// Exit implements Conn.
func (p *Process) Exit() <-chan ExitStatus { return p.exit }

// Wait blocks until the exit is published and returns it.
func (p *Process) Wait() ExitStatus {
	<-p.done
	return p.status
}

// Stats returns a snapshot of the line tallies.
func (p *Process) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{Lines: p.lines, BadLines: p.bad}
}

// Close ends the tree: stdin closed, SIGTERM, then SIGKILL after the grace.
func (p *Process) Close() error {
	p.closeOnce.Do(func() {
		close(p.stop)
		p.closeErr = p.cmd.Close(context.Background())
		<-p.done
	})
	return p.closeErr
}

func (p *Process) writeLine(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err = p.cmd.Stdin().Write(line)
	return err
}

// Call implements Conn.
func (p *Process) Call(ctx context.Context, method string, params any, hook func(Reply)) (Reply, error) {
	if ctx == nil {
		return Reply{}, errors.New("codex: Call(nil context)")
	}
	if err := ctx.Err(); err != nil {
		return Reply{}, err
	}
	if params == nil {
		params = map[string]any{}
	}
	id := p.nextID.Add(1)
	c := &call{method: method, hook: hook, got: make(chan struct{}), settled: make(chan struct{})}
	p.mu.Lock()
	if p.exited {
		st := p.status
		p.mu.Unlock()
		return Reply{}, &ExitedError{Status: st}
	}
	p.pending[id] = c
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
	}()

	tctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	if werr := p.writeLine(map[string]any{"id": id, "method": method, "params": params}); werr != nil {
		select {
		case <-p.done:
			return Reply{}, &ExitedError{Status: p.status}
		case <-time.After(2 * time.Second):
		}
		return Reply{}, fmt.Errorf("codex: write %q: %w", method, werr)
	}
	wait := func(ch <-chan struct{}) error {
		select {
		case <-ch:
			return nil
		case <-p.done:
			select {
			case <-ch:
				return nil
			default:
			}
			return &ExitedError{Status: p.status}
		case <-tctx.Done():
			select {
			case <-ch:
				return nil
			default:
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &TimeoutError{Method: method, After: p.timeout}
		}
	}
	if err := wait(c.got); err != nil {
		return Reply{}, err
	}
	if hook != nil {
		if err := wait(c.settled); err != nil {
			return c.reply, err
		}
	}
	if c.reply.Err != nil {
		return c.reply, c.reply.Err
	}
	return c.reply, nil
}

// Notify implements Conn.
func (p *Process) Notify(method string, params any) error {
	if params == nil {
		params = map[string]any{}
	}
	return p.writeLine(map[string]any{"method": method, "params": params})
}

// RespondResult implements Conn.
func (p *Process) RespondResult(id json.RawMessage, result any) error {
	if result == nil {
		result = map[string]any{}
	}
	return p.writeLine(map[string]any{"id": id, "result": result})
}

// ---- reading -------------------------------------------------------------

func (p *Process) readStdout() {
	defer close(p.readerDone)
	br := bufio.NewReaderSize(p.cmd.Stdout(), readChunk)
	for {
		raw, err := p.readLine(br)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				p.fatal(err)
			}
			return
		}
		if isBlank(raw) {
			continue
		}
		p.dispatchLine(raw)
	}
}

type wireMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
}

func (p *Process) dispatchLine(raw []byte) {
	p.mu.Lock()
	p.lines++
	p.mu.Unlock()
	first := bytes.TrimLeft(raw, " \t\r\n")
	var m wireMessage
	if len(first) == 0 || first[0] != '{' || json.Unmarshal(raw, &m) != nil {
		p.mu.Lock()
		p.bad++
		p.mu.Unlock()
		return
	}
	hasID := len(m.ID) > 0 && !bytes.Equal(m.ID, []byte("null"))
	var f Frame
	switch {
	case m.Method != "" && hasID:
		f = Frame{Kind: FrameServerRequest, Raw: raw, Method: m.Method, ID: m.ID, Params: m.Params}
	case m.Method != "":
		f = Frame{Kind: FrameNotification, Raw: raw, Method: m.Method, Params: m.Params}
	case hasID:
		var id int64
		var num json.Number
		reply := Reply{Raw: raw}
		if json.Unmarshal(m.ID, &num) == nil {
			if n, err := num.Int64(); err == nil {
				id = n
			}
		}
		reply.ID = id
		f = Frame{Kind: FrameResponse, Raw: raw, ID: m.ID}
		p.mu.Lock()
		c := p.pending[id]
		p.mu.Unlock()
		if c != nil {
			if m.Error != nil {
				reply.Err = &RPCError{Method: c.method, Code: m.Error.Code, Message: m.Error.Message, Data: m.Error.Data}
			} else {
				reply.Result = m.Result
			}
			c.reply = reply
			f.call = c
			f.Method = c.method
		} else {
			if m.Error != nil {
				reply.Err = &RPCError{Code: m.Error.Code, Message: m.Error.Message, Data: m.Error.Data}
			} else {
				reply.Result = m.Result
			}
		}
		f.Reply = &reply
		if c != nil {
			// The frame is queued first and got is closed after, so a waiting
			// Call never returns before the frame is on the feed.
			defer close(c.got)
		}
	default:
		p.mu.Lock()
		p.bad++
		p.mu.Unlock()
		return
	}
	select {
	case p.frames <- f:
		return
	default:
	}
	select {
	case p.frames <- f:
	case <-p.stop:
	case <-p.ctx.Done():
	}
}

func (p *Process) readLine(br *bufio.Reader) ([]byte, error) {
	var acc []byte
	for {
		piece, err := br.ReadSlice('\n')
		switch {
		case err == nil:
			content := piece[:len(piece)-1]
			if len(acc)+len(content) > p.maxLine+1 {
				return nil, p.tooLong()
			}
			line := append(append(make([]byte, 0, len(acc)+len(content)), acc...), content...)
			if n := len(line); n > 0 && line[n-1] == '\r' {
				line = line[:n-1]
			}
			if len(line) > p.maxLine {
				return nil, p.tooLong()
			}
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			if len(acc)+len(piece) > p.maxLine+1 {
				return nil, p.tooLong()
			}
			acc = append(acc, piece...)
		default:
			tail := append(acc, piece...)
			if errors.Is(err, io.EOF) {
				if len(tail) == 0 {
					return nil, io.EOF
				}
				head := tail
				if len(head) > quoteBytes {
					head = head[:quoteBytes]
				}
				return nil, fmt.Errorf("%w (%d bytes): %q", ErrTruncatedLine, len(tail), head)
			}
			return nil, fmt.Errorf("codex: read stdout: %w", err)
		}
	}
}

func (p *Process) tooLong() error {
	return fmt.Errorf("%w (limit %d bytes)", ErrLineTooLong, p.maxLine)
}

func isBlank(line []byte) bool {
	for _, c := range line {
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			return false
		}
	}
	return true
}

func (p *Process) fatal(err error) {
	p.mu.Lock()
	if p.frameErr == nil {
		p.frameErr = err
	}
	p.mu.Unlock()
	_ = p.cmd.Signal(os.Kill)
	_ = p.cmd.Stdout().Close()
}

func (p *Process) readStderr() {
	defer close(p.stderrDone)
	_, _ = io.Copy(tailWriter{p}, p.cmd.Stderr())
}

type tailWriter struct{ p *Process }

func (w tailWriter) Write(b []byte) (int, error) {
	p := w.p
	p.tailMu.Lock()
	defer p.tailMu.Unlock()
	p.tail = append(p.tail, b...)
	if len(p.tail) > p.tailCap {
		p.tail = append([]byte(nil), p.tail[len(p.tail)-p.tailCap:]...)
	}
	return len(b), nil
}

func (p *Process) stderrTail() string {
	p.tailMu.Lock()
	defer p.tailMu.Unlock()
	out := p.tail
	for i := 0; len(out) > 0 && i < utf8.UTFMax-1 && !utf8.RuneStart(out[0]); i++ {
		out = out[1:]
	}
	return string(out)
}

func (p *Process) reap() {
	ws, werr := p.cmd.Wait(context.Background())
	<-p.readerDone
	<-p.stderrDone
	close(p.frames)

	p.mu.Lock()
	st := ExitStatus{Code: ws.Code, Signal: ws.Signal, StderrTail: p.stderrTail(), Err: errors.Join(p.frameErr, werr)}
	if st.Err != nil && p.frameErr == nil && werr == nil {
		st.Err = nil
	}
	p.exited = true
	p.status = st
	p.mu.Unlock()

	p.exit <- st
	close(p.exit)
	close(p.done)
}
