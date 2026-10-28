// Package pi is the transport for the Pi coding agent's RPC mode (`pi --mode
// rpc`): one child process, one JSON object per LF-terminated line in each
// direction, commands correlated to responses by id, everything else streamed.
//
// It is deliberately only a transport. It starts the process tree through
// internal/exec, frames stdout, matches responses to the commands that asked
// for them, keeps stderr as evidence and reports how the process ended. It has
// no opinion about turns: it never synthesises a turn boundary, never maps a
// native event to a record and never interprets an exit code. Prompt, queueing,
// abort and take-over belong to the layers above (#10, #11); the version gate
// and the refused options belong to #15.
//
//   - Framing is LF only. U+2028, U+2029 and U+0085 inside a JSON string never
//     split a record, exactly one trailing CR is stripped, blank lines are
//     skipped, and the line is delivered verbatim (Message.Raw is never
//     re-encoded). bufio.Scanner is not used on purpose: it would hand an
//     unterminated tail over as if it were a line.
//   - A line longer than Options.MaxLineBytes, a line that is not a JSON object
//     and a stream that ends inside a line are errors, never silent truncation.
//     All three are fatal and fail closed: a skipped garbled line could hide
//     the very frame that ends a turn.
//   - Every command gets a Conn-assigned id and a timeout. A command that is
//     not answered within Options.CommandTimeout kills the whole process group
//     (SIGKILL) and fails together with every other command still in flight:
//     a Pi that is silent is not a Pi that is slow.
//   - A cancelled ctx abandons exactly one wait. It never kills the child.
//   - Events() carries every stdout message in wire order, correlated responses
//     included, so one ordered feed can show that `response(prompt)` precedes
//     `agent_start`. Filter with Message.IsResponse.
//   - Message types outside the evidenced set are delivered verbatim and counted
//     in Stats.Unknown; they are never coerced, renamed or dropped.
//   - A non-zero exit code or a signal death is data (ExitStatus), never an
//     error. Exit 0 is not a turn boundary either.
//
// # Usage
//
//	p, err := pi.Launch(ctx, pi.Options{})        // runs `pi --mode rpc`
//	defer p.Close()
//	go func() {
//		for m := range p.Events() { handle(m) }   // drain concurrently
//	}()
//	var state json.RawMessage
//	err = p.Do(ctx, pi.Command{Type: "get_state"}, &state)
//	st := p.Wait()                                // how it ended, with the stderr tail
package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/zzjcool/oarlock/internal/exec"
)

const (
	// DefaultCommandTimeout bounds one Do call: the time to get the command
	// onto the wire plus the time to the matching response.
	DefaultCommandTimeout = 30 * time.Second

	// DefaultMaxLineBytes caps one stdout line (content, LF excluded). It is
	// the cap Pi's own reader is documented with: a 5 MiB payload line plus its
	// envelope must read intact.
	DefaultMaxLineBytes = 16 << 20

	// DefaultStderrTailBytes is how much of the end of stderr is kept.
	DefaultStderrTailBytes = 8 << 10

	// DefaultEventBuffer is the capacity of the Events channel.
	DefaultEventBuffer = 256

	// DefaultExecutable is the program Launch runs when Options.Command is "".
	DefaultExecutable = "pi"
)

const (
	// readChunk is the size of the bufio buffer over stdout. A line longer than
	// it is accumulated piece by piece, so the cap is checked while reading.
	readChunk = 64 << 10

	// exitPublishWait is how long Do waits for the reaper to publish the exit
	// status after it has killed the group or after a write failed, so the
	// error it returns can carry the stderr tail.
	exitPublishWait = 5 * time.Second

	// quoteBytes is the most bytes of a bad line or a truncated tail that an
	// error message quotes.
	quoteBytes = 200
)

// DefaultArgs returns a fresh copy of the arguments Launch uses when
// Options.Args is nil: `--mode rpc`.
func DefaultArgs() []string { return []string{"--mode", "rpc"} }

var (
	// ErrExited is the sentinel of ExitedError: the process is gone, so the
	// command was not (or could not be) answered.
	ErrExited = errors.New("pi: process exited")

	// ErrTimeout is the sentinel of TimeoutError: no response arrived within
	// Options.CommandTimeout and the process group was killed.
	ErrTimeout = errors.New("pi: command timed out")

	// ErrLineTooLong reports a stdout line whose content exceeded
	// Options.MaxLineBytes.
	ErrLineTooLong = errors.New("pi: stdout line exceeds MaxLineBytes")

	// ErrTruncatedLine reports that stdout ended inside an unterminated line.
	// The partial line is never delivered as a message.
	ErrTruncatedLine = errors.New("pi: stdout ended inside an unterminated line")

	// ErrBadFrame reports a stdout line that is not a JSON object.
	ErrBadFrame = errors.New("pi: stdout line is not a JSON object")
)

// Conn is the whole surface a caller needs from a Pi RPC process.
type Conn interface {
	// Do sends cmd, waits for the response that carries its id and decodes
	// the response's data member into out.
	Do(ctx context.Context, cmd Command, out any) error

	// Events is the ordered feed of every message the process wrote.
	Events() <-chan Message

	// Exit delivers the ExitStatus once, then closes.
	Exit() <-chan ExitStatus

	// Close ends the process tree and releases everything the Conn holds.
	Close() error
}

// Command is one request to Pi.
type Command struct {
	// Type is the native command type ("prompt", "abort", "get_state", ...). It
	// is required.
	Type string

	// Params is nil or a value that marshals to a JSON object. Its members are
	// written flat beside "type" and "id", which Params must not contain: the
	// Conn assigns the id.
	Params any
}

// Message is one stdout line.
type Message struct {
	// Type is the string member "type"; "" when it is absent or not a string.
	Type string

	// ID is the string member "id"; "" when it is absent or not a string.
	ID string

	// Raw is the line verbatim: the LF and one trailing CR removed, never
	// re-encoded. It is a private copy that the receiver owns, but it is
	// shared with every other receiver of the same Message: do not mutate it.
	Raw json.RawMessage
}

// IsResponse reports whether the message answers a command.
func (m Message) IsResponse() bool { return m.Type == "response" }

// ExitCause says who ended the process, as far as the Conn knows.
type ExitCause string

const (
	// CauseExited means the process ended by itself.
	CauseExited ExitCause = "exited"

	// CauseClosed means Close or the Launch ctx ended it.
	CauseClosed ExitCause = "closed"

	// CauseTimeout means a command timeout fired and the Conn killed the group.
	CauseTimeout ExitCause = "timeout"

	// CauseProtocol means the Conn killed the group after an unrecoverable
	// framing error on stdout.
	CauseProtocol ExitCause = "protocol"
)

// ExitStatus is how the process ended. It is data: a non-zero code or a
// signal death is a normal outcome, only the framing and I/O faults are Err.
type ExitStatus struct {
	// Code is the exit code; -1 when the process was killed by a signal.
	Code int

	// Signal is the signal that killed the process; nil otherwise.
	Signal os.Signal

	// StderrTail is the last StderrTailBytes bytes of stderr, starting on a
	// rune boundary. It is raw: callers redact it before it enters a record.
	StderrTail string

	// Cause says who ended the process.
	Cause ExitCause

	// Err is nil after a clean EOF; otherwise ErrLineTooLong, ErrTruncatedLine,
	// ErrBadFrame, a stdout read error or exec.ErrWaitDelay (joined if several).
	Err error
}

// ExitedError is the error of a command that the process could not answer
// because it ended first. It satisfies errors.Is(err, ErrExited) and unwraps
// to Status.Err, so errors.Is(err, ErrTruncatedLine) works as well.
type ExitedError struct {
	Status ExitStatus
}

// Error describes the exit and carries the stderr tail.
func (e *ExitedError) Error() string {
	var b strings.Builder
	b.WriteString("pi: process exited")
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

// Is reports whether target is ErrExited.
func (e *ExitedError) Is(target error) bool { return target == ErrExited }

// Unwrap returns Status.Err.
func (e *ExitedError) Unwrap() error { return e.Status.Err }

// TimeoutError is the error of a command that got no response within the
// command timeout. The process group was killed.
type TimeoutError struct {
	Command string        // the command type
	ID      string        // the id the Conn assigned
	After   time.Duration // the timeout that fired

	tail string // stderr tail, when the exit was published in time
}

// Error says that the group was killed and, when known, what stderr said.
func (e *TimeoutError) Error() string {
	s := fmt.Sprintf("pi: command %q (id %s) got no response within %v; the process group was killed", e.Command, e.ID, e.After)
	if tail := strings.TrimSpace(e.tail); tail != "" {
		s += "; stderr: " + tail
	}
	return s
}

// Is reports whether target is ErrTimeout.
func (e *TimeoutError) Is(target error) bool { return target == ErrTimeout }

// RPCError is the error of a command that Pi answered with success other than
// true. The response is kept whole for callers that want its fields.
type RPCError struct {
	Command  string  // the command type that was sent
	Message  string  // the response's error member, if it is a string
	Response Message // the response as received
}

// Error reports the command and Pi's message.
func (e *RPCError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("pi: command %q failed", e.Command)
	}
	return fmt.Sprintf("pi: command %q failed: %s", e.Command, e.Message)
}

// Options configures Launch. The zero value runs `pi --mode rpc`.
type Options struct {
	Command         string        // "" = DefaultExecutable, resolved through PATH by internal/exec
	Args            []string      // nil = DefaultArgs(); non-nil (even empty) is used verbatim
	Dir             string        // working directory; "" = inherit
	Env             []string      // nil = inherit the parent environment
	CommandTimeout  time.Duration // 0 = DefaultCommandTimeout; negative is an error
	MaxLineBytes    int           // 0 = DefaultMaxLineBytes; negative is an error
	StderrTailBytes int           // 0 = DefaultStderrTailBytes; negative is an error
	EventBuffer     int           // 0 = DefaultEventBuffer; negative is an error
	KillGrace       time.Duration // SIGTERM to SIGKILL grace of Close; 0 = exec's default
	WaitDelay       time.Duration // drain bound after the leader exits; 0 = exec's default
}

// Stats is a snapshot of what the process has written so far.
type Stats struct {
	Messages  uint64         // every well-formed stdout line
	Responses uint64         // responses matched to a waiting Do
	Orphans   uint64         // responses nobody was waiting for (late, unknown or id-less)
	Unknown   map[string]int // non-response types outside the evidenced set; "" = no string type
}

// knownTypes is the set of non-response message types that the evidence
// (docs/runtimes/pi.md §9) records. Anything else is tallied, never dropped.
var knownTypes = map[string]struct{}{
	"agent_start":          {},
	"turn_start":           {},
	"message_start":        {},
	"message_update":       {},
	"message_end":          {},
	"turn_end":             {},
	"agent_end":            {},
	"agent_settled":        {},
	"extension_ui_request": {},
	"extension_error":      {},
}

// Process is a running Pi RPC child. It implements Conn.
type Process struct {
	cmd *exec.Cmd
	ctx context.Context // the Launch ctx: its cancellation ends the tree

	timeout time.Duration
	maxLine int

	events chan Message
	exit   chan ExitStatus

	done chan struct{} // closed last, after the exit is published
	stop chan struct{} // closed by Close: release a reader blocked on events

	readerDone chan struct{}
	stderrDone chan struct{}

	slot   chan struct{} // write token, cap 1, pre-filled
	nextID atomic.Uint64

	tail *tailRing

	mu       sync.Mutex
	pending  map[string]chan Message
	exited   bool
	status   ExitStatus
	cause    ExitCause
	frameErr error
	messages uint64
	respd    uint64
	orphans  uint64
	unknown  map[string]int

	closeOnce sync.Once
	closeErr  error
}

var _ Conn = (*Process)(nil)

// Launch starts the process and returns once it exists. ctx owns the lifetime
// of the whole process tree exactly as it does for internal/exec.Start:
// cancelling it terminates the tree, and the exit is then reported with
// CauseClosed. Launch leaks nothing when it fails.
func Launch(ctx context.Context, opt Options) (*Process, error) {
	if ctx == nil {
		return nil, errors.New("pi: Launch(nil context)")
	}
	switch {
	case opt.CommandTimeout < 0:
		return nil, fmt.Errorf("pi: Options.CommandTimeout is negative (%v)", opt.CommandTimeout)
	case opt.MaxLineBytes < 0:
		return nil, fmt.Errorf("pi: Options.MaxLineBytes is negative (%d)", opt.MaxLineBytes)
	case opt.StderrTailBytes < 0:
		return nil, fmt.Errorf("pi: Options.StderrTailBytes is negative (%d)", opt.StderrTailBytes)
	case opt.EventBuffer < 0:
		return nil, fmt.Errorf("pi: Options.EventBuffer is negative (%d)", opt.EventBuffer)
	}
	command := opt.Command
	if command == "" {
		command = DefaultExecutable
	}
	args := DefaultArgs()
	if opt.Args != nil {
		args = append([]string(nil), opt.Args...)
	}
	timeout := opt.CommandTimeout
	if timeout == 0 {
		timeout = DefaultCommandTimeout
	}
	maxLine := opt.MaxLineBytes
	if maxLine == 0 {
		maxLine = DefaultMaxLineBytes
	}
	tailBytes := opt.StderrTailBytes
	if tailBytes == 0 {
		tailBytes = DefaultStderrTailBytes
	}
	buffer := opt.EventBuffer
	if buffer == 0 {
		buffer = DefaultEventBuffer
	}

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
		timeout:    timeout,
		maxLine:    maxLine,
		events:     make(chan Message, buffer),
		exit:       make(chan ExitStatus, 1),
		done:       make(chan struct{}),
		stop:       make(chan struct{}),
		readerDone: make(chan struct{}),
		stderrDone: make(chan struct{}),
		slot:       make(chan struct{}, 1),
		tail:       &tailRing{buf: make([]byte, tailBytes)},
		pending:    make(map[string]chan Message),
		unknown:    make(map[string]int),
	}
	p.slot <- struct{}{}

	// internal/exec drains a stream nobody has read yet when the leader exits,
	// which would steal the output of a child that is gone before the reader
	// goroutines are scheduled. A zero-length Read claims a stream
	// synchronously and consumes nothing.
	_, _ = cmd.Stdout().Read(nil)
	_, _ = cmd.Stderr().Read(nil)

	go p.readStdout()
	go p.readStderr()
	go p.reap()
	return p, nil
}

// Pid is the process id of the leader of the tree.
func (p *Process) Pid() int { return p.cmd.Pid() }

// Events is the ordered feed of every message the process wrote, correlated
// responses included (filter with Message.IsResponse). It is closed after the
// last message, before Exit publishes the status.
//
// The channel is bounded (Options.EventBuffer) and lossless: when it is full
// the stdout reader blocks, so the backpressure goes into the pipe and on to
// the child. The consumer therefore must drain Events concurrently, or call
// Close, which releases the reader and discards what could not be delivered.
func (p *Process) Events() <-chan Message { return p.events }

// Exit delivers the ExitStatus exactly once and then closes. Wait returns the
// same status to any number of callers.
func (p *Process) Exit() <-chan ExitStatus { return p.exit }

// Wait blocks until the exit has been published and returns the status. Any
// number of callers may use it, before or after Close.
func (p *Process) Wait() ExitStatus {
	<-p.done
	return p.status
}

// Stats returns a snapshot of the message tallies. Unknown is a copy.
func (p *Process) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	unknown := make(map[string]int, len(p.unknown))
	for k, v := range p.unknown {
		unknown[k] = v
	}
	return Stats{Messages: p.messages, Responses: p.respd, Orphans: p.orphans, Unknown: unknown}
}

// Close ends the process tree and waits for everything to be over: stdin is
// closed, the group gets SIGTERM, and after Options.KillGrace SIGKILL. It is
// idempotent and returns the same result every time. When it returns, Events
// is closed, Exit is published and none of the package's goroutines is alive.
// Undelivered events are discarded.
func (p *Process) Close() error {
	p.closeOnce.Do(func() {
		p.record(CauseClosed)
		close(p.stop)
		p.closeErr = p.cmd.Close(context.Background())
		<-p.done
	})
	return p.closeErr
}

// Do sends cmd with a fresh id and waits for the response that carries it.
//
// On success=true the response's data member is decoded into out: out == nil
// ignores it, *json.RawMessage receives it verbatim, anything else goes
// through json.Unmarshal; a missing or null data leaves out untouched.
// success other than true is an *RPCError.
//
// A command that is not answered within Options.CommandTimeout kills the
// process group and returns a *TimeoutError; commands in flight at that moment
// fail with an *ExitedError whose Status.Cause is CauseTimeout. A process that
// ended before answering gives an *ExitedError. A ctx that ends abandons this
// one wait and returns ctx.Err(): it never kills the child, and a late response
// is then an orphan that still reaches Events and Stats.Orphans.
func (p *Process) Do(ctx context.Context, cmd Command, out any) error {
	if ctx == nil {
		return errors.New("pi: Do(nil context)")
	}
	id := "oar-pi-" + strconv.FormatUint(p.nextID.Add(1), 10)
	line, err := encodeCommand(id, cmd)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	waiter := make(chan Message, 1)
	p.mu.Lock()
	if p.exited {
		st := p.status
		p.mu.Unlock()
		return &ExitedError{Status: st}
	}
	p.pending[id] = waiter
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
	}()

	// The timer is armed before the write, so a write blocked on a full pipe
	// is bounded too. state makes the callback and the return race-free: the
	// callback kills the group only if the call has not finished first.
	var state atomic.Int32 // 0 waiting, 1 finished, 2 fired
	expired := make(chan struct{})
	timer := time.AfterFunc(p.timeout, func() {
		if len(waiter) > 0 || !state.CompareAndSwap(0, 2) {
			return // an answer is already in hand: it wins over the timer
		}
		p.killForTimeout()
		close(expired)
	})
	defer func() {
		state.CompareAndSwap(0, 1)
		timer.Stop()
	}()

	// Take the write slot; a waiter for it still honours ctx, timeout and exit.
	select {
	case <-p.slot:
	case <-expired:
		return p.afterExpiry(cmd, id, waiter, out)
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return &ExitedError{Status: p.status}
	}
	// select picks at random among ready cases: look again, now that the slot
	// is ours, before anything is written.
	select {
	case <-expired:
		p.slot <- struct{}{}
		return p.afterExpiry(cmd, id, waiter, out)
	case <-p.done:
		p.slot <- struct{}{}
		return &ExitedError{Status: p.status}
	case <-ctx.Done():
		p.slot <- struct{}{}
		return ctx.Err()
	default:
	}

	_, werr := p.cmd.Stdin().Write(line)
	p.slot <- struct{}{}
	if werr != nil {
		return p.writeFailed(ctx, cmd, id, waiter, out, expired, werr)
	}

	select {
	case m := <-waiter:
		return p.result(cmd, m, out)
	case <-expired:
		return p.afterExpiry(cmd, id, waiter, out)
	case <-ctx.Done():
		select {
		case m := <-waiter:
			return p.result(cmd, m, out)
		default:
		}
		return ctx.Err()
	case <-p.done:
		select {
		case m := <-waiter:
			return p.result(cmd, m, out)
		default:
		}
		return &ExitedError{Status: p.status}
	}
}

// killForTimeout records the timeout as the cause (the first cause wins) and
// kills the process group. A process that is already gone is left alone.
func (p *Process) killForTimeout() {
	p.mu.Lock()
	if p.exited {
		p.mu.Unlock()
		return
	}
	if p.cause == "" {
		p.cause = CauseTimeout
	}
	p.mu.Unlock()
	_ = p.cmd.Signal(os.Kill)
}

// afterExpiry builds the error of a Do whose timer fired. The group has been
// killed, so the reaper gets a bounded moment to publish the exit and the
// error can carry the stderr tail. A response that arrived in the meantime
// still wins, and an exit that another cause explains is an *ExitedError.
func (p *Process) afterExpiry(cmd Command, id string, waiter chan Message, out any) error {
	t := time.NewTimer(exitPublishWait)
	defer t.Stop()
	select {
	case <-p.done:
	case <-t.C:
	}
	select {
	case m := <-waiter:
		return p.result(cmd, m, out)
	default:
	}
	p.mu.Lock()
	exited, st := p.exited, p.status
	p.mu.Unlock()
	if exited && st.Cause != CauseTimeout {
		return &ExitedError{Status: st}
	}
	e := &TimeoutError{Command: cmd.Type, ID: id, After: p.timeout}
	if exited {
		e.tail = st.StderrTail
	}
	return e
}

// writeFailed explains a failed write. A write blocked on a full pipe is
// released by the group kill, so the timer may be the reason; otherwise the
// process died (give the reaper a bounded moment to say how), and only when it
// did not is the write error itself the answer.
func (p *Process) writeFailed(ctx context.Context, cmd Command, id string, waiter chan Message, out any, expired <-chan struct{}, werr error) error {
	select {
	case <-expired:
		return p.afterExpiry(cmd, id, waiter, out)
	default:
	}
	t := time.NewTimer(exitPublishWait)
	defer t.Stop()
	select {
	case <-p.done:
		select {
		case m := <-waiter:
			return p.result(cmd, m, out)
		default:
		}
		return &ExitedError{Status: p.status}
	case <-expired:
		return p.afterExpiry(cmd, id, waiter, out)
	case <-ctx.Done():
	case <-t.C:
	}
	return fmt.Errorf("pi: write %q: %w", cmd.Type, werr)
}

// result turns a correlated response into Do's return value.
func (p *Process) result(cmd Command, m Message, out any) error {
	var r struct {
		Success any             `json:"success"`
		Data    json.RawMessage `json:"data"`
		Error   any             `json:"error"`
	}
	if err := json.Unmarshal(m.Raw, &r); err != nil {
		return fmt.Errorf("pi: decode %q response: %w", cmd.Type, err)
	}
	if ok, _ := r.Success.(bool); !ok {
		msg, _ := r.Error.(string)
		return &RPCError{Command: cmd.Type, Message: msg, Response: m}
	}
	if out == nil || len(r.Data) == 0 || bytes.Equal(r.Data, []byte("null")) {
		return nil
	}
	if raw, ok := out.(*json.RawMessage); ok && raw != nil {
		*raw = r.Data
		return nil
	}
	if err := json.Unmarshal(r.Data, out); err != nil {
		return fmt.Errorf("pi: decode %q response data: %w", cmd.Type, err)
	}
	return nil
}

// encodeCommand renders cmd as one line: {"id":..,"type":..,<params>}\n.
func encodeCommand(id string, cmd Command) ([]byte, error) {
	if cmd.Type == "" {
		return nil, errors.New("pi: Command.Type is required")
	}
	var members []byte
	if cmd.Params != nil {
		pb, err := json.Marshal(cmd.Params)
		if err != nil {
			return nil, fmt.Errorf("pi: encode %q params: %w", cmd.Type, err)
		}
		pb = bytes.TrimSpace(pb)
		if !bytes.Equal(pb, []byte("null")) {
			if len(pb) < 2 || pb[0] != '{' {
				return nil, fmt.Errorf("pi: %q params must marshal to a JSON object", cmd.Type)
			}
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(pb, &keys); err != nil {
				return nil, fmt.Errorf("pi: %q params must marshal to a JSON object: %w", cmd.Type, err)
			}
			for _, k := range []string{"type", "id"} {
				if _, bad := keys[k]; bad {
					return nil, fmt.Errorf("pi: %q params must not contain the key %q", cmd.Type, k)
				}
			}
			members = bytes.TrimSpace(pb[1 : len(pb)-1])
		}
	}
	idJSON, _ := json.Marshal(id)
	typeJSON, _ := json.Marshal(cmd.Type)
	line := make([]byte, 0, len(members)+len(idJSON)+len(typeJSON)+24)
	line = append(line, `{"id":`...)
	line = append(line, idJSON...)
	line = append(line, `,"type":`...)
	line = append(line, typeJSON...)
	if len(members) > 0 {
		line = append(line, ',')
		line = append(line, members...)
	}
	line = append(line, '}')
	if bytes.IndexByte(line, '\n') >= 0 {
		return nil, fmt.Errorf("pi: %q encodes to more than one line", cmd.Type)
	}
	return append(line, '\n'), nil
}

// record stores cause unless one is already recorded or the exit has been
// published.
func (p *Process) record(cause ExitCause) {
	p.mu.Lock()
	if p.cause == "" && !p.exited {
		p.cause = cause
	}
	p.mu.Unlock()
}

// readStdout frames stdout and dispatches every message until the stream ends
// or a framing error makes it unusable.
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
		typ, id, err := parseHead(raw)
		if err != nil {
			p.fatal(err, true)
			return
		}
		p.dispatch(Message{Type: typ, ID: id, Raw: raw})
	}
}

// readLine returns the next LF-terminated line without its LF and without one
// trailing CR. The result is a private copy. The cap is enforced while the
// line accumulates, so a runaway line never grows beyond the cap plus one
// read chunk. io.EOF means a clean end; an unterminated tail is
// ErrTruncatedLine.
func (p *Process) readLine(br *bufio.Reader) ([]byte, error) {
	var acc []byte
	for {
		piece, err := br.ReadSlice('\n')
		switch {
		case err == nil:
			content := piece[:len(piece)-1]
			if len(acc)+len(content) > p.maxLine+1 { // +1: a CR that is stripped below
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
				head := tail
				if len(head) > quoteBytes {
					head = head[:quoteBytes]
				}
				return nil, fmt.Errorf("%w (%d bytes): %q", ErrTruncatedLine, len(tail), head)
			}
			return nil, fmt.Errorf("pi: read stdout: %w", err)
		}
	}
}

func (p *Process) tooLong() error {
	return fmt.Errorf("%w (limit %d bytes)", ErrLineTooLong, p.maxLine)
}

// isBlank reports whether line holds nothing but ASCII whitespace.
func isBlank(line []byte) bool {
	for _, c := range line {
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			return false
		}
	}
	return true
}

// parseHead checks that line is one JSON object and reads its "type" and "id"
// members; a member that is not a string reads as "".
func parseHead(line []byte) (typ, id string, err error) {
	first := bytes.TrimLeft(line, " \t\r\n")
	if len(first) == 0 || first[0] != '{' {
		return "", "", fmt.Errorf("%w: %q", ErrBadFrame, clip(line))
	}
	var head struct {
		Type any `json:"type"`
		ID   any `json:"id"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return "", "", fmt.Errorf("%w: %v: %q", ErrBadFrame, err, clip(line))
	}
	typ, _ = head.Type.(string)
	id, _ = head.ID.(string)
	return typ, id, nil
}

// clip is the part of a bad line that an error message quotes.
func clip(line []byte) []byte {
	if len(line) > quoteBytes {
		return line[:quoteBytes]
	}
	return line
}

// fatal records the first framing error and ends the tree: stdout can no
// longer be trusted, and a skipped line could hide the frame that ends a turn.
// Closing the stdout stream keeps cmd.Wait from waiting out WaitDelay for a
// reader that has stopped. A truncated tail is not a protocol cause: the
// stream simply ended.
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

// dispatch counts m, hands a response to its waiter first and then delivers
// every message to Events in wire order.
func (p *Process) dispatch(m Message) {
	p.mu.Lock()
	p.messages++
	if m.Type == "response" {
		if w, ok := p.pending[m.ID]; ok {
			delete(p.pending, m.ID)
			p.respd++
			w <- m // buffered(1) and the entry was just removed: never blocks
		} else {
			p.orphans++
		}
	} else if _, known := knownTypes[m.Type]; !known {
		p.unknown[m.Type]++
	}
	p.mu.Unlock()

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

// readStderr keeps the end of stderr in a fixed-size ring.
func (p *Process) readStderr() {
	defer close(p.stderrDone)
	_, _ = io.Copy(p.tail, p.cmd.Stderr())
}

// reap waits for the process, then for both readers, and publishes the exit.
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

// joinErrors joins a and b, returning the single non-nil one unwrapped.
func joinErrors(a, b error) error {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return errors.Join(a, b)
}

// tailRing keeps the last len(buf) bytes written to it, never more.
type tailRing struct {
	mu   sync.Mutex
	buf  []byte
	next int  // where the next byte goes
	full bool // buf has wrapped at least once
}

// Write implements io.Writer.
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

// String returns the kept bytes in order. When the ring has dropped older
// bytes, the start is moved forward to a rune boundary.
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
