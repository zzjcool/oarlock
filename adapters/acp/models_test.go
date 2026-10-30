package acp

// Models listing, fake agent only (issue #98). The harness is the one
// helpers_test.go uses for a session: this test binary is the agent. The one
// difference is the lookup — a listing resolves the binary the way the probe
// does (PATH, then the vendor's install location), so the fake is staged on
// PATH under the profile's own name instead of being handed in as an
// Installation.
//
// No live account, no third-party binary: every case drives the in-process
// fake through ACP_FAKE_MODELS (fake_test.go), so what is proved here is the
// mapping and the error taxonomy, not a vendor's behaviour.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
)

// modelsProfileIDs is the whole family: one implementation, six runtime ids.
var modelsProfileIDs = []string{"grok", "kimi", "opencode", "qwen", "qoder", "antigravity"}

// modelsProfile is the real profile with the harness argv: the id (and so the
// extension branch) stays, the vendor argv becomes the test binary's own.
func modelsProfile(t *testing.T, id string) Profile {
	t.Helper()
	for _, p := range Profiles() {
		if p.ID == id {
			p.Args = func(oarlock.Options) []string { return []string{"-test.run=^$"} }
			return p
		}
	}
	t.Fatalf("no profile %q in the family", id)
	return Profile{}
}

// fakeOnPath stages this test binary under the profile's name, so the lister's
// own probe lookup finds it exactly where a real installation would be.
func fakeOnPath(t *testing.T, id string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	dir := t.TempDir()
	name := id
	if runtime.GOOS == "windows" {
		name += ".exe" // the PATH lookup on Windows needs the extension
	}
	target := filepath.Join(dir, name)
	if err := os.Symlink(exe, target); err != nil {
		// A host that forbids symlinks still gets the same bytes.
		data, readErr := os.ReadFile(exe)
		if readErr != nil {
			t.Fatalf("stage the fake agent: %v; %v", err, readErr)
		}
		if writeErr := os.WriteFile(target, data, 0o755); writeErr != nil {
			t.Fatalf("stage the fake agent: %v", writeErr)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// listModels runs one listing through the same Runtime the CLI calls.
func listModels(t *testing.T, id, mode string) ([]oarlock.Model, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return listModelsCtx(t, ctx, id, mode)
}

// listModelsCtx is listModels with the caller's own deadline.
func listModelsCtx(t *testing.T, ctx context.Context, id, mode string) ([]oarlock.Model, error) {
	t.Helper()
	prof := modelsProfile(t, id)
	t.Setenv("ACP_FAKE_AGENT", id)
	t.Setenv("ACP_FAKE_MODELS", mode)
	fakeOnPath(t, id)
	return prof.Runtime().Models.ListModels(ctx)
}

func assertModels(t *testing.T, got, want []oarlock.Model) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("models = %+v\nwant      %+v", got, want)
	}
}

// TestListModelsSessionConfigOptions is the §5 success row for the five
// profiles whose catalogue is the session/new configOptions: two or more
// models, provider/id/name correct, the grouped options list flattened, the
// unnamed entry named by its value, and the thought_level menu attached only to
// the selected model with the literal "off" dropped.
func TestListModelsSessionConfigOptions(t *testing.T) {
	for _, id := range modelsProfileIDs {
		if id == Grok.ID {
			continue // grok answers its own extension: next test
		}
		t.Run(id, func(t *testing.T) {
			got, err := listModels(t, id, "ok")
			if err != nil {
				t.Fatalf("ListModels: %v", err)
			}
			assertModels(t, got, []oarlock.Model{
				{Provider: id, ID: "opus", Name: "Opus", EffortLevels: []string{"low", "high"}},
				{Provider: id, ID: "sonnet", Name: "Sonnet"},
				{Provider: id, ID: "haiku", Name: "haiku"},
			})
		})
	}
}

// TestListModelsGrokExtension is the same row for grok: its own
// double-enveloped `_x.ai/models/list` payload, hidden and non-selectable
// entries dropped, and the reasoning menu attached per model.
func TestListModelsGrokExtension(t *testing.T) {
	got, err := listModels(t, Grok.ID, "ok")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	assertModels(t, got, []oarlock.Model{
		{Provider: "grok", ID: "grok-4", Name: "Grok 4", EffortLevels: []string{"low", "high"}},
		{Provider: "grok", ID: "grok-3", Name: "Grok 3"},
		{Provider: "grok", ID: "grok-mini", Name: "grok-mini", EffortLevels: []string{"medium"}},
	})
}

// TestListModelsGrokFallsBackToSessionNew: a build without the extension
// answers -32601, which is not a failure — session/new carries the same
// catalogue, in the model-option spelling that omits `category`.
func TestListModelsGrokFallsBackToSessionNew(t *testing.T) {
	got, err := listModels(t, Grok.ID, "plain")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	assertModels(t, got, []oarlock.Model{
		{Provider: "grok", ID: "m1", Name: "Model One"},
		{Provider: "grok", ID: "m2", Name: "Model Two"},
	})
}

// TestListModelsCategoryOmitted covers the second spelling on a profile that
// never speaks the extension.
func TestListModelsCategoryOmitted(t *testing.T) {
	got, err := listModels(t, "kimi", "plain")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	assertModels(t, got, []oarlock.Model{
		{Provider: "kimi", ID: "m1", Name: "Model One"},
		{Provider: "kimi", ID: "m2", Name: "Model Two"},
	})
}

// TestListModelsGrokHandlerError: the extension's own envelope can carry a
// handler error. That is a plain error — not a fallback, not an empty list.
func TestListModelsGrokHandlerError(t *testing.T) {
	got, err := listModels(t, Grok.ID, "handler-error")
	if got != nil {
		t.Fatalf("models = %+v on failure, want nil", got)
	}
	if err == nil || !strings.Contains(err.Error(), "grok models/list handler error: models handler exploded") {
		t.Fatalf("err = %v, want the handler error", err)
	}
}

// TestListModelsHonestEmpty: the runtime answered "zero models". That is data,
// not an error, and nothing is invented to fill the list — but it is a slice:
// nil is this method's failure value, so an honest empty must not look like one.
func TestListModelsHonestEmpty(t *testing.T) {
	for _, id := range modelsProfileIDs {
		t.Run(id, func(t *testing.T) {
			got, err := listModels(t, id, "empty")
			if err != nil {
				t.Fatalf("ListModels: %v", err)
			}
			if got == nil {
				t.Fatal("models = nil, want the non-nil empty slice")
			}
			assertModels(t, got, []oarlock.Model{})
		})
	}
}

// TestListModelsMissingOptionsArray: a model config option with no `options` key
// at all is a field this build cannot read, not "zero models". The contrast is
// the test above: an explicit `"options": []` is the honest empty, an absent
// array is a loud failure — otherwise a rename would make the CLI print
// "(no models)" and be believed.
func TestListModelsMissingOptionsArray(t *testing.T) {
	for _, id := range modelsProfileIDs {
		t.Run(id, func(t *testing.T) {
			got, err := listModels(t, id, "no-options")
			if got != nil {
				t.Fatalf("models = %+v on failure, want nil", got)
			}
			if err == nil || !strings.Contains(err.Error(), "the model config option carried no options array") {
				t.Fatalf("err = %v, want the missing-options-array error", err)
			}
			if !strings.Contains(err.Error(), "acp: "+id+": session/new:") {
				t.Errorf("err = %v, want the runtime and the method named", err)
			}
		})
	}
}

// TestListModelsBlankIDDropped: an entry with no usable id is skipped, never
// emitted as "".
func TestListModelsBlankIDDropped(t *testing.T) {
	for _, id := range modelsProfileIDs {
		t.Run(id, func(t *testing.T) {
			got, err := listModels(t, id, "missing-id")
			if err != nil {
				t.Fatalf("ListModels: %v", err)
			}
			if id == Grok.ID {
				assertModels(t, got, []oarlock.Model{{Provider: "grok", ID: "grok-4", Name: "Grok 4"}})
				return
			}
			assertModels(t, got, []oarlock.Model{
				{Provider: id, ID: "opus", Name: "Opus"},
				{Provider: id, ID: "sonnet", Name: "Sonnet"},
			})
		})
	}
}

// TestListModelsNoModelOption: a build with no model config option has no
// catalogue surface, so it is an UnsupportedOperation — never an empty list.
// Grok's own method answering without an availableModels array is the other
// shape of "no catalogue": a plain error, because an absent array is not an
// answer.
func TestListModelsNoModelOption(t *testing.T) {
	for _, id := range modelsProfileIDs {
		t.Run(id, func(t *testing.T) {
			got, err := listModels(t, id, "no-model-option")
			if got != nil {
				t.Fatalf("models = %+v on failure, want nil", got)
			}
			if id == Grok.ID {
				if err == nil || !strings.Contains(err.Error(), "grok models/list returned no availableModels array") {
					t.Fatalf("err = %v, want the missing-array error", err)
				}
				return
			}
			var unsupported *oarlock.UnsupportedOperation
			if !errors.As(err, &unsupported) {
				t.Fatalf("err = %v, want *oarlock.UnsupportedOperation", err)
			}
			if unsupported.Operation != "list-models" {
				t.Errorf("Operation = %q, want list-models", unsupported.Operation)
			}
			if !strings.Contains(unsupported.Reason, "no model config option on session/new") {
				t.Errorf("Reason = %q, want the missing model option", unsupported.Reason)
			}
		})
	}
}

// TestListModelsMethodNotFound: -32601 is the build saying it has no such
// method. For the five that is the listing surface; grok's extension falls back
// first and session/new then says it too. Either way: UnsupportedOperation.
func TestListModelsMethodNotFound(t *testing.T) {
	for _, id := range modelsProfileIDs {
		t.Run(id, func(t *testing.T) {
			got, err := listModels(t, id, "no-models-method")
			if got != nil {
				t.Fatalf("models = %+v on failure, want nil", got)
			}
			var unsupported *oarlock.UnsupportedOperation
			if !errors.As(err, &unsupported) {
				t.Fatalf("err = %v, want *oarlock.UnsupportedOperation", err)
			}
			if !strings.Contains(unsupported.Reason, id) || !strings.Contains(unsupported.Reason, "session/new") {
				t.Errorf("Reason = %q, want the runtime and the method", unsupported.Reason)
			}
		})
	}
}

// TestListModelsNotAuthenticated: both spellings of "no account" — the -32000
// code with an auth message, and a plain message that says logged in — are the
// typed sentinel, never an empty catalogue. The -32000 code is what kimi sends
// for "Authentication required", and the wording is what makes it an account
// problem; the next test is the same code with no such claim.
func TestListModelsNotAuthenticated(t *testing.T) {
	for _, id := range modelsProfileIDs {
		for _, mode := range []string{"unauth", "unauth-text"} {
			t.Run(id+"/"+mode, func(t *testing.T) {
				got, err := listModels(t, id, mode)
				if got != nil {
					t.Fatalf("models = %+v on failure, want nil", got)
				}
				if !errors.Is(err, ErrNotAuthenticated) {
					t.Fatalf("err = %v, want ErrNotAuthenticated", err)
				}
			})
		}
	}
}

// TestListModelsServerErrorIsNotAuth: -32000 is the vendors' catch-all code, so
// the code alone proves nothing. A `-32000 "Internal error: database is
// locked"` is a broken runtime, not a signed-out account, and it must not be
// reported as "not signed in". Neither is it a missing listing surface.
func TestListModelsServerErrorIsNotAuth(t *testing.T) {
	for _, id := range modelsProfileIDs {
		t.Run(id, func(t *testing.T) {
			got, err := listModels(t, id, "server-error")
			if got != nil {
				t.Fatalf("models = %+v on failure, want nil", got)
			}
			if err == nil {
				t.Fatal("ListModels succeeded, want the vendor's server error")
			}
			if errors.Is(err, ErrNotAuthenticated) {
				t.Fatalf("err = %v, want a plain error, not the not-signed-in sentinel", err)
			}
			var unsupported *oarlock.UnsupportedOperation
			if errors.As(err, &unsupported) {
				t.Fatalf("err = %v, want a plain error, not UnsupportedOperation", err)
			}
			if !strings.Contains(err.Error(), "Internal error: database is locked") {
				t.Errorf("err = %v, want the vendor's message quoted", err)
			}
		})
	}
}

// TestListModelsRequestMethods pins the handshake and the teardown by name: the
// fake records every method it received (ACP_FAKE_METHODS_SEEN), so deleting a
// call — the profile's `authenticate`, the best-effort `session/close` — turns
// this red even though the catalogue still comes back.
//
// grok: initialize, then authenticate (its profile names `cached_token` out of
// the initialize answer), then its own extension — no session is opened.
// kimi: initialize, session/new, session/close; no authenticate, because its
// AuthMethod only names the `login` method and this build offers none.
// opencode: initialize, session/new, session/close; it declares no AuthMethod at
// all, so an authenticate here would be an invented call.
func TestListModelsRequestMethods(t *testing.T) {
	cases := []struct {
		id   string
		mode string
		want []string
	}{
		{Grok.ID, "ok", []string{"initialize", "authenticate", modelsGrokModelsMethod}},
		{Grok.ID, "plain", []string{"initialize", "authenticate", modelsGrokModelsMethod, "session/new", "session/close"}},
		{"kimi", "ok", []string{"initialize", "session/new", "session/close"}},
		{"opencode", "ok", []string{"initialize", "session/new", "session/close"}},
	}
	for _, c := range cases {
		t.Run(c.id+"/"+c.mode, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "methods")
			t.Setenv("ACP_FAKE_METHODS_SEEN", file)
			if _, err := listModels(t, c.id, c.mode); err != nil {
				t.Fatalf("ListModels: %v", err)
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("the fake agent recorded no methods: %v", err)
			}
			got := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("methods = %v\nwant       %v", got, c.want)
			}
		})
	}
}

// TestListModelsProcessExits: a process that dies before answering is an error,
// never an empty catalogue.
func TestListModelsProcessExits(t *testing.T) {
	for _, id := range modelsProfileIDs {
		t.Run(id, func(t *testing.T) {
			got, err := listModels(t, id, "exit")
			if got != nil {
				t.Fatalf("models = %+v on failure, want nil", got)
			}
			if err == nil || !strings.Contains(err.Error(), "process exited before answering") {
				t.Fatalf("err = %v, want the before-answering error", err)
			}
		})
	}
}

// TestListModelsTimeout: a runtime that never answers is a timeout naming the
// method, not a hang. A caller deadline shortens the per-request budget, so the
// test does not wait out the shipped 30s.
func TestListModelsTimeout(t *testing.T) {
	cases := []struct{ id, method string }{
		{Grok.ID, "_x.ai/models/list"},
		{"kimi", "session/new"},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
			defer cancel()
			start := time.Now()
			got, err := listModelsCtx(t, ctx, c.id, "hold")
			if got != nil {
				t.Fatalf("models = %+v on failure, want nil", got)
			}
			if err == nil || !strings.Contains(err.Error(), "timed out") {
				t.Fatalf("err = %v, want a timeout", err)
			}
			if !strings.Contains(err.Error(), c.method) {
				t.Fatalf("err = %v, want the method %q", err, c.method)
			}
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				t.Fatalf("timeout took %s", elapsed)
			}
		})
	}
}

// TestListModelsAnswersNoServerRequest: the fake sends a server→client request
// mid-listing and fails the listing loudly if it is answered. A catalogue read
// must never drive a permission or terminal flow.
func TestListModelsAnswersNoServerRequest(t *testing.T) {
	got, err := listModels(t, "kimi", "server-request")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	assertModels(t, got, []oarlock.Model{
		{Provider: "kimi", ID: "opus", Name: "Opus", EffortLevels: []string{"low", "high"}},
		{Provider: "kimi", ID: "sonnet", Name: "Sonnet"},
		{Provider: "kimi", ID: "haiku", Name: "haiku"},
	})
}

// TestListModelsInitializeParams: the handshake sends exactly the frozen params
// — protocol 1, no fs and no terminal capability, and no profile `_meta`
// (session-shaping hints have no place in a listing).
func TestListModelsInitializeParams(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "initialize.json")
	t.Setenv("ACP_FAKE_MODELS_SEEN", seen)
	if _, err := listModels(t, "kimi", "ok"); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	raw, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("the fake agent recorded no initialize params: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("recorded initialize params: %v", err)
	}
	want := map[string]any{
		"protocolVersion": float64(1),
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": map[string]any{"name": "oarlock", "version": "0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("initialize params = %v\nwant               %v", got, want)
	}
}

// TestListModelsUsesProfileArgs: the listing launches the profile's own argv
// and nothing else (no LaunchArgs: a listing is not a session).
func TestListModelsUsesProfileArgs(t *testing.T) {
	file := filepath.Join(t.TempDir(), "args")
	t.Setenv("ACP_FAKE_ARGS_FILE", file)
	want := strings.Join(modelsProfile(t, "opencode").Args(oarlock.Options{}), "\n")
	if _, err := listModels(t, "opencode", "ok"); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("the fake agent recorded no argv: %v", err)
	}
	if got := strings.TrimSpace(string(raw)); got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}
