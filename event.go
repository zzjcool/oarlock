package oarlock

// EventKind is the cross-runtime vocabulary of facts read out of a frame.
// It is deliberately smaller than any vendor's event set: an adapter that
// does not understand a native event keeps it in Record.Native and counts it;
// it never invents an Event to fill the gap.
type EventKind string

const (
	// Conversation and turn lifecycle.
	EventUserMessage  EventKind = "user_message"
	EventTurnActive   EventKind = "turn_active"
	EventTextDelta    EventKind = "text_delta"
	EventReasoning    EventKind = "reasoning"
	EventTurnEnded    EventKind = "turn_ended"
	EventSessionLink  EventKind = "session_linked"
	EventInputQueued  EventKind = "input_queued"
	EventInputDropped EventKind = "input_dropped"
	// EventInputHandled is the runtime itself reporting that a prompt was
	// handled without opening a turn (pi: the prompt response carries
	// disposition "handled", as for a slash command). It is a real runtime
	// fact carried by a frame, never synthesized by oarlock. It names the
	// prompt by RequestID (always the prompt request's id) and by InputID (the
	// host's optional id, so it may be empty); it names no turn and is followed
	// by no turn_active and no turn_ended.
	EventInputHandled EventKind = "input_handled"

	// Tools.
	EventToolCallStarted    EventKind = "tool_call_started"
	EventToolCallInput      EventKind = "tool_call_input"
	EventToolCallInputDelta EventKind = "tool_call_input_delta"
	EventToolCallProgress   EventKind = "tool_call_progress"
	EventToolCallEnded      EventKind = "tool_call_ended"

	// Context and accounting.
	EventUsage       EventKind = "usage"
	EventModel       EventKind = "model"
	EventEffort      EventKind = "effort"
	EventServiceTier EventKind = "service_tier"
	EventRetry       EventKind = "retry"

	// Compaction.
	EventCompactionStarted EventKind = "compaction_started"
	EventCompactionEnded   EventKind = "compaction_ended"

	// Native task lifecycle (sub-agents, background work).
	EventTaskStarted EventKind = "task_started"
	EventTaskUpdated EventKind = "task_updated"
	EventTaskEnded   EventKind = "task_ended"

	// Application requests.
	EventAppRequestCancelled EventKind = "app_request_cancelled"
)

// Evidence explains how a native user message was observed.
type Evidence string

const (
	EvidenceAcknowledged Evidence = "acknowledged"
	EvidenceTurnItem     Evidence = "turn_item"
	EvidenceConversation Evidence = "conversation"
)

// DroppedReason says why an accepted input was discarded by the runtime.
type DroppedReason string

const (
	// DroppedTurnInterrupted hands ownership back to the caller for resend.
	DroppedTurnInterrupted DroppedReason = "turn_interrupted"
	// DroppedRuntimeRefused is a classified native refusal.
	DroppedRuntimeRefused DroppedReason = "runtime_refused"
)

// CompactionOutcome is how a native compaction ended.
type CompactionOutcome string

const (
	CompactionCompleted CompactionOutcome = "completed"
	CompactionAborted   CompactionOutcome = "aborted"
	CompactionFailed    CompactionOutcome = "failed"
)

// Event is one attributed fact read out of a frame. It is a tagged struct
// rather than an interface so a record stream can be written to disk and
// replayed without a hand-written decoder per event type. A field is
// meaningful only for the kinds documented on it; adapters must leave all
// other fields at their zero value.
type Event struct {
	Kind EventKind `json:"kind"`

	// text_delta, reasoning, user_message.
	Text            string `json:"text,omitempty"`
	MessageID       string `json:"messageId,omitempty"`
	NativeMessageID string `json:"nativeMessageId,omitempty"`
	TurnID          string `json:"turnId,omitempty"`
	// Redacted marks a reasoning item whose content the runtime withheld.
	Redacted bool `json:"redacted,omitempty"`

	// user_message.
	Evidence Evidence `json:"evidence,omitempty"`

	// tool_call_started, tool_call_input, tool_call_input_delta,
	// tool_call_progress, tool_call_ended.
	CallID string `json:"callId,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Input  string `json:"input,omitempty"`
	Delta  string `json:"delta,omitempty"`

	// tool_call_progress.
	Output      string `json:"output,omitempty"`
	OutputDelta string `json:"outputDelta,omitempty"`

	// tool_call_ended.
	Content []ToolOutputPart `json:"content,omitempty"`
	Result  ToolResult       `json:"result,omitempty"`
	// ExitCode is the process exit status the runtime reported for a command
	// it ran. ExitCodeReported distinguishes an explicit null (signal without
	// a number) from "not reported".
	ExitCode         *int `json:"exitCode,omitempty"`
	ExitCodeReported bool `json:"exitCodeReported,omitempty"`

	// turn_ended.
	Outcome *TurnOutcome `json:"outcome,omitempty"`

	// compaction_started, compaction_ended.
	Trigger          string            `json:"trigger,omitempty"`
	CompactionResult CompactionOutcome `json:"compactionResult,omitempty"`

	// retry.
	Attempt     int    `json:"attempt,omitempty"`
	MaxAttempts int    `json:"maxAttempts,omitempty"`
	DelayMS     int64  `json:"delayMs,omitempty"`
	Reason      string `json:"reason,omitempty"`

	// usage, model, effort, service_tier.
	Usage       *UsageReport `json:"usage,omitempty"`
	Model       string       `json:"model,omitempty"`
	Effort      string       `json:"effort,omitempty"`
	ServiceTier string       `json:"serviceTier,omitempty"`

	// session_linked.
	Parent string `json:"parent,omitempty"`
	Child  string `json:"child,omitempty"`
	Via    string `json:"via,omitempty"`

	// turn_active, input_queued, input_dropped, input_handled.
	InputID string        `json:"inputId,omitempty"`
	Failure FailureClass  `json:"failure,omitempty"`
	Message string        `json:"message,omitempty"`
	Dropped DroppedReason `json:"dropped,omitempty"`

	// task_started, task_updated, task_ended.
	Task *TaskEvent `json:"task,omitempty"`

	// app_request_cancelled, input_handled.
	RequestID string `json:"requestId,omitempty"`
}

// ReportedExitCode returns the runtime's exit code and whether it reported
// one at all. The second result is false for an unreported exit; a reported
// numeric-less exit returns (0, true) with ok=false semantics left to the
// caller, so use ExitCodeReported on the event directly for that case.
func (e Event) ReportedExitCode() (code int, reported bool) {
	if !e.ExitCodeReported {
		return 0, false
	}
	if e.ExitCode == nil {
		return 0, true
	}
	return *e.ExitCode, true
}
