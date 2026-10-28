// Package oarlock is the Go implementation of the OAR contract: one ordered,
// lossless record stream over any coding-agent runtime.
//
// The types in this file are the frozen shape of that stream. The semantics
// are in docs/spec/record-stream.md; the five invariants are enforced by the
// kernel (internal/kernel) rather than by this file.
package oarlock

import (
	"encoding/json"
	"time"
)

// Kind classifies a record by the obligation it carries, not by its shape.
type Kind string

const (
	// KindFrame is the runtime's own words: append-only, never synthesized.
	KindFrame Kind = "frame"
	// KindRequest is an action that expects an outcome. Direction says who
	// acts and who answers.
	KindRequest Kind = "request"
	// KindResponse points at a RequestID and exists only when oarlock
	// observed an outcome the runtime would not state itself.
	KindResponse Kind = "response"
)

// Direction is the sole test for "must the application answer this?".
// A toApp request body is runtime-verbatim and has an open vocabulary, so
// consumers must never parse the body to make that decision.
type Direction string

const (
	ToRuntime Direction = "toRuntime"
	ToApp     Direction = "toApp"
)

// Action names the toRuntime control a request records.
type Action string

const (
	ActionPrompt   Action = "prompt"
	ActionSteer    Action = "steer"
	ActionQueue    Action = "queue"
	ActionWithdraw Action = "withdraw"
	ActionAbort    Action = "abort"
	ActionDispose  Action = "dispose"
	// ActionNative is a runtime-to-application request, kept verbatim.
	ActionNative Action = "native"
)

// ResponseKind is the outcome recorded against a request.
type ResponseKind string

const (
	ResponseAccepted ResponseKind = "accepted"
	ResponseRejected ResponseKind = "rejected"
	ResponseAnswered ResponseKind = "answered"
	ResponseExited   ResponseKind = "exited"
)

// Code is the typed branch word carried by a rejected control response.
// The constants are frozen with the error taxonomy (docs/spec/errors.md).
type Code string

// Record is one element of the append-only stream.
//
// Seq is dense and monotonic inside one stream instance and starts at 1, so
// the zero Cursor ({AfterSeq: 0}) means "replay everything". Seq is never
// reused and never renumbered when a record is retained or redacted.
//
// ReceivedAt is best-effort observation time at adapter ingress. It is not
// part of the determinism guarantee.
//
// Request records use ID, Direction and Request. Response records use
// RequestID and Response. Frame records use Native and Events. The remaining
// fields stay at their zero value for the other kinds; a consumer must switch
// on Kind before reading them.
type Record struct {
	Seq        uint64    `json:"seq"`
	ReceivedAt time.Time `json:"receivedAt"`
	Kind       Kind      `json:"kind"`

	// SessionID is the runtime-native session the record belongs to. A
	// derived child session carries its own id here, never the root's.
	SessionID string `json:"sessionId"`
	// AgentPath is sub-agent lineage inside a session; nil and [] both mean
	// the root agent. Identity of a tool call or span is the composite
	// (AgentPath, id), never a bare call id.
	AgentPath []string `json:"agentPath,omitempty"`
	// SpanID is a runtime-native turn/span id. oarlock never generates one.
	SpanID string `json:"spanId,omitempty"`

	// Native is the runtime frame as sent (credential fields redacted before
	// retention). It is never trimmed, re-shaped, or replaced by a
	// "reasonable-looking" event.
	Native json.RawMessage `json:"native,omitempty"`
	// Events is what oarlock read out of Native, in frame order. Empty when
	// oarlock read nothing. One frame is always one record: events are never
	// split into sibling records, and frames are never merged.
	Events []Event `json:"events,omitempty"`

	// ID is the request's own id (request records only).
	ID string `json:"id,omitempty"`
	// Direction is set on request records only.
	Direction Direction `json:"direction,omitempty"`
	// Request is set on request records only.
	Request *RequestBody `json:"request,omitempty"`
	// RequestID points a response at the request it answers.
	RequestID string `json:"requestId,omitempty"`
	// Response is set on response records only.
	Response *ResponseBody `json:"response,omitempty"`
}

// InputImage is an image that travels with a prompt, steer or queue action.
// The record keeps the path, never the bytes.
type InputImage struct {
	Path      string `json:"path"`
	MediaType string `json:"mediaType,omitempty"`
}

// RequestBody is the body of a request record.
//
// For ActionNative (direction toApp) NativeType and Native hold the runtime's
// method and payload verbatim; the vocabulary is open on purpose.
type RequestBody struct {
	Action  Action `json:"action"`
	Input   string `json:"input,omitempty"`
	InputID string `json:"inputId,omitempty"`
	// Images are the native image inputs for prompt/steer/queue.
	Images []InputImage `json:"images,omitempty"`
	// Origin is host bookkeeping (who sent the input); it is never sent to
	// the runtime.
	Origin string `json:"origin,omitempty"`
	// Carries names earlier logical inputs delivered by this prompt. It is
	// copied to the prompt request only, never into model text.
	Carries []string `json:"carries,omitempty"`

	NativeType string          `json:"nativeType,omitempty"`
	Native     json.RawMessage `json:"native,omitempty"`
}

// ResponseBody is the body of a response record.
//
// Accepted means only "the action was taken over"; where input landed is a
// later frame. Rejected means the caller still owns the input. Answered is
// oarlock's own reply to a toApp request. Exited is the process exit, the one
// outcome the runtime cannot report itself.
type ResponseBody struct {
	Kind   ResponseKind    `json:"kind"`
	Code   Code            `json:"code,omitempty"`
	Reason string          `json:"reason,omitempty"`
	Native json.RawMessage `json:"native,omitempty"`

	// ExitCode is meaningful for ResponseExited. ExitCodeReported
	// distinguishes "the runtime reported a numeric-less exit" (true with a
	// nil ExitCode) from "no exit code was reported" (false).
	ExitCode         *int `json:"exitCode,omitempty"`
	ExitCodeReported bool `json:"exitCodeReported,omitempty"`
}

// Cursor is a resumable position in one stream instance. Records(cursor)
// replays retained records with Seq > AfterSeq, then continues live.
//
// SessionID identifies the stream instance's runtime-native session. Sequence
// numbers restart when a stream instance is recreated (for example after a
// resume); the host must keep the instance identity it stored with the cursor
// and must not mix cursors across instances.
type Cursor struct {
	SessionID string `json:"sessionId"`
	AfterSeq  uint64 `json:"afterSeq"`
}

// OutcomeKind is how a turn ended.
type OutcomeKind string

const (
	OutcomeCompleted OutcomeKind = "completed"
	OutcomeAborted   OutcomeKind = "aborted"
	OutcomeFailed    OutcomeKind = "failed"
)

// FailureClass is the adapter's coarse classification of a failed turn.
// "unknown" is an honest answer; vendor prose is never the branch word.
type FailureClass string

const (
	FailureAuth           FailureClass = "auth"
	FailureQuota          FailureClass = "quota"
	FailureRateLimited    FailureClass = "rate_limited"
	FailureBilling        FailureClass = "billing"
	FailureModelUnavail   FailureClass = "model_unavailable"
	FailureInputTooLarge  FailureClass = "input_too_large"
	FailureInvalidRequest FailureClass = "invalid_request"
	FailureOverloaded     FailureClass = "overloaded"
	FailureProvider       FailureClass = "provider"
	FailureRuntimeExited  FailureClass = "runtime_exited"
	FailureLimitReached   FailureClass = "limit_reached"
	FailureOutputSchema   FailureClass = "output_schema"
	FailureUnknown        FailureClass = "unknown"
)

// TurnOutcome is the runtime's own report of how a turn ended.
//
// StructuredOutput is the runtime-native value on completed turns, present
// only when the runtime reported one. It is never parsed or validated by
// oarlock. A nil value means "not reported"; that differs from a reported
// JSON null.
type TurnOutcome struct {
	Kind             OutcomeKind     `json:"kind"`
	Failure          FailureClass    `json:"failure,omitempty"`
	Reason           string          `json:"reason,omitempty"`
	Status           int             `json:"status,omitempty"`
	Credential       string          `json:"credential,omitempty"`
	ResetsAt         *time.Time      `json:"resetsAt,omitempty"`
	StructuredOutput json.RawMessage `json:"structuredOutput,omitempty"`
}

// ContextUsage is current context fullness as the runtime reports it. A nil
// field means the runtime did not report that number; it never means zero.
type ContextUsage struct {
	Tokens        *int64   `json:"tokens"`
	ContextWindow *int64   `json:"contextWindow"`
	Percent       *float64 `json:"percent"`
}

// TokenTotals are runtime-reported totals for the attributed agent, counted
// from this session's opening. Cache counts are parts of input; reasoning is
// part of output. A nil optional count means "not reported"; a reported zero
// stays zero.
type TokenTotals struct {
	Input      int64  `json:"input"`
	Output     int64  `json:"output"`
	Reasoning  *int64 `json:"reasoning,omitempty"`
	CacheRead  *int64 `json:"cacheRead,omitempty"`
	CacheWrite *int64 `json:"cacheWrite,omitempty"`
}

// UsageReport is what a usage-bearing frame says. Context is current
// fullness; Tokens is the authoritative figure for the record's AgentPath;
// Total is the session-wide total when the runtime reports one covering more
// than its agents (otherwise nil, and the agent figures sum to the session).
type UsageReport struct {
	Context *ContextUsage `json:"context,omitempty"`
	Tokens  *TokenTotals  `json:"tokens,omitempty"`
	Total   *TokenTotals  `json:"total,omitempty"`
}

// ToolOutputPart is one part of a tool result as the runtime reported it, in
// the runtime's order. Type is "text", "image" or "other".
type ToolOutputPart struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	MediaType string          `json:"mediaType,omitempty"`
	Data      string          `json:"data,omitempty"`
	Value     json.RawMessage `json:"value,omitempty"`
}

// ToolResult is the runtime's explicit tool outcome. The empty value means
// the runtime reported none; oarlock never infers it from output or timing.
type ToolResult string

const (
	ToolOK      ToolResult = "ok"
	ToolFailed  ToolResult = "failed"
	ToolUnknown ToolResult = ""
)

// TaskStatus is a native task lifecycle state.
type TaskStatus string

const (
	TaskPending   TaskStatus = "pending"
	TaskRunning   TaskStatus = "running"
	TaskPaused    TaskStatus = "paused"
	TaskCompleted TaskStatus = "completed"
	TaskFailed    TaskStatus = "failed"
	TaskStopped   TaskStatus = "stopped"
)

// TaskEvent carries native task facts. It is embedded in task_started,
// task_updated and task_ended events.
type TaskEvent struct {
	TaskID         string     `json:"taskId"`
	TaskType       string     `json:"taskType,omitempty"`
	NativeType     string     `json:"nativeType,omitempty"`
	Description    string     `json:"description,omitempty"`
	ToolCallID     string     `json:"toolCallId,omitempty"`
	ChildSessionID string     `json:"childSessionId,omitempty"`
	Background     bool       `json:"background,omitempty"`
	Ambient        bool       `json:"ambient,omitempty"`
	Status         TaskStatus `json:"status,omitempty"`
	Error          string     `json:"error,omitempty"`
	Summary        string     `json:"summary,omitempty"`
	OutputFile     string     `json:"outputFile,omitempty"`
}
