package acp

// Models for the ACP family (issue #98): what a profile can run now, over the
// same connection every session speaks, without opening a session that answers
// a prompt.
//
// Where the catalogue really comes from:
//
//   - grok: the vendor's own `_x.ai/models/list` extension, whose answer is
//     double-enveloped (the JSON-RPC result carries the handler's `result` or
//     `error`). A build without the extension answers -32601, which is not a
//     failure: the ACP-standard source below carries the same catalogue.
//   - the other five (kimi, opencode, qwen, qoder, antigravity), plus grok
//     through the fallback: the `configOptions` of a `session/new` answer. The
//     entry whose `category` is "model" (or, the spelling OAR also tolerated,
//     whose `category` is absent and whose `id` is "model") lists the models;
//     its `currentValue` is the selected one.
//
// The transport is a small hand-rolled NDJSON JSON-RPC/2.0 client, not
// sdk.ClientSideConnection. The SDK's typed SessionConfigOption union drops the
// `id`, `category` and `name` fields this file has to read (acp-go-sdk v0.13.5
// types_gen.go:4533,4690), so the listing reads raw JSON, one request in
// flight, ids correlated by number, and every frame that carries a `method`
// (a server→client request or a notification) skipped without an answer: a
// listing must never drive a permission or terminal flow.
//
// Handshake, identical for all six: `initialize` with the protocol version and
// the two capabilities a listing can honour (no fs reads, no terminal) and
// deliberately without the profile's `_meta` — those payloads carry
// session-shaping hints (rules, a system prompt) that a catalogue read must not
// send; then `authenticate` only when the profile can name a method from the
// initialize answer (the same rule open.go:126-131 uses). Each request gets its
// own 30s deadline (modelsTimeout), shortened by an earlier caller deadline and
// named in the timeout error. A `session/new` answer that names a session is
// closed best-effort (2s, every error swallowed): the process is killed
// afterwards regardless.
//
// The effort menu is the `thought_level` config option. It is session-scoped,
// not per model, so it is attached only to the entry whose id equals the model
// option's `currentValue` — claiming an effort menu for a model the agent never
// reported one for would invent data. The literal "off" is dropped from that
// menu: kimi and opencode send it, and it is the absence of an effort level,
// not a level. grok is the exception: its reasoning menu travels per model
// (`_meta.reasoningEfforts`), so there it is attached per entry.
//
// What is dropped, on purpose, because oarlock.Model has no field for it: the
// default/current-model marker (`currentModelId` / `current_model_id`, the
// model option's `currentValue` as a flag), grok's `hidden` and
// `userSelectable=false` entries (a model the account cannot select is not
// something it "can run now"), and the group labels a grouped options list
// carries. An entry with no usable id is skipped, never emitted as "".
//
// Failures are typed, never an empty list: ErrNotAuthenticated when the build
// says no account is signed in, *oarlock.UnsupportedOperation when the build has
// no listing surface (no model config option, or -32601), and a plain error for
// everything else — including a stream that ends before the answer and a
// catalogue array that is not there at all, neither of which is "zero models".
// "Signed out" is decided by wording, not by code: -32000 is the vendors'
// catch-all, so it is the typed sentinel only when its message also says auth /
// logged in (kimi's `-32000 "Authentication required"`; a `-32000 "Internal
// error: database is locked"` is the plain error it looks like).
//
// The honest empty is the one success that carries no models, and it is always a
// non-nil slice: nil is this method's failure value everywhere, so a host can
// tell "zero models" from "could not read". Only an explicit `"options": []`
// (or grok's `availableModels: []`) earns it; a model config option with no
// options key at all is a field this build no longer understands — a rename or a
// vendor shape change — and must fail loudly instead of printing "(no models)".
//
// LIVE STATUS: UNVERIFIED for all six. Only the in-process fake agent exercises
// this path (models_test.go); docs/runtimes/opencode.md reports the model config
// option inside a real open session (v2, live 2.0.26), not through this listing.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/exec"
)

// ErrNotAuthenticated is ListModels' typed "the account cannot list models"
// failure: the runtime answered that no account is signed in, or that this
// account may not read the model catalogue. It is a typed error and never an
// empty list.
var ErrNotAuthenticated = errors.New("acp: not signed in")

const (
	// modelsKillGrace bounds the polite stop of the throwaway agent: it exists
	// for one listing, so a tree that ignores SIGTERM is killed rather than
	// waited out. modelsWaitDelay bounds the drain of the output pipes after it
	// exits.
	modelsKillGrace = 2 * time.Second
	modelsWaitDelay = time.Second

	// modelsTimeout bounds one JSON-RPC request: initialize, authenticate, the
	// models method, session/new and session/close each get their own budget.
	modelsTimeout = 30 * time.Second
	// modelsCloseTimeout is the best-effort session/close budget. It never
	// changes a result.
	modelsCloseTimeout = 2 * time.Second

	// modelsReadChunk is the reader's initial buffer and modelsMaxLine the
	// longest line accepted: a listing reply is a few KiB, and a line past the
	// cap is a protocol failure, not a catalogue.
	modelsReadChunk = 64 << 10
	modelsMaxLine   = 16 << 20

	// modelsDetailLimit bounds one vendor line quoted into an error.
	modelsDetailLimit = 400

	// modelsAuthRequired is the JSON-RPC code the vendors use for "no account is
	// signed in"; modelsMethodNotFound is the code for "this build has no such
	// method".
	modelsAuthRequired   = -32000
	modelsMethodNotFound = -32601

	// modelsGrokModelsMethod is grok's own listing extension.
	modelsGrokModelsMethod = "_x.ai/models/list"

	// modelsEffortOff is the menu value that means "no effort level", not a
	// level: kimi and opencode send it in the thought_level menu.
	modelsEffortOff = "off"
)

// models is one profile's lister. The profile is the only per-runtime
// difference; the flow below is shared by all six.
type models struct{ prof Profile }

var _ oarlock.ModelLister = models{}

// ListModels lists what the profile's installation can run now.
func (m models) ListModels(ctx context.Context) ([]oarlock.Model, error) {
	if ctx == nil {
		return nil, errors.New("acp: ListModels(nil context)")
	}
	prof := m.prof
	// The probe's own lookup, so a host that probed and a host that lists cannot
	// disagree about which binary is the runtime (PATH first, then the vendor's
	// install location). An uninstalled runtime is an error: the CLI never asks a
	// runtime it did not see installed.
	command, err := probe(prof.ID).find()
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("acp: %s: cannot determine the working directory: %w", prof.ID, err)
	}
	if abs, absErr := filepath.Abs(cwd); absErr == nil {
		cwd = abs
	}
	// The profile's own argv, exactly as an open would build it, and nothing
	// else: a listing is not a session, so the host's LaunchArgs are not
	// appended. Profile.Select is not consulted — both opencode release lines
	// launch the same `opencode acp`, and nothing else here depends on the line.
	var args []string
	if prof.Args != nil {
		args = prof.Args(oarlock.Options{})
	}

	// The process is owned here, not by the caller's context: it is stopped on
	// every return path below (Close), so a listing never leaves an agent behind
	// even when the caller gave up.
	pctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cmd, err := exec.Start(pctx, &exec.Command{
		Path:      command,
		Args:      args,
		Dir:       cwd,
		Env:       envFor(os.Environ(), nil),
		KillGrace: modelsKillGrace,
		WaitDelay: modelsWaitDelay,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("acp: launch %s: %w", prof.ID, err)
	}
	// Claim both streams before anything can drain them, then drop stderr into a
	// bounded tail: a real agent logs there, a full pipe would stop it mid-reply,
	// and its output is error context, never part of a catalogue.
	_, _ = cmd.Stdout().Read(nil)
	_, _ = cmd.Stderr().Read(nil)
	stderr := &tail{}
	go func() { _, _ = io.Copy(stderr, cmd.Stderr()) }()

	lines := make(chan modelsLine)
	stop := make(chan struct{})
	go modelsReadLines(cmd.Stdout(), lines, stop)

	// Nothing outlives this listing: the reader is released, the tree is stopped
	// (stdin closed, SIGTERM, then SIGKILL after the grace) and reaped.
	defer func() {
		close(stop)
		_ = cmd.Close(context.Background())
		cancel()
		_, _ = cmd.Wait(context.Background())
	}()

	c := &modelsClient{id: prof.ID, stdin: cmd.Stdin(), lines: lines, stderr: stderr}
	return c.list(ctx, prof, cwd)
}

// list is the shared handshake and the two catalogue sources.
func (c *modelsClient) list(ctx context.Context, prof Profile, cwd string) ([]oarlock.Model, error) {
	// initialize: the protocol version every session speaks and the capabilities
	// a listing can honour. The profile's InitializeMeta is omitted on purpose —
	// see the file comment.
	initResult, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": map[string]any{"name": "oarlock", "version": "0"},
	})
	if err != nil {
		return nil, err
	}
	// authenticate only when the profile knows how to pick a method out of the
	// initialize answer, exactly as open.go does.
	if prof.AuthMethod != nil {
		var initialized map[string]any
		if err := json.Unmarshal(initResult, &initialized); err != nil {
			return nil, fmt.Errorf("acp: %s: initialize: unreadable result: %w", prof.ID, err)
		}
		if methodID := prof.AuthMethod(initialized); methodID != "" {
			if _, err := c.call(ctx, "authenticate", map[string]any{"methodId": methodID}); err != nil {
				return nil, err
			}
		}
	}
	// grok is the one vendor with a listing method of its own. A build without it
	// answers -32601, which is not a failure: the ACP-standard source below is
	// the same catalogue.
	if prof.ID == Grok.ID {
		result, err := c.call(ctx, modelsGrokModelsMethod, map[string]any{})
		if err == nil {
			return grokModels(prof.ID, result)
		}
		var unsupported *oarlock.UnsupportedOperation
		if !errors.As(err, &unsupported) {
			return nil, err
		}
	}
	payload, err := c.call(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	if err != nil {
		return nil, err
	}
	list, sessionID, parseErr := sessionModels(prof.ID, payload)
	if sessionID != "" {
		// A listing leaves no session behind. Best-effort by design: the result is
		// already in hand and the process is killed a moment later anyway.
		c.closeSession(ctx, sessionID)
	}
	return list, parseErr
}

// ---- transport -----------------------------------------------------------
//
// The framing below (one line → one frame, a reply recognised by its id, a
// `method` meaning notification or server request) is a deliberate duplicate of
// wire.go's parseWire/lineSplitter. The tap in wire.go exists for a session: it
// records the wire and is handed to the SDK connection. A listing must not share
// the session tap — it opens no SDK connection, sends no session traffic, and a
// catalogue read has no business inside a session record — so it carries its own
// small reader and leaves wire.go to the sessions that need a record.

// modelsLine is one line the reader took off stdout, or the failure that ended
// the stream.
type modelsLine struct {
	raw []byte
	err error
}

// modelsReadLines frames stdout into LF-terminated lines for the client, the
// way every JSON-RPC-over-stdio reader in this repo does. A line that is not a
// JSON-RPC object is skipped by the consumer, not here.
func modelsReadLines(stdout io.Reader, lines chan<- modelsLine, stop <-chan struct{}) {
	defer close(lines)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, modelsReadChunk), modelsMaxLine)
	for scanner.Scan() {
		line := modelsLine{raw: append([]byte(nil), scanner.Bytes()...)}
		select {
		case lines <- line:
		case <-stop:
			return
		}
	}
	if err := scanner.Err(); err != nil {
		select {
		case lines <- modelsLine{err: err}:
		case <-stop:
		}
	}
}

// modelsFrame is the part of an inbound line this client reads: an id and a
// result mark a reply, a method marks a notification or a server request.
type modelsFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *modelsProblem  `json:"error"`
}

// modelsProblem is a JSON-RPC error object.
type modelsProblem struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// modelsClient is the request side of one listing: one request in flight,
// replies correlated by numeric id, everything else ignored.
type modelsClient struct {
	id     string // the profile id, for error text
	stdin  io.Writer
	lines  <-chan modelsLine
	stderr *tail
	next   int
}

// call sends one request and returns its result frame. Every request gets its
// own deadline, and a frame that is not the answer to this request — another
// id, a notification, a server→client request — is skipped.
func (c *modelsClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	budget := modelsBudget(ctx)
	rctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()

	id := c.next + 1
	c.next = id
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, fmt.Errorf("acp: %s: %s: %w", c.id, method, err)
	}
	if _, err := c.stdin.Write(append(frame, '\n')); err != nil {
		// A write fails when the process is already gone: the same event as a
		// stream that ended before the answer.
		return nil, c.noAnswer(rctx, budget, method)
	}
	for {
		select {
		case <-rctx.Done():
			return nil, modelsTimeoutFailure(c.id, method, budget)
		case line, ok := <-c.lines:
			if !ok || line.err != nil {
				return nil, c.noAnswer(rctx, budget, method)
			}
			trimmed := bytes.TrimSpace(line.raw)
			if len(trimmed) == 0 || trimmed[0] != '{' {
				continue // not a JSON-RPC frame
			}
			var reply modelsFrame
			if json.Unmarshal(trimmed, &reply) != nil {
				continue // an unreadable line is not an answer
			}
			if reply.Method != "" {
				// A server→client request or a notification. Answer nothing: a
				// listing must never drive a permission or terminal flow.
				continue
			}
			if !modelsReplyID(reply.ID, id) {
				continue // a stale or foreign answer is not this request's
			}
			if reply.Error != nil {
				return nil, c.rpcError(method, reply.Error)
			}
			return reply.Result, nil
		}
	}
}

// noAnswer classifies a request that will never be answered: a stopped context
// is the timeout, anything else is a process that went away.
func (c *modelsClient) noAnswer(ctx context.Context, budget time.Duration, method string) error {
	if ctx.Err() != nil {
		return modelsTimeoutFailure(c.id, method, budget)
	}
	suffix := ""
	if detail := strings.TrimSpace(c.stderr.String()); detail != "" {
		suffix = "; stderr: " + detail
	}
	return fmt.Errorf("acp: %s: %s: process exited before answering%s", c.id, method, suffix)
}

// rpcError classifies one JSON-RPC error object. A vendor refusal can quote a
// credential back, so the message is bounded before it reaches an error.
//
// The wording is the whole auth test, for -32000 as much as for any other code:
// modelsAuthRequired is the vendors' catch-all (kimi sends it for
// "Authentication required", a locked database sends it too), so the code alone
// would tell a reader to sign in when the runtime is merely broken.
func (c *modelsClient) rpcError(method string, problem *modelsProblem) error {
	if modelsAuthWording.MatchString(problem.Message) {
		return fmt.Errorf("%w: %s", ErrNotAuthenticated, modelsDetail(problem.Message))
	}
	if problem.Code == modelsMethodNotFound {
		return &oarlock.UnsupportedOperation{
			Operation: "list-models",
			Reason:    fmt.Sprintf("this %s build has no %s method", c.id, method),
		}
	}
	detail := modelsDetail(problem.Message)
	if detail == "" {
		detail = strconv.Itoa(problem.Code)
	} else if problem.Code == modelsAuthRequired {
		// A catch-all -32000 whose wording carried no auth claim: keep the code
		// in the text, because the message alone ("Internal error: database is
		// locked") does not say which vendor code it arrived with.
		detail = strconv.Itoa(problem.Code) + ": " + detail
	}
	return fmt.Errorf("acp: %s: %s: %s", c.id, method, detail)
}

// closeSession is the best-effort session/close after a session/new answer.
// Every error is swallowed and no result depends on it.
func (c *modelsClient) closeSession(ctx context.Context, sessionID string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), modelsCloseTimeout)
	defer cancel()
	_, _ = c.call(cctx, "session/close", map[string]any{"sessionId": sessionID})
}

// modelsAuthWording is the frozen auth classifier: a code other than -32000 can
// still be an auth refusal, and the vendors that do that say so in words.
var modelsAuthWording = regexp.MustCompile(`(?i)auth(entication)?|log(ged)? ?in`)

// modelsBudget is the time one request gets: modelsTimeout, shortened by an
// earlier caller deadline so a caller that gives a listing 300ms is not told it
// waited 30s.
func modelsBudget(ctx context.Context) time.Duration {
	budget := modelsTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline); left < budget {
			budget = left
		}
	}
	if budget < 0 {
		budget = 0
	}
	return budget
}

// modelsTimeoutFailure names the method that ran out of time.
func modelsTimeoutFailure(id, method string, budget time.Duration) error {
	return fmt.Errorf("acp: %s: %s timed out after %s", id, method, budget.Round(time.Millisecond))
}

// modelsReplyID reports whether raw is the numeric id wanted.
func modelsReplyID(raw json.RawMessage, id int) bool {
	var got int
	if err := json.Unmarshal(raw, &got); err != nil {
		return false
	}
	return got == id
}

// modelsDetail bounds one vendor line for an error: one line, at most
// modelsDetailLimit bytes, never a multi-line dump.
func modelsDetail(message string) string {
	detail := strings.Join(strings.Fields(message), " ")
	if len(detail) > modelsDetailLimit {
		detail = detail[:modelsDetailLimit]
		for len(detail) > 0 && !utf8.ValidString(detail) {
			detail = detail[:len(detail)-1]
		}
	}
	return detail
}

// ---- session/new: the ACP-standard catalogue -----------------------------

// sessionModels reads the catalogue out of a session/new answer, tolerant of the
// two option spellings OAR tolerated. It returns the session id next to the
// models so the caller can close a session it only opened to read.
//
// The options array must be there: an absent one is a field this build cannot
// read (a rename, a vendor shape change), not "zero models", and it fails
// loudly. An explicit empty array is the honest empty, and it is the non-nil
// slice — nil is this method's failure value.
func sessionModels(provider string, payload json.RawMessage) ([]oarlock.Model, string, error) {
	var answer struct {
		SessionID     string            `json:"sessionId"`
		ConfigOptions []json.RawMessage `json:"configOptions"`
	}
	if err := json.Unmarshal(payload, &answer); err != nil {
		return nil, "", fmt.Errorf("acp: %s: session/new: unreadable result: %w", provider, err)
	}
	var modelOption, effortOption map[string]any
	for _, raw := range answer.ConfigOptions {
		entry := modelsObject(raw)
		if entry == nil {
			continue
		}
		category, id := asString(entry["category"]), asString(entry["id"])
		switch {
		case modelOption == nil && (category == "model" || (category == "" && id == "model")):
			modelOption = entry
		case effortOption == nil && category == "thought_level":
			effortOption = entry
		}
	}
	if modelOption == nil {
		return nil, answer.SessionID, &oarlock.UnsupportedOperation{
			Operation: "list-models",
			Reason:    fmt.Sprintf("this %s build returns no model config option on session/new", provider),
		}
	}
	// Presence, not length: `"options": []` is a catalogue of zero models, and a
	// missing key (or a null, or a scalar) is a catalogue this file cannot read.
	// modelsFlatten answers nil for both, which is exactly how a silent empty
	// list used to be born.
	rawOptions, present := modelOption["options"]
	if _, isArray := rawOptions.([]any); !present || !isArray {
		return nil, answer.SessionID, fmt.Errorf("acp: %s: session/new: the model config option carried no options array", provider)
	}
	selected := strings.TrimSpace(asString(modelOption["currentValue"]))
	levels := thoughtLevels(effortOption)
	list := []oarlock.Model{} // nil is this method's failure value; this is data
	for _, option := range modelsFlatten(rawOptions) {
		id := strings.TrimSpace(asString(option["value"]))
		if id == "" {
			continue // an entry with no id is not a model
		}
		name := strings.TrimSpace(asString(option["name"]))
		if name == "" {
			name = id
		}
		model := oarlock.Model{Provider: provider, ID: id, Name: name}
		// The thought_level menu is session-scoped: only the selected model may
		// claim it (see the file comment).
		if id == selected {
			model.EffortLevels = levels
		}
		list = append(list, model)
	}
	return list, answer.SessionID, nil
}

// modelsFlatten turns one config option's `options` into a flat list: a leaf
// carries `value`, a group carries a `name` and a nested `options` array, and
// both spellings are in the wild.
func modelsFlatten(raw any) []map[string]any {
	var out []map[string]any
	for _, item := range asSlice(raw) {
		entry := asMap(item)
		if entry == nil {
			continue
		}
		if nested := asSlice(entry["options"]); len(nested) > 0 {
			out = append(out, modelsFlatten(nested)...)
			continue
		}
		out = append(out, entry)
	}
	return out
}

// thoughtLevels reads the effort menu of one session, dropping the literal
// "off" (see the file comment).
func thoughtLevels(option map[string]any) []string {
	if option == nil {
		return nil
	}
	var levels []string
	seen := map[string]bool{}
	for _, entry := range modelsFlatten(option["options"]) {
		value := strings.TrimSpace(asString(entry["value"]))
		if value == "" || value == modelsEffortOff || seen[value] {
			continue
		}
		seen[value] = true
		levels = append(levels, value)
	}
	return levels
}

// ---- grok: _x.ai/models/list ---------------------------------------------

// grokModels reads grok's own catalogue. The answer is double-enveloped in the
// OAR recording: the JSON-RPC result is an object that may carry the handler's
// `result` (unwrap it) or the handler's `error` (that is a failure), or be the
// payload itself.
func grokModels(provider string, payload json.RawMessage) ([]oarlock.Model, error) {
	body := payload
	if envelope := modelsRawObject(payload); envelope != nil {
		if raw, ok := envelope["error"]; ok && !modelsNull(raw) {
			return nil, fmt.Errorf("grok models/list handler error: %s", grokHandlerMessage(raw))
		}
		if raw, ok := envelope["result"]; ok && !modelsNull(raw) {
			body = raw
		}
	}
	fields := modelsRawObject(body)
	available, ok := modelsArrays(fields["availableModels"], fields["available_models"])
	if !ok {
		// An absent array is not an answer: it is not "zero models". An explicit
		// `availableModels: []` is, and stays the honest empty below.
		return nil, errors.New("grok models/list returned no availableModels array")
	}
	list := []oarlock.Model{} // nil is this method's failure value; this is data
	for _, raw := range available {
		entry := modelsObject(raw)
		if entry == nil {
			continue
		}
		id := firstText(entry["modelId"], entry["model_id"], entry["id"])
		if id == "" {
			continue // an entry with no id is not a model
		}
		if grokUnselectable(entry) {
			// oarlock.Model has no hidden/disabled field: the entry is dropped,
			// documented in the file comment, not mislabelled into Name.
			continue
		}
		name := firstText(entry["displayName"], entry["display_name"], entry["name"])
		if name == "" {
			name = id
		}
		list = append(list, oarlock.Model{Provider: provider, ID: id, Name: name, EffortLevels: grokEfforts(entry)})
	}
	return list, nil
}

// grokUnselectable reports the two ways grok says an entry is not something this
// account can run now.
func grokUnselectable(entry map[string]any) bool {
	if flag, ok := entry["hidden"].(bool); ok && flag {
		return true
	}
	for _, key := range []string{"user_selectable", "userSelectable"} {
		if flag, ok := entry[key].(bool); ok && !flag {
			return true
		}
	}
	return false
}

// grokEfforts reads one entry's reasoning menu. Unlike the session-scoped
// thought_level option this menu travels per model, so every entry that carries
// one states its own levels; `_meta.supportsReasoningEffort == false` is the
// vendor saying this model has none.
func grokEfforts(entry map[string]any) []string {
	meta := asMap(entry["_meta"])
	if supported, ok := meta["supportsReasoningEffort"].(bool); ok && !supported {
		return nil
	}
	raw, ok := meta["reasoningEfforts"]
	if !ok {
		raw, ok = meta["reasoning_efforts"]
	}
	if !ok {
		raw = entry["reasoningEfforts"] // the top-level spelling
	}
	var levels []string
	seen := map[string]bool{}
	for _, item := range asSlice(raw) {
		option := asMap(item)
		if option == nil {
			continue
		}
		value := firstText(option["id"], option["value"])
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		levels = append(levels, value)
	}
	return levels
}

// grokHandlerMessage is the one line the handler's own error envelope carries:
// an object with `message`, or a bare string.
func grokHandlerMessage(raw json.RawMessage) string {
	if problem := modelsObject(raw); problem != nil {
		if message := modelsDetail(asString(problem["message"])); message != "" {
			return message
		}
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if message := modelsDetail(text); message != "" {
			return message
		}
	}
	return "no message"
}

// ---- raw JSON helpers ----------------------------------------------------

// modelsObject decodes one JSON object. null, an absent field and a scalar are
// all "not an object".
func modelsObject(raw json.RawMessage) map[string]any {
	var object map[string]any
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	return object
}

// modelsRawObject decodes one JSON object without decoding its values: the
// envelope and catalogue fields are read as raw JSON, so a null and an absent
// key stay distinguishable.
func modelsRawObject(raw json.RawMessage) map[string]json.RawMessage {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	return object
}

// modelsArrays returns the first raw value that really is a JSON array.
func modelsArrays(primary, fallback json.RawMessage) ([]json.RawMessage, bool) {
	for _, raw := range []json.RawMessage{primary, fallback} {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '[' {
			continue
		}
		var list []json.RawMessage
		if json.Unmarshal(trimmed, &list) == nil {
			return list, true
		}
	}
	return nil, false
}

// modelsNull reports an absent or JSON null value.
func modelsNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// firstText is the first non-blank string among the candidate spellings.
func firstText(values ...any) string {
	for _, value := range values {
		if text := strings.TrimSpace(asString(value)); text != "" {
			return text
		}
	}
	return ""
}
