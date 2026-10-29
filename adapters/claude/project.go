package claude

import (
	"encoding/json"
	"time"

	"github.com/zzjcool/oarlock"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

// profile is Claude's half of the shared fold (internal/streamjson.Profile):
// result classification, usage, context, failure facts and task frames.
// Only the pump goroutine drives it (through the Folder, which locks), so it
// holds plain fields.
type profile struct {
	tokens        map[string]oarlock.TokenTotals // per agent lane: the root's main loop
	usage         modelUsage
	context       contextReading
	failureCat    string
	limitResetsAt *time.Time
	taskDescs     map[string]string
}

func newProfile(resumed bool) *profile {
	p := &profile{tokens: map[string]oarlock.TokenTotals{}, taskDescs: map[string]string{}}
	if resumed {
		p.usage = resumedModelUsage()
	} else {
		p.usage = freshModelUsage()
	}
	return p
}

var _ sj.Profile = (*profile)(nil)

func (p *profile) Assistant(m sj.Rec, root bool) {
	if !root {
		return
	}
	if e, ok := sj.AsStr(m["error"]); ok {
		p.failureCat = e
	}
	p.context.sent(m)
}

func (p *profile) Compacted(root bool) {
	if root {
		p.context.compacted()
	}
}

func (p *profile) Init(m sj.Rec, root bool) []oarlock.Event { return tierEvents(m) }

func (p *profile) Other(m sj.Rec) {
	if sj.Str(m["type"]) == "rate_limit_event" {
		p.limitResetsAt = limitReset(sj.AsRec(m["rate_limit_info"]))
	}
}

// tierEvents is claude's native fast-mode status, including a temporary
// downgrade during cooldown.
func tierEvents(m sj.Rec) []oarlock.Event {
	switch sj.Str(m["fast_mode_state"]) {
	case "on":
		return []oarlock.Event{{Kind: oarlock.EventServiceTier, ServiceTier: "fast"}}
	case "off", "cooldown":
		return []oarlock.Event{{Kind: oarlock.EventServiceTier, ServiceTier: "default"}}
	}
	return nil
}

func (p *profile) Result(m sj.Rec, path []string, aborted bool) (oarlock.TurnOutcome, []oarlock.Event, []oarlock.Event) {
	// The per-agent figure folds before the outcome so the usage event, which
	// follows turn_ended, carries this turn's tokens.
	var agentTokens *oarlock.TokenTotals
	if p.usage.kind == baselineKnown {
		if t := mainLoop(m); t != nil {
			key := laneOf(path)
			sum := addTokens(p.tokens[key], *t)
			p.tokens[key] = sum
			agentTokens = &sum
		}
	}
	total := p.usage.fold(m)
	ctx := p.context.atResult(m)
	var after []oarlock.Event
	if agentTokens != nil || ctx != nil || total != nil {
		after = []oarlock.Event{{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{Context: ctx, Tokens: agentTokens, Total: total}}}
	}
	outcome := p.outcome(m, aborted)
	p.failureCat = ""
	return outcome, tierEvents(m), after
}

func laneOf(path []string) string {
	b, _ := json.Marshal(path)
	return string(b)
}

// outcome is the turn's end. A requested abort wins; then is_error — checked
// BEFORE subtype, because claude can report is_error=true with subtype
// "success" and put the actual error text in result (vendor quirk, pinned
// 2026-08-22).
func (p *profile) outcome(m sj.Rec, aborted bool) oarlock.TurnOutcome {
	if aborted {
		return oarlock.TurnOutcome{Kind: oarlock.OutcomeAborted}
	}
	if isErr, _ := m["is_error"].(bool); isErr {
		text := sj.Str(m["result"])
		subtype := sj.Str(m["subtype"])
		if subtype == "success" {
			subtype = ""
		}
		if text == "" {
			text = resultReason(subtype, m["errors"])
		}
		status := 0
		if v, ok := sj.AsNum(m["api_error_status"]); ok {
			status = int(v)
		}
		return *classify(text, failureFacts{
			category:       p.failureCat,
			status:         status,
			terminalReason: sj.Str(m["terminal_reason"]),
			subtype:        subtype,
			limitResetsAt:  p.limitResetsAt,
		})
	}
	out := oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted}
	if so, ok := m["structured_output"]; ok {
		if b, err := json.Marshal(so); err == nil {
			out.StructuredOutput = b
		}
	}
	return out
}

// ---- system frames: tasks -------------------------------------------------------

var taskTypes = map[string]string{
	"local_bash": "shell", "local_agent": "agent", "remote_agent": "agent",
	"in_process_teammate": "agent", "mcp_task": "tool", "local_workflow": "workflow",
}

var taskStatuses = map[string]oarlock.TaskStatus{
	"pending": oarlock.TaskPending, "running": oarlock.TaskRunning, "paused": oarlock.TaskPaused,
	"completed": oarlock.TaskCompleted, "failed": oarlock.TaskFailed,
	"killed": oarlock.TaskStopped, "stopped": oarlock.TaskStopped,
}

func nonEmpty(v any) string {
	s, _ := v.(string)
	return s
}

// System reads claude's task frames (system/task_started, task_updated,
// task_progress, task_notification) and its service tier. Other system frames
// (status, thinking_tokens, background_tasks_changed, ...) stay in Native.
func (p *profile) System(m sj.Rec, path []string) []oarlock.Event {
	events := p.taskEvents(m, path)
	return append(events, tierEvents(m)...)
}

func (p *profile) taskEvents(m sj.Rec, path []string) []oarlock.Event {
	id := nonEmpty(m["task_id"])
	if id == "" {
		return nil
	}
	key := laneOf(append(append([]string(nil), path...), id))
	switch sj.Str(m["subtype"]) {
	case "task_started":
		te := &oarlock.TaskEvent{TaskID: id, TaskType: "other"}
		if nt := nonEmpty(m["task_type"]); nt != "" {
			te.NativeType = nt
			if t, ok := taskTypes[nt]; ok {
				te.TaskType = t
			}
		}
		te.Description = nonEmpty(m["description"])
		te.ToolCallID = nonEmpty(m["tool_use_id"])
		te.Background, _ = m["is_backgrounded"].(bool)
		te.Ambient, _ = m["ambient"].(bool)
		if te.Description != "" {
			p.taskDescs[key] = te.Description
		}
		return []oarlock.Event{{Kind: oarlock.EventTaskStarted, Task: te}}
	case "task_updated":
		patch := sj.AsRec(m["patch"])
		te := &oarlock.TaskEvent{TaskID: id, Status: taskStatuses[sj.Str(patch["status"])],
			Description: nonEmpty(patch["description"]), Error: nonEmpty(patch["error"])}
		te.Background, _ = patch["is_backgrounded"].(bool)
		if te.Description != "" {
			p.taskDescs[key] = te.Description
		}
		return []oarlock.Event{{Kind: oarlock.EventTaskUpdated, Task: te}}
	case "task_progress":
		d, ok := m["description"].(string)
		if !ok || d == p.taskDescs[key] {
			return nil
		}
		p.taskDescs[key] = d
		return []oarlock.Event{{Kind: oarlock.EventTaskUpdated, Task: &oarlock.TaskEvent{TaskID: id, Description: d}}}
	case "task_notification":
		st, ok := taskStatuses[sj.Str(m["status"])]
		if !ok || (st != oarlock.TaskCompleted && st != oarlock.TaskFailed && st != oarlock.TaskStopped) {
			return nil
		}
		return []oarlock.Event{{Kind: oarlock.EventTaskEnded, Task: &oarlock.TaskEvent{
			TaskID: id, Status: st, Summary: nonEmpty(m["summary"]), OutputFile: nonEmpty(m["output_file"])}}}
	}
	return nil
}

// ---- command_lifecycle: an input claude dropped ---------------------------------

// lifecycleEvents reads claude's own receipt of one input (msg_lifecycle_v1): a
// cancelled command_uuid is an input claude dropped without ever running it, so
// the event names that input and hands it back to the caller (resend is the
// right move). Only the root lane can end the root's span, so a child lane's
// frame states nothing here. The live pump and a replay both call this, so they
// cannot disagree.
func lifecycleEvents(m sj.Rec, root bool) []oarlock.Event {
	if !root || sj.Str(m["type"]) != "command_lifecycle" || sj.Str(m["state"]) != lifecycleCancelled {
		return nil
	}
	uuid := sj.Str(m["command_uuid"])
	if uuid == "" {
		return nil
	}
	return []oarlock.Event{{Kind: oarlock.EventInputDropped, InputID: uuid, Dropped: oarlock.DroppedTurnInterrupted}}
}

// Project is the pure replay entry point: it folds raw native frames, in order,
// into the events each states. The live session and an offline replay use the
// same profile and Folder, so they cannot disagree. resumed selects the usage
// baseline policy (a resumed stream reports no total without a baseline).
func Project(frames [][]byte, resumed bool) []sj.Frame {
	f := sj.NewFolder(newProfile(resumed))
	out := make([]sj.Frame, 0, len(frames))
	for _, raw := range frames {
		fr := f.Fold(raw)
		// The same command_lifecycle projection the live pump appends, so the two
		// paths state the same facts for the same frame.
		fr.Events = append(fr.Events, lifecycleEvents(fr.Rec, len(fr.AgentPath) == 0)...)
		out = append(out, fr)
	}
	return out
}
