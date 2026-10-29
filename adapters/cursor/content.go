package cursor

// Cursor's own assistant content projection (sj.ContentReader). The shared one
// (internal/streamjson/content.go) is claude's: it keys a completed block
// against the deltas of the SAME message id, because claude sends every message
// twice (stream_event deltas, then the final block). Cursor sends no message
// ids at all — with --stream-partial-output its assistant frames are a series
// of pieces followed by one cumulative snapshot of the whole turn — so the
// shared projector has nothing to match on and would repeat the cumulative
// text. Hence a per-lane accumulator here, and the fold calls this instead of
// Content + RememberToolUses for cursor's stream.

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/zzjcool/oarlock"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

// contentBlock is one block of an assistant frame's message.content. Pointer
// fields keep "absent" apart from "stated empty": a block that states no text
// states no text event.
type contentBlock struct {
	Type     string          `json:"type"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Text     *string         `json:"text"`
	Thinking *string         `json:"thinking"`
	Input    json.RawMessage `json:"input"`
}

// text reports whether this block carries assistant text. `output_text` is the
// name cursor-agent uses for the same thing in some payloads (multica's
// production cursor handler reads both; testdata/cursor carries only `text`).
func (b contentBlock) text() bool { return b.Type == "text" || b.Type == "output_text" }

// assistantBlocks reads message.content when it is an array of blocks. A
// non-object frame or a string content states no blocks: nothing is invented.
func assistantBlocks(raw []byte) []contentBlock {
	var f struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return nil
	}
	c := bytes.TrimSpace(f.Message.Content)
	if len(c) == 0 || c[0] != '[' {
		return nil
	}
	var raws []json.RawMessage
	if json.Unmarshal(c, &raws) != nil {
		return nil
	}
	blocks := make([]contentBlock, 0, len(raws))
	for _, r := range raws {
		var b contentBlock
		if json.Unmarshal(r, &b) == nil {
			blocks = append(blocks, b)
		}
	}
	return blocks
}

// contentLane names the accumulator of one agent lane (nil path is the root).
func contentLane(path []string) string {
	b, _ := json.Marshal(path)
	return string(b)
}

// advance is cursor's assistant-text accumulator: given the lane and the text
// this frame states, it returns the part that was not emitted yet and records
// it. The rules, pinned on testdata/cursor (both fixtures, 2026-10-28):
//
//  1. Frames are snapshots of the turn's assistant text with no message id, so
//     "is this the same message" cannot be answered from the frame; the lane
//     keeps the text already emitted for the turn instead.
//  2. A snapshot that extends what was emitted (the emitted text is a prefix of
//     it) emits only the new suffix: that is both the first piece of a message
//     and its cumulative frame, which repeats the whole turn. An identical
//     snapshot emits nothing.
//  3. A snapshot that is a prefix of what was emitted states only text already
//     emitted (a retraction/rewind of a partial message) and emits nothing.
//  4. A snapshot that is the tail of what was emitted states text just said:
//     cursor repeats a model call's text as its cumulative frame, so when the
//     call was already emitted piece by piece the repetition is identical (the
//     two `done` frames of the shell fixture). Emitting it again would double
//     the transcript; with no message id to compare, "already emitted" wins.
//  5. Any other snapshot is new text: a message uncorrelated with what came
//     before. It is emitted in full and appended to the lane's emitted text, so
//     the cumulative frame that follows the pieces still matches rule 2.
//  6. The accumulator is per turn: reset at system/init and at result.
func (p *profile) advance(lane, snapshot string) string {
	if snapshot == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	emitted := p.lanes[lane]
	switch {
	case strings.HasPrefix(snapshot, emitted):
		p.lanes[lane] = snapshot
		return snapshot[len(emitted):]
	case strings.HasPrefix(emitted, snapshot), strings.HasSuffix(emitted, snapshot):
		return ""
	default:
		p.lanes[lane] = emitted + snapshot
		return snapshot
	}
}

// resetLanes forgets what was emitted: a new turn (init) or the end of one
// (result) starts the accumulator empty.
func (p *profile) resetLanes() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lanes = map[string]string{}
}

// Content is sj.ContentReader: one assistant frame, the facts it adds.
//
// A frame states text (text/output_text → text_delta through the accumulator),
// reasoning (thinking → reasoning) and tool calls (tool_use → tool_call_started
// with the compact JSON of its input, only when the frame states them). A block
// type nobody declared produces no event: it stays in the frame's Native and is
// never invented into the stream.
//
// stream_event is claude's dialect; cursor sends none, so there is nothing to
// project and the frame stays in Native untouched.
func (p *profile) Content(raw []byte, m sj.Rec, path []string) []oarlock.Event {
	if sj.Str(m["type"]) != "assistant" {
		return nil
	}
	blocks := assistantBlocks(raw)
	snapshot := ""
	for _, b := range blocks {
		if b.text() && b.Text != nil {
			snapshot += *b.Text
		}
	}
	delta := p.advance(contentLane(path), snapshot)
	var events []oarlock.Event
	textDone := false
	for _, b := range blocks {
		switch {
		case b.text():
			// The frame's text is one snapshot; its new part is stated once,
			// where the text starts.
			if textDone || delta == "" {
				continue
			}
			textDone = true
			events = append(events, oarlock.Event{Kind: oarlock.EventTextDelta, Text: delta})
		case b.Type == "thinking":
			if b.Thinking != nil && *b.Thinking != "" {
				events = append(events, oarlock.Event{Kind: oarlock.EventReasoning, Text: *b.Thinking})
			}
		case b.Type == "tool_use":
			events = append(events, oarlock.Event{
				Kind: oarlock.EventToolCallStarted, CallID: b.ID, Tool: b.Name, Input: compactInput(b.Input),
			})
		}
	}
	return events
}

// compactInput is the block's input as one line of JSON: the same document
// without insignificant whitespace, key order untouched. "" when the frame
// states no input.
func compactInput(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}
