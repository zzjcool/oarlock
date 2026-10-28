package fold

import "github.com/zzjcool/oarlock"

// foldStatus applies one fed record to the status reducer. Only a root record
// (root session, root agent) can move the root's status: a child agent's turn
// events, and a child session's records, never do.
func (f *Fold) foldStatus(r oarlock.Record, root bool) {
	if f.status.Kind == "" {
		f.status = Status{Kind: StatusIdle}
	}
	if !root {
		return
	}
	f.status = reduceStatus(f.status, r)
}

// Status answers the root agent's status. The value is a copy: mutating it
// cannot reach the fold.
func (f *Fold) Status() QueryResult[Status] {
	status := cloneStatus(f.status)
	if status.Kind == "" {
		status.Kind = StatusIdle
	}
	return QueryResult[Status]{Value: status, Seq: f.seq}
}

// reduceStatus is the status fold: a pure function of the previous status and
// one root record.
//
// Transition table:
//
//	request prompt (toRuntime)        → running/waiting_model, request-based start
//	response rejected → that request  → idle (the turn never began)
//	response exited                   → exited, final for the rest of the stream
//	event turn_active                 → running/waiting_model (adopts a mid-turn subscribe)
//	event reasoning                   → running/thinking
//	event text_delta                  → running/responding
//	event tool_call_started           → running/{tool, callId}
//	event tool_call_input_delta       → that call's phase, writing
//	event tool_call_input             → that call's phase, arguments complete
//	event tool_call_progress          → phase unchanged
//	event tool_call_ended, retry      → running/waiting_model
//	event compaction_started          → running/compacting
//	event compaction_ended            → running/waiting_model only when running
//	event turn_ended                  → idle{lastTurnOutcome}
//	event input_dropped → own input   → idle
//
// Everything else changes nothing. The fold is total: a mid-turn event while
// idle adopts that turn, because a consumer may subscribe mid-turn and a queued
// input runs as a turn of its own with no request recorded.
func reduceStatus(previous Status, r oarlock.Record) Status {
	if previous.Kind == StatusExited {
		// Exit is final: no late frame, no request and no response reopens it.
		return previous
	}
	switch r.Kind {
	case oarlock.KindRequest:
		previous = reduceRequest(previous, r)
	case oarlock.KindResponse:
		previous = reduceResponse(previous, r)
	}
	for _, ev := range r.Events {
		previous = reduceEvent(previous, r.Seq, ev)
	}
	return previous
}

// reduceRequest opens a turn: only a toRuntime prompt, and only from idle. A
// prompt recorded while running never reopens or restarts the running span.
func reduceRequest(previous Status, r oarlock.Record) Status {
	if previous.Kind != StatusIdle || r.Direction != oarlock.ToRuntime || r.Request == nil {
		return previous
	}
	if r.Request.Action != oarlock.ActionPrompt {
		return previous
	}
	return Status{
		Kind:      StatusRunning,
		Phase:     Phase{Kind: PhaseWaitingModel},
		SinceSeq:  r.Seq,
		RequestID: r.ID,
		InputID:   r.Request.InputID,
	}
}

// reduceResponse answers the current span. A rejection of the request that
// opened this span means the turn never began; any other response changes
// nothing. An exit is final: whatever phase the turn was in stays readable and
// the turn's start stays in SinceSeq.
func reduceResponse(previous Status, r oarlock.Record) Status {
	if r.Response == nil {
		return previous
	}
	switch r.Response.Kind {
	case oarlock.ResponseRejected:
		if previous.Kind == StatusRunning && previous.RequestID != "" && r.RequestID == previous.RequestID {
			return Status{Kind: StatusIdle}
		}
	case oarlock.ResponseExited:
		previous.Kind = StatusExited
		return previous
	}
	return previous
}

// reduceEvent applies one frame event of a root record, in frame order.
func reduceEvent(previous Status, seq uint64, ev oarlock.Event) Status {
	switch ev.Kind {
	case oarlock.EventTurnActive:
		if previous.Kind == StatusRunning {
			previous.Phase = Phase{Kind: PhaseWaitingModel}
			return previous
		}
		// An adopted turn: no recorded prompt of ours started it, so RequestID
		// and InputID stay empty and the adopted turn starts at this record.
		return Status{Kind: StatusRunning, Phase: Phase{Kind: PhaseWaitingModel}, SinceSeq: seq}
	case oarlock.EventReasoning:
		return withPhase(previous, seq, Phase{Kind: PhaseThinking})
	case oarlock.EventTextDelta:
		return withPhase(previous, seq, Phase{Kind: PhaseResponding})
	case oarlock.EventToolCallStarted:
		return withPhase(previous, seq, Phase{Kind: PhaseTool, Tool: ev.Tool, CallID: ev.CallID})
	case oarlock.EventToolCallInputDelta:
		return writing(previous, ev.CallID, true)
	case oarlock.EventToolCallInput:
		return writing(previous, ev.CallID, false)
	case oarlock.EventToolCallProgress:
		// The clock moves in the reference's view; a progress report says
		// nothing new about the phase.
		return previous
	case oarlock.EventToolCallEnded, oarlock.EventRetry:
		return withPhase(previous, seq, Phase{Kind: PhaseWaitingModel})
	case oarlock.EventCompactionStarted:
		return withPhase(previous, seq, Phase{Kind: PhaseCompacting})
	case oarlock.EventCompactionEnded:
		if previous.Kind != StatusRunning {
			return previous
		}
		return withPhase(previous, seq, Phase{Kind: PhaseWaitingModel})
	case oarlock.EventTurnEnded:
		return Status{Kind: StatusIdle, LastTurnOutcome: cloneTurnOutcome(ev.Outcome)}
	case oarlock.EventInputDropped:
		if previous.Kind == StatusRunning && previous.InputID != "" && previous.InputID == ev.InputID {
			return Status{Kind: StatusIdle}
		}
	case oarlock.EventInputQueued, oarlock.EventUserMessage, oarlock.EventUsage,
		oarlock.EventModel, oarlock.EventEffort, oarlock.EventServiceTier,
		oarlock.EventSessionLink, oarlock.EventAppRequestCancelled,
		oarlock.EventTaskStarted, oarlock.EventTaskUpdated, oarlock.EventTaskEnded:
		// Not phase facts: the status is unchanged.
	}
	return previous
}

// withPhase moves the status into a running phase. A running span keeps its
// start, its request and its input; an idle status adopts the turn at seq.
func withPhase(previous Status, seq uint64, phase Phase) Status {
	if previous.Kind == StatusRunning {
		previous.Phase = phase
		return previous
	}
	return Status{Kind: StatusRunning, Phase: phase, SinceSeq: seq}
}

// writing flips the streaming flag of one tool call's phase. Only the current
// call's phase changes: another call's arguments never touch it.
func writing(previous Status, callID string, writing bool) Status {
	if previous.Kind != StatusRunning || previous.Phase.Kind != PhaseTool || previous.Phase.CallID != callID {
		return previous
	}
	previous.Phase.Writing = writing
	return previous
}
