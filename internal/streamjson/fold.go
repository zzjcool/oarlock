package streamjson

// The shared fold: one native stream-json frame in, the facts it states out.
// It is the parser claude and cursor share (issue #22 must reuse it, not write
// another): the protocol bookkeeping lives here, everything a vendor decides
// differently (usage, context, failure, tasks, service tier) is a Profile hook.
//
// The fold is a plain state machine over one frame at a time. The live pump and
// an offline replay call the same Fold, so they cannot disagree.

import (
	"sync"

	"github.com/zzjcool/oarlock"
)

// Profile is the vendor half of the fold. Hooks run in frame order; a hook may
// keep its own state. nil events mean "nothing to say".
type Profile interface {
	// Assistant sees every assistant frame; root is false inside a sub-agent.
	Assistant(m Rec, root bool)
	// Result reads a result frame. aborted is true when an abort was requested
	// since the turn began (a requested abort ends the turn aborted whatever
	// the result says). before/after wrap the turn_ended event.
	Result(m Rec, path []string, aborted bool) (outcome oarlock.TurnOutcome, before, after []oarlock.Event)
	// Init reads a system/init frame (model, service tier, ...).
	Init(m Rec, root bool) []oarlock.Event
	// Compacted sees a compact_boundary; root is false inside a sub-agent.
	Compacted(root bool)
	// System reads every other system/* frame.
	System(m Rec, path []string) []oarlock.Event
	// Other sees a frame of a type the fold does not know (rate_limit_event, ...).
	Other(m Rec)
}

// ControlResponse is the runtime answering one of OUR control_requests.
type ControlResponse struct {
	RequestID string
	OK        bool
	Error     string
}

// ControlRequest is the runtime asking US something (a permission, a question).
type ControlRequest struct {
	ID      string
	Subtype string
}

// Frame is the reading of one native frame.
type Frame struct {
	// Type is the discriminator: "assistant", "system/init", "result/success".
	Type string
	// Rec is the decoded frame.
	Rec Rec
	// AgentPath is the sub-agent lane; nil is the root.
	AgentPath []string
	// Events are the facts the frame states, in frame order.
	Events []oarlock.Event
	// Response is set for a control_response that names a request_id.
	Response *ControlResponse
	// Request is set for a control_request from the runtime.
	Request *ControlRequest
	// Cancelled is the request id of a control_cancel_request.
	Cancelled string
	// TurnEnded is true for a root result frame.
	TurnEnded bool
}

// known are the top-level types the fold reads; anything else is tallied.
var known = map[string]bool{
	"system": true, "assistant": true, "stream_event": true, "user": true,
	"result": true, "control_response": true, "control_request": true,
	"control_cancel_request": true, "rate_limit_event": true,
	// command_lifecycle is the runtime's own input receipt (msg_lifecycle_v1).
	// The adapter reads steer/prompt landing off the replayed user message, so
	// it is known and states nothing here.
	"command_lifecycle": true,
}

// Folder is the stream-json fold state of one process.
type Folder struct {
	mu       sync.Mutex // the control plane and the pump both reach the fold
	prof     Profile
	partials map[string]*partialMessage
	agents   map[string][]string
	unknown  map[string]int

	turnActive bool
	aborted    bool
}

// NewFolder returns a fold over prof.
func NewFolder(prof Profile) *Folder {
	return &Folder{
		prof:     prof,
		partials: map[string]*partialMessage{},
		agents:   map[string][]string{},
		unknown:  map[string]int{},
	}
}

// Prompted is the control plane telling the fold a prompt was written: the next
// root init belongs to it and is not a spontaneous turn.
func (f *Folder) Prompted() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turnActive, f.aborted = true, false
}

// AbortRequested is the control plane telling the fold an interrupt was sent:
// the turn that ends next ends aborted.
func (f *Folder) AbortRequested() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborted = true
}

// AbortCleared undoes AbortRequested: the runtime refused the interrupt, or the
// turn had already ended, so the next end is not an abort.
func (f *Folder) AbortCleared() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborted = false
}

// Do runs fn under the fold's lock. A vendor profile's state is only safe to
// touch from outside the pump (a resume baseline read at open) through Do.
func (f *Folder) Do(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// Unknown is a copy of the tally of top-level frame types the fold does not
// know. A non-zero tally is a test signal; the frames stay in Native.
func (f *Folder) Unknown() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int, len(f.unknown))
	for k, v := range f.unknown {
		out[k] = v
	}
	return out
}

func frameType(m Rec) string {
	t := Str(m["type"])
	if t == "" {
		t = "unknown"
	}
	if s := Str(m["subtype"]); s != "" {
		return t + "/" + s
	}
	return t
}

// attribution is the agent a frame belongs to: root, or the Task call that
// spawned it (nested through that call's own agent).
func (f *Folder) attribution(m Rec) []string {
	parent := Str(m["parent_tool_use_id"])
	if parent == "" {
		return nil
	}
	path := append([]string(nil), f.agents[parent]...)
	return append(path, parent)
}

// Fold reads one native frame. A frame that is not a JSON object yields a Frame
// with Type "unknown" and no events.
func (f *Folder) Fold(raw []byte) Frame {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := ParseRec(raw)
	if m == nil {
		f.unknown[""]++
		return Frame{Type: "unknown"}
	}
	path := f.attribution(m)
	root := len(path) == 0
	fr := Frame{Type: frameType(m), Rec: m, AgentPath: path}
	top := Str(m["type"])
	if !known[top] {
		f.unknown[top]++
	}
	switch top {
	case "assistant", "stream_event":
		fr.Events = f.Content(raw, m, path)
		f.RememberToolUses(raw, m, path)
		if top == "assistant" {
			f.prof.Assistant(m, root)
		}
	case "user":
		fr.Events = ToolResults(raw)
		body := AsRec(m["message"])
		if replay, _ := m["isReplay"].(bool); replay {
			if id := Str(m["uuid"]); id != "" {
				if parts, ok := body["content"].([]any); ok {
					text := ""
					for _, p := range parts {
						if pr := AsRec(p); Str(pr["type"]) == "text" {
							text += Str(pr["text"])
						}
					}
					fr.Events = append(fr.Events, oarlock.Event{
						Kind: oarlock.EventUserMessage, Text: text, InputID: id,
						MessageID: id, NativeMessageID: id, Evidence: oarlock.EvidenceAcknowledged,
					})
				}
			}
		}
	case "result":
		outcome, before, after := f.prof.Result(m, path, f.aborted && root)
		fr.Events = append(fr.Events, before...)
		fr.Events = append(fr.Events, oarlock.Event{Kind: oarlock.EventTurnEnded, Outcome: &outcome})
		fr.Events = append(fr.Events, after...)
		if root {
			f.turnActive, f.aborted, fr.TurnEnded = false, false, true
		}
	case "system":
		switch Str(m["subtype"]) {
		case "compact_boundary":
			ev := oarlock.Event{Kind: oarlock.EventCompactionEnded, CompactionResult: oarlock.CompactionCompleted}
			if t := Str(AsRec(m["compact_metadata"])["trigger"]); t != "" {
				ev.Trigger = t
			}
			fr.Events = []oarlock.Event{ev}
			f.prof.Compacted(root)
		case "init":
			if root && !f.turnActive {
				// A turn nobody prompted (a queued input or task notification).
				fr.Events = append(fr.Events, oarlock.Event{Kind: oarlock.EventTurnActive})
			}
			if root {
				f.turnActive = true
			}
			if model := Str(m["model"]); model != "" {
				fr.Events = append(fr.Events, oarlock.Event{Kind: oarlock.EventModel, Model: model})
			}
			fr.Events = append(fr.Events, f.prof.Init(m, root)...)
		default:
			fr.Events = f.prof.System(m, path)
		}
	case "control_response":
		resp := AsRec(m["response"])
		if id := Str(resp["request_id"]); id != "" {
			cr := &ControlResponse{RequestID: id, OK: Str(resp["subtype"]) != "error" && Str(resp["error"]) == ""}
			if !cr.OK {
				cr.Error = Str(resp["error"])
				if cr.Error == "" {
					cr.Error = "control request failed"
				}
			}
			fr.Response = cr
		}
	case "control_request":
		id := Str(m["request_id"])
		sub := Str(AsRec(m["request"])["subtype"])
		if sub == "" {
			sub = "control_request"
		}
		fr.Request = &ControlRequest{ID: id, Subtype: sub}
	case "control_cancel_request":
		fr.Cancelled = Str(m["request_id"])
		if fr.Cancelled != "" {
			fr.Events = []oarlock.Event{{Kind: oarlock.EventAppRequestCancelled, RequestID: fr.Cancelled}}
		}
	default:
		f.prof.Other(m)
	}
	return fr
}
