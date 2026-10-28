package scripted

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/zzjcool/oarlock"
)

// Turn is the script of one turn: the steps the fake runtime performs, in
// order, between its turn_active and its turn_ended.
type Turn []Step

// Step is one thing the fake runtime does during a turn. Steps are made only by
// the constructors in this file.
type Step interface {
	apply(c *turnRun) action
}

// action is what a step tells the turn loop to do next.
type action int

const (
	actNext action = iota // run the next step
	actEnd                // the runtime ends the turn with c.outcome
	actGone               // the process is gone (dispose / take-over): record nothing more
	actDied               // the script killed the process: the exit is already recorded
)

// Say emits one assistant text chunk (a text_delta event).
func Say(text string) Step { return sayStep{text: text} }

// Echo emits the input that began the turn as assistant text.
func Echo() Step { return sayStep{echo: true} }

// Think emits one reasoning item.
func Think(text string) Step { return thinkStep{text: text} }

// Delay waits d of wall-clock time. An abort or a process kill ends it early.
func Delay(d time.Duration) Step { return delayStep{d: d} }

// Fail makes the runtime end the turn failed with reason. Later steps are
// ignored.
func Fail(reason string) Step { return failStep{reason: reason} }

// Frame emits one native frame verbatim, with the events the script says were
// read out of it. native must be one JSON object. A frame whose "type" is not
// one of the scripted types is counted by Session.Unknown.
func Frame(native string, events ...oarlock.Event) Step {
	return frameStep{native: native, events: events}
}

// UnknownEvent emits a native frame of a type the session does not understand:
// it is kept verbatim, carries no events and is counted, never coerced.
func UnknownEvent(typ string) Step {
	return frameStep{native: string(mustJSON(map[string]string{"type": typ}))}
}

// Handled makes the prompt a handled one (a slash command): the runtime says so
// with an input_handled event and no turn opens. It must be the first step of a
// turn; any other step of that turn is ignored.
func Handled() Step { return handledStep{} }

// Die injects a mid-turn process death: the process exits by itself with code.
// The stream gets one standalone exited response and no turn_ended.
func Die(exitCode int) Step { return dieStep{code: exitCode} }

// Hang injects a turn that never ends by itself. It ends only by an abort the
// runtime honours, a take-over after the abort grace, Dispose or Die.
func Hang() Step { return delayStep{d: -1} }

// IgnoreAbort injects a runtime that does not respond to abort during the turn
// it appears in. It marks the whole turn (it is read when the turn is accepted),
// so an abort can never race the step that declares it; wherever it stands, only
// the take-over after AbortGrace, Dispose or Die ends the turn.
func IgnoreAbort() Step { return ignoreAbortStep{} }

// ToolStep is a tool call the fake runtime makes. Build it with Tool.
type ToolStep struct {
	name, input, output string
	took                time.Duration
	failing             bool
	reason              string
}

// Tool is a tool call: tool_call_started, then tool_call_ended carrying output
// as text content with result ok.
func Tool(name, input, output string) ToolStep {
	return ToolStep{name: name, input: input, output: output}
}

// Taking keeps the call in flight for d (abortable).
func (t ToolStep) Taking(d time.Duration) ToolStep { t.took = d; return t }

// Failing makes the call end with result failed and reason as its text.
func (t ToolStep) Failing(reason string) ToolStep { t.failing, t.reason = true, reason; return t }

type sayStep struct {
	text string
	echo bool
}

func (s sayStep) apply(c *turnRun) action {
	text := s.text
	if s.echo {
		text = c.input
	}
	c.addOutput(text)
	c.frame("scripted/text", map[string]any{"text": text}, oarlock.Event{Kind: oarlock.EventTextDelta, Text: text})
	return actNext
}

type thinkStep struct{ text string }

func (s thinkStep) apply(c *turnRun) action {
	c.frame("scripted/reasoning", map[string]any{"text": s.text}, oarlock.Event{Kind: oarlock.EventReasoning, Text: s.text})
	return actNext
}

type delayStep struct{ d time.Duration }

func (s delayStep) apply(c *turnRun) action { return c.sleep(s.d) }

type failStep struct{ reason string }

func (s failStep) apply(c *turnRun) action {
	c.outcome = &oarlock.TurnOutcome{Kind: oarlock.OutcomeFailed, Failure: oarlock.FailureUnknown, Reason: s.reason}
	return actEnd
}

type frameStep struct {
	native string
	events []oarlock.Event
}

func (s frameStep) apply(c *turnRun) action {
	c.rawFrame(json.RawMessage(s.native), s.events)
	return actNext
}

type handledStep struct{}

func (handledStep) apply(*turnRun) action { return actNext }

type dieStep struct{ code int }

func (s dieStep) apply(c *turnRun) action {
	code := s.code
	c.s.recordExit("", &code, "died")
	return actDied
}

type ignoreAbortStep struct{}

// apply is a no-op: the turn was marked when it was accepted (hasIgnoreAbort).
func (ignoreAbortStep) apply(*turnRun) action { return actNext }

func (t ToolStep) apply(c *turnRun) action {
	c.callSeq++
	id := fmt.Sprintf("call-%d", c.callSeq)
	c.frame("scripted/tool_start", map[string]any{"callId": id, "name": t.name, "input": t.input},
		oarlock.Event{Kind: oarlock.EventToolCallStarted, CallID: id, Tool: t.name, Input: t.input})
	if t.took > 0 {
		if a := c.sleep(t.took); a != actNext {
			return a
		}
	}
	text, result := t.output, oarlock.ToolOK
	if t.failing {
		text, result = t.reason, oarlock.ToolFailed
	}
	c.frame("scripted/tool_end", map[string]any{"callId": id, "output": text, "result": string(result)},
		oarlock.Event{Kind: oarlock.EventToolCallEnded, CallID: id, Result: result, Content: []oarlock.ToolOutputPart{{Type: "text", Text: text}}})
	return actNext
}
