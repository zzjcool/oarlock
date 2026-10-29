package codex

// The control plane of one Codex app-server session (issue #20):
// oarlock.Session over a Conn, recorded on a real kernel.
//
//   - Every notification becomes exactly one frame, Native verbatim (the whole
//     JSON-RPC line), in wire order. A notification of another thread is a frame
//     of a derived child session (SessionID = that thread); collaboration items
//     add the session_linked edge to the first frame that names it.
//   - A control is a request record, an RPC, and a response record written at the
//     reply's wire position (Conn.Call hook), so the response precedes every
//     notification codex wrote after the reply. kernel.Control cannot do that
//     (it records the response after decide returns), so rpcControl repeats its
//     gates: reachability, empty input, then the plan's own gate.
//   - A late reply (after an abort take-over or a timeout answered the control)
//     stays a frame; it never becomes a second response.
//   - Status is never cached: busy and no_active_turn fold the retained records
//     (internal/fold), so the answer agrees with what a host reading the stream
//     sees. The adapter keeps only transport state: the root thread's running
//     turn id (steer/abort identity) and the pending toApp requests.
//   - Steer exists (turn/steer with expectedTurnId); Withdraw does not: the
//     queue is codex's own (thread/queue/add) and thread/queue/delete is
//     experimental and not live-verified, so *Session does not satisfy
//     oarlock.Withdrawer.
//   - A toApp request (approval, user input, dynamic tool) is recorded verbatim
//     and answered by the host through Answer; the adapter never answers one.
//
// Known gaps, on purpose: Events callbacks run inline on the pump, so they must
// not call controls; a late native error reply after a control timed out is a
// frame, not a response.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
	"github.com/zzjcool/oarlock/internal/kernel"
)

const (
	// DefaultAbortGrace is how long an accepted interrupt may leave the turn
	// running before oarlock takes over and kills the process group (OAR's
	// createAbortFallback default).
	DefaultAbortGrace = 10 * time.Second

	exitRecordedWait = 30 * time.Second
)

// ErrUnknownRequest is returned by Answer for a request id that is not a
// pending toApp request of this session.
var ErrUnknownRequest = errors.New("codex: no pending toApp request with that id")

// ErrAlreadyAnswered is returned by Answer for a request that was answered.
var ErrAlreadyAnswered = errors.New("codex: the toApp request was already answered")

// SessionOptions configures NewSession.
type SessionOptions struct {
	// ThreadID is the runtime-native thread id (thread/start reply). Required:
	// it is the stream instance's identity.
	ThreadID string
	// Resumed is true when the thread was opened with thread/resume: token totals
	// then count from codex's re-report of the thread's total.
	Resumed bool
	// OpenEvents are the model/effort/service_tier facts read out of the
	// thread/start|resume reply; they ride on that reply's frame.
	OpenEvents []oarlock.Event
	// OutputSchema is passed to every turn/start unchanged.
	OutputSchema json.RawMessage
	// AbortGrace: 0 = DefaultAbortGrace; negative is an error.
	AbortGrace time.Duration
	// Redact is the kernel's credential hook; nil retains records as built.
	Redact kernel.Redactor
	// CodexVersion is the version the gate verified; informational.
	CodexVersion string
}

// Session is a Codex app-server session. It implements oarlock.Session and
// oarlock.Steerer; it is not an oarlock.Withdrawer.
type Session struct {
	conn    Conn
	k       kernel.Kernel
	thread  string
	grace   time.Duration
	schema  json.RawMessage
	version string
	open    []oarlock.Event

	pumpDone     chan struct{}
	exitRecorded chan struct{}

	mu          sync.Mutex
	changed     chan struct{}
	proj        codexState
	turnID      string
	pending     map[string]json.RawMessage // pending toApp request id -> JSON-RPC id as sent
	answered    map[string]bool
	fb          *fallbackState
	disposeReq  string
	disposeDone chan struct{}
	disposeErr  error

	// settings carries the root thread's thread/settings/updated params to an
	// armed waiter (applyResumeEffort, effort.go). The pump only ever writes it
	// without blocking and it is never closed, so a report can neither stall nor
	// panic the pump.
	settings      chan obj
	settingsArmed bool // guarded by mu: the next root settings report is wanted
}

var (
	_ oarlock.Session = (*Session)(nil)
	_ oarlock.Steerer = (*Session)(nil)
)

// NewSession wraps an already-initialized Conn whose thread is open. It takes
// ownership of conn. The pump starts immediately, so frames the app-server
// wrote before this call become the first records.
func NewSession(conn Conn, opt SessionOptions) (*Session, error) {
	switch {
	case conn == nil:
		return nil, errors.New("codex: NewSession(nil Conn)")
	case opt.ThreadID == "":
		return nil, errors.New("codex: SessionOptions.ThreadID is required")
	case opt.AbortGrace < 0:
		return nil, fmt.Errorf("codex: SessionOptions.AbortGrace is negative (%v)", opt.AbortGrace)
	}
	grace := opt.AbortGrace
	if grace == 0 {
		grace = DefaultAbortGrace
	}
	s := &Session{
		conn: conn, k: kernel.New(opt.ThreadID, opt.Redact), thread: opt.ThreadID,
		grace: grace, schema: opt.OutputSchema, version: opt.CodexVersion, open: opt.OpenEvents,
		pumpDone: make(chan struct{}), exitRecorded: make(chan struct{}), changed: make(chan struct{}),
		settings: make(chan obj, 1),
		proj:     initialState(opt.ThreadID, opt.Resumed),
		pending:  map[string]json.RawMessage{}, answered: map[string]bool{},
	}
	// Whatever the app-server wrote during the handshake and the open is already
	// queued: record it before NewSession returns, so the first control a host
	// sends is ordered after it (the replies are the first records).
	s.drainBuffered()
	go s.pump()
	go s.watchExit()
	return s, nil
}

// Capabilities: the queue is codex's own and durable, children are sessions of
// their own (nested) and images are native inputs.
func (s *Session) Capabilities() oarlock.Capabilities {
	return oarlock.Capabilities{
		Queue:       oarlock.QueueCapability{Durable: true},
		Attribution: oarlock.AttributionNested,
		Images:      true,
	}
}

// ThreadID is the root thread id (also the stream's SessionID).
func (s *Session) ThreadID() string { return s.thread }

// CodexVersion is the version the gate verified ("" without a gate).
func (s *Session) CodexVersion() string { return s.version }

// Records returns the retained records after the cursor.
func (s *Session) Records(cursor oarlock.Cursor) []oarlock.Record { return s.k.Records(cursor) }

// Graph is the session graph the stream knows: the root, the child threads
// that spoke and the lineage the runtime reported.
func (s *Session) Graph() kernel.SessionGraph { return s.k.Graph() }

// Events replays the retained records after *cursor and then delivers live
// ones. A nil cursor is live only. The callback runs inline on the goroutine
// that appends, so it must be cheap and must not call a control.
func (s *Session) Events(cursor *oarlock.Cursor, fn func(oarlock.Record)) (unsubscribe func()) {
	if cursor == nil {
		return s.k.Observe(fn)
	}
	cancel, err := s.k.ObserveFrom(*cursor, fn)
	if err != nil {
		return func() {}
	}
	return cancel
}

// Unknown is a copy of the tally of notification methods the projection does
// not know. The frames are retained verbatim and produce no event.
func (s *Session) Unknown() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyCounts(s.proj.unknown)
}

// ---- controls ----------------------------------------------------------------

// Prompt starts a turn (turn/start). Rejected busy while the stream says a turn
// is running; never queued implicitly.
func (s *Session) Prompt(ctx context.Context, input string, opt oarlock.PromptOptions) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	body := oarlock.RequestBody{Action: oarlock.ActionPrompt, Input: input, InputID: opt.InputID, Images: opt.Images, Origin: opt.Origin, Carries: opt.Carries}
	return s.rpcControl(ctx, plan{
		body: body,
		gate: func(req oarlock.Record) *oarlock.ResponseBody {
			switch s.statusBefore(req.Seq).Kind {
			case fold.StatusRunning:
				return rejected(oarlock.Busy, "a turn is already running; wait for its end or abort it")
			case fold.StatusExited:
				return rejected(oarlock.RuntimeExited, "runtime exited")
			}
			return nil
		},
		method: "turn/start",
		params: func() any {
			p := map[string]any{"threadId": s.thread, "input": userInput(input, opt.Images)}
			if opt.InputID != "" {
				p["clientUserMessageId"] = opt.InputID
			}
			if len(s.schema) > 0 {
				p["outputSchema"] = s.schema
			}
			return p
		},
		onReply: func(r Reply) oarlock.ResponseBody {
			id, _ := parseObj(r.Result).obj("turn").str("id")
			if id == "" {
				return *rejected(oarlock.RuntimeRefused, "codex turn/start returned no turn id")
			}
			s.mu.Lock()
			s.turnID = id
			s.mu.Unlock()
			return accepted(r.Result)
		},
		onError: refusedBy,
	}), nil
}

// Steer adds input to the active turn (turn/steer). The expected turn id is the
// root turn this adapter knows to be running; codex refuses the steer when it
// is no longer the active one, and that refusal is the rejected answer
// (runtime_refused), never silence. Use SteerTurn to pin the turn explicitly.
func (s *Session) Steer(ctx context.Context, input string, opt oarlock.InputOptions) (oarlock.ControlOutcome, error) {
	return s.steer(ctx, input, opt, "")
}

// SteerTurn is Steer pinned to the turn the host believes is running. When
// expectedTurnID is not the root turn the adapter knows to be active the steer
// is rejected no_active_turn without being sent, and the reason names both ids.
func (s *Session) SteerTurn(ctx context.Context, input string, opt oarlock.InputOptions, expectedTurnID string) (oarlock.ControlOutcome, error) {
	if expectedTurnID == "" {
		return oarlock.ControlOutcome{}, errors.New("codex: SteerTurn needs a non-empty expectedTurnID")
	}
	return s.steer(ctx, input, opt, expectedTurnID)
}

func (s *Session) steer(ctx context.Context, input string, opt oarlock.InputOptions, pinned string) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	var expected string
	body := oarlock.RequestBody{Action: oarlock.ActionSteer, Input: input, InputID: opt.InputID, Images: opt.Images, Origin: opt.Origin}
	return s.rpcControl(ctx, plan{
		body: body,
		gate: func(oarlock.Record) *oarlock.ResponseBody {
			s.mu.Lock()
			expected = s.turnID
			s.mu.Unlock()
			if s.status().Kind != fold.StatusRunning || expected == "" {
				return rejected(oarlock.NoActiveTurn, "not_steerable: no active turn")
			}
			if pinned != "" && pinned != expected {
				return rejected(oarlock.NoActiveTurn, fmt.Sprintf("not_steerable: expectedTurnId %q does not match the active turn %q", pinned, expected))
			}
			return nil
		},
		method: "turn/steer",
		params: func() any {
			p := map[string]any{"threadId": s.thread, "input": userInput(input, opt.Images), "expectedTurnId": expected}
			if opt.InputID != "" {
				p["clientUserMessageId"] = opt.InputID
			}
			return p
		},
		onReply: func(r Reply) oarlock.ResponseBody {
			if opt.InputID != "" {
				s.mu.Lock()
				s.proj.inputs = acceptSteer(s.proj.inputs, expected, opt.InputID)
				s.mu.Unlock()
			}
			return accepted(r.Result)
		},
		onError: func(e *RPCError) oarlock.ResponseBody {
			return *rejected(oarlock.RuntimeRefused, "not_steerable: "+e.Message)
		},
	}), nil
}

// Queue hands input to codex's own queue (thread/queue/add). Codex starts a turn
// from it by itself once idle; that turn is adopted off the stream. There is no
// Withdraw: thread/queue/delete is experimental and not live-verified.
func (s *Session) Queue(ctx context.Context, input string, opt oarlock.InputOptions) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	cid := opt.InputID
	if cid == "" {
		cid = randomID()
	}
	return s.rpcControl(ctx, plan{
		body: oarlock.RequestBody{Action: oarlock.ActionQueue, Input: input, InputID: opt.InputID, Images: opt.Images, Origin: opt.Origin},
		gate: func(oarlock.Record) *oarlock.ResponseBody {
			if len(s.schema) > 0 {
				return rejected(oarlock.Unsupported, "codex queued input cannot carry outputSchema; use Prompt after the current turn ends")
			}
			return nil
		},
		method: "thread/queue/add",
		params: func() any {
			return map[string]any{"threadId": s.thread, "input": userInput(input, opt.Images), "clientUserMessageId": cid}
		},
		onReply: func(r Reply) oarlock.ResponseBody { return accepted(r.Result) },
		onError: refusedBy,
	}), nil
}

// Abort interrupts the running turn (turn/interrupt). With no turn it is
// rejected no_active_turn. The turn's own turn/completed(interrupted) is the
// outcome; if the turn has not ended when AbortGrace is up, oarlock takes over
// and kills the process group (the abort is still accepted, "we took the
// stop"), and no turn_ended is invented.
func (s *Session) Abort(ctx context.Context) (oarlock.ControlOutcome, error) {
	if err := ctx.Err(); err != nil {
		return oarlock.ControlOutcome{}, err
	}
	var turn string
	var disarm func()
	return s.rpcControl(ctx, plan{
		body: oarlock.RequestBody{Action: oarlock.ActionAbort},
		gate: func(oarlock.Record) *oarlock.ResponseBody {
			s.mu.Lock()
			turn = s.turnID
			s.mu.Unlock()
			if s.status().Kind != fold.StatusRunning || turn == "" {
				return rejected(oarlock.NoActiveTurn, "no active turn")
			}
			return nil
		},
		onPending: func(accept func()) { disarm = s.armFallback(accept) },
		method:    "turn/interrupt",
		params:    func() any { return map[string]any{"threadId": s.thread, "turnId": turn} },
		onReply:   func(r Reply) oarlock.ResponseBody { return accepted(r.Result) },
		onError: func(e *RPCError) oarlock.ResponseBody {
			if disarm != nil {
				disarm() // a refusal cancels only this attempt and kills nothing
			}
			return *rejected(oarlock.RuntimeRefused, e.Message)
		},
	}), nil
}

// Dispose releases the session. It is idempotent: the first call records one
// dispose request, ends the process tree and waits for the exited response;
// later calls wait for and return the first call's result.
func (s *Session) Dispose(ctx context.Context) error {
	s.mu.Lock()
	if s.disposeDone == nil {
		s.disposeDone = make(chan struct{})
		go s.runDispose(s.disposeDone)
	}
	done := s.disposeDone
	s.mu.Unlock()
	select {
	case <-done:
		return s.disposeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Session) runDispose(done chan struct{}) {
	defer close(done)
	s.k.Control(oarlock.RequestBody{Action: oarlock.ActionDispose}, func(req oarlock.Record) oarlock.ResponseBody {
		s.mu.Lock()
		s.disposeReq = req.ID
		s.mu.Unlock()
		return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
	}, kernel.RecordAt{})
	s.clearFallback()
	s.disposeErr = s.conn.Close()
	s.waitExitRecorded(context.Background())
}

// Answer replies to a pending toApp request with a JSON result and records the
// `answered` response. The result is the runtime's own shape for that request
// (for an approval, e.g. {"decision":"accept"}); oarlock does not interpret it.
// An unknown or already answered id, or a session that can no longer reach the
// runtime, is an error and nothing is sent.
func (s *Session) Answer(ctx context.Context, requestID string, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if un := s.k.Unreachable(); un != nil {
		return un
	}
	s.mu.Lock()
	id, ok := s.pending[requestID]
	if s.answered[requestID] {
		s.mu.Unlock()
		return ErrAlreadyAnswered
	}
	if !ok {
		s.mu.Unlock()
		return ErrUnknownRequest
	}
	s.answered[requestID] = true
	delete(s.pending, requestID)
	s.mu.Unlock()
	native, err := json.Marshal(result)
	if err != nil {
		s.unanswer(requestID, id)
		return err
	}
	if err := s.conn.RespondResult(id, json.RawMessage(native)); err != nil {
		s.unanswer(requestID, id)
		return err
	}
	s.k.Respond(requestID, oarlock.ResponseBody{Kind: oarlock.ResponseAnswered, Native: native}, kernel.RecordAt{})
	return nil
}

func (s *Session) unanswer(requestID string, id json.RawMessage) {
	s.mu.Lock()
	delete(s.answered, requestID)
	s.pending[requestID] = id
	s.mu.Unlock()
}

// ---- rpcControl ----------------------------------------------------------------

type plan struct {
	body      oarlock.RequestBody
	gate      func(req oarlock.Record) *oarlock.ResponseBody
	method    string
	params    func() any
	onReply   func(Reply) oarlock.ResponseBody
	onError   func(*RPCError) oarlock.ResponseBody
	onPending func(accept func())
}

func accepted(native json.RawMessage) oarlock.ResponseBody {
	return oarlock.ResponseBody{Kind: oarlock.ResponseAccepted, Native: native}
}

func rejected(code oarlock.Code, reason string) *oarlock.ResponseBody {
	return &oarlock.ResponseBody{Kind: oarlock.ResponseRejected, Code: code, Reason: reason}
}

func refusedBy(e *RPCError) oarlock.ResponseBody {
	return *rejected(oarlock.RuntimeRefused, e.Message)
}

// rpcControl records the request, decides, sends the RPC and records the
// response at the reply's wire position (see the file comment).
func (s *Session) rpcControl(ctx context.Context, p plan) oarlock.ControlOutcome {
	blocked := s.k.Unreachable()
	req := s.k.Request(oarlock.ToRuntime, p.body, kernel.RecordAt{})

	var refusal *oarlock.ResponseBody
	switch {
	case blocked != nil:
		refusal = rejected(blocked.Code, blocked.Reason)
	case carriesInput(p.body.Action) && p.body.Input == "" && len(p.body.Images) == 0:
		refusal = rejected(oarlock.Unsupported, "empty input: give text or images")
	default:
		refusal = p.gate(req)
	}
	if refusal != nil {
		return outcome(req, s.k.Respond(req.ID, *refusal, kernel.RecordAt{}))
	}

	var (
		mu       sync.Mutex
		recorded bool
		resp     oarlock.Record
		decided  = make(chan struct{})
	)
	record := func(b oarlock.ResponseBody) bool {
		mu.Lock()
		if recorded {
			mu.Unlock()
			return false
		}
		recorded = true
		mu.Unlock()
		resp = s.k.Respond(req.ID, b, kernel.RecordAt{})
		close(decided)
		return true
	}
	if p.onPending != nil {
		p.onPending(func() { record(oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}) })
	}
	hook := func(r Reply) {
		var b oarlock.ResponseBody
		if r.Err != nil {
			b = p.onError(r.Err)
		} else {
			b = p.onReply(r)
		}
		if !record(b) {
			// A fallback already answered: the late reply stays a frame.
			s.k.Frame(kernel.FrameBody{Native: r.Raw}, kernel.RecordAt{})
		}
	}
	// Not ctx: an abandoned turn/start may still open a turn and the stream
	// would then say idle while codex runs. The Conn's call timeout bounds it.
	_, err := s.conn.Call(context.WithoutCancel(ctx), p.method, p.params(), hook)
	if err != nil {
		var rpc *RPCError
		switch {
		case errors.As(err, &rpc):
			// the hook recorded it
		case errors.Is(err, ErrExited):
			record(*rejected(oarlock.RuntimeExited, err.Error()))
		default:
			record(*rejected(oarlock.ErrorCode, err.Error()))
		}
	}
	<-decided
	return outcome(req, resp)
}

func carriesInput(a oarlock.Action) bool {
	return a == oarlock.ActionPrompt || a == oarlock.ActionSteer || a == oarlock.ActionQueue
}

func outcome(req, resp oarlock.Record) oarlock.ControlOutcome {
	out := oarlock.ControlOutcome{Kind: oarlock.ControlAccepted, Seq: req.Seq, RequestID: req.ID, Request: req, Response: resp}
	if resp.Response == nil {
		return out
	}
	switch resp.Response.Kind {
	case oarlock.ResponseRejected:
		out.Kind, out.Code, out.Reason = oarlock.ControlRejected, resp.Response.Code, resp.Response.Reason
	case oarlock.ResponseExited:
		out.Kind, out.Code, out.Reason = oarlock.ControlRejected, oarlock.RuntimeExited, "runtime exited"
	}
	return out
}

func userInput(input string, images []oarlock.InputImage) []map[string]any {
	out := []map[string]any{}
	if input != "" {
		out = append(out, map[string]any{"type": "text", "text": input})
	}
	for _, img := range images {
		out = append(out, map[string]any{"type": "localImage", "path": img.Path})
	}
	return out
}

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *Session) statusBefore(seq uint64) fold.Status {
	records := s.k.Records(oarlock.Cursor{})
	if n := int(seq) - 1; n >= 0 && n < len(records) {
		records = records[:n]
	}
	return fold.StatusOf(records).Value
}

func (s *Session) status() fold.Status {
	return fold.StatusOf(s.k.Records(oarlock.Cursor{})).Value
}

// ---- abort take-over -----------------------------------------------------------

// fallbackState is the take-over timer of an accepted abort. Repeated aborts
// share one deadline; a refusal withdraws only its own attempt; a turn end or
// the process exit clears every attempt.
type fallbackState struct {
	timer     *time.Timer
	triggered bool
	attempts  map[int]func()
	next      int
}

func (s *Session) armFallback(accept func()) (disarm func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fb != nil && s.fb.triggered {
		go accept()
		return func() {}
	}
	if s.fb == nil {
		fb := &fallbackState{attempts: map[int]func(){}}
		fb.timer = time.AfterFunc(s.grace, func() { s.fireFallback(fb) })
		s.fb = fb
	}
	fb := s.fb
	n := fb.next
	fb.next++
	fb.attempts[n] = accept
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.fb != fb || fb.triggered {
			return
		}
		delete(fb.attempts, n)
		if len(fb.attempts) == 0 {
			fb.timer.Stop()
			s.fb = nil
		}
	}
}

func (s *Session) fireFallback(fb *fallbackState) {
	s.mu.Lock()
	if s.fb != fb {
		s.mu.Unlock()
		return
	}
	fb.triggered = true
	accepts := make([]func(), 0, len(fb.attempts))
	for _, a := range fb.attempts {
		accepts = append(accepts, a)
	}
	s.mu.Unlock()
	for _, a := range accepts {
		a()
	}
	_ = s.conn.Close()
}

func (s *Session) clearFallback() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fb != nil {
		s.fb.timer.Stop()
		s.fb = nil
	}
}

func (s *Session) waitExitRecorded(ctx context.Context) {
	t := time.NewTimer(exitRecordedWait)
	defer t.Stop()
	select {
	case <-s.exitRecorded:
	case <-ctx.Done():
	case <-t.C:
	}
}

// ---- pump and exit ---------------------------------------------------------------

// pump records everything the app-server wrote, in order. It never waits for a
// control.
func (s *Session) pump() {
	defer close(s.pumpDone)
	for f := range s.conn.Frames() {
		s.ingest(f)
	}
}

func (s *Session) drainBuffered() {
	for {
		select {
		case f, ok := <-s.conn.Frames():
			if !ok {
				return
			}
			s.ingest(f)
		default:
			return
		}
	}
}

func (s *Session) ingest(f Frame) {
	switch f.Kind {
	case FrameNotification:
		s.ingestNotification(f)
	case FrameServerRequest:
		s.ingestServerRequest(f)
	case FrameResponse:
		if f.Matched() && !f.Direct() {
			f.Settle() // the hook writes the response record here, in wire order
			s.signal()
			return
		}
		var events []oarlock.Event
		if f.Direct() && (f.Method == "thread/start" || f.Method == "thread/resume") {
			events = s.open
		}
		s.k.Frame(kernel.FrameBody{Native: f.Raw, Events: events}, kernel.RecordAt{})
		f.Settle()
		s.signal()
	}
}

func (s *Session) ingestNotification(f Frame) {
	params := parseObj(f.Params)
	thread, hasThread := params.str("threadId")
	isRoot := !hasThread || thread == s.thread
	if isRoot && f.Method == "turn/started" {
		if id, ok := params.obj("turn").str("id"); ok {
			s.mu.Lock()
			s.turnID = id
			s.mu.Unlock()
		}
	}

	s.mu.Lock()
	prev := s.proj
	s.mu.Unlock()
	next, cmd := foldNotification(prev, f.Method, params)
	events := cmd.Events

	// Lineage enters the graph once per edge, on the first frame that names it.
	if len(cmd.Links) > 0 {
		known := map[[2]string]bool{}
		for _, e := range s.k.Graph().Edges {
			known[[2]string{e.Parent, e.Child}] = true
		}
		for _, e := range cmd.Links {
			key := [2]string{e.Parent, e.Child}
			if known[key] {
				continue
			}
			known[key] = true
			events = append(events, kernel.Link(kernel.SessionEdge{Parent: e.Parent, Child: e.Child, Via: "tool_call"}))
		}
	}
	// A serverRequest/resolved that follows our own answer is not a cancellation.
	if f.Method == "serverRequest/resolved" {
		events = s.dropAnsweredCancel(events)
	}

	s.mu.Lock()
	s.proj = next
	if f.Method == "serverRequest/resolved" {
		if id, ok := requestIDOf(params["requestId"]); ok {
			delete(s.pending, id)
		}
	}
	s.mu.Unlock()

	s.k.Frame(kernel.FrameBody{Native: f.Raw, Events: events}, kernel.RecordAt{SessionID: cmd.SessionID, SpanID: cmd.SpanID})

	// The report reaches a waiter only after its frame is recorded, so a
	// confirmed effort is readable from the stream as soon as the wait returns.
	if isRoot && f.Method == "thread/settings/updated" {
		s.deliverSettings(params)
	}

	if isRoot && f.Method == "turn/completed" {
		s.mu.Lock()
		s.turnID = ""
		s.mu.Unlock()
		s.clearFallback()
	}
	s.signal()
}

// ---- settings reports (issue #54) ------------------------------------------

// armSettings registers the one-shot waiter for the root thread's next
// thread/settings/updated report. A report left behind by an earlier wait is
// dropped: it cannot answer a wait that starts after it.
func (s *Session) armSettings() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.settings:
	default:
	}
	s.settingsArmed = true
}

// deliverSettings hands one report to the armed waiter. It is one-shot and
// never blocks, so the pump cannot be held up by a host that stopped waiting;
// a report nobody wants is dropped rather than buffered.
func (s *Session) deliverSettings(params obj) {
	s.mu.Lock()
	armed := s.settingsArmed
	s.settingsArmed = false
	s.mu.Unlock()
	if !armed {
		return
	}
	select {
	case s.settings <- params:
	default:
	}
}

// awaitSettings takes the one armed report, or reports that the wait ended
// first (ctx bounds it) and stops listening.
func (s *Session) awaitSettings(ctx context.Context) (obj, bool) {
	select {
	case report := <-s.settings:
		return report, true
	case <-ctx.Done():
		s.disarmSettings()
		return nil, false
	}
}

// disarmSettings stops listening without taking a report.
func (s *Session) disarmSettings() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settingsArmed = false
	select {
	case <-s.settings:
	default:
	}
}

func (s *Session) dropAnsweredCancel(events []oarlock.Event) []oarlock.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := events[:0:0]
	for _, ev := range events {
		if ev.Kind == oarlock.EventAppRequestCancelled && s.answered[ev.RequestID] {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// requestIDOf renders a JSON-RPC request id the way records name it: strings as
// they are, numbers in decimal.
func requestIDOf(v any) (string, bool) {
	switch id := v.(type) {
	case string:
		return id, true
	case float64:
		return strconv.FormatFloat(id, 'f', -1, 64), true
	}
	return "", false
}

func (s *Session) ingestServerRequest(f Frame) {
	var id string
	if err := json.Unmarshal(f.ID, &id); err != nil {
		id = string(f.ID)
	}
	params := parseObj(f.Params)
	var sessionID string
	if t, ok := params.str("threadId"); ok && t != s.thread {
		sessionID = t
	}
	s.mu.Lock()
	s.pending[id] = f.ID
	s.mu.Unlock()
	s.k.Request(oarlock.ToApp, oarlock.RequestBody{
		Action: oarlock.ActionNative, NativeType: f.Method, Native: f.Params,
	}, kernel.RecordAt{ID: id, SessionID: sessionID, SpanID: spanID(params)})
	s.signal()
}

// watchExit records the process exit once everything the app-server wrote has
// been recorded: the exited response answers the dispose request when oarlock
// ended the process that way, and stands alone otherwise.
func (s *Session) watchExit() {
	st := <-s.conn.Exit()
	<-s.pumpDone

	s.mu.Lock()
	requestID := s.disposeReq
	s.mu.Unlock()
	s.clearFallback()

	body := oarlock.ResponseBody{Kind: oarlock.ResponseExited, Reason: st.StderrTail, ExitCodeReported: true}
	if st.Signal == nil {
		code := st.Code
		body.ExitCode = &code
	}
	if native, err := json.Marshal(map[string]any{"signal": signalName(st)}); err == nil {
		body.Native = native
	}
	s.k.Respond(requestID, body, kernel.RecordAt{})
	close(s.exitRecorded)
	s.signal()
}

func signalName(st ExitStatus) string {
	if st.Signal == nil {
		return ""
	}
	return st.Signal.String()
}

func (s *Session) signal() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}
