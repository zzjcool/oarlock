package claude

// Fixture generator: OAR_REGEN_FIXTURES=<dir of OAR tests/replay/fixtures> go test
// -run TestRegenFixtures ./adapters/claude copies the chosen real claude
// stream-json recordings into testdata/ and redacts them with internal/redact.
// It is not part of a normal run (skipped), and the committed result is checked
// by TestFixturesCarryNoCredentials.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock/internal/redact"
)

// fixtureSources are the recordings kept: text, a tool round, an is_error
// result with subtype success, sub-agent + compaction + usage, a steer with its
// echo and an interrupt after start.
var fixtureSources = map[string]string{
	"tool-round":      "claude-tool-round.raw.jsonl",
	"multi-turn":      "claude-multi-turn.raw.jsonl",
	"error":           "claude-error.raw.jsonl",
	"usage-subagent":  "claude-usage-subagent-compact.raw.jsonl",
	"steer-lifecycle": "claude-steer-lifecycle.raw.jsonl",
	"interrupt":       "claude-plain-interrupt-after-start.raw.jsonl",
	"steer-stdin":     "claude-plain-interrupt-after-start.stdin.jsonl",
}

// opaqueValues collects values that are credentials or account-bound in a
// frame: thinking signatures (opaque, long) and any env/header style secret.
func opaqueValues(line []byte) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, x := range t {
				if s, ok := x.(string); ok && (k == "signature" || redact.NameIsCredential(k)) && redact.SelectValue(s) {
					out = append(out, s)
				}
				walk(x)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		}
	}
	var v any
	if json.Unmarshal(line, &v) == nil {
		walk(v)
	}
	return out
}

func TestRegenFixtures(t *testing.T) {
	src := os.Getenv("OAR_REGEN_FIXTURES")
	if src == "" {
		t.Skip("set OAR_REGEN_FIXTURES=<OAR tests/replay/fixtures> to regenerate testdata/")
	}
	for name, file := range fixtureSources {
		raw, err := os.ReadFile(filepath.Join(src, file))
		if err != nil {
			t.Fatal(err)
		}
		var known []string
		for _, line := range bytes.Split(raw, []byte("\n")) {
			known = append(known, opaqueValues(line)...)
		}
		red := redact.New(known)
		var out bytes.Buffer
		for _, line := range bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n")) {
			r, _ := red.Bytes(line)
			out.Write(r)
			out.WriteByte('\n')
		}
		if err := os.WriteFile(filepath.Join("testdata", name+".jsonl"), out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFixturesCarryNoCredentials(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.jsonl")
	if len(files) == 0 {
		t.Fatal("no fixtures in testdata/")
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		s := string(b)
		for _, bad := range []string{"sk-ant", "Bearer ", "oauth-token", "password", "-----BEGIN"} {
			if strings.Contains(strings.ToLower(s), strings.ToLower(bad)) {
				t.Errorf("%s contains %q", f, bad)
			}
		}
		for _, v := range opaqueValues([]byte(strings.ReplaceAll(s, "\n", " "))) {
			t.Errorf("%s still carries a credential-shaped value %.12q…", f, v)
		}
		for i, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
			if json.Valid([]byte(line)) == false {
				t.Errorf("%s:%d is not JSON", f, i+1)
			}
		}
	}
}
