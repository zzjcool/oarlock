// Package fold reduces an oarlock record stream to the answers a host asks
// for: the agent's status, token usage, context fullness and the conversation.
//
// Every answer is a fold over the retained records. There is no cached
// snapshot beside the stream, so an answer can always be aligned with the
// record that produced it (QueryResult.Seq) and rebuilt by replaying the log.
// Feed is the only mutation path; every query method is pure and returns
// freshly copied values, so a caller holding an answer can never reach back
// into the fold.
//
// # Scope
//
// The root session id is the SessionID of the first fed record (the empty
// string is a valid id). A record is a root record iff its SessionID is that
// id and its AgentPath is empty. Only root records drive Status, ContextUsage
// and the root session's usage: a child agent's turn_ended never ends the root
// turn, and a child session's figures never overwrite the root's. Usage reads
// a derived child session only to add its own total once under WithChildren.
//
// # The stream is dense and ordered
//
// Feed assumes what the kernel guarantees: Seq is dense, monotonic and ordered
// inside one stream instance ([Record.Seq]). There is no dedupe; feeding a
// duplicate Seq folds it twice. A resumed runtime restarts Seq at 1, so a Fold
// must hold the records of one stream instance only.
//
// # Gaps against the TypeScript reference
//
// The reference folds
// packages/oar/src/observe/{agent-status,usage,usage-totals,conversation}.ts
// does more than this issue asks for. What is deliberately not ported, rather
// than invented:
//
//   - No clock. Status carries no lastEventAt and there is no stallOf: how
//     long a running status has been silent is fold(records) × clock and
//     belongs beside this package, not inside it.
//
//   - No queued-input ownership. The reference tracks a pendingPrompt so a
//     queued input can adopt a turn and a runtime notification can finish
//     while the host's accepted input still owns the slot. Here input_queued
//     changes nothing, and input_dropped ends the span only when the status
//     already holds that InputID.
//
//   - No turn-stop evidence. The reference remembers an accepted abort or
//     dispose and maps the exit that follows to idle{aborted}. Here exit is
//     final: StatusExited for the rest of the stream, and no TurnOutcome is
//     synthesized for it — LastTurnOutcome is only ever the runtime's own
//     turn_ended report.
//
//   - No model/effort/service_tier folds. Those are separate queries in the
//     reference and outside this issue.
//
//   - No input ledger. The reference conversation folds requests, delivery
//     attempts, accepted/rejected/dropped state, carries, images and origin
//     into inputs. This port folds the message view only — user, assistant,
//     reasoning and tool — which is what a UI renders.
//
//   - No per-model breakdown, no provider accounting. An adapter resolves its
//     runtime's own accounting into running totals before the record is
//     stamped; this package only folds what the records already say.
package fold

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/zzjcool/oarlock"
)

// QueryResult is a fold answer plus the Seq it was computed from.
type QueryResult[T any] struct {
	Value T
	Seq   uint64 // Seq of the last consumed record; 0 = no record consumed yet
}

// StatusKind is the coarse state of the root agent.
type StatusKind string

const (
	// StatusIdle means no turn is running.
	StatusIdle StatusKind = "idle"
	// StatusRunning means a turn is running; Phase says what it is doing.
	StatusRunning StatusKind = "running"
	// StatusExited means the runtime's process exit was observed. Final: no
	// later record reopens the stream.
	StatusExited StatusKind = "exited"
)

// PhaseKind is what a running turn is doing.
type PhaseKind string

const (
	// PhaseWaitingModel is a turn that has not produced model output yet.
	PhaseWaitingModel PhaseKind = "waiting_model"
	// PhaseThinking is streaming reasoning.
	PhaseThinking PhaseKind = "thinking"
	// PhaseResponding is streaming assistant text.
	PhaseResponding PhaseKind = "responding"
	// PhaseCompacting is compacting its context.
	PhaseCompacting PhaseKind = "compacting"
	// PhaseTool is running (or streaming the arguments of) a tool call.
	PhaseTool PhaseKind = "tool"
)

// Phase is the running turn's phase. Tool, CallID and Writing are meaningful
// only for PhaseTool.
type Phase struct {
	Kind    PhaseKind
	Tool    string // PhaseTool: the tool the call names
	CallID  string // PhaseTool: identity of the call, with the record's AgentPath
	Writing bool   // PhaseTool: arguments still streaming
}

// Status is the root agent's status.
//
// SinceSeq is the Seq of the record that started the current span: the prompt
// request, or the turn_active that adopted a running turn. It is 0 for idle
// and for an exit that never had a turn; an exit keeps the start of the turn
// that ran.
//
// RequestID and InputID are the request and input that opened the span; they
// stay empty for an adopted turn, which no recorded prompt of ours started.
//
// LastTurnOutcome is the runtime's own turn_ended report for the root turn;
// nil means the runtime reported none.
type Status struct {
	Kind            StatusKind
	Phase           Phase
	SinceSeq        uint64
	RequestID       string
	InputID         string
	LastTurnOutcome *oarlock.TurnOutcome
}

// AgentUsage is one agent's latest reported running total. AgentPath is the
// sub-agent lineage; empty is the root agent.
type AgentUsage struct {
	AgentPath []string
	Tokens    oarlock.TokenTotals
}

// Usage is the root session's token accounting.
//
// Total is the runtime's own session total when it reported one, else the sum
// of the agents' figures; nil means nothing was reported and never a guessed
// zero. ByAgent is nil when only the single root agent reported and nothing is
// unattributed. Unattributed is Total minus what the agents account for,
// present only when it carries tokens; it is never split by estimate.
// WithChildren is Total plus every derived child session's own total once, nil
// when nothing was reported or no child reported a total.
//
// Invariant: sum(ByAgent) + Unattributed == Total component-wise, with an
// absent optional part counting as 0, when the runtime's own views agree.
// A runtime can report a total smaller than its attributed agents; that view
// is internally inconsistent. Total stays the runtime's authoritative figure
// and Unattributed is omitted rather than made negative, so in that one case
// the attributed sum intentionally exceeds Total.
type Usage struct {
	Total        *oarlock.TokenTotals
	ByAgent      []AgentUsage
	Unattributed *oarlock.TokenTotals
	WithChildren *oarlock.TokenTotals
}

// ContextReport is one reported context fullness and the record that said it.
type ContextReport struct {
	Usage oarlock.ContextUsage // latest reported fullness for the root agent
	Seq   uint64               // Seq of the record that reported it
}

// Role is what a conversation message is.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleReasoning Role = "reasoning"
	RoleTool      Role = "tool"
)

// Message is one entry of the folded conversation.
//
// Seq is the first record that produced the message, EndSeq the last record
// merged into it. A contiguous run of text_delta (or reasoning) events that
// keeps the same role, agent path and non-empty MessageID is one message
// whose Text grows; a tool call is one message keyed by (AgentPath, CallID)
// wherever its events land.
//
// Text is the message text. For RoleTool, Input is the call's arguments
// (InputPartial while they are still streaming), Output the streamed preview,
// and Content/Result/ExitCode the runtime's own end report; Output is replaced
// by the result text — the text parts of Content joined by newlines — when the
// result holds any text part. ExitCodeReported distinguishes a reported
// numeric-less exit from an exit code the runtime never reported.
type Message struct {
	Role             Role
	Seq              uint64 // first record that produced it
	EndSeq           uint64 // last record merged into it
	AgentPath        []string
	Text             string
	MessageID        string
	NativeMessageID  string
	TurnID           string
	Evidence         oarlock.Evidence
	Redacted         bool
	CallID           string
	Tool             string
	Input            string
	InputPartial     bool
	Output           string
	Content          []oarlock.ToolOutputPart
	Result           oarlock.ToolResult
	ExitCode         *int
	ExitCodeReported bool
}

// Fold is the incremental state behind the four queries. Its zero value is a
// fold that has consumed no record; New is the documented constructor and Feed
// the only mutation path, so every query is a pure read of the same state
// whatever order the queries are asked in.
type Fold struct {
	seq    uint64
	root   string
	seeded bool

	status  Status
	usage   usageState
	context *ContextReport
	conv    conversationState
}

// New returns an empty fold: nothing consumed, status idle, no usage, no
// context, no messages.
func New() *Fold {
	return &Fold{status: Status{Kind: StatusIdle}}
}

// Of folds a batch of records: exactly New() fed every record in order.
// The records are consumed by value; the fold keeps copies of everything it
// retains.
func Of(records []oarlock.Record) *Fold {
	f := New()
	for _, r := range records {
		f.Feed(r)
	}
	return f
}

// Feed folds one record. It is the only mutation path: the stream is dense and
// ordered, so records are folded in the order they are fed, a duplicate Seq
// included.
func (f *Fold) Feed(r oarlock.Record) {
	if !f.seeded {
		f.root = r.SessionID
		f.seeded = true
	}
	f.seq = r.Seq
	root := f.isRoot(r)
	f.foldStatus(r, root)
	f.foldUsage(r)
	f.foldContext(r, root)
	f.foldConversation(r, f.inRootSession(r))
}

// Seq is the Seq of the last consumed record, 0 before any.
func (f *Fold) Seq() uint64 { return f.seq }

// inRootSession reports whether r belongs to the root session at all. A
// sub-agent's records live in the root session — they carry a non-empty
// AgentPath — while a derived child session's records carry their own id.
func (f *Fold) inRootSession(r oarlock.Record) bool { return r.SessionID == f.root }

// isRoot reports whether r is a record of the root session made by its root
// agent. Only those move the root's status and context; a child agent's turn
// never ends the root's.
func (f *Fold) isRoot(r oarlock.Record) bool {
	return r.SessionID == f.root && len(r.AgentPath) == 0
}

// StatusOf folds records and answers their status.
func StatusOf(records []oarlock.Record) QueryResult[Status] { return Of(records).Status() }

// UsageOf folds records and answers their usage.
func UsageOf(records []oarlock.Record) QueryResult[Usage] { return Of(records).Usage() }

// ContextUsageOf folds records and answers the latest reported context
// fullness.
func ContextUsageOf(records []oarlock.Record) QueryResult[*ContextReport] {
	return Of(records).ContextUsage()
}

// ConversationOf folds records and answers their messages.
func ConversationOf(records []oarlock.Record) QueryResult[[]Message] {
	return Of(records).Conversation()
}

// pathKey encodes an agent path so nil and [] (both the root agent) share one
// key and no two distinct paths collide.
func pathKey(path []string) string { return partsKey(path...) }

// partsKey encodes a sequence of strings injectively: each part is written as
// its length, a colon, then the bytes, which no two different sequences share.
func partsKey(parts ...string) string {
	var b strings.Builder
	for _, part := range parts {
		b.WriteString(strconv.Itoa(len(part)))
		b.WriteByte(':')
		b.WriteString(part)
	}
	return b.String()
}

// The clone helpers below are what keeps a query answer from aliasing the
// fold: every pointer, slice and raw message a caller can reach is copied on
// the way in and again on the way out.

func clonePath(path []string) []string {
	if len(path) == 0 {
		return nil
	}
	out := make([]string, len(path))
	copy(out, path)
	return out
}

// samePath reports whether two agent paths name the same lineage; nil and []
// both mean the root agent.
func samePath(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func cloneInt(v *int) *int {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}

func cloneInt64(v *int64) *int64 {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}

func cloneFloat64(v *float64) *float64 {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}

func cloneTokens(t oarlock.TokenTotals) oarlock.TokenTotals {
	return oarlock.TokenTotals{
		Input:      t.Input,
		Output:     t.Output,
		Reasoning:  cloneInt64(t.Reasoning),
		CacheRead:  cloneInt64(t.CacheRead),
		CacheWrite: cloneInt64(t.CacheWrite),
	}
}

func cloneTokensPtr(t *oarlock.TokenTotals) *oarlock.TokenTotals {
	if t == nil {
		return nil
	}
	out := cloneTokens(*t)
	return &out
}

func cloneContextUsage(u oarlock.ContextUsage) oarlock.ContextUsage {
	return oarlock.ContextUsage{
		Tokens:        cloneInt64(u.Tokens),
		ContextWindow: cloneInt64(u.ContextWindow),
		Percent:       cloneFloat64(u.Percent),
	}
}

func cloneTurnOutcome(o *oarlock.TurnOutcome) *oarlock.TurnOutcome {
	if o == nil {
		return nil
	}
	out := *o
	out.ResetsAt = cloneTime(o.ResetsAt)
	if o.StructuredOutput != nil {
		out.StructuredOutput = append(json.RawMessage(nil), o.StructuredOutput...)
	}
	return &out
}

// cloneParts copies tool output parts, raw values included.
func cloneParts(parts []oarlock.ToolOutputPart) []oarlock.ToolOutputPart {
	if len(parts) == 0 {
		return nil
	}
	out := make([]oarlock.ToolOutputPart, len(parts))
	for i, part := range parts {
		out[i] = part
		if part.Value != nil {
			out[i].Value = append(json.RawMessage(nil), part.Value...)
		}
	}
	return out
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	out := *t
	return &out
}

func cloneStatus(s Status) Status {
	s.LastTurnOutcome = cloneTurnOutcome(s.LastTurnOutcome)
	return s
}

func cloneMessage(m Message) Message {
	m.AgentPath = clonePath(m.AgentPath)
	m.Content = cloneParts(m.Content)
	m.ExitCode = cloneInt(m.ExitCode)
	return m
}
