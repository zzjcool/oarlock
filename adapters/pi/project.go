package pi

// The Pi projection (issue #11): one pure fold from a native Pi frame to the
// unified events read out of it. The same function drives the live pump
// (Session.ingest) and any replay of retained records, so a replay cannot
// disagree with what was observed live.
//
//	foldPiEvent(state, native) -> (state', []oarlock.Event)
//
// Rules the fold keeps (docs/spec/record-stream.md, docs/runtimes/pi.md):
//
//   - One frame in, the events that frame states out; nothing is read from any
//     other frame and nothing is synthesized. The only turn end is the one
//     agent_settled states (turn_end and agent_end are not turn ends: a run
//     with tools has several turn_end).
//   - No I/O, no clock, no goroutine, no mutation of the state it is given: the
//     returned state shares nothing mutable with the argument.
//   - A native type the fold does not know produces no event and is counted in
//     the state (and, one level down, a message_update sub-type it does not
//     know, counted as "message_update/<sub>"). It is never coerced into a
//     plausible event and never dropped: the frame stays in Record.Native.
//   - A number the runtime did not report is nil or absent, never 0. A frame
//     whose usage lacks a numeric input or output states nothing about usage.
//
// Not in the fold, on purpose: the prompt response (its input_handled event
// needs the request that sent the prompt, which only the control plane knows,
// session.go) and the post-settlement get_session_stats read (an action, not a
// reading; its response frame is read by this fold like any other frame).

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"strings"

	"github.com/zzjcool/oarlock"
)

// piState is the fold's state. Its zero value is the state of a session that
// has seen no frame. It is a value: foldPiEvent never mutates the one it is
// given (the unknown map is copied on write).
type piState struct {
	// tokens is the running total of this session's assistant messages,
	// counted from when the session opened. It is never reset by a turn.
	tokens oarlock.TokenTotals

	// reasoningHadText: the current thinking block streamed readable text.
	reasoningHadText bool

	// lastStop is the stopReason of the run's last assistant turn_end; failure
	// the provider error that turn (or a stream error) reported. Both are per
	// run: agent_settled consumes them.
	lastStop string
	failure  *piProviderError

	// unknown tallies native types (and message_update sub-types) the fold does
	// not know. Frames are still recorded verbatim; a non-zero tally is a test
	// signal (docs/runtimes/pi.md §9).
	unknown map[string]int
}

// piProviderError is a failure the provider stream reported for the run.
type piProviderError struct {
	message  string
	overflow bool // pi-ai's isContextOverflow on the message that carried it
}

// foldPiEvent reads one native Pi message. It never fails: a frame it cannot
// read yields no events (and, if its type is not known, a tally).
func foldPiEvent(st piState, native json.RawMessage) (piState, []oarlock.Event) {
	o, ok := parseObj(native)
	if !ok {
		return st.countUnknown(""), nil
	}
	typ, _ := o.str("type")
	switch typ {
	case "agent_start":
		return st, []oarlock.Event{{Kind: oarlock.EventTurnActive}}

	case "agent_settled":
		// pi's run-settled signal is the turn's end, not agent_end (which
		// precedes threshold compaction and auto-retries).
		aborted, _ := o.boolean("aborted")
		out := st.outcome(aborted)
		st.lastStop, st.failure, st.reasoningHadText = "", nil, false
		return st, []oarlock.Event{{Kind: oarlock.EventTurnEnded, Outcome: out}}

	case "turn_end":
		// Not a turn end. It only supplies the stopReason (and the provider
		// error) of the settlement that follows; the last assistant turn_end of
		// a run wins, so a retry that succeeded is not reported as a failure.
		msg, _ := o.obj("message")
		if role, _ := msg.str("role"); role == "assistant" {
			stop, _ := msg.str("stopReason")
			earlier := st.failure // what a message_update error said about this turn
			st.lastStop = stop
			st.failure = nil
			if stop == "error" {
				if text, has := msg.str("errorMessage"); has {
					st.failure = &piProviderError{message: text, overflow: isContextOverflow(msg)}
				} else if earlier != nil {
					st.failure = earlier // keep the specific text over a generic one
				} else {
					st.failure = &piProviderError{message: "provider error"}
				}
			}
		}
		return st, nil

	case "message_start":
		msg, _ := o.obj("message")
		if role, _ := msg.str("role"); role == "user" {
			return st, []oarlock.Event{{Kind: oarlock.EventUserMessage, Evidence: oarlock.EvidenceConversation, Text: userText(msg["content"])}}
		}
		return st, nil

	case "message_end":
		msg, _ := o.obj("message")
		return st.addAssistantUsage(msg)

	case "message_update":
		return st.foldMessageUpdate(o)

	case "tool_execution_start":
		ev := oarlock.Event{Kind: oarlock.EventToolCallStarted}
		ev.CallID, _ = o.str("toolCallId")
		ev.Tool, _ = o.str("toolName")
		if args, has := o["args"]; has {
			ev.Input = compactJSON(args)
		}
		return st, []oarlock.Event{ev}

	case "tool_execution_update":
		ev := oarlock.Event{Kind: oarlock.EventToolCallProgress}
		ev.CallID, _ = o.str("toolCallId")
		if partial, has := o["partialResult"]; has {
			// A snapshot of the partial result as the runtime sent it.
			ev.Output = compactJSON(partial)
		}
		return st, []oarlock.Event{ev}

	case "tool_execution_end":
		return st, []oarlock.Event{toolEnded(o)}

	case "compaction_start":
		reason, _ := o.str("reason")
		return st, []oarlock.Event{{Kind: oarlock.EventCompactionStarted, Trigger: reason}}

	case "compaction_end":
		// pi's own flags decide: aborted wins, then an error message, else done.
		// The error text has no slot in Event; it stays in Native.
		reason, _ := o.str("reason")
		aborted, _ := o.boolean("aborted")
		result := oarlock.CompactionCompleted
		if aborted {
			result = oarlock.CompactionAborted
		} else if _, failed := o.str("errorMessage"); failed {
			result = oarlock.CompactionFailed
		}
		return st, []oarlock.Event{{Kind: oarlock.EventCompactionEnded, Trigger: reason, CompactionResult: result}}

	case "thinking_level_changed":
		// pi's own report of the level its next request runs.
		if level, has := o.str("level"); has && level != "" {
			return st, []oarlock.Event{{Kind: oarlock.EventEffort, Effort: level}}
		}
		return st, nil

	case "auto_retry_start", "summarization_retry_scheduled":
		ev := oarlock.Event{Kind: oarlock.EventRetry}
		if n, has := wholeNumber(o, "attempt"); has {
			ev.Attempt = int(n)
		}
		if n, has := wholeNumber(o, "maxAttempts"); has {
			ev.MaxAttempts = int(n)
		}
		if n, has := wholeNumber(o, "delayMs"); has {
			ev.DelayMS = n
		}
		ev.Reason, _ = o.str("errorMessage")
		return st, []oarlock.Event{ev}

	case "response":
		return st, responseEvents(o)

	// Known and deliberately event-less: they stay in Native.
	case "turn_start", "agent_end", "queue_update", "entry_appended", "session_info_changed",
		"auto_retry_end", "summarization_retry_attempt_start", "summarization_retry_finished",
		"bash_execution_update", "extension_ui_request", "extension_error":
		return st, nil
	}
	return st.countUnknown(typ), nil
}

// foldMessageUpdate reads the assistantMessageEvent of a message_update.
func (st piState) foldMessageUpdate(o jobj) (piState, []oarlock.Event) {
	inner, _ := o.obj("assistantMessageEvent")
	sub, _ := inner.str("type")
	switch sub {
	case "text_delta":
		delta, _ := inner.str("delta")
		return st, []oarlock.Event{{Kind: oarlock.EventTextDelta, Text: delta}}
	case "thinking_delta":
		delta, _ := inner.str("delta")
		if delta == "" {
			return st, nil
		}
		st.reasoningHadText = true
		return st, []oarlock.Event{{Kind: oarlock.EventReasoning, Text: delta}}
	case "thinking_start":
		st.reasoningHadText = false
		return st, nil
	case "thinking_end":
		// A thinking block that streamed no readable text still happened: one
		// reasoning item with no text (OAR's reasoning{kind:"empty"}).
		if st.reasoningHadText {
			return st, nil
		}
		return st, []oarlock.Event{{Kind: oarlock.EventReasoning}}
	case "error":
		// pi's prompt() resolves even when the provider errored; the failure
		// surfaces here and on the turn's last assistant message.
		errMsg, _ := inner.obj("error")
		text, has := errMsg.str("errorMessage")
		if !has {
			text, _ = inner.str("reason")
		}
		st.failure = &piProviderError{message: text, overflow: isContextOverflow(errMsg)}
		return st, nil
	case "start", "done", "text_start", "text_end", "toolcall_start", "toolcall_delta", "toolcall_end":
		// Block boundaries and tool-call framing carry no event: the tool call
		// arrives through the outer tool_execution_* frames.
		return st, nil
	}
	return st.countUnknown("message_update/" + sub), nil
}

// outcome is the settled turn's outcome from the runtime's own words: the
// aborted flag of agent_settled, else the run's provider failure, else the last
// turn_end's stopReason. It never comes from a timer or an exit.
func (st piState) outcome(aborted bool) *oarlock.TurnOutcome {
	switch {
	case aborted || st.lastStop == "aborted":
		return &oarlock.TurnOutcome{Kind: oarlock.OutcomeAborted}
	case st.failure != nil:
		return classifyPiFailure(st.failure.message, st.failure.overflow)
	default:
		return &oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted}
	}
}

// countUnknown returns st with key tallied once more. The map is copied.
func (st piState) countUnknown(key string) piState {
	next := make(map[string]int, len(st.unknown)+1)
	for k, v := range st.unknown {
		next[k] = v
	}
	next[key]++
	st.unknown = next
	return st
}

// ---- usage and context -----------------------------------------------------

// addAssistantUsage folds the usage of a finished assistant message into the
// running total and states the new total. pi's input excludes the cache reads
// and writes, so input counts them back in; each also stands as its own part
// (cacheWrite1h is a subset of cacheWrite and adds nothing). A message without
// a numeric input and output says nothing: nil, never a guessed zero.
func (st piState) addAssistantUsage(msg jobj) (piState, []oarlock.Event) {
	if role, _ := msg.str("role"); role != "assistant" {
		return st, nil
	}
	usage, ok := msg.obj("usage")
	if !ok {
		return st, nil
	}
	input, okIn := wholeNumber(usage, "input")
	output, okOut := wholeNumber(usage, "output")
	if !okIn || !okOut {
		return st, nil
	}
	report := oarlock.TokenTotals{Input: input, Output: output}
	if n, has := wholeNumber(usage, "cacheRead"); has {
		report.CacheRead = &n
		report.Input += n
	}
	if n, has := wholeNumber(usage, "cacheWrite"); has {
		report.CacheWrite = &n
		report.Input += n
	}
	if n, has := wholeNumber(usage, "reasoning"); has {
		report.Reasoning = &n
	}
	st.tokens = addTokens(st.tokens, report)
	// Pi has no sub-agents (attribution none): this is the root agent's figure,
	// and Total stays nil because nothing covers more than the agent.
	tokens := cloneTokens(st.tokens)
	return st, []oarlock.Event{{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{Tokens: &tokens}}}
}

// responseEvents reads the responses whose data states a fact: get_state (the
// model and thinking level in effect, the runtime's word) and get_session_stats
// (pi's own context fullness). A refusal, any other command and the prompt
// response (control plane) state nothing here.
func responseEvents(o jobj) []oarlock.Event {
	if ok, _ := o.boolean("success"); !ok {
		return nil
	}
	cmd, _ := o.str("command")
	data, _ := o.obj("data")
	switch cmd {
	case "get_state":
		var events []oarlock.Event
		if model, has := data.obj("model"); has {
			provider, _ := model.str("provider")
			id, _ := model.str("id")
			if provider != "" && id != "" {
				events = append(events, oarlock.Event{Kind: oarlock.EventModel, Model: provider + "/" + id})
			}
		}
		if level, has := data.str("thinkingLevel"); has && level != "" {
			events = append(events, oarlock.Event{Kind: oarlock.EventEffort, Effort: level})
		}
		return events
	case "get_session_stats":
		cu, has := data.obj("contextUsage")
		if !has {
			return nil
		}
		var c oarlock.ContextUsage
		if n, has := wholeNumber(cu, "tokens"); has {
			c.Tokens = &n
		}
		if n, has := wholeNumber(cu, "contextWindow"); has {
			c.ContextWindow = &n
		}
		if f, has := cu.num("percent"); has {
			c.Percent = &f
		}
		if c.Tokens == nil && c.ContextWindow == nil && c.Percent == nil {
			return nil
		}
		return []oarlock.Event{{Kind: oarlock.EventUsage, Usage: &oarlock.UsageReport{Context: &c}}}
	}
	return nil
}

// addTokens sums two running totals. Input and output always add; an optional
// part exists only once a report carried it and stays once it has appeared.
func addTokens(previous, report oarlock.TokenTotals) oarlock.TokenTotals {
	return oarlock.TokenTotals{
		Input:      previous.Input + report.Input,
		Output:     previous.Output + report.Output,
		Reasoning:  sumPart(previous.Reasoning, report.Reasoning),
		CacheRead:  sumPart(previous.CacheRead, report.CacheRead),
		CacheWrite: sumPart(previous.CacheWrite, report.CacheWrite),
	}
}

func sumPart(previous, report *int64) *int64 {
	if previous == nil && report == nil {
		return nil
	}
	var sum int64
	if previous != nil {
		sum += *previous
	}
	if report != nil {
		sum += *report
	}
	return &sum
}

func cloneTokens(t oarlock.TokenTotals) oarlock.TokenTotals {
	return oarlock.TokenTotals{Input: t.Input, Output: t.Output, Reasoning: clonePart(t.Reasoning), CacheRead: clonePart(t.CacheRead), CacheWrite: clonePart(t.CacheWrite)}
}

func clonePart(v *int64) *int64 {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}

// ---- tools -----------------------------------------------------------------

// toolEnded reads a tool_execution_end. The result is the AgentToolResult's
// content blocks (details stays in Native); isError, when a boolean, is the
// runtime's explicit outcome; pi's CodingTools report a command's process exit
// in structuredContent.exit_code, the one real exit status (a signal is 128 plus
// the signal number, never null). Absent stays absent: nothing is inferred from
// output or from the other field.
func toolEnded(o jobj) oarlock.Event {
	ev := oarlock.Event{Kind: oarlock.EventToolCallEnded}
	ev.CallID, _ = o.str("toolCallId")
	result := o["result"]
	ev.Content = toolParts(result)
	if isErr, has := o.boolean("isError"); has {
		ev.Result = oarlock.ToolOK
		if isErr {
			ev.Result = oarlock.ToolFailed
		}
	}
	if r, ok := parseObj(result); ok {
		if sc, ok := r.obj("structuredContent"); ok {
			if n, has := integer(sc, "exit_code"); has {
				code := int(n)
				ev.ExitCode = &code
				ev.ExitCodeReported = true
			}
		}
	}
	return ev
}

// toolParts is a tool result as output parts: the content blocks of an
// AgentToolResult in order; any other shape is kept whole (a string is one text
// part); an absent or null result, or an empty block list, is no content.
func toolParts(result json.RawMessage) []oarlock.ToolOutputPart {
	if len(result) == 0 || isNull(result) {
		return nil
	}
	if r, ok := parseObj(result); ok {
		// An AgentToolResult: its content blocks are the result, and an empty
		// list is an empty result, not a reason to keep the whole object (details
		// and structuredContent stay in Native).
		if blocks, ok := r.array("content"); ok {
			return partsOf(blocks)
		}
	}
	var s string
	if json.Unmarshal(result, &s) == nil {
		return []oarlock.ToolOutputPart{{Type: "text", Text: s}}
	}
	var blocks []json.RawMessage
	if json.Unmarshal(result, &blocks) == nil {
		return partsOf(blocks)
	}
	return []oarlock.ToolOutputPart{{Type: "other", Value: append(json.RawMessage(nil), result...)}}
}

func partsOf(blocks []json.RawMessage) []oarlock.ToolOutputPart {
	if len(blocks) == 0 {
		return nil
	}
	parts := make([]oarlock.ToolOutputPart, 0, len(blocks))
	for _, block := range blocks {
		parts = append(parts, toolPart(block))
	}
	return parts
}

// toolPart is one content block: text, an image in Anthropic's
// {source:{type:base64,media_type,data}} or pi's {data,mimeType} shape, else the
// block kept whole as other.
func toolPart(block json.RawMessage) oarlock.ToolOutputPart {
	if b, ok := parseObj(block); ok {
		typ, _ := b.str("type")
		switch typ {
		case "text":
			if text, has := b.str("text"); has {
				return oarlock.ToolOutputPart{Type: "text", Text: text}
			}
		case "image":
			if src, ok := b.obj("source"); ok {
				kind, _ := src.str("type")
				data, hasData := src.str("data")
				media, hasMedia := src.str("media_type")
				if kind == "base64" && hasData && hasMedia {
					return oarlock.ToolOutputPart{Type: "image", MediaType: media, Data: data}
				}
			}
			data, hasData := b.str("data")
			media, hasMedia := b.str("mimeType")
			if hasData && hasMedia {
				return oarlock.ToolOutputPart{Type: "image", MediaType: media, Data: data}
			}
		}
	}
	return oarlock.ToolOutputPart{Type: "other", Value: append(json.RawMessage(nil), block...)}
}

// userText is the text of a user message: a string, or the text parts joined.
func userText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, block := range blocks {
		if o, ok := parseObj(block); ok {
			if typ, _ := o.str("type"); typ == "text" {
				text, _ := o.str("text")
				b.WriteString(text)
			}
		}
	}
	return b.String()
}

// ---- failure classification --------------------------------------------------

// Classes follow the evidence order OAR's pi failure.ts uses: pi-ai's own
// isContextOverflow, then the provider error body's code or type, then the HTTP
// status the message is prefixed with ("<status> <body>"). Vendor prose is never
// the branch word; "unknown" is an honest answer.
//
// The issue's "context_overflow" is oarlock's FailureInputTooLarge and its
// "rate_limit" is FailureRateLimited: the failure vocabulary is frozen.
func classifyPiFailure(message string, overflow bool) *oarlock.TurnOutcome {
	out := &oarlock.TurnOutcome{Kind: oarlock.OutcomeFailed, Reason: message}
	body := message
	if m := statusPrefix.FindStringSubmatch(message); m != nil {
		out.Status = int(m[1][0]-'0')*100 + int(m[1][1]-'0')*10 + int(m[1][2]-'0')
		body = m[2]
	}
	switch {
	case overflow:
		out.Failure = oarlock.FailureInputTooLarge
	default:
		if named, ok := failureFromErrorBody(body); ok {
			out.Failure = named
		} else if out.Status == 0 {
			out.Failure = oarlock.FailureUnknown
		} else {
			out.Failure = failureFromStatus(out.Status)
		}
	}
	return out
}

var statusPrefix = regexp.MustCompile(`^(\d{3}) ([\s\S]*)$`)

func failureFromStatus(status int) oarlock.FailureClass {
	switch status {
	case 400:
		return oarlock.FailureInvalidRequest
	case 401:
		return oarlock.FailureAuth
	case 402:
		return oarlock.FailureBilling
	case 404:
		return oarlock.FailureModelUnavail
	case 429:
		return oarlock.FailureRateLimited
	case 503, 529:
		return oarlock.FailureOverloaded
	}
	if status >= 500 && status < 600 {
		return oarlock.FailureProvider
	}
	return oarlock.FailureUnknown
}

// errorCodes are the provider error codes and types some runtime was observed
// to carry (OpenAI's codes, Anthropic's types).
var errorCodes = map[string]oarlock.FailureClass{
	"invalid_api_key":                   oarlock.FailureAuth,
	"model_not_found":                   oarlock.FailureModelUnavail,
	"rate_limit_exceeded":               oarlock.FailureRateLimited,
	"insufficient_quota":                oarlock.FailureQuota,
	"organization_usage_limit_exceeded": oarlock.FailureQuota,
	"usage_limit_reached":               oarlock.FailureQuota,
	"usage_not_included":                oarlock.FailureQuota,
	"credit_balance_exhausted":          oarlock.FailureBilling,
	"context_length_exceeded":           oarlock.FailureInputTooLarge,
	"server_is_overloaded":              oarlock.FailureOverloaded,
	"authentication_error":              oarlock.FailureAuth,
	"not_found_error":                   oarlock.FailureModelUnavail,
	"rate_limit_error":                  oarlock.FailureRateLimited,
	"billing_error":                     oarlock.FailureBilling,
	"overloaded_error":                  oarlock.FailureOverloaded,
	"api_error":                         oarlock.FailureProvider,
	"server_error":                      oarlock.FailureProvider,
	"invalid_request_error":             oarlock.FailureInvalidRequest,
}

// failureFromErrorBody reads a provider's JSON error body, as Anthropic
// ({type:"error", error:{type,message}}) and OpenAI ({error:{code,type,message}})
// send it: the class its code names, else its type's.
func failureFromErrorBody(text string) (oarlock.FailureClass, bool) {
	root, ok := parseObj(json.RawMessage(text))
	if !ok {
		return "", false
	}
	e, ok := root.obj("error")
	if !ok {
		return "", false
	}
	if code, has := e.str("code"); has {
		if class, known := errorCodes[code]; known {
			return class, true
		}
	}
	typ, hasType := e.str("type")
	if typ == "invalid_request_error" {
		msg, _ := e.str("message")
		if class, limited := anthropicLimitText(msg); limited {
			return class, true
		}
		return oarlock.FailureInvalidRequest, true
	}
	if hasType {
		class, known := errorCodes[typ]
		return class, known
	}
	return "", false
}

var (
	creditBalanceText = regexp.MustCompile(`(?i)credit balance is too low`)
	usageLimitText    = regexp.MustCompile(`(?i)reached your specified API usage limits`)
)

// anthropicLimitText names Anthropic's two 400s that have no type of their own.
func anthropicLimitText(message string) (oarlock.FailureClass, bool) {
	switch {
	case creditBalanceText.MatchString(message):
		return oarlock.FailureBilling, true
	case usageLimitText.MatchString(message):
		return oarlock.FailureQuota, true
	}
	return "", false
}

// overflowPatterns and nonOverflowPatterns are pi-ai 1.4.0's isContextOverflow
// tables (dist/utils/overflow.js), verbatim. Its silent-overflow cases need the
// model's context window, which a frame does not carry, so they are not ported.
var overflowPatterns = compilePatterns(
	`(?i)prompt (?:is )?too long`,
	`(?i)prompt exceeds max length`,
	`(?i)request_too_large`,
	`(?i)input is too long for requested model`,
	`(?i)exceeds the context window`,
	`(?i)exceeds (?:the )?(?:model'?s )?maximum context length(?: of [\d,]+ tokens?|\s*\([\d,]+\))`,
	`(?i)input token count.*exceeds the maximum`,
	`(?i)maximum prompt length is \d+`,
	`(?i)reduce the length of the messages`,
	`(?i)maximum context length is \d+ tokens`,
	`(?i)exceeds (?:the )?maximum allowed input length of [\d,]+ tokens?`,
	`(?i)input \(\d+ tokens\) is longer than the model'?s context length \(\d+ tokens\)`,
	`(?i)exceeds the limit of \d+`,
	`(?i)exceeds the available context size`,
	`(?i)greater than the context length`,
	`(?i)context window exceeds limit`,
	`(?i)exceeded model token limit`,
	`(?i)too large for model with \d+ maximum context length`,
	`(?i)prompt has [\d,]+ tokens?, but the configured context size is [\d,]+ tokens?`,
	`(?i)model_context_window_exceeded`,
	`(?i)prompt too long; exceeded (?:max )?context length`,
	`(?i)range of input length should be`,
	`(?i)context[_ ]length[_ ]exceeded`,
	`(?i)too many tokens`,
	`(?i)token limit exceeded`,
	`(?i)^(?:[\w -]+ API error \(413\):|413(?:\s|:|$))`,
)

var nonOverflowPatterns = compilePatterns(
	`(?i)^(Throttling error|Service unavailable):`,
	`(?i)rate limit`,
	`(?i)too many requests`,
)

var cerebrasBodylessOverflow = regexp.MustCompile(`(?i)^4(?:00|13)\s*(?:status code)?\s*\(no body\)`)

func compilePatterns(patterns ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		out[i] = regexp.MustCompile(p)
	}
	return out
}

// isContextOverflow is pi-ai's test on a failed assistant message: stopReason
// "error" with an errorMessage that names an oversized input, and that does not
// read as throttling.
func isContextOverflow(msg jobj) bool {
	if stop, _ := msg.str("stopReason"); stop != "error" {
		return false
	}
	text, _ := msg.str("errorMessage")
	if text == "" {
		return false
	}
	for _, p := range nonOverflowPatterns {
		if p.MatchString(text) {
			return false
		}
	}
	for _, p := range overflowPatterns {
		if p.MatchString(text) {
			return true
		}
	}
	if provider, _ := msg.str("provider"); provider == "cerebras" && cerebrasBodylessOverflow.MatchString(text) {
		return true
	}
	return false
}

// ---- reading helpers ---------------------------------------------------------

// jobj is a decoded JSON object whose members are read one by one, so a member
// of an unexpected type costs only that member, never the whole frame. The nil
// jobj is a valid empty object.
type jobj map[string]json.RawMessage

func parseObj(raw json.RawMessage) (jobj, bool) {
	var o map[string]json.RawMessage
	if err := json.Unmarshal(raw, &o); err != nil || o == nil {
		return nil, false
	}
	return jobj(o), true
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

func (o jobj) str(key string) (string, bool) {
	raw, ok := o[key]
	if !ok || isNull(raw) {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func (o jobj) boolean(key string) (bool, bool) {
	raw, ok := o[key]
	if !ok || isNull(raw) {
		return false, false
	}
	var b bool
	if json.Unmarshal(raw, &b) != nil {
		return false, false
	}
	return b, true
}

func (o jobj) num(key string) (float64, bool) {
	raw, ok := o[key]
	if !ok || isNull(raw) {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func (o jobj) obj(key string) (jobj, bool) {
	raw, ok := o[key]
	if !ok || isNull(raw) {
		return nil, false
	}
	return parseObj(raw)
}

func (o jobj) array(key string) ([]json.RawMessage, bool) {
	raw, ok := o[key]
	if !ok || isNull(raw) {
		return nil, false
	}
	var a []json.RawMessage
	if json.Unmarshal(raw, &a) != nil || a == nil {
		return nil, false
	}
	return a, true
}

// wholeNumber reads a non-negative whole number the runtime reported (a token
// count, an attempt, a delay, an exit status is checked by the caller's
// meaning). Anything else - absent, null, a string, a fraction, a negative -
// is "not reported".
func wholeNumber(o jobj, key string) (int64, bool) {
	f, ok := o.num(key)
	if !ok || f != math.Trunc(f) || f < 0 || f > 1<<53 {
		return 0, false
	}
	return int64(f), true
}

// integer reads a whole number the runtime reported, sign included (an exit
// status is whatever the tool said). A fraction, a string, null or absence is
// "not reported".
func integer(o jobj, key string) (int64, bool) {
	f, ok := o.num(key)
	if !ok || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		return 0, false
	}
	return int64(f), true
}

// compactJSON is raw with insignificant whitespace removed; raw itself when it
// does not compact.
func compactJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return string(raw)
	}
	return b.String()
}

// ---- reading: coalesced text --------------------------------------------------

// readEvent is one event as a reader of the stream sees it: the event plus the
// Seq of the record that carried it (FirstSeq..Seq when several pieces merged).
type readEvent struct {
	FirstSeq  uint64
	Seq       uint64
	AgentPath []string
	Event     oarlock.Event
}

// coalesceText is an optional READING of the stream, never a change to it. It
// lists the events of records in stream order and merges every run of
// consecutive text_delta events (and of readable reasoning events) of one agent
// and one MessageID into one readEvent, whose Text is the pieces' text in order,
// whose FirstSeq is the first piece's record and whose Seq is the last piece's,
// as OAR's coalesceText carries the last piece's envelope.
//
// What it does not do: it does not touch a record. The stream keeps one record
// per native frame, Native verbatim, Events as projected and a dense Seq; the
// merge exists only in the returned slice. Event count after merging is
// (events before) - (pieces merged away); record count and Seq are unchanged.
// A run ends at any other event (a tool call, a different kind, another agent
// or MessageID). A frame that carries no event (a text_start, a host extension
// notification) does not end a run: it states nothing.
func coalesceText(records []oarlock.Record) []readEvent {
	var out []readEvent
	for _, r := range records {
		for _, ev := range r.Events {
			if mergeable(ev) && len(out) > 0 {
				last := &out[len(out)-1]
				if mergeable(last.Event) && last.Event.Kind == ev.Kind &&
					last.Event.MessageID == ev.MessageID && samePath(last.AgentPath, r.AgentPath) {
					last.Event.Text += ev.Text
					last.Seq = r.Seq
					continue
				}
			}
			out = append(out, readEvent{FirstSeq: r.Seq, Seq: r.Seq, AgentPath: append([]string(nil), r.AgentPath...), Event: ev})
		}
	}
	return out
}

// mergeable: a streamed text piece or a readable reasoning piece. A redacted or
// empty reasoning item is not a text piece and never merges.
func mergeable(ev oarlock.Event) bool {
	switch ev.Kind {
	case oarlock.EventTextDelta:
		return true
	case oarlock.EventReasoning:
		return !ev.Redacted && ev.Text != ""
	}
	return false
}

func samePath(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
