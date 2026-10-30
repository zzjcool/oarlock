package codex

// Models for Codex (issue #98): the catalogue the app-server says this build
// can run.
//
// Codex has no CLI surface that lists models (`codex --help` has none, and the
// Inventory section of ops.go says the same), so the only native source is the
// app-server's `model/list`. This file drives it on the same throwaway
// app-server accountusage.go already uses:
//
//	initialize     clientInfo oarlock/0.0.0, experimentalApi
//	initialized    notification
//	model/list     repeated while the reply carries a non-empty nextCursor
//
// and nothing else. `config/read` is deliberately NOT called: its only purpose
// in OAR is the "which model is the configured default" marker, and
// oarlock.Model has no default (nor disabled/resolved-id) field, so asking for
// a fact the output type cannot carry would invent a selection the host never
// made. For the same reason a `hidden` model is skipped and an absent
// displayName falls back to the model id instead of being dressed up as state.
//
// The transport is reused, not re-implemented: exec.Start, codexClient,
// codexReadLines, usageMaxLine, usageKillGrace, usageWaitDelay and
// codexRPCError are accountusage.go's, so the two reads of the same app-server
// cannot drift on argv, on the JSONL dialect, on id correlation, on frame
// skipping (notifications and server requests are ignored, a line that is not a
// JSON-RPC object is skipped) or on the one-line cap. Each read is one process,
// stopped and reaped on every path, timeout included.
//
// The failure taxonomy is ListModels', not account usage's: a reauth-class
// JSON-RPC error (ops.IsUsageKind(err, ops.UsageKindReauth)) is
// ErrNotAuthenticated, a build without model/list (-32601) is
// *oarlock.UnsupportedOperation, and everything else — a transport fault, a
// stream that ended before the answer, an unreadable reply — is a plain error
// wrapped "codex app-server model/list", never an empty list. A `model/list`
// result with no `data` array is one of those errors: "zero models" is only
// reported when codex itself answered `data: []`. A failed listing returns nil
// models; a successful one returns a slice, empty when codex named none.
//
// LIVE STATUS: UNVERIFIED. No codex binary on this host ever answered
// model/list; every branch below is exercised through a fake `codex` on PATH
// (models_test.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/exec"
	"github.com/zzjcool/oarlock/internal/ops"
)

const (
	// modelsTimeout bounds one whole listing: a catalogue that needs longer is a
	// timeout, not a slow success.
	modelsTimeout = 15 * time.Second
	// modelsMaxPages caps the pagination walk. model/list pages are server-
	// driven, so an app-server that keeps handing out cursors must fail the
	// listing instead of being followed forever.
	modelsMaxPages = 50

	// modelsProvider is oarlock's runtime id: model/list reports no vendor
	// provider per model, so every entry carries the runtime that answered.
	modelsProvider = "codex"
	// modelsOperation names the capability in *oarlock.UnsupportedOperation.
	modelsOperation = "list-models"
	// modelsNoModelList is the reason for a build that has no model/list.
	modelsNoModelList = "this codex build has no app-server model/list method"
	// modelsNoData is the answer to a result with no data array: an absent field
	// is not "zero models", it is a reply this reader cannot trust.
	modelsNoData = "codex model/list returned no data array"
	// modelsRepeated is the answer to a pagination cursor handed out twice.
	modelsRepeated = "codex model/list repeated a pagination cursor"
)

// ErrNotAuthenticated is ListModels' typed "the account cannot list models"
// failure: the runtime answered that no account is signed in, or that this
// account may not read the model catalogue. It is a typed error and never an
// empty list.
var ErrNotAuthenticated = errors.New("codex: not signed in")

// codexModels lists what the running codex build can run, as its app-server
// reports it. The zero value is what Runtime() installs: it finds `codex` the
// way the probe does, inherits the process environment and uses modelsTimeout.
type codexModels struct {
	Command string        // "" = `codex` from PATH or the installer's location
	Env     []string      // nil = os.Environ()
	Timeout time.Duration // 0 = modelsTimeout
}

var _ oarlock.ModelLister = codexModels{}

// ListModels reads the app-server's model catalogue. Every failure is an error:
// a signed-out account is ErrNotAuthenticated, a build without model/list is an
// UnsupportedOperation, and nothing here ever turns "did not answer" into an
// empty list.
func (x codexModels) ListModels(ctx context.Context) ([]oarlock.Model, error) {
	if ctx == nil {
		return nil, errors.New("codex: ListModels(nil context)")
	}
	command := x.Command
	if command == "" {
		// The same lookup the probe does, so a host that probed and a host that
		// lists disagree on nothing.
		found, err := findCodex()
		if err != nil {
			return nil, fmt.Errorf("codex app-server model/list: %w", err)
		}
		command = found
	}
	env := x.Env
	if env == nil {
		env = os.Environ()
	}
	timeout := x.Timeout
	if timeout <= 0 {
		timeout = modelsTimeout
	}
	return readCodexModels(ctx, command, env, timeout)
}

// ---- transport -----------------------------------------------------------

// readCodexModels runs the handshake and the paginated model/list on one
// throwaway app-server. One timeout covers the whole exchange.
func readCodexModels(ctx context.Context, command string, env []string, timeout time.Duration) ([]oarlock.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, err := exec.Start(ctx, &exec.Command{
		Path:      command,
		Args:      []string{"app-server", "--listen", "stdio://"},
		Env:       env,
		KillGrace: usageKillGrace,
		WaitDelay: usageWaitDelay,
	})
	if err != nil {
		return nil, codexModelsError(ctx, err, timeout)
	}
	// Claim both streams before anything can drain them (the seam ops.Run uses),
	// then drop stderr: a real app-server logs there, and a full pipe would stop
	// it mid-reply. Its output is never part of an error.
	_, _ = cmd.Stdout().Read(nil)
	_, _ = cmd.Stderr().Read(nil)
	go func() { _, _ = io.Copy(io.Discard, cmd.Stderr()) }()

	lines := make(chan codexLine)
	stop := make(chan struct{})
	go codexReadLines(cmd.Stdout(), lines, stop)

	// Nothing outlives this listing: the reader is released, the tree is stopped
	// (stdin closed, SIGTERM, then SIGKILL after the grace) and reaped.
	defer func() {
		close(stop)
		_ = cmd.Close(context.Background())
		cancel()
		_, _ = cmd.Wait(context.Background())
	}()

	client := &codexClient{stdin: cmd.Stdin(), lines: lines}
	if _, err := client.call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": ClientName, "version": ClientVersion},
		"capabilities": map[string]any{"experimentalApi": true},
	}); err != nil {
		return nil, codexModelsError(ctx, err, timeout)
	}
	if err := client.send(ctx, map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, codexModelsError(ctx, err, timeout)
	}
	models, err := collectCodexModels(ctx, client, timeout)
	if err != nil {
		return nil, err
	}
	// Honest empty is a slice: nil is this method's failure value.
	if models == nil {
		models = []oarlock.Model{}
	}
	return models, nil
}

// collectCodexModels walks model/list until the app-server reports no further
// page. The previous page's nextCursor is the only pagination input; a cursor
// handed out twice is a loop, and modelsMaxPages bounds a server that keeps
// inventing new ones.
func collectCodexModels(ctx context.Context, client *codexClient, timeout time.Duration) ([]oarlock.Model, error) {
	var models []oarlock.Model
	seen := make(map[string]bool)
	params := map[string]any{}
	for page := 1; ; page++ {
		if page > modelsMaxPages {
			return nil, fmt.Errorf("codex model/list did not finish within %d pages", modelsMaxPages)
		}
		result, err := client.call(ctx, "model/list", params)
		if err != nil {
			return nil, codexModelsError(ctx, err, timeout)
		}
		entries, next, err := decodeCodexModelPage(result)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if model, ok := codexModel(entry); ok {
				models = append(models, model)
			}
		}
		if next == "" {
			return models, nil
		}
		if seen[next] {
			return nil, errors.New(modelsRepeated)
		}
		seen[next] = true
		params = map[string]any{"cursor": next}
	}
}

// ---- projection ----------------------------------------------------------

// codexModelPage is one model/list result: the entries and the cursor of the
// next page (null or "" ends the listing).
type codexModelPage struct {
	Data       json.RawMessage `json:"data"`
	NextCursor *string         `json:"nextCursor"`
}

// codexModelEntry is one entry of data[]. Every field is optional on the wire;
// an entry with no usable model id is skipped, never emitted as "".
type codexModelEntry struct {
	Model       string             `json:"model"`
	DisplayName string             `json:"displayName"`
	Hidden      bool               `json:"hidden"`
	Efforts     []codexModelEffort `json:"supportedReasoningEfforts"`
	Tiers       []codexModelTier   `json:"serviceTiers"`
}

// codexModelEffort is the one field this adapter reads from an effort entry.
type codexModelEffort struct {
	ReasoningEffort string `json:"reasoningEffort"`
}

// codexModelTier is the one field this adapter reads from a service tier.
type codexModelTier struct {
	ID string `json:"id"`
}

// decodeCodexModelPage reads one model/list result: the entries and the next
// cursor ("" = none). A result without a data array is not an answer; an entry
// this client cannot parse is skipped rather than failing the catalogue it sits
// in, because one odd entry is not the app-server refusing to answer.
func decodeCodexModelPage(result json.RawMessage) ([]codexModelEntry, string, error) {
	var page codexModelPage
	if err := json.Unmarshal(result, &page); err != nil {
		return nil, "", errors.New(modelsNoData)
	}
	data := bytes.TrimSpace(page.Data)
	if len(data) == 0 || data[0] != '[' {
		return nil, "", errors.New(modelsNoData)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, "", errors.New(modelsNoData)
	}
	entries := make([]codexModelEntry, 0, len(raw))
	for _, item := range raw {
		var entry codexModelEntry
		if json.Unmarshal(item, &entry) != nil {
			continue
		}
		entries = append(entries, entry)
	}
	next := ""
	if page.NextCursor != nil {
		next = strings.TrimSpace(*page.NextCursor)
	}
	return entries, next, nil
}

// codexModel projects one entry. A hidden model is skipped and an entry with no
// usable id cannot be selected, so both are left out; an absent displayName
// falls back to the id.
func codexModel(entry codexModelEntry) (oarlock.Model, bool) {
	if entry.Hidden {
		return oarlock.Model{}, false
	}
	id := strings.TrimSpace(entry.Model)
	if id == "" {
		return oarlock.Model{}, false
	}
	name := strings.TrimSpace(entry.DisplayName)
	if name == "" {
		name = id
	}
	return oarlock.Model{
		Provider:     modelsProvider,
		ID:           id,
		Name:         name,
		EffortLevels: codexModelEfforts(entry.Efforts),
		ServiceTiers: codexModelTiers(entry.Tiers),
	}, true
}

// codexModelEfforts takes the reasoning efforts in wire order, without repeats.
// An array codex sent is echoed as a slice — empty when it sent [] — and an
// absent or null one stays nil: the difference is codex's, not ours.
func codexModelEfforts(efforts []codexModelEffort) []string {
	if efforts == nil {
		return nil
	}
	levels := make([]string, 0, len(efforts))
	seen := make(map[string]bool, len(efforts))
	for _, effort := range efforts {
		level := strings.TrimSpace(effort.ReasoningEffort)
		if level == "" || seen[level] {
			continue
		}
		seen[level] = true
		levels = append(levels, level)
	}
	return levels
}

// codexModelTiers takes the service tier ids in wire order, without repeats. A
// tier that names no id is not a selectable tier and is skipped; the presence
// rule is the one codexModelEfforts documents.
func codexModelTiers(tiers []codexModelTier) []string {
	if tiers == nil {
		return nil
	}
	ids := make([]string, 0, len(tiers))
	seen := make(map[string]bool, len(tiers))
	for _, tier := range tiers {
		id := strings.TrimSpace(tier.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// ---- failure classification ----------------------------------------------

// codexModelsError translates a failure of the shared app-server client into
// ListModels' taxonomy. codexRPCError has already classified the frame and
// dropped the vendor text, so this only picks the caller-facing type: reauth is
// ErrNotAuthenticated, a build without model/list is an UnsupportedOperation,
// a stopped context is named as the timeout it is, and everything else — the
// transport fault of a stream that ended before the answer included — is a
// plain wrapped error.
func codexModelsError(ctx context.Context, err error, timeout time.Duration) error {
	if ops.IsUsageKind(err, ops.UsageKindReauth) {
		// The only text available is the client's stable reason code: the vendor
		// message is never carried, so ErrNotAuthenticated's detail cannot leak
		// one.
		detail := ""
		var usageErr *ops.UsageError
		if errors.As(err, &usageErr) {
			detail = usageErr.Reason
		}
		if detail == "" {
			detail = "the account cannot list models"
		}
		return fmt.Errorf("%w: %s", ErrNotAuthenticated, detail)
	}
	if ops.IsUsageKind(err, ops.UsageKindUnsupported) {
		return &oarlock.UnsupportedOperation{Operation: modelsOperation, Reason: modelsNoModelList}
	}
	if ops.IsUsageKind(err, ops.UsageKindTimeout) || ctx.Err() != nil {
		// The shared client classifies a stopped context as a timeout, so the
		// listing names the bound it ran out of.
		return fmt.Errorf("codex app-server model/list: timed out after %s: %w", timeout, err)
	}
	return fmt.Errorf("codex app-server model/list: %w", err)
}
