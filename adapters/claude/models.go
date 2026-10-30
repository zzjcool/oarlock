package claude

// Models for Claude (issue #98): the catalogue claude answers on its own
// stream-json control channel.
//
// There is no local read of the catalogue: none of the commands this adapter
// already runs (`claude --version`, `claude auth *`, `claude update`) says what a
// subscription may run, and the answer comes from a `list_models` control
// request — the same dialect as the `get_usage` request open.go sends — so this
// file starts one throwaway `claude -p` process, writes one line to stdin, reads
// until the matching answer, and then stops the tree. It reuses the spawn,
// framing and teardown style of accountusage.go (and of adapters/codex's
// app-server read) rather than inventing a second one: exec.Start with a kill
// grace and a wait delay, `Read(nil)` to claim both streams before anything
// drains them, stderr drained so a loud build cannot block on a full pipe (the
// bytes are discarded, never quoted), and one deferred stop that closes stdin,
// asks the tree to exit, escalates to SIGKILL after the grace and reaps it. No
// session, no kernel, no records, nothing written.
//
// The argv is claude's own, and everything after --verbose is mandatory:
//
//	-p --input-format stream-json --output-format stream-json --verbose
//	--settings {"disableAllHooks":true} --strict-mcp-config --mcp-config {"mcpServers":{}}
//
// --verbose is required next to --output-format stream-json in print mode, and
// without the settings/MCP pair the CLI fires SessionStart hooks and starts
// every configured MCP server before it answers a listing — work a model read
// must not cause.
//
// Exactly one line goes to stdin:
//
//	{"type":"control_request","request_id":"oarlock-models-<uuid>","request":{"subtype":"list_models"}}
//
// and the answer is the one `control_response` whose `response.request_id` is
// that id. Hook traffic, claude's own `control_request`s, non-JSON banners and
// another exchange's answer all share this channel and are skipped: matching on
// the id is what makes the read independent of whatever else the CLI emits.
//
// The session-json `list_models` success payload is projected like this:
//
//   - `value` -> ID. A blank value is not a model, so the entry is skipped
//     instead of being emitted as "".
//   - `displayName`, else `resolvedModel`, else `value` -> Name.
//   - `supportedEffortLevels[]` -> EffortLevels. An entry is either a bare level
//     string ("low") or an object carrying it under `effort` or `id`; a blank
//     level is skipped.
//   - `supportsFastMode == true` -> ServiceTiers ["fast"], else nil.
//   - `disabled == true` -> the entry is skipped.
//   - Provider -> "claude".
//
// A disabled model is dropped rather than reported, because oarlock.Model has no
// "not selectable" field: an entry a host could see but never select would claim
// a model this account cannot run. The same rule drops the rest of the vendor's
// bookkeeping (the `description` text, and `resolvedModel` when a display name
// exists) instead of dressing it up as state.
//
// `models: []` is an answer — claude said this account has none — and comes back
// as an empty slice with a nil error. Everything else is an error, and a failure
// always returns nil models:
//
//   - a refusal whose text carries an anchored auth phrase — "logged in" /
//     "log-in", "auth"/"authentication"/"authorization", "api key" — is
//     ErrNotAuthenticated, never an empty list (a signed-out account has an
//     unknown catalogue, not zero models). A bare substring does not count:
//     "unknown author field" is a request complaint, not a sign-out;
//   - "Unsupported control request subtype" is *oarlock.UnsupportedOperation,
//     because this build does not answer list_models at all;
//   - any other refusal text is a plain error carrying the vendor's line;
//   - a success reply with no models array is a plain error: an absent field is
//     not "zero models";
//   - a stream that ends before a matching answer is
//     "claude list_models: exited before answering";
//   - the modelsTimeout bound (15 s) is "claude list_models: timed out after
//     <bound>".
//
// LIVE STATUS: UNVERIFIED. No claude binary on this host ever answered
// `list_models`; every branch below is exercised through the fake `claude` on
// PATH the ops tests already use (models_test.go).

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/exec"
)

const (
	// modelsTimeout bounds the whole exchange: start, one request line, one
	// answer (OAR: 15 s). A catalogue that needs longer is a timeout, not a slow
	// success.
	modelsTimeout = 15 * time.Second

	// modelsKillGrace is the polite stop of the throwaway process: SIGTERM, then
	// SIGKILL after this long.
	modelsKillGrace = 2 * time.Second
	// modelsWaitDelay bounds the drain of the output pipes after it exits.
	modelsWaitDelay = time.Second
	// modelsReadChunk is stdout's initial buffer and modelsMaxLine the longest
	// single frame this reader accepts (a catalogue is one line).
	modelsReadChunk = 64 << 10
	modelsMaxLine   = 16 << 20

	// modelsProvider is oarlock's runtime id: claude's catalogue names no vendor
	// provider per entry, so every model carries the runtime that answered.
	modelsProvider = "claude"
	// modelsOperation names the capability in *oarlock.UnsupportedOperation.
	modelsOperation = "list-models"
	// modelsRequestIDPrefix marks the one request this file sends. The id is the
	// only thing an answer is matched on.
	modelsRequestIDPrefix = "oarlock-models-"
	// modelsSubtype is the control-request subtype a build must answer.
	modelsSubtype = "list_models"
	// modelsUnsupportedSubtype is the lower-cased fragment of claude's refusal
	// for a build that has no such subtype. It is the positive evidence behind
	// the *oarlock.UnsupportedOperation branch.
	modelsUnsupportedSubtype = "unsupported control request subtype"
	// modelsNoSubtype is that branch's reason, in one line.
	modelsNoSubtype = "this claude build has no list_models control request subtype"
	// modelsNoCatalogue is the answer to a success reply with no models array:
	// the field is optional on the wire, but its absence is not "zero models".
	modelsNoCatalogue = "claude list_models: success reply carried no models array"
)

// ErrNotAuthenticated is ListModels' typed "the account cannot list models"
// failure: the runtime answered that no account is signed in, or that this
// account may not read the model catalogue. It is a typed error and never an
// empty list.
var ErrNotAuthenticated = errors.New("claude: not signed in")

// modelsNotSignedIn is claude's "no account" refusal, as the contract freezes
// it. It is matched case-insensitively against the vendor's own line and is the
// only prose this file reads; every other refusal is a plain error.
//
// Every alternative is word-anchored on both ends, because a bare substring
// classifies the wrong lines: "auth" inside "unknown author field" and "log in"
// inside prose about logging describe the request, not the account. The lexical
// variants claude actually uses are the ones spelled out here — "log in" /
// "logged in" / "log-in", the auth word family, and "api key".
var modelsNotSignedIn = regexp.MustCompile(`(?i)\blog(ged)?[ -]?in\b|\bauth(?:entication|orization)?\b|\bapi key\b`)

// claudeModels lists what the running claude build can run, as its stream-json
// control channel reports it. The zero value is what Runtime() installs: it
// finds `claude` the way the installation probe does, inherits the process
// environment and uses modelsTimeout.
type claudeModels struct {
	// Timeout bounds the whole exchange; 0 means modelsTimeout. It is a seam so a
	// test can drive a real timeout without waiting 15 s.
	Timeout time.Duration
}

var _ oarlock.ModelLister = claudeModels{}

// ListModels reads claude's catalogue once. Every failure is an error: a
// signed-out account is ErrNotAuthenticated, a build without the list_models
// subtype is an UnsupportedOperation, and nothing here ever turns "did not
// answer" into an empty list.
func (m claudeModels) ListModels(ctx context.Context) ([]oarlock.Model, error) {
	if ctx == nil {
		return nil, errors.New("claude list_models: nil context")
	}
	// The same lookup the probe and the ops commands use, so a host that probed
	// and a host that lists disagree on nothing.
	path, err := resolveBinary()
	if err != nil {
		return nil, err
	}
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = modelsTimeout
	}
	return readClaudeModels(ctx, path, timeout)
}

// ---- transport -----------------------------------------------------------

// readClaudeModels runs one listing on one throwaway `claude -p` process. One
// timeout covers the whole exchange.
func readClaudeModels(ctx context.Context, path string, timeout time.Duration) ([]oarlock.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, err := exec.Start(ctx, &exec.Command{
		Path: path,
		Args: []string{
			"-p",
			"--input-format", "stream-json",
			"--output-format", "stream-json",
			"--verbose",
			"--settings", `{"disableAllHooks":true}`,
			"--strict-mcp-config",
			"--mcp-config", `{"mcpServers":{}}`,
		},
		Env:       os.Environ(),
		KillGrace: modelsKillGrace,
		WaitDelay: modelsWaitDelay,
	})
	if err != nil {
		return nil, claudeModelsError(ctx, err, timeout)
	}
	// Claim both streams before anything can drain them (the seam ops.Run uses).
	// stdout is read below; stderr is drained and discarded because a real claude
	// logs there and a full pipe would stop it mid-answer. The bytes are not kept:
	// every message this file returns is one frozen line, and vendor stderr can
	// quote the account, so the only safe use for it is a drain.
	_, _ = cmd.Stdout().Read(nil)
	_, _ = cmd.Stderr().Read(nil)
	go func() { _, _ = io.Copy(io.Discard, cmd.Stderr()) }()

	// Nothing outlives this listing: stdout is released (the answer is in hand,
	// so Wait must not sit out the drain bound waiting for a reader that has
	// already stopped), stdin closed, SIGTERM, SIGKILL after the grace, reaped.
	defer func() {
		_ = cmd.Stdout().Close()
		_ = cmd.Close(context.Background())
		cancel()
		_, _ = cmd.Wait(context.Background())
	}()

	requestID := modelsRequestIDPrefix + newUUID()
	request := modelsRequest{Type: "control_request", RequestID: requestID}
	request.Request.Subtype = modelsSubtype
	line, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("claude list_models: %w", err)
	}
	// A failed write means the child is already gone; the stream end is the
	// answer to that, and it is reported below as "exited before answering".
	_, _ = cmd.Stdin().Write(append(line, '\n'))

	scanner := bufio.NewScanner(cmd.Stdout())
	scanner.Buffer(make([]byte, 0, modelsReadChunk), modelsMaxLine)
	for scanner.Scan() {
		answer, ok := modelsAnswer(scanner.Bytes(), requestID)
		if !ok {
			// A hook frame, claude's own control_request, a non-JSON banner or
			// another exchange's answer: not this read's business.
			continue
		}
		return answer.project()
	}
	if err := ctx.Err(); err != nil {
		// The tree is already being torn down; readClaudeModels' defer finishes it.
		return nil, claudeModelsError(ctx, err, timeout)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("claude list_models: unreadable stream: %w", err)
	}
	return nil, errors.New("claude list_models: exited before answering")
}

// modelsRequest is the one line written to claude's stdin. It is a struct, not a
// map, so the keys go out in the order the contract spells them; the request id
// names this exchange and is the only thing the answer is matched on.
type modelsRequest struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Request   struct {
		Subtype string `json:"subtype"`
	} `json:"request"`
}

// modelsFrame is one stream-json frame as far as a listing cares: its type (only
// control_response counts), the response's own request_id (the answer is matched
// on it alone), and the raw body of whichever subtype it carries. Frames are one
// JSON object per line — that is claude's framing, not pretty-printed JSON split
// across lines.
//
// The RawMessage fields point into the buffer modelsAnswer was handed, so a
// frame must be projected before the next frame is read.
type modelsFrame struct {
	Type     string `json:"type"`
	Response struct {
		Subtype   string          `json:"subtype"`
		RequestID string          `json:"request_id"`
		Error     json.RawMessage `json:"error"`
		Message   json.RawMessage `json:"message"`
		Response  json.RawMessage `json:"response"`
	} `json:"response"`
}

// modelsAnswer parses raw and reports whether it is the answer to requestID.
// Every other line is ignored: another request_id is another exchange's answer,
// a frame with no type is not a control_response, and a line that is not JSON is
// not a frame at all.
func modelsAnswer(raw []byte, requestID string) (modelsFrame, bool) {
	var frame modelsFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		return modelsFrame{}, false
	}
	if frame.Type != "control_response" || frame.Response.RequestID != requestID {
		return modelsFrame{}, false
	}
	return frame, true
}

// project turns the matching answer into models or a typed failure.
func (f modelsFrame) project() ([]oarlock.Model, error) {
	switch f.Response.Subtype {
	case "success":
		return f.catalogue()
	case "error":
		return nil, claudeRefusal(modelsText(f.Response.Error), modelsText(f.Response.Message))
	default:
		return nil, fmt.Errorf("claude list_models: unknown control_response subtype %q", f.Response.Subtype)
	}
}

// catalogue maps a success reply's models array. A success reply with no models
// array is an error, not an empty list: only claude saying `[]` may come back as
// zero models.
func (f modelsFrame) catalogue() ([]oarlock.Model, error) {
	if len(f.Response.Response) == 0 {
		return nil, errors.New(modelsNoCatalogue)
	}
	var payload struct {
		// A pointer distinguishes "no models array" from "an empty one".
		Models *[]claudeModel `json:"models"`
	}
	if err := json.Unmarshal(f.Response.Response, &payload); err != nil {
		return nil, fmt.Errorf("claude list_models: unreadable catalogue: %w", err)
	}
	if payload.Models == nil {
		return nil, errors.New(modelsNoCatalogue)
	}
	models := make([]oarlock.Model, 0, len(*payload.Models))
	for _, entry := range *payload.Models {
		if model, ok := entry.project(); ok {
			models = append(models, model)
		}
	}
	return models, nil
}

// modelsText reads a vendor string that may be absent, null or not a string at
// all. Anything but a JSON string is "", so the next candidate is used instead
// of a Go-formatted payload.
func modelsText(raw json.RawMessage) string {
	var text string
	if len(raw) == 0 || json.Unmarshal(raw, &text) != nil {
		return ""
	}
	return text
}

// ---- projection ----------------------------------------------------------

// claudeModel is one entry of the success reply's models array. Every field is
// optional on the wire; an entry with no usable value is skipped, never emitted
// as "".
type claudeModel struct {
	Value         string            `json:"value"`
	ResolvedModel string            `json:"resolvedModel"`
	DisplayName   string            `json:"displayName"`
	Efforts       []json.RawMessage `json:"supportedEffortLevels"`
	SupportsFast  bool              `json:"supportsFastMode"`
	Disabled      bool              `json:"disabled"`
}

// project maps one entry. A disabled model and an entry with no usable value are
// both left out: oarlock.Model has no field to say "not selectable", and "" names
// nothing a host could run.
func (e claudeModel) project() (oarlock.Model, bool) {
	if e.Disabled {
		return oarlock.Model{}, false
	}
	id := strings.TrimSpace(e.Value)
	if id == "" {
		return oarlock.Model{}, false
	}
	model := oarlock.Model{
		Provider: modelsProvider,
		ID:       id,
		Name:     firstNonBlank(e.DisplayName, e.ResolvedModel, id),
	}
	for _, raw := range e.Efforts {
		if level := effortLevel(raw); level != "" {
			model.EffortLevels = append(model.EffortLevels, level)
		}
	}
	if e.SupportsFast {
		model.ServiceTiers = []string{"fast"}
	}
	return model, true
}

// effortLevel reads one supportedEffortLevels entry: the vendor sends either a
// bare level string or an object carrying the level under `effort` or `id`. A
// level this reader cannot name is skipped rather than echoed as "".
func effortLevel(raw json.RawMessage) string {
	var level string
	if err := json.Unmarshal(raw, &level); err == nil {
		return strings.TrimSpace(level)
	}
	var object struct {
		Effort string `json:"effort"`
		ID     string `json:"id"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return ""
	}
	return firstNonBlank(object.Effort, object.ID)
}

// firstNonBlank is the first candidate with content, trimmed; "" when none has
// any. A whitespace-only vendor value names nothing and must not win the
// fallback chain.
func firstNonBlank(candidates ...string) string {
	for _, candidate := range candidates {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// ---- failure classification ----------------------------------------------

// claudeRefusal classifies claude's error reply: a build without the list_models
// subtype, an account that is not signed in, or anything else. The text is the
// `error` field, else the `message` field; a missing one is not a success.
func claudeRefusal(candidates ...string) error {
	text := firstNonBlank(candidates...)
	if text != "" && strings.Contains(strings.ToLower(text), modelsUnsupportedSubtype) {
		return &oarlock.UnsupportedOperation{
			Operation: modelsOperation,
			Reason:    modelsNoSubtype + ": " + boundedDetail(text),
		}
	}
	if modelsNotSignedIn.MatchString(text) {
		return fmt.Errorf("%w: %s", ErrNotAuthenticated, boundedDetail(text))
	}
	return fmt.Errorf("claude list_models: %s", boundedDetail(text))
}

// claudeModelsError names a stopped exchange. A context that ran out is reported
// as the timeout it is (the bound modelsTimeout set, or the caller's shorter
// one); anything else keeps the cause.
func claudeModelsError(ctx context.Context, err error, timeout time.Duration) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("claude list_models: timed out after %s: %w", timeout, err)
	}
	return fmt.Errorf("claude list_models: %w", err)
}

// boundedDetail keeps a vendor line to the same 400 bytes the ops errors carry
// (outputTailLimit, ops.go), on one line: an error message is the one place a
// refusal is visible, and a multi-line payload must not become it.
//
// The cut walks back to a rune boundary before it slices, so a detail in a
// multi-byte script never emits half a rune: an error carrying invalid UTF-8 is
// not a message a host can print. The walk is the same one clipVersion and
// clipOutput do.
func boundedDetail(detail string) string {
	detail = strings.Join(strings.Fields(detail), " ")
	if detail == "" {
		return "(no detail)"
	}
	if len(detail) > outputTailLimit {
		cut := outputTailLimit
		for cut > 0 && !utf8.RuneStart(detail[cut]) {
			cut--
		}
		detail = detail[:cut] + "..."
	}
	return detail
}
