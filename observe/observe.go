// Package observe is the public read surface over a session's record stream.
//
// The folds themselves live in internal/fold; this package exposes their
// result types and functions under names a host can import without depending
// on an internal path. The aliases are the same types, so an adapter method
// returning internal/fold's QueryResult[Status] satisfies StatusReader here.
package observe

import (
	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

// QueryResult is a fold answer and the Seq it was computed from.
type QueryResult[T any] struct {
	Value T
	Seq   uint64
}

type Status = fold.Status
type StatusKind = fold.StatusKind

const (
	StatusIdle    = fold.StatusIdle
	StatusRunning = fold.StatusRunning
	StatusExited  = fold.StatusExited
)

type Phase = fold.Phase
type PhaseKind = fold.PhaseKind

const (
	PhaseWaitingModel = fold.PhaseWaitingModel
	PhaseThinking     = fold.PhaseThinking
	PhaseResponding   = fold.PhaseResponding
	PhaseCompacting   = fold.PhaseCompacting
	PhaseTool         = fold.PhaseTool
)

type AgentUsage = fold.AgentUsage
type Usage = fold.Usage
type ContextReport = fold.ContextReport
type Message = fold.Message
type Role = fold.Role

const (
	RoleUser      = fold.RoleUser
	RoleAssistant = fold.RoleAssistant
	RoleReasoning = fold.RoleReasoning
	RoleTool      = fold.RoleTool
)

// StatusOf folds the root agent's status over records. The zero stream is idle.
func StatusOf(records []oarlock.Record) QueryResult[Status] {
	r := fold.StatusOf(records)
	return QueryResult[Status]{Value: r.Value, Seq: r.Seq}
}

// UsageOf folds the session's token accounting over records.
func UsageOf(records []oarlock.Record) QueryResult[Usage] {
	r := fold.UsageOf(records)
	return QueryResult[Usage]{Value: r.Value, Seq: r.Seq}
}

// ContextUsageOf folds the latest reported context fullness.
func ContextUsageOf(records []oarlock.Record) QueryResult[*ContextReport] {
	r := fold.ContextUsageOf(records)
	return QueryResult[*ContextReport]{Value: r.Value, Seq: r.Seq}
}

// ConversationOf folds the records into the user/assistant/tool/reasoning view.
func ConversationOf(records []oarlock.Record) QueryResult[[]Message] {
	r := fold.ConversationOf(records)
	return QueryResult[[]Message]{Value: r.Value, Seq: r.Seq}
}

// ModelOf is the latest model the runtime reported, with the Seq that said it.
func ModelOf(records []oarlock.Record) QueryResult[string] {
	return latest(records, oarlock.EventModel)
}

// EffortOf is the latest reasoning effort the runtime reported.
func EffortOf(records []oarlock.Record) QueryResult[string] {
	return latest(records, oarlock.EventEffort)
}

// ServiceTierOf is the latest service tier the runtime reported.
func ServiceTierOf(records []oarlock.Record) QueryResult[string] {
	return latest(records, oarlock.EventServiceTier)
}

func latest(records []oarlock.Record, kind oarlock.EventKind) QueryResult[string] {
	var out QueryResult[string]
	for _, rec := range records {
		for _, ev := range rec.Events {
			if ev.Kind != kind {
				continue
			}
			switch kind {
			case oarlock.EventModel:
				out.Value = ev.Model
			case oarlock.EventEffort:
				out.Value = ev.Effort
			case oarlock.EventServiceTier:
				out.Value = ev.ServiceTier
			}
			out.Seq = rec.Seq
		}
	}
	return out
}

// StatusReader is implemented by a session that exposes the status fold.
type StatusReader interface {
	Status() QueryResult[Status]
}

// UsageReader is implemented by a session that exposes the usage fold.
type UsageReader interface {
	Usage() QueryResult[Usage]
}

// ContextUsageReader is implemented by a session that reports context fullness.
type ContextUsageReader interface {
	ContextUsage() QueryResult[*ContextReport]
}

// ModelReader is implemented by a session whose runtime reports the model in
// effect.
type ModelReader interface {
	Model() QueryResult[string]
}

// EffortReader is implemented by a session whose runtime reports its effort.
type EffortReader interface {
	Effort() QueryResult[string]
}

// Readback is the whole read surface a host needs after a turn.
//
// These are five folds over the same record stream, so a host type-asserts once
// instead of five times:
//
//	if rb, ok := sess.(observe.Readback); ok { ... rb.Status() ... }
//
// The first-party adapters this module ships assert it at compile time in their
// readback_assert.go, so a break in any one of them fails the build here.
//
// Readback is a union, not a fallback: a session that lacks any one reader does
// not satisfy it. Absence is the capability — a host discovers it with the
// two-result form of a type assertion, never as a runtime error.
type Readback interface {
	StatusReader
	UsageReader
	ContextUsageReader
	ModelReader
	EffortReader
}
