package kernel

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzjcool/oarlock"
)

// The kernel returns the frozen root types. Type aliases keep the issue's
// spelling without copying the root shape: there is one Go record type, and
// Kind discriminates which half of it carries meaning.
type (
	// Record is one element of the append-only stream.
	Record = oarlock.Record
	// RequestRecord is a Record whose Kind is KindRequest.
	RequestRecord = oarlock.Record
	// ResponseRecord is a Record whose Kind is KindResponse.
	ResponseRecord = oarlock.Record
	// Frame is a Record whose Kind is KindFrame.
	Frame = oarlock.Record
	// Cursor is a resumable position in one stream instance.
	Cursor = oarlock.Cursor
	// Rejected is the typed refusal an unreachable control is answered with.
	Rejected = oarlock.Rejected
	// ControlOutcome is the read face of the two records a control produced.
	ControlOutcome = oarlock.ControlOutcome
)

// FrameBody is exactly the frame half of oarlock.Record: Native + Events.
//
// The frozen record shape has no separate native-type field, so a frame's
// discriminator stays inside Native ("every native frame is retained whole in
// Native", docs/spec/record-stream.md).
type FrameBody struct {
	// Native is the runtime frame as sent.
	Native json.RawMessage
	// Events is what the adapter read out of Native, in frame order.
	Events []oarlock.Event
}

// RecordAt is the per-record attribution an adapter supplies. The kernel
// stamps Seq, ReceivedAt (unless given) and, for requests, a default id.
type RecordAt struct {
	// SessionID is the runtime-native session of this record. "" means the
	// kernel's own session (a derived child carries its own id).
	SessionID string
	// AgentPath is sub-agent lineage inside a session. nil/empty means the
	// root agent.
	AgentPath []string
	// SpanID is a runtime-native turn/span id. The kernel never invents one.
	SpanID string
	// ReceivedAt is best-effort observation time. Zero means time.Now().UTC().
	ReceivedAt time.Time
	// ID is the request's own id and is used by Request only. "" means the
	// kernel-generated "req-<seq>". Control ignores it.
	ID string
}

// SessionNode is one true session observed in the stream.
type SessionNode struct {
	ID string
}

// SessionEdge is runtime-reported lineage between two sessions.
type SessionEdge struct {
	Parent string
	Child  string
	Via    string
}

// SessionGraph is the sessions the stream knows about and the lineage the
// runtime reported for them.
type SessionGraph struct {
	Nodes []SessionNode
	Edges []SessionEdge
}

// Redactor runs before retention and observer delivery. It matches
// (*redact.Redactor).Record exactly, so an adapter passes red.Record directly.
//
// The bool ("changed") is advisory; the returned record is retained either
// way, but a redactor may change content only. After the hook returns, the
// kernel restores the stream-structural fields (Seq, Kind, SessionID,
// AgentPath, SpanID, ID, Direction, RequestID, Request.Action, Response.Kind)
// from the pre-redaction record, because identity, order and reachability are
// not credentials: a redactor can never renumber the stream, turn a dispose
// into another action, or make a rejection look like an exit.
type Redactor func(oarlock.Record) (oarlock.Record, bool)

// Kernel is one session's record stream: append, observe, control, cursor and
// graph. See the package documentation for the semantics it enforces.
type Kernel interface {
	// SessionID is the runtime-native session this instance belongs to.
	SessionID() string

	// Frame appends one runtime frame and returns the retained record. It is
	// never gated: a frame after exit or dispose is still retained.
	Frame(body FrameBody, at RecordAt) Frame

	// Request appends a request record and returns the retained record. The id
	// is at.ID when non-empty and "req-<seq>" otherwise.
	Request(dir oarlock.Direction, body oarlock.RequestBody, at RecordAt) RequestRecord

	// Respond appends a response pointing at requestID and returns the
	// retained record.
	Respond(requestID string, body oarlock.ResponseBody, at RecordAt) ResponseRecord

	// Control records a toRuntime request, decides, and records the response.
	// Reachability and the empty-input refusal are checked before decide, and
	// in that order. A nil decide is answered rejected{error}; a panicking
	// decide is recovered into rejected{error, <panic text>}.
	//
	// ControlOutcome.Request and .Response are the retained (redacted) records.
	// Its response RequestID is the request's pre-redaction id, so redaction can
	// never break the response→request link.
	Control(body oarlock.RequestBody, decide func(RequestRecord) oarlock.ResponseBody, at RecordAt) ControlOutcome

	// Unreachable reports why a control cannot reach the runtime any more,
	// derived from the stream only. It is nil while the runtime is reachable.
	Unreachable() *Rejected

	// Records replays the retained records after the cursor, in Seq order. It
	// returns nil for a cursor that belongs to another session instance; use
	// Replay to observe that error.
	Records(cursor Cursor) []Record

	// Replay is Records with the cursor error: a cursor whose SessionID names
	// another instance is reported as an error wrapping ErrCursorMismatch.
	Replay(cursor Cursor) ([]Record, error)

	// Observe registers a live observer with no replay and returns an
	// idempotent cancel.
	Observe(fn func(Record)) (cancel func())

	// ObserveFrom replays every retained record with Seq > AfterSeq, then
	// delivers live records, with no gap and no duplicate across the seam.
	// It must not be called from inside an observer callback.
	ObserveFrom(cursor Cursor, fn func(Record)) (cancel func(), err error)

	// ObserveBuffered registers an observer whose callback runs on its own
	// goroutine behind a bounded buffer of size records, so a slow consumer can
	// never slow the run. Like Observe it has no replay. When the buffer is full
	// the policy decides what is discarded (DropOldest, DropNewest, SignalGap),
	// every discard is counted by Dropped, and SignalGap also reports the exact
	// lost Seq range through OnGap as a Gap, never as a Record. A configuration
	// error wraps ErrBufferedConfig and registers nothing. Why there is no
	// backpressure: docs/spec/observers.md.
	ObserveBuffered(fn func(Record), size int, policy Policy, opts ...BufferedOption) (*BufferedObserver, error)

	// Graph returns a deep copy of the session graph.
	Graph() SessionGraph
}

// Link builds the session_linked event an adapter puts in a frame's Events
// before calling Frame. Lineage enters the graph only through that frame; the
// kernel offers no out-of-band graph mutation.
func Link(edge SessionEdge) oarlock.Event {
	return oarlock.Event{
		Kind:   oarlock.EventSessionLink,
		Parent: edge.Parent,
		Child:  edge.Child,
		Via:    edge.Via,
	}
}

var _ Kernel = (*sessionKernel)(nil)

// observerEntry is one registered side-tap. after is the highest Seq already
// delivered to it; it is written only by the goroutine that holds the
// publisher role, which is also the only goroutine that reads it.
//
// cancelled is set by cancel under mu and read by the publisher before every
// callback, so a cancel stops the rest of the current delivery batch too: at
// most the callback already running when cancel is called may finish.
type observerEntry struct {
	id        uint64
	fn        func(Record)
	after     uint64
	cancelled atomic.Bool
}

// sessionKernel is the implementation of Kernel.
//
// One mutex (mu) guards Seq, the retained log, the session graph, the observer
// registry, the notify queue, the reachability flags and the publishing flag.
// A second mutex (publishMu) serializes the publisher role, so at most one
// goroutine delivers records and the notify queue is drained in Seq order.
// Observer callbacks are never invoked while either mutex is held.
type sessionKernel struct {
	sessionID string
	redact    Redactor

	mu        sync.Mutex
	publishMu sync.Mutex

	seq        uint64
	log        []Record
	graph      SessionGraph
	nodeIDs    map[string]struct{}
	edgeKeys   map[[2]string]struct{}
	observers  map[uint64]*observerEntry
	nextObsID  uint64
	notified   []Record
	publishing bool
	exited     bool
	disposing  bool
}

// New returns a kernel for one session instance. A new instance restarts Seq
// at 1 even when it reopens the same runtime-native session id.
//
// redact may be nil, in which case records are retained as they were built,
// and it is called once per record before the record is retained or published.
// Pass (*redact.Redactor).Record to apply the documented credential rules.
func New(sessionID string, redact Redactor) Kernel {
	return newKernel(sessionID, redact)
}

// newKernel is New without the interface; tests use it to inspect state that
// Kernel deliberately does not expose.
func newKernel(sessionID string, redact Redactor) *sessionKernel {
	k := &sessionKernel{
		sessionID: sessionID,
		redact:    redact,
		nodeIDs:   make(map[string]struct{}),
		edgeKeys:  make(map[[2]string]struct{}),
		observers: make(map[uint64]*observerEntry),
	}
	k.addNodeLocked(sessionID)
	return k
}

// SessionID implements Kernel.
func (k *sessionKernel) SessionID() string { return k.sessionID }

// envelope is the part of a record the kernel stamps rather than the adapter.
type envelope struct {
	seq        uint64
	sessionID  string
	agentPath  []string
	spanID     string
	receivedAt time.Time
}

// append allocates the next Seq, builds the record, redacts a deep clone and
// retains it, then publishes it to the observers.
//
// It returns the unredacted original (which is what decide must see) and the
// retained record. The returned records copy every caller-owned slice and
// pointer, so reusing an adapter buffer cannot mutate the log.
//
// The mutex is released before the record is delivered: an observer callback
// may append again, and that record is queued for the active drain.
func (k *sessionKernel) append(build func(envelope) Record, at RecordAt) (Record, Record) {
	k.mu.Lock()
	k.seq++
	seq := k.seq

	env := envelope{
		seq:        seq,
		sessionID:  k.sessionID,
		agentPath:  at.AgentPath,
		spanID:     at.SpanID,
		receivedAt: at.ReceivedAt,
	}
	if at.SessionID != "" {
		env.sessionID = at.SessionID
	}
	if env.receivedAt.IsZero() {
		env.receivedAt = time.Now().UTC()
	}

	original := build(env)
	original.Seq = seq

	// The hook is handed a clone of its own, so an in-place edit can only ever
	// reach the record it returns, never the original that decide and the caller
	// keep.
	retained := cloneRecord(original)
	if k.redact != nil {
		redacted, ok := callRedactor(k.redact, retained)
		if ok {
			// A redactor changes content only: the structural fields are
			// restored from the record as the kernel built it (never from the
			// clone the hook was handed, which it may have mutated in place),
			// so it cannot renumber or re-Kind the stream, hide a dispose, or
			// fabricate an exit.
			retained = restoreStructure(redacted, original)
		} else {
			// The hook panicked before it finished, so none of its edits are
			// trusted: the record is retained exactly as it was built. A fresh
			// clone is required because the clone the hook was handed may already
			// have been mutated in place, and keeping that would let a broken hook
			// renumber, re-Kind or hide a record.
			retained = cloneRecord(original)
		}
	}
	// Redaction never renumbers the stream.
	retained.Seq = seq

	k.log = append(k.log, retained)
	k.noteRecordLocked(retained)
	if retained.Kind == oarlock.KindResponse && retained.Response != nil &&
		retained.Response.Kind == oarlock.ResponseExited {
		k.exited = true
	}
	if retained.Kind == oarlock.KindRequest && retained.Direction == oarlock.ToRuntime &&
		retained.Request != nil && retained.Request.Action == oarlock.ActionDispose {
		k.disposing = true
	}
	k.notified = append(k.notified, retained)
	// A record appended from inside an observer is queued; the active drain
	// delivers it in Seq order before the outer append returns. Returning here
	// also keeps a nested append from blocking on publishMu it already holds.
	publishing := k.publishing
	k.mu.Unlock()

	if !publishing {
		k.publish()
	}
	return original, retained
}

// callRedactor runs the hook with panic containment. A panicking hook reports
// ok == false and the caller keeps the record as it was built: a broken redactor
// must not deadlock the kernel, lose the record, or smuggle a half-applied edit
// (the clone it was handed may already have been mutated in place) into the
// stream.
func callRedactor(hook Redactor, record Record) (out Record, ok bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			ok = false
		}
	}()
	out, _ = hook(record)
	return out, true
}

// restoreStructure copies the stream-structural fields back from the record as
// the kernel built it, before the redactor saw a clone of it.
//
// A redactor may change content only. Identity, order and reachability are not
// credentials, so after the hook runs the kernel restores Seq, Kind, SessionID,
// AgentPath, SpanID, ID, Direction and RequestID, plus Request.Action and
// Response.Kind when the corresponding body is still present. A hook that drops
// the Request or Response body entirely gets the original body back, so a
// dispose request or an exit response cannot be hidden. Everything else
// (Native, Events, Input, Reason, native payloads) is whatever the redactor
// returned.
func restoreStructure(record Record, original Record) Record {
	record.Seq = original.Seq
	record.Kind = original.Kind
	record.SessionID = original.SessionID
	record.AgentPath = cloneStrings(original.AgentPath)
	record.SpanID = original.SpanID
	record.ID = original.ID
	record.Direction = original.Direction
	record.RequestID = original.RequestID
	if original.Request != nil {
		if record.Request == nil {
			record.Request = cloneRequestBody(original.Request)
		} else {
			record.Request.Action = original.Request.Action
		}
	}
	if original.Response != nil {
		if record.Response == nil {
			record.Response = cloneResponseBody(original.Response)
		} else {
			record.Response.Kind = original.Response.Kind
		}
	}
	return record
}

// Frame implements Kernel.
func (k *sessionKernel) Frame(body FrameBody, at RecordAt) Frame {
	_, retained := k.append(func(env envelope) Record {
		return frameRecord(env, body)
	}, at)
	return retained
}

// Request implements Kernel.
func (k *sessionKernel) Request(direction oarlock.Direction, body oarlock.RequestBody, at RecordAt) RequestRecord {
	_, retained := k.append(func(env envelope) Record {
		record := requestRecord(env, direction, body)
		record.ID = at.ID
		if record.ID == "" {
			record.ID = requestID(env.seq)
		}
		return record
	}, at)
	return retained
}

// Respond implements Kernel.
func (k *sessionKernel) Respond(requestID string, body oarlock.ResponseBody, at RecordAt) ResponseRecord {
	_, retained := k.append(func(env envelope) Record {
		return responseRecord(env, requestID, body)
	}, at)
	return retained
}

// Control implements Kernel.
//
// The request is recorded before decide runs and the response after, so the
// pair is ordered in the stream even when frames land in between: the two
// records are ordered, not adjacent. The response points at the request's
// pre-redaction id. ControlOutcome.Request and .Response are the retained
// (redacted) records.
func (k *sessionKernel) Control(body oarlock.RequestBody, decide func(RequestRecord) oarlock.ResponseBody, at RecordAt) ControlOutcome {
	// Step 1: snapshot the two gates before anything is recorded.
	blocked := k.Unreachable()
	refusal := emptyInputRefusal(body)

	// Control always generates its own request id; RecordAt.ID is ignored.
	at.ID = ""

	// Step 2: the request is recorded even when it will be refused.
	original, retainedRequest := k.append(func(env envelope) Record {
		record := requestRecord(env, oarlock.ToRuntime, body)
		record.ID = requestID(env.seq)
		return record
	}, at)

	// Steps 3 and 4: precedence is reachability, then empty input, then decide.
	var decision oarlock.ResponseBody
	switch {
	case blocked != nil && blocked.Code == oarlock.RuntimeExited && isDispose(body):
		// Nothing is left to release after an observed exit, so a dispose is
		// recorded and answered by the kernel itself (errors.md, "Process
		// exit").
		decision = oarlock.ResponseBody{Kind: oarlock.ResponseAccepted}
	case blocked != nil:
		decision = rejectedBody(blocked.Code, blocked.Reason)
	case refusal != nil:
		decision = *refusal
	default:
		decision = safeDecide(decide, original)
	}

	retainedResponse := k.Respond(original.ID, decision, at)
	return controlOutcome(retainedRequest, retainedResponse)
}

// Unreachable implements Kernel.
func (k *sessionKernel) Unreachable() *Rejected {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.exited {
		return &Rejected{Code: oarlock.RuntimeExited, Reason: "runtime exited"}
	}
	if k.disposing {
		return &Rejected{Code: oarlock.Disposed, Reason: "session disposed"}
	}
	return nil
}

// Graph implements Kernel.
func (k *sessionKernel) Graph() SessionGraph {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := SessionGraph{
		Nodes: make([]SessionNode, len(k.graph.Nodes)),
		Edges: make([]SessionEdge, len(k.graph.Edges)),
	}
	copy(out.Nodes, k.graph.Nodes)
	copy(out.Edges, k.graph.Edges)
	return out
}

// noteRecordLocked folds one retained record into the session graph. The
// caller holds mu.
//
// Only frames can report lineage: a session_linked event carried by a frame
// adds its edge, deduplicated by (Parent, Child). Foreign session ids establish
// nodes and never edges.
func (k *sessionKernel) noteRecordLocked(record Record) {
	k.addNodeLocked(record.SessionID)
	if record.Kind != oarlock.KindFrame {
		return
	}
	for _, event := range record.Events {
		if event.Kind != oarlock.EventSessionLink {
			continue
		}
		k.addNodeLocked(event.Parent)
		k.addNodeLocked(event.Child)
		key := [2]string{event.Parent, event.Child}
		if _, known := k.edgeKeys[key]; known {
			continue
		}
		k.edgeKeys[key] = struct{}{}
		k.graph.Edges = append(k.graph.Edges, SessionEdge{Parent: event.Parent, Child: event.Child, Via: event.Via})
	}
}

// addNodeLocked adds one session node once. The caller holds mu.
func (k *sessionKernel) addNodeLocked(id string) {
	if _, known := k.nodeIDs[id]; known {
		return
	}
	k.nodeIDs[id] = struct{}{}
	k.graph.Nodes = append(k.graph.Nodes, SessionNode{ID: id})
}

// requestID is the kernel-generated request id for a Seq.
func requestID(seq uint64) string { return fmt.Sprintf("req-%d", seq) }

// isDispose reports whether a control body releases the session.
func isDispose(body oarlock.RequestBody) bool { return body.Action == oarlock.ActionDispose }

// rejectedBody builds a rejected response body.
func rejectedBody(code oarlock.Code, reason string) oarlock.ResponseBody {
	return oarlock.ResponseBody{Kind: oarlock.ResponseRejected, Code: code, Reason: reason}
}

// emptyInputRefusal refuses a prompt, steer or queue that carries neither text
// nor images, before the adapter is consulted. Whitespace is left intact: only
// the empty string is refused.
func emptyInputRefusal(body oarlock.RequestBody) *oarlock.ResponseBody {
	switch body.Action {
	case oarlock.ActionPrompt, oarlock.ActionSteer, oarlock.ActionQueue:
	default:
		return nil
	}
	if body.Input != "" || len(body.Images) > 0 {
		return nil
	}
	refusal := rejectedBody(oarlock.Unsupported, "empty input: give text or images")
	return &refusal
}

// safeDecide runs the adapter's decision. A panic becomes a rejected response
// carrying the panic text: the process does not crash and the request stays in
// the stream.
func safeDecide(decide func(RequestRecord) oarlock.ResponseBody, request RequestRecord) (body oarlock.ResponseBody) {
	if decide == nil {
		return rejectedBody(oarlock.ErrorCode, "no decide function")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			body = rejectedBody(oarlock.ErrorCode, fmt.Sprint(recovered))
		}
	}()
	return decide(request)
}

// controlOutcome maps the two retained records to the read face. A
// ResponseExited body becomes rejected{runtime_exited}; accepted and answered
// are accepted.
func controlOutcome(request, response Record) ControlOutcome {
	outcome := ControlOutcome{
		Kind:      oarlock.ControlAccepted,
		Seq:       request.Seq,
		RequestID: request.ID,
		Request:   request,
		Response:  response,
	}
	if response.Response == nil {
		return outcome
	}
	switch response.Response.Kind {
	case oarlock.ResponseRejected:
		outcome.Kind = oarlock.ControlRejected
		outcome.Code = response.Response.Code
		outcome.Reason = response.Response.Reason
	case oarlock.ResponseExited:
		outcome.Kind = oarlock.ControlRejected
		outcome.Code = oarlock.RuntimeExited
		outcome.Reason = "runtime exited"
	}
	return outcome
}

// frameRecord builds the frame half of a record. Native and Events are copied.
func frameRecord(env envelope, body FrameBody) Record {
	return Record{
		Seq:        env.seq,
		ReceivedAt: env.receivedAt,
		Kind:       oarlock.KindFrame,
		SessionID:  env.sessionID,
		AgentPath:  cloneStrings(env.agentPath),
		SpanID:     env.spanID,
		Native:     cloneBytes(body.Native),
		Events:     cloneEvents(body.Events),
	}
}

// requestRecord builds a request record. The body is copied.
func requestRecord(env envelope, direction oarlock.Direction, body oarlock.RequestBody) Record {
	return Record{
		Seq:        env.seq,
		ReceivedAt: env.receivedAt,
		Kind:       oarlock.KindRequest,
		SessionID:  env.sessionID,
		AgentPath:  cloneStrings(env.agentPath),
		SpanID:     env.spanID,
		Direction:  direction,
		Request:    cloneRequestBody(&body),
	}
}

// responseRecord builds a response record. The body is copied.
func responseRecord(env envelope, requestID string, body oarlock.ResponseBody) Record {
	return Record{
		Seq:        env.seq,
		ReceivedAt: env.receivedAt,
		Kind:       oarlock.KindResponse,
		SessionID:  env.sessionID,
		AgentPath:  cloneStrings(env.agentPath),
		SpanID:     env.spanID,
		RequestID:  requestID,
		Response:   cloneResponseBody(&body),
	}
}

// cloneRecord copies every slice and pointer a record can reach, so the
// retained record shares no memory with the adapter that built it.
func cloneRecord(record Record) Record {
	out := record
	out.AgentPath = cloneStrings(record.AgentPath)
	out.Native = cloneBytes(record.Native)
	out.Events = cloneEvents(record.Events)
	out.Request = cloneRequestBody(record.Request)
	out.Response = cloneResponseBody(record.Response)
	return out
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneBytes(in json.RawMessage) json.RawMessage {
	if in == nil {
		return nil
	}
	out := make(json.RawMessage, len(in))
	copy(out, in)
	return out
}

func cloneInt(in *int) *int {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneInt64(in *int64) *int64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneFloat64(in *float64) *float64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneTime(in *time.Time) *time.Time {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneImages(in []oarlock.InputImage) []oarlock.InputImage {
	if in == nil {
		return nil
	}
	out := make([]oarlock.InputImage, len(in))
	copy(out, in)
	return out
}

func cloneRequestBody(in *oarlock.RequestBody) *oarlock.RequestBody {
	if in == nil {
		return nil
	}
	out := *in
	out.Images = cloneImages(in.Images)
	out.Carries = cloneStrings(in.Carries)
	out.Native = cloneBytes(in.Native)
	return &out
}

func cloneResponseBody(in *oarlock.ResponseBody) *oarlock.ResponseBody {
	if in == nil {
		return nil
	}
	out := *in
	out.Native = cloneBytes(in.Native)
	out.ExitCode = cloneInt(in.ExitCode)
	return &out
}

func cloneEvents(in []oarlock.Event) []oarlock.Event {
	if in == nil {
		return nil
	}
	out := make([]oarlock.Event, len(in))
	for i, event := range in {
		out[i] = cloneEvent(event)
	}
	return out
}

func cloneEvent(in oarlock.Event) oarlock.Event {
	out := in
	out.Content = cloneToolOutputParts(in.Content)
	out.ExitCode = cloneInt(in.ExitCode)
	out.Outcome = cloneTurnOutcome(in.Outcome)
	out.Usage = cloneUsageReport(in.Usage)
	out.Task = cloneTaskEvent(in.Task)
	return out
}

func cloneToolOutputParts(in []oarlock.ToolOutputPart) []oarlock.ToolOutputPart {
	if in == nil {
		return nil
	}
	out := make([]oarlock.ToolOutputPart, len(in))
	for i, part := range in {
		part.Value = cloneBytes(part.Value)
		out[i] = part
	}
	return out
}

func cloneTurnOutcome(in *oarlock.TurnOutcome) *oarlock.TurnOutcome {
	if in == nil {
		return nil
	}
	out := *in
	out.ResetsAt = cloneTime(in.ResetsAt)
	out.StructuredOutput = cloneBytes(in.StructuredOutput)
	return &out
}

func cloneUsageReport(in *oarlock.UsageReport) *oarlock.UsageReport {
	if in == nil {
		return nil
	}
	out := *in
	if in.Context != nil {
		context := *in.Context
		context.Tokens = cloneInt64(in.Context.Tokens)
		context.ContextWindow = cloneInt64(in.Context.ContextWindow)
		context.Percent = cloneFloat64(in.Context.Percent)
		out.Context = &context
	}
	out.Tokens = cloneTokenTotals(in.Tokens)
	out.Total = cloneTokenTotals(in.Total)
	return &out
}

func cloneTokenTotals(in *oarlock.TokenTotals) *oarlock.TokenTotals {
	if in == nil {
		return nil
	}
	out := *in
	out.Reasoning = cloneInt64(in.Reasoning)
	out.CacheRead = cloneInt64(in.CacheRead)
	out.CacheWrite = cloneInt64(in.CacheWrite)
	return &out
}

func cloneTaskEvent(in *oarlock.TaskEvent) *oarlock.TaskEvent {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
