package fold

import (
	"strings"

	"github.com/zzjcool/oarlock"
)

// conversationState is the message fold: messages in first-appearance order
// plus the index that finds a tool call's message by (agent path, call id).
type conversationState struct {
	messages []Message
	tools    map[string]int // toolKey(agentPath, callID) → position in messages
}

// foldConversation applies one fed record to the message fold. Only
// root-session records fold; a derived child session carries its own
// transcript. A record's agent path is copied onto its messages, so a
// sub-agent's words stay attributable without ending or interrupting the root
// turn's messages.
func (f *Fold) foldConversation(r oarlock.Record, root bool) {
	if !root {
		return
	}
	for _, ev := range r.Events {
		switch ev.Kind {
		case oarlock.EventUserMessage:
			f.conv.messages = append(f.conv.messages, Message{
				Role:            RoleUser,
				Seq:             r.Seq,
				EndSeq:          r.Seq,
				AgentPath:       clonePath(r.AgentPath),
				Text:            ev.Text,
				MessageID:       ev.MessageID,
				NativeMessageID: ev.NativeMessageID,
				TurnID:          ev.TurnID,
				Evidence:        ev.Evidence,
			})
		case oarlock.EventTextDelta:
			f.appendText(r, ev, RoleAssistant)
		case oarlock.EventReasoning:
			f.appendText(r, ev, RoleReasoning)
		case oarlock.EventToolCallStarted, oarlock.EventToolCallInput,
			oarlock.EventToolCallInputDelta, oarlock.EventToolCallProgress,
			oarlock.EventToolCallEnded:
			f.foldTool(r, ev)
		}
	}
}

// Conversation answers the root session's messages in first-appearance order.
// The slice and every pointer, slice and raw value inside it is a copy.
func (f *Fold) Conversation() QueryResult[[]Message] {
	var messages []Message
	if len(f.conv.messages) > 0 {
		messages = make([]Message, len(f.conv.messages))
		for i, message := range f.conv.messages {
			messages[i] = cloneMessage(message)
		}
	}
	return QueryResult[[]Message]{Value: messages, Seq: f.seq}
}

// appendText folds a text_delta or reasoning event into the previous message
// when that message is the same kind of text for the same agent under the same
// non-empty MessageID, and appends a new message otherwise. Nothing between the
// two events may have produced a message of its own: a tool call that started
// in between ends the run.
func (f *Fold) appendText(r oarlock.Record, ev oarlock.Event, role Role) {
	if n := len(f.conv.messages); n > 0 && ev.MessageID != "" {
		previous := &f.conv.messages[n-1]
		if previous.Role == role && previous.MessageID == ev.MessageID && samePath(previous.AgentPath, r.AgentPath) {
			previous.Text += ev.Text
			previous.EndSeq = r.Seq
			if ev.Redacted {
				// Redaction evidence survives a merge: part of the message was
				// withheld even if the later fragment carries text.
				previous.Redacted = true
			}
			return
		}
	}
	message := Message{
		Role:            role,
		Seq:             r.Seq,
		EndSeq:          r.Seq,
		AgentPath:       clonePath(r.AgentPath),
		Text:            ev.Text,
		MessageID:       ev.MessageID,
		NativeMessageID: ev.NativeMessageID,
		TurnID:          ev.TurnID,
		Redacted:        ev.Redacted,
	}
	f.conv.messages = append(f.conv.messages, message)
}

// foldTool folds one tool lifecycle event into the message of its
// (AgentPath, CallID) — not the message next to it, because a runtime may report
// a call's last output after other messages appeared. The first event of a call
// creates its message; later ones update it where it already sits.
func (f *Fold) foldTool(r oarlock.Record, ev oarlock.Event) {
	key := toolKey(r.AgentPath, ev.CallID)
	at, ok := f.conv.tools[key]
	if !ok {
		if f.conv.tools == nil {
			f.conv.tools = make(map[string]int)
		}
		f.conv.messages = append(f.conv.messages, Message{
			Role:      RoleTool,
			Seq:       r.Seq,
			EndSeq:    r.Seq,
			AgentPath: clonePath(r.AgentPath),
			CallID:    ev.CallID,
		})
		at = len(f.conv.messages) - 1
		f.conv.tools[key] = at
	}
	message := &f.conv.messages[at]
	message.EndSeq = r.Seq
	switch ev.Kind {
	case oarlock.EventToolCallStarted:
		message.Tool = ev.Tool
		message.Input = ev.Input
	case oarlock.EventToolCallInput:
		// The complete arguments replace the streamed fragments.
		message.Input = ev.Input
		message.InputPartial = false
	case oarlock.EventToolCallInputDelta:
		message.Input += ev.Delta
		message.InputPartial = true
	case oarlock.EventToolCallProgress:
		// A snapshot replaces the preview, a delta appends to it.
		if ev.Output != "" {
			message.Output = ev.Output
		}
		message.Output += ev.OutputDelta
	case oarlock.EventToolCallEnded:
		message.Result = ev.Result
		message.Content = cloneParts(ev.Content)
		message.ExitCode = cloneInt(ev.ExitCode)
		message.ExitCodeReported = ev.ExitCodeReported
		// The result gives way to the streamed preview, but only when it holds
		// text at all: an image-only or unrecognized result says nothing about
		// the preview it replaces.
		if text, ok := toolText(message.Content); ok {
			message.Output = text
		}
	}
}

// toolKey identifies a tool call the way the record stream does: the composite
// (AgentPath, CallID), never a bare call id.
func toolKey(path []string, callID string) string {
	parts := make([]string, 0, len(path)+1)
	parts = append(parts, path...)
	parts = append(parts, callID)
	return partsKey(parts...)
}

// toolText is a tool result as text: its text parts joined by newlines, present
// only when the result has a text part at all. Render Content itself to show
// images.
func toolText(content []oarlock.ToolOutputPart) (string, bool) {
	var texts []string
	for _, part := range content {
		if part.Type == "text" {
			texts = append(texts, part.Text)
		}
	}
	if len(texts) == 0 {
		return "", false
	}
	return strings.Join(texts, "\n"), true
}
