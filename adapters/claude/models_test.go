package claude

// Models (issue #98) against a fake `claude`: the whole exchange is one argv,
// one stdin line and one stdout frame, so the same /bin/sh fake the ops tests
// use (fakeClaude) pins all of it — including the failures a live account would
// have to produce (not signed in, no list_models subtype, a process that exits
// without answering, a bound that expires). No real claude is needed or used and
// nothing reaches the network.
//
// LIVE STATUS: UNVERIFIED. Every case below is a fake; a live claude has never
// been observed answering list_models on this host.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
)

// fakeModelsReply is a fake claude body: read the one request line off stdin, take
// its request_id out of it, and print frame on stdout. frame is a heredoc body,
// so a multi-line reply (or a reply preceded by noise) is written verbatim and
// $id is the id the fake read — which is the only way the answer can be matched,
// since the production id carries a fresh uuid.
func fakeModelsReply(frame string) string {
	return `read -r line
id=$(printf '%s\n' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
cat <<EOF
` + frame + `
EOF
`
}

// modelsRequestLine is the exact line the adapter must write to stdin: one
// control_request for list_models, under an id of its own.
var modelsRequestLine = regexp.MustCompile(
	`^\{"type":"control_request","request_id":"oarlock-models-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}","request":\{"subtype":"list_models"\}\}$`)

// fileLines reads a fake's recording, one line per entry.
func fileLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the fake recorded nothing at %s: %v", path, err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// TestListModels_Success pins the projection of a success reply: two entries
// carrying effort levels (one of them also offering the fast tier), one with
// neither, and the exact oarlock.Model values they become.
func TestListModels_Success(t *testing.T) {
	fakeClaude(t, fakeModelsReply(`{"type":"control_response","response":{"subtype":"success","request_id":"$id","response":{"models":[{"value":"sonnet","resolvedModel":"claude-sonnet-5","displayName":"Sonnet","supportedEffortLevels":["low","high"],"supportsFastMode":true,"disabled":false},{"value":"opus","resolvedModel":"claude-opus-5","supportedEffortLevels":[{"effort":"high"},{"id":"max"}],"supportsFastMode":false},{"value":"haiku","displayName":"Haiku"}]}}}`))

	models, err := Runtime().Models.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []oarlock.Model{
		{
			Provider:     "claude",
			ID:           "sonnet",
			Name:         "Sonnet",
			EffortLevels: []string{"low", "high"},
			ServiceTiers: []string{"fast"},
		},
		{
			// No displayName: the resolved model id names it.
			Provider:     "claude",
			ID:           "opus",
			Name:         "claude-opus-5",
			EffortLevels: []string{"high", "max"},
		},
		{
			Provider: "claude",
			ID:       "haiku",
			Name:     "Haiku",
		},
	}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %#v, want %#v", models, want)
	}
}

// TestListModels_IgnoresEveryOtherLine pins the id correlation: hook traffic,
// claude's own control_request, a non-JSON banner and another exchange's answer
// all share the channel, and none of them is this read's answer.
func TestListModels_IgnoresEveryOtherLine(t *testing.T) {
	fakeClaude(t, fakeModelsReply(`{"type":"control_request","request_id":"perm-1","request":{"subtype":"can_use_tool"}}
this line is not JSON at all
{}
{"type":"control_response","response":{"subtype":"success"}}
{"type":"control_response","response":{"subtype":"success","request_id":"oarlock-models-someone-else","response":{"models":[{"value":"wrong"}]}}}
{"type":"control_response","response":{"subtype":"success","request_id":"$id","response":{"models":[{"value":"sonnet","displayName":"Sonnet"}]}}}`))

	models, err := Runtime().Models.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []oarlock.Model{{Provider: "claude", ID: "sonnet", Name: "Sonnet"}}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %#v, want %#v (only the frame carrying our request_id)", models, want)
	}
}

// TestListModels_HonestEmpty is claude answering "this account has no models":
// the empty list is the answer, so it comes back as a slice with a nil error —
// never nil, which is this method's failure value.
func TestListModels_HonestEmpty(t *testing.T) {
	fakeClaude(t, fakeModelsReply(`{"type":"control_response","response":{"subtype":"success","request_id":"$id","response":{"models":[]}}}`))

	models, err := Runtime().Models.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if models == nil {
		t.Fatal("models = nil, want the empty slice claude answered with")
	}
	if len(models) != 0 {
		t.Fatalf("models = %#v, want none", models)
	}
	if !reflect.DeepEqual(models, []oarlock.Model{}) {
		t.Fatalf("models = %#v, want []oarlock.Model{}", models)
	}
}

// TestListModels_NotSignedIn is the typed auth failure: the sentinel matches, the
// models are nil (a signed-out account has an unknown catalogue, not zero
// models) and the vendor's own line is the detail.
func TestListModels_NotSignedIn(t *testing.T) {
	if got, want := ErrNotAuthenticated.Error(), "claude: not signed in"; got != want {
		t.Fatalf("ErrNotAuthenticated = %q, want %q", got, want)
	}
	fakeClaude(t, fakeModelsReply(`{"type":"control_response","response":{"subtype":"error","request_id":"$id","error":"Not logged in. Please run /login"}}`))

	models, err := Runtime().Models.ListModels(context.Background())
	if !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("err = %v, want errors.Is(err, ErrNotAuthenticated)", err)
	}
	if models != nil {
		t.Fatalf("models = %#v, want nil on failure", models)
	}
	if !strings.Contains(err.Error(), "Not logged in") {
		t.Fatalf("err = %v, want the vendor's line as the detail", err)
	}
	// Never an UnsupportedOperation: this build has list_models, the account is
	// the problem.
	var unsupported *oarlock.UnsupportedOperation
	if errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want a plain auth error, not %v", err, unsupported)
	}
}

// TestListModels_AuthTextInMessage covers the other spelling of the same
// failure: the text lives in `message` instead of `error`, and "api key" is one
// of the auth words the contract matches.
func TestListModels_AuthTextInMessage(t *testing.T) {
	fakeClaude(t, fakeModelsReply(`{"type":"control_response","response":{"subtype":"error","request_id":"$id","message":"API key not valid for this workspace"}}`))

	models, err := Runtime().Models.ListModels(context.Background())
	if !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("err = %v, want errors.Is(err, ErrNotAuthenticated)", err)
	}
	if models != nil {
		t.Fatalf("models = %#v, want nil on failure", models)
	}
}

// TestListModels_OtherRefusalIsAPlainError keeps the auth classification honest:
// a refusal that is neither an auth failure nor a missing subtype is a plain
// error, not ErrNotAuthenticated and not an unsupported claim.
func TestListModels_OtherRefusalIsAPlainError(t *testing.T) {
	fakeClaude(t, fakeModelsReply(`{"type":"control_response","response":{"subtype":"error","request_id":"$id","error":"list_models is not available for this account tier"}}`))

	models, err := Runtime().Models.ListModels(context.Background())
	if err == nil {
		t.Fatal("a refusal: want an error, never an empty list")
	}
	if models != nil {
		t.Fatalf("models = %#v, want nil on failure", models)
	}
	if errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("err = %v, want a plain error (nothing said the account is signed out)", err)
	}
	var unsupported *oarlock.UnsupportedOperation
	if errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want a plain error, not %v", err, unsupported)
	}
	if !strings.Contains(err.Error(), "not available for this account tier") {
		t.Fatalf("err = %v, want it to carry the vendor's line", err)
	}
}

// TestListModels_UnsupportedSubtype is a claude build that does not answer
// list_models at all: typed UnsupportedOperation, never an empty list.
func TestListModels_UnsupportedSubtype(t *testing.T) {
	fakeClaude(t, fakeModelsReply(`{"type":"control_response","response":{"subtype":"error","request_id":"$id","error":"Unsupported control request subtype: list_models"}}`))

	models, err := Runtime().Models.ListModels(context.Background())
	var unsupported *oarlock.UnsupportedOperation
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want *oarlock.UnsupportedOperation", err)
	}
	if models != nil {
		t.Fatalf("models = %#v, want nil on failure", models)
	}
	if unsupported.Operation != "list-models" {
		t.Fatalf("operation = %q, want list-models", unsupported.Operation)
	}
	if !strings.Contains(unsupported.Reason, "list_models") {
		t.Fatalf("reason = %q, want it to name the missing subtype", unsupported.Reason)
	}
}

// TestListModels_DisabledModelsAreSkipped pins the deliberate drop: oarlock.Model
// has no "not selectable" field, so a disabled entry must not be reported as a
// model a host could run.
func TestListModels_DisabledModelsAreSkipped(t *testing.T) {
	fakeClaude(t, fakeModelsReply(`{"type":"control_response","response":{"subtype":"success","request_id":"$id","response":{"models":[{"value":"legacy","displayName":"Legacy","description":"needs a newer CLI","disabled":true},{"value":"sonnet","displayName":"Sonnet"}]}}}`))

	models, err := Runtime().Models.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []oarlock.Model{{Provider: "claude", ID: "sonnet", Name: "Sonnet"}}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %#v, want %#v", models, want)
	}
}

// TestListModels_BlankIDIsSkipped pins that an entry with no usable value is not
// emitted as "": a model with an empty id names nothing a host could select.
func TestListModels_BlankIDIsSkipped(t *testing.T) {
	fakeClaude(t, fakeModelsReply(`{"type":"control_response","response":{"subtype":"success","request_id":"$id","response":{"models":[{"value":"","displayName":"Nameless"},{"value":"   ","displayName":"Whitespace"},{"displayName":"No value field at all"},{"value":"  haiku  ","displayName":"Haiku"}]}}}`))

	models, err := Runtime().Models.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []oarlock.Model{{Provider: "claude", ID: "haiku", Name: "Haiku"}}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %#v, want %#v", models, want)
	}
	for _, model := range models {
		if strings.TrimSpace(model.ID) == "" {
			t.Fatalf("a model was emitted with no id: %#v", model)
		}
	}
}

// TestListModels_NoModelsArrayIsAnError: a success reply without the array is a
// reply this reader cannot trust, not "zero models".
func TestListModels_NoModelsArrayIsAnError(t *testing.T) {
	fakeClaude(t, fakeModelsReply(`{"type":"control_response","response":{"subtype":"success","request_id":"$id","response":{}}}`))

	models, err := Runtime().Models.ListModels(context.Background())
	if err == nil {
		t.Fatal("a success reply with no models array: want an error, never an empty list")
	}
	if models != nil {
		t.Fatalf("models = %#v, want nil on failure", models)
	}
	if !strings.Contains(err.Error(), "no models array") {
		t.Fatalf("err = %v, want it to say the reply carried no models array", err)
	}
}

// TestListModels_ExitsBeforeAnswering is the frozen message for a process that
// ends without answering: an error, never an empty list.
func TestListModels_ExitsBeforeAnswering(t *testing.T) {
	fakeClaude(t, "exit 0\n")

	models, err := Runtime().Models.ListModels(context.Background())
	if err == nil {
		t.Fatal("a process that exited: want an error, never an empty list")
	}
	if models != nil {
		t.Fatalf("models = %#v, want nil on failure", models)
	}
	if got, want := err.Error(), "claude list_models: exited before answering"; got != want {
		t.Fatalf("err = %q, want %q", got, want)
	}
}

// TestListModels_Timeout pins the bound: a claude that never answers fails as a
// timeout naming the bound it ran out of, and the process does not outlive the
// call.
func TestListModels_Timeout(t *testing.T) {
	fakeClaude(t, "sleep 30\n")

	const bound = 250 * time.Millisecond
	started := time.Now()
	models, err := claudeModels{Timeout: bound}.ListModels(context.Background())
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("an unresponsive claude: want a bounded error, never a hang or an empty list")
	}
	if models != nil {
		t.Fatalf("models = %#v, want nil on failure", models)
	}
	if !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), bound.String()) {
		t.Fatalf("err = %v, want it to name the %v timeout", err, bound)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("ListModels returned after %v; the bound was %v", elapsed, bound)
	}
}

// TestListModels_ArgvAndRequestLine pins the two things the contract fixes
// literally: claude's argv (order included — --verbose is mandatory next to
// --output-format stream-json, and the settings/MCP flags keep hooks and MCP
// servers from starting) and the one line written to stdin.
func TestListModels_ArgvAndRequestLine(t *testing.T) {
	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv")
	linePath := filepath.Join(dir, "line")
	fakeClaude(t, `printf '%s\n' "$@" > '`+argvPath+`'
read -r line
printf '%s\n' "$line" > '`+linePath+`'
id=$(printf '%s\n' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
cat <<EOF
{"type":"control_response","response":{"subtype":"success","request_id":"$id","response":{"models":[]}}}
EOF
`)

	if _, err := Runtime().Models.ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	wantArgv := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--settings", `{"disableAllHooks":true}`,
		"--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{}}`,
	}
	if argv := fileLines(t, argvPath); !reflect.DeepEqual(argv, wantArgv) {
		t.Fatalf("argv = %q, want %q", argv, wantArgv)
	}

	lines := fileLines(t, linePath)
	if len(lines) != 1 {
		t.Fatalf("cli stdin = %q, want exactly one line", lines)
	}
	if !modelsRequestLine.MatchString(lines[0]) {
		t.Fatalf("stdin line = %q, want %s", lines[0], modelsRequestLine)
	}
}

// TestListModels_NoBinary: an uninstalled runtime is a plain error from the same
// lookup the probe does, not an empty catalogue.
func TestListModels_NoBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	models, err := Runtime().Models.ListModels(context.Background())
	if err == nil {
		t.Fatal("no claude anywhere: want an error, never an empty catalogue")
	}
	if models != nil {
		t.Fatalf("models = %#v, want nil on failure", models)
	}
	if !strings.Contains(err.Error(), "no claude executable") {
		t.Fatalf("err = %v, want it to name the missing executable", err)
	}
}

// TestListModels_StopsTheProcess: a claude that keeps running after answering
// does not keep ListModels waiting — the throwaway tree is stopped on return.
func TestListModels_StopsTheProcess(t *testing.T) {
	fakeClaude(t, fakeModelsReply(`{"type":"control_response","response":{"subtype":"success","request_id":"$id","response":{"models":[{"value":"sonnet"}]}}}`)+"sleep 30\n")

	started := time.Now()
	models, err := Runtime().Models.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "sonnet" {
		t.Fatalf("models = %#v, want the one answered model", models)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("ListModels waited %v for a process it answers before the end of", elapsed)
	}
}
