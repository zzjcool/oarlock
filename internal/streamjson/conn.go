// Package streamjson is the stream-json family: the one parser and transport
// shared by every runtime that speaks Claude Code's dialect (adapters/claude,
// and adapters/cursor, issue #22, which must reuse it rather than add a parser).
//
// The transport: one child process, one JSON object per LF-terminated line in
// each direction (`claude -p --input-format stream-json --output-format
// stream-json ...`).
//
// It is only a transport. It starts the tree through internal/exec, frames
// stdout, serialises stdin writes, keeps stderr as evidence and reports how the
// process ended. It never maps a frame to a record and never invents a turn
// boundary.
//
//   - Reading and writing are decoupled. stdout is drained by its own goroutine
//     into Events(); stdin is written by a dedicated writer goroutine that
//     Write hands lines to. A caller therefore never blocks the reader, and a
//     child that is busy writing a large frame cannot deadlock against a large
//     stdin line: the two pipes are serviced independently.
//   - Framing is LF only. U+2028, U+2029 and U+0085 inside a JSON string never
//     split a frame, exactly one trailing CR is stripped, blank lines are
//     skipped and the line is delivered verbatim. An over-long line, a line
//     that is not a JSON object and a stream that ends inside a line are fatal
//     and fail closed: a skipped line could hide the frame that ends a turn.
//   - A write that is not taken within Options.WriteTimeout means the child
//     stopped reading stdin; the whole process group is SIGKILLed (Job Object on
//     Windows) rather than left hanging.
//   - A non-zero exit or a signal death is data (ExitStatus), never an error.
package streamjson

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zzjcool/oarlock/internal/exec"
)

const (
	// DefaultWriteTimeout bounds one Write: the time to hand a line to the child.
	DefaultWriteTimeout = 30 * time.Second

	// DefaultMaxLineBytes caps one stdout line (content, LF excluded).
	DefaultMaxLineBytes = 16 << 20

	// DefaultStderrTailBytes is how much of the end of stderr is kept.
	DefaultStderrTailBytes = 8 << 10

	// DefaultEventBuffer is the capacity of the Events channel.
	DefaultEventBuffer = 256

	readChunk        = 64 << 10
	exitPublishWait  = 5 * time.Second
	quoteBytes       = 200
	writeFailureWait = 2 * time.Second
)

var (
	// ErrExited is the sentinel of ExitedError: the process is gone.
	ErrExited = errors.New("streamjson: process exited")

	// ErrWriteTimeout is the sentinel of a Write the child did not take in time.
	// The process group was killed.
	ErrWriteTimeout = errors.New("streamjson: write to stdin timed out")

	// ErrLineTooLong reports a stdout line longer than Options.MaxLineBytes.
	ErrLineTooLong = errors.New("streamjson: stdout line exceeds MaxLineBytes")

	// ErrTruncatedLine reports that stdout ended inside an unterminated line.
	ErrTruncatedLine = errors.New("streamjson: stdout ended inside an unterminated line")

	// ErrBadFrame reports a stdout line that is not a JSON object.
	ErrBadFrame = errors.New("streamjson: stdout line is not a JSON object")
)

// Conn is the whole surface the session needs from a stream-json process.
type Conn interface {
	// Write sends one complete line (it must end in LF) to stdin. Lines are
	// delivered whole and in call order. It returns once the child's pipe took
	// the line, the process exited, ctx ended or the write timeout fired.
	Write(ctx context.Context, line []byte) error

	// Events is the ordered feed of every stdout line.
	Events() <-chan Message

	// Exit delivers the ExitStatus once, then closes.
	Exit() <-chan ExitStatus

	// Close ends the process tree (SIGTERM, then SIGKILL after the kill grace)
	// and releases everything the Conn holds. Idempotent.
	Close() error
}

// Message is one stdout line.
type Message struct {
	// Type is the string member "type"; "" when absent or not a string.
	Type string
	// Raw is the line verbatim: LF and one trailing CR removed, never
	// re-encoded. Receivers share it: do not mutate it.
	Raw json.RawMessage
}

// ExitCause says who ended the process, as far as the Conn knows.
type ExitCause string

const (
	CauseExited   ExitCause = "exited"   // the process ended by itself
	CauseClosed   ExitCause = "closed"   // Close or the Launch ctx ended it
	CauseTimeout  ExitCause = "timeout"  // a write timeout killed the group
	CauseProtocol ExitCause = "protocol" // a framing error killed the group
)

// ExitStatus is how the process ended. A non-zero code or a signal death is a
// normal outcome; only framing and I/O faults are Err.
type ExitStatus struct {
	Code   int // -1 when killed by a signal
	Signal os.Signal

	// StderrTail is the end of stderr, starting on a rune boundary. It is raw:
	// the session redacts it before it enters a record.
	StderrTail string
	Cause      ExitCause
	Err        error
}

// ExitedError is the error of a Write the process could not take because it
// ended first. errors.Is(err, ErrExited) holds.
type ExitedError struct{ Status ExitStatus }

func (e *ExitedError) Error() string {
	var b strings.Builder
	b.WriteString("streamjson: process exited")
	if e.Status.Signal != nil {
		fmt.Fprintf(&b, " (signal %v, cause %s)", e.Status.Signal, e.Status.Cause)
	} else {
		fmt.Fprintf(&b, " (code %d, cause %s)", e.Status.Code, e.Status.Cause)
	}
	if e.Status.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Status.Err)
	}
	if tail := strings.TrimSpace(e.Status.StderrTail); tail != "" {
		fmt.Fprintf(&b, "; stderr: %s", tail)
	}
	return b.String()
}

func (e *ExitedError) Is(target error) bool { return target == ErrExited }
func (e *ExitedError) Unwrap() error        { return e.Status.Err }

// Options configures Launch.
type Options struct {
	Command         string // "" = DefaultExecutable
	Args            []string
	Dir             string
	Env             []string      // nil = inherit
	WriteTimeout    time.Duration // 0 = DefaultWriteTimeout; negative is an error
	MaxLineBytes    int           // 0 = DefaultMaxLineBytes; negative is an error
	StderrTailBytes int           // 0 = DefaultStderrTailBytes; negative is an error
	EventBuffer     int           // 0 = DefaultEventBuffer; negative is an error
	KillGrace       time.Duration // SIGTERM to SIGKILL grace; 0 = exec's default
	WaitDelay       time.Duration // drain bound after the leader exits; 0 = exec's default
}

type writeReq struct {
	line []byte
	done chan error
}

// Process is a running stream-json child. It implements Conn.
type Process struct {
	cmd *exec.Cmd
	ctx context.Context

	wtimeout time.Duration
	maxLine  int

	events chan Message
	exit   chan ExitStatus
	wq     chan writeReq

	done       chan struct{} // closed last, after the exit is published
	stop       chan struct{} // closed by Close
	readerDone chan struct{}
	stderrDone chan struct{}

	tail *tailRing

	mu       sync.Mutex
	exited   bool
	status   ExitStatus
	cause    ExitCause
	frameErr error

	closeOnce sync.Once
	closeErr  error

	stdinOnce sync.Once
	stdinErr  error
}

var _ Conn = (*Process)(nil)

// Launch starts the process and returns once it exists. ctx owns the lifetime
// of the whole tree exactly as for internal/exec.Start.
func Launch(ctx context.Context, opt Options) (*Process, error) {
	if ctx == nil {
		return nil, errors.New("streamjson: Launch(nil context)")
	}
	switch {
	case opt.WriteTimeout < 0:
		return nil, fmt.Errorf("streamjson: Options.WriteTimeout is negative (%v)", opt.WriteTimeout)
	case opt.MaxLineBytes < 0:
		return nil, fmt.Errorf("streamjson: Options.MaxLineBytes is negative (%d)", opt.MaxLineBytes)
	case opt.StderrTailBytes < 0:
		return nil, fmt.Errorf("streamjson: Options.StderrTailBytes is negative (%d)", opt.StderrTailBytes)
	case opt.EventBuffer < 0:
		return nil, fmt.Errorf("streamjson: Options.EventBuffer is negative (%d)", opt.EventBuffer)
	}
	if opt.Command == "" {
		return nil, errors.New("streamjson: Options.Command is required")
	}
	command := opt.Command
	args := append([]string(nil), opt.Args...)
	wtimeout := orDefault(opt.WriteTimeout, DefaultWriteTimeout)
	maxLine := orDefault(opt.MaxLineBytes, DefaultMaxLineBytes)
	tailBytes := orDefault(opt.StderrTailBytes, DefaultStderrTailBytes)
	buffer := orDefault(opt.EventBuffer, DefaultEventBuffer)

	cmd, err := exec.Start(ctx, &exec.Command{
		Path:      command,
		Args:      args,
		Dir:       opt.Dir,
		Env:       opt.Env,
		KillGrace: opt.KillGrace,
		WaitDelay: opt.WaitDelay,
	})
	if err != nil {
		return nil, err
	}
	p := &Process{
		cmd:        cmd,
		ctx:        ctx,
		wtimeout:   wtimeout,
		maxLine:    maxLine,
		events:     make(chan Message, buffer),
		exit:       make(chan ExitStatus, 1),
		wq:         make(chan writeReq),
		done:       make(chan struct{}),
		stop:       make(chan struct{}),
		readerDone: make(chan struct{}),
		stderrDone: make(chan struct{}),
		tail:       &tailRing{buf: make([]byte, tailBytes)},
	}
	// internal/exec drains a stream nobody has read when the leader exits, which
	// would steal the output of a child that is gone before the readers run. A
	// zero-length Read claims a stream synchronously and consumes nothing.
	_, _ = cmd.Stdout().Read(nil)
	_, _ = cmd.Stderr().Read(nil)

	go p.readStdout()
	go p.readStderr()
	go p.writer()
	go p.reap()
	return p, nil
}

func orDefault[T int | time.Duration](v, def T) T {
	if v == 0 {
		return def
	}
	return v
}

// Pid is the process id of the leader of the tree.
func (p *Process) Pid() int { return p.cmd.Pid() }

// Events is the ordered, lossless feed of stdout. It is bounded: when full the
// reader blocks and the pressure goes into the pipe, so the consumer must drain
// it concurrently or call Close. It is closed after the last message.
func (p *Process) Events() <-chan Message { return p.events }

// Exit delivers the ExitStatus once and then closes.
func (p *Process) Exit() <-chan ExitStatus { return p.exit }

// Wait blocks until the exit has been published and returns the status.
func (p *Process) Wait() ExitStatus {
	<-p.done
	return p.status
}

// Close ends the process tree and waits for everything to be over. Idempotent.
func (p *Process) Close() error {
	p.closeOnce.Do(func() {
		p.record(CauseClosed)
		close(p.stop)
		p.closeErr = p.cmd.Close(context.Background())
		<-p.done
	})
	return p.closeErr
}

// CloseStdin closes the child's stdin, which is EOF for a child that reads its
// prompt as free text (cursor-agent). It is idempotent and must be called after
// the last WriteRaw returned: a write already owned by the writer goroutine is
// not cancelled by it.
func (p *Process) CloseStdin() error {
	p.stdinOnce.Do(func() { p.stdinErr = p.cmd.Stdin().Close() })
	return p.stdinErr
}

// Write implements Conn.
func (p *Process) Write(ctx context.Context, line []byte) error {
	if ctx == nil {
		return errors.New("streamjson: Write(nil context)")
	}
	if n := len(line); n == 0 || line[n-1] != '\n' || bytes.IndexByte(line[:n-1], '\n') >= 0 {
		return errors.New("streamjson: Write wants exactly one LF-terminated line")
	}
	return p.write(ctx, line)
}

// WriteRaw sends arbitrary bytes to the child's stdin with no line framing:
// cursor-agent's prompt *is* stdin, read to EOF, and it may be megabytes and
// contain newlines (issue #22). It is written by the same serialising writer
// goroutine as Write, so a large prompt cannot deadlock against a child that is
// busy writing stdout. Call CloseStdin after the last WriteRaw to signal EOF.
func (p *Process) WriteRaw(ctx context.Context, data []byte) error {
	if ctx == nil {
		return errors.New("streamjson: WriteRaw(nil context)")
	}
	if len(data) == 0 {
		return nil
	}
	return p.write(ctx, data)
}

// write queues one chunk for the writer goroutine and waits for the child's
// pipe to take it.
func (p *Process) write(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	if p.exited {
		st := p.status
		p.mu.Unlock()
		return &ExitedError{Status: st}
	}
	p.mu.Unlock()

	req := writeReq{line: data, done: make(chan error, 1)}
	timer := time.NewTimer(p.wtimeout)
	defer timer.Stop()

	select {
	case p.wq <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return p.writeTimedOut()
	case <-p.done:
		return &ExitedError{Status: p.status}
	}
	select {
	case err := <-req.done:
		if err != nil {
			return p.writeFailed(err)
		}
		return nil
	case <-ctx.Done():
		// The line may still land: the writer owns it now.
		return ctx.Err()
	case <-timer.C:
		return p.writeTimedOut()
	case <-p.done:
		select {
		case err := <-req.done:
			if err == nil {
				return nil
			}
		default:
		}
		return &ExitedError{Status: p.status}
	}
}

// writer is the only goroutine that touches stdin, so lines never interleave.
func (p *Process) writer() {
	for {
		select {
		case req := <-p.wq:
			_, err := p.cmd.Stdin().Write(req.line)
			req.done <- err
		case <-p.done:
			return
		case <-p.stop:
			return
		}
	}
}

func (p *Process) writeTimedOut() error {
	p.mu.Lock()
	if p.cause == "" && !p.exited {
		p.cause = CauseTimeout
	}
	p.mu.Unlock()
	_ = p.cmd.Signal(os.Kill)
	return fmt.Errorf("%w (after %v; the process group was killed)", ErrWriteTimeout, p.wtimeout)
}

// writeFailed explains a failed write: normally the process died, so give the
// reaper a bounded moment to say how.
func (p *Process) writeFailed(werr error) error {
	t := time.NewTimer(writeFailureWait)
	defer t.Stop()
	select {
	case <-p.done:
		return &ExitedError{Status: p.status}
	case <-t.C:
	}
	return fmt.Errorf("streamjson: write stdin: %w", werr)
}

func (p *Process) record(cause ExitCause) {
	p.mu.Lock()
	if p.cause == "" && !p.exited {
		p.cause = cause
	}
	p.mu.Unlock()
}

func (p *Process) readStdout() {
	defer close(p.readerDone)
	br := bufio.NewReaderSize(p.cmd.Stdout(), readChunk)
	for {
		raw, err := p.readLine(br)
		if err != nil {
			switch {
			case errors.Is(err, io.EOF):
			case errors.Is(err, ErrTruncatedLine):
				p.fatal(err, false)
			default:
				p.fatal(err, true)
			}
			return
		}
		if isBlank(raw) {
			continue
		}
		typ, err := parseHead(raw)
		if err != nil {
			p.fatal(err, true)
			return
		}
		p.deliver(Message{Type: typ, Raw: raw})
	}
}

func (p *Process) deliver(m Message) {
	select {
	case p.events <- m:
		return
	default:
	}
	select {
	case p.events <- m:
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
			var line []byte
			if acc == nil {
				line = append(make([]byte, 0, len(content)), content...)
			} else {
				line = append(acc, content...)
			}
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
				return nil, fmt.Errorf("%w (%d bytes): %q", ErrTruncatedLine, len(tail), clip(tail))
			}
			return nil, fmt.Errorf("streamjson: read stdout: %w", err)
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

func parseHead(line []byte) (string, error) {
	first := bytes.TrimLeft(line, " \t\r\n")
	if len(first) == 0 || first[0] != '{' {
		return "", fmt.Errorf("%w: %q", ErrBadFrame, clip(line))
	}
	var head struct {
		Type any `json:"type"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return "", fmt.Errorf("%w: %v: %q", ErrBadFrame, err, clip(line))
	}
	typ, _ := head.Type.(string)
	return typ, nil
}

func clip(line []byte) []byte {
	if len(line) > quoteBytes {
		return line[:quoteBytes]
	}
	return line
}

// fatal records the first framing error and ends the tree: stdout can no
// longer be trusted. A truncated tail is not a protocol cause: the stream simply
// ended.
func (p *Process) fatal(err error, protocol bool) {
	p.mu.Lock()
	if p.frameErr == nil {
		p.frameErr = err
	}
	if protocol && p.cause == "" && !p.exited {
		p.cause = CauseProtocol
	}
	p.mu.Unlock()
	_ = p.cmd.Signal(os.Kill)
	_ = p.cmd.Stdout().Close()
}

func (p *Process) readStderr() {
	defer close(p.stderrDone)
	_, _ = io.Copy(p.tail, p.cmd.Stderr())
}

func (p *Process) reap() {
	ws, werr := p.cmd.Wait(context.Background())
	<-p.readerDone
	<-p.stderrDone
	close(p.events)

	p.mu.Lock()
	cause := p.cause
	if cause == "" {
		if p.ctx.Err() != nil {
			cause = CauseClosed
		} else {
			cause = CauseExited
		}
	}
	st := ExitStatus{
		Code:       ws.Code,
		Signal:     ws.Signal,
		StderrTail: p.tail.String(),
		Cause:      cause,
		Err:        joinErrors(p.frameErr, werr),
	}
	p.exited = true
	p.status = st
	p.mu.Unlock()

	p.exit <- st
	close(p.exit)
	close(p.done)
}

func joinErrors(a, b error) error {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return errors.Join(a, b)
}

// tailRing keeps the last len(buf) bytes written to it.
type tailRing struct {
	mu   sync.Mutex
	buf  []byte
	next int
	full bool
}

func (r *tailRing) Write(b []byte) (int, error) {
	n := len(b)
	r.mu.Lock()
	defer r.mu.Unlock()
	size := len(r.buf)
	if size == 0 {
		return n, nil
	}
	if n >= size {
		copy(r.buf, b[n-size:])
		r.next, r.full = 0, true
		return n, nil
	}
	c := copy(r.buf[r.next:], b)
	if c < n {
		copy(r.buf, b[c:])
		r.next, r.full = n-c, true
		return n, nil
	}
	r.next += c
	if r.next == size {
		r.next, r.full = 0, true
	}
	return n, nil
}

func (r *tailRing) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return string(r.buf[:r.next])
	}
	out := make([]byte, 0, len(r.buf))
	out = append(out, r.buf[r.next:]...)
	out = append(out, r.buf[:r.next]...)
	i := 0
	for i < len(out) && i < utf8.UTFMax-1 && !utf8.RuneStart(out[i]) {
		i++
	}
	return string(out[i:])
}
