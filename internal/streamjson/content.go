package streamjson

// Content projection: assistant / stream_event / user frames to text, reasoning
// and tool events. A pure fold step over the projector's partial-message state.
//
// `--include-partial-messages` makes claude send a message twice: first as
// stream_event deltas, then as one completed `assistant` frame per block. The
// completed frame must not repeat what the deltas already said, so the deltas
// are remembered per agent lane and the completed block contributes only the
// part of its text the deltas did not carry (OAR shared/stream-json/content.ts,
// the 2.1.293 ordering: a completed block arrives just before its
// content_block_stop, every block of one API message carrying the same id).

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/zzjcool/oarlock"
)

type blockKind int

const (
	blockText blockKind = iota + 1
	blockThinking
	blockTool
)

type partialBlock struct {
	index     int
	kind      blockKind
	text      string
	finalized bool
	callID    string
}

// partialMessage is the unfinished API message of one agent lane.
type partialMessage struct {
	id     string
	hasID  bool
	blocks []*partialBlock // insertion order, like the reference's Map
}

func (m *partialMessage) block(index int) *partialBlock {
	for _, b := range m.blocks {
		if b.index == index {
			return b
		}
	}
	return nil
}

func (m *partialMessage) set(b partialBlock) {
	if old := m.block(b.index); old != nil {
		*old = b
		return
	}
	nb := b
	m.blocks = append(m.blocks, &nb)
}

// contentBlock is one block of a message's content array.
type contentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Text      *string         `json:"text"`
	Thinking  *string         `json:"thinking"`
	Input     json.RawMessage `json:"input"`
	ToolUseID *string         `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   *bool           `json:"is_error"`
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`
}

type messageFrame struct {
	Message struct {
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// contentBlocks reads message.content when it is an array.
func contentBlocks(raw []byte) (id string, blocks []contentBlock) {
	var f messageFrame
	if json.Unmarshal(raw, &f) != nil {
		return "", nil
	}
	c := bytes.TrimSpace(f.Message.Content)
	if len(c) == 0 || c[0] != '[' {
		return f.Message.ID, nil
	}
	var raws []json.RawMessage
	if json.Unmarshal(c, &raws) != nil {
		return f.Message.ID, nil
	}
	for _, r := range raws {
		var b contentBlock
		if json.Unmarshal(r, &b) == nil {
			blocks = append(blocks, b)
		}
	}
	return f.Message.ID, blocks
}

func laneKey(path []string) string {
	b, _ := json.Marshal(path)
	return string(b)
}

func textEvent(kind blockKind, text, id string) oarlock.Event {
	if kind == blockText {
		return oarlock.Event{Kind: oarlock.EventTextDelta, Text: text, MessageID: id}
	}
	return oarlock.Event{Kind: oarlock.EventReasoning, Text: text, MessageID: id}
}

// streamEvent reads one stream_event's inner event.
func (p *Folder) streamEvent(lane string, event Rec) []oarlock.Event {
	typ := Str(event["type"])
	prev := p.partials[lane]
	if typ == "message_start" {
		id, hasID := AsStr(AsRec(event["message"])["id"])
		p.partials[lane] = &partialMessage{id: id, hasID: hasID}
		return nil
	}
	idxF, ok := AsNum(event["index"])
	if !ok {
		return nil
	}
	index := int(idxF)
	var body Rec
	switch typ {
	case "content_block_start":
		body = AsRec(event["content_block"])
	case "content_block_delta":
		body = AsRec(event["delta"])
	}
	msg := prev
	if msg == nil {
		msg = &partialMessage{}
	}
	btype := Str(body["type"])

	if id, ok1 := AsStr(body["id"]); btype == "tool_use" && ok1 {
		if name, ok2 := AsStr(body["name"]); ok2 {
			prior := msg.block(index)
			msg.set(partialBlock{index: index, kind: blockTool, callID: id})
			p.partials[lane] = msg
			if prior != nil && prior.kind == blockTool && prior.callID == id {
				return nil
			}
			return []oarlock.Event{{Kind: oarlock.EventToolCallStarted, CallID: id, Tool: name}}
		}
	}
	if pj, ok1 := AsStr(body["partial_json"]); btype == "input_json_delta" && ok1 {
		if tool := msg.block(index); tool != nil && tool.kind == blockTool {
			return []oarlock.Event{{Kind: oarlock.EventToolCallInputDelta, CallID: tool.callID, Delta: pj}}
		}
		return nil
	}
	var kind blockKind
	var field string
	switch btype {
	case "text", "text_delta":
		kind, field = blockText, "text"
	case "thinking", "thinking_delta":
		kind, field = blockThinking, "thinking"
	default:
		return nil
	}
	text := Str(body[field])
	var prior *partialBlock
	if typ != "content_block_start" {
		prior = msg.block(index)
	}
	carried := ""
	if prior != nil && prior.kind == kind {
		carried = prior.text
	}
	msg.set(partialBlock{index: index, kind: kind, text: carried + text})
	p.partials[lane] = msg
	if text == "" {
		return nil
	}
	return []oarlock.Event{textEvent(kind, text, msg.id)}
}

// finalContent reads a completed assistant frame against the partials of its
// lane: only what the deltas did not already say becomes an event.
func (p *Folder) finalContent(lane string, raw []byte) []oarlock.Event {
	id, blocks := contentBlocks(raw)
	partial := p.partials[lane]
	remainder := func(kind blockKind, full string) (string, bool) {
		if id == "" || partial == nil || !partial.hasID || partial.id != id {
			return full, true
		}
		var pending *partialBlock
		for _, b := range partial.blocks {
			if b.kind == kind && !b.finalized {
				pending = b
				break
			}
		}
		if pending == nil {
			return full, true
		}
		pending.finalized = true
		if pending.text == "" || !strings.HasPrefix(full, pending.text) {
			return full, true
		}
		suffix := full[len(pending.text):]
		if suffix == "" {
			return "", false
		}
		return suffix, true
	}
	var events []oarlock.Event
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text == nil {
				continue
			}
			if t, ok := remainder(blockText, *b.Text); ok {
				events = append(events, textEvent(blockText, t, id))
			}
		case "thinking":
			full := ""
			if b.Thinking != nil {
				full = *b.Thinking
			}
			if t, ok := remainder(blockThinking, full); ok {
				events = append(events, textEvent(blockThinking, t, id))
			}
		case "redacted_thinking":
			events = append(events, oarlock.Event{Kind: oarlock.EventReasoning, Redacted: true, MessageID: id})
		case "tool_use":
			input := compactJSON(b.Input)
			started := false
			if partial != nil && (!partial.hasID || partial.id == id) {
				for _, pb := range partial.blocks {
					if pb.kind == blockTool && pb.callID == b.ID {
						started = true
					}
				}
			}
			if started {
				if input != "" {
					events = append(events, oarlock.Event{Kind: oarlock.EventToolCallInput, CallID: b.ID, Input: input})
				}
				continue
			}
			call := b.ID
			if call == "" {
				call = "unknown"
			}
			name := b.Name
			if name == "" {
				name = "unknown"
			}
			events = append(events, oarlock.Event{Kind: oarlock.EventToolCallStarted, CallID: call, Tool: name, Input: input})
		}
	}
	return events
}

// compactJSON is JSON.stringify of an already-decoded value: the same document
// without insignificant whitespace, key order untouched. "" when absent.
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}

// content is the content projection of one assistant or stream_event frame.
func (p *Folder) Content(raw []byte, m Rec, path []string) []oarlock.Event {
	lane := laneKey(path)
	if Str(m["type"]) == "stream_event" {
		event := AsRec(m["event"])
		if Str(event["type"]) == "message_stop" {
			// Every completed block precedes this boundary; only this agent is done.
			delete(p.partials, lane)
			return nil
		}
		return p.streamEvent(lane, event)
	}
	return p.finalContent(lane, raw)
}

// rememberToolUses records which agent issued each tool call, so a frame whose
// parent_tool_use_id names it is attributed to that call's agent path.
func (p *Folder) RememberToolUses(raw []byte, m Rec, path []string) {
	if Str(m["type"]) == "stream_event" {
		event := AsRec(m["event"])
		if Str(event["type"]) == "content_block_start" {
			cb := AsRec(event["content_block"])
			if id, ok := AsStr(cb["id"]); ok && Str(cb["type"]) == "tool_use" {
				p.agents[id] = path
			}
		}
		return
	}
	_, blocks := contentBlocks(raw)
	for _, b := range blocks {
		if b.Type == "tool_use" && b.ID != "" {
			p.agents[b.ID] = path
		}
	}
}

// toolResults reads every tool_result block of a user frame.
func ToolResults(raw []byte) []oarlock.Event {
	_, blocks := contentBlocks(raw)
	var events []oarlock.Event
	for _, b := range blocks {
		if b.Type != "tool_result" || b.ToolUseID == nil {
			continue
		}
		// is_error is optional and false by default: an absent field is the
		// protocol's own "no error" (claude 2.1.288 leaves it out of a
		// successful Read, Write or Edit).
		result := oarlock.ToolOK
		if b.IsError != nil && *b.IsError {
			result = oarlock.ToolFailed
		}
		events = append(events, oarlock.Event{
			Kind:    oarlock.EventToolCallEnded,
			CallID:  *b.ToolUseID,
			Content: toolContent(b.Content),
			Result:  result,
		})
	}
	return events
}

// toolContent reads a tool_result's content: a string is one text part, an
// array is its blocks in order, anything else one "other" part, and an absent
// value, null or an empty array is no content.
func toolContent(raw json.RawMessage) []oarlock.ToolOutputPart {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return []oarlock.ToolOutputPart{{Type: "text", Text: s}}
		}
	case '[':
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
			return nil
		}
		parts := make([]oarlock.ToolOutputPart, 0, len(items))
		for _, it := range items {
			parts = append(parts, toolOutputPart(it))
		}
		return parts
	}
	return []oarlock.ToolOutputPart{{Type: "other", Value: append(json.RawMessage(nil), raw...)}}
}

func toolOutputPart(raw json.RawMessage) oarlock.ToolOutputPart {
	var b contentBlock
	if json.Unmarshal(raw, &b) == nil {
		if b.Type == "text" && b.Text != nil {
			return oarlock.ToolOutputPart{Type: "text", Text: *b.Text}
		}
		if b.Type == "image" && b.Source != nil && b.Source.Type == "base64" && b.Source.Data != "" && b.Source.MediaType != "" {
			return oarlock.ToolOutputPart{Type: "image", MediaType: b.Source.MediaType, Data: b.Source.Data}
		}
	}
	return oarlock.ToolOutputPart{Type: "other", Value: append(json.RawMessage(nil), raw...)}
}
