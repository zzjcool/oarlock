package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

func TestBaseArgsAreTheIssueFlagsInOrder(t *testing.T) {
	want := "-p --input-format stream-json --output-format stream-json --verbose --replay-user-messages --include-partial-messages --dangerously-skip-permissions"
	if got := strings.Join(BaseArgs(), " "); got != want {
		t.Fatalf("BaseArgs = %q\nwant      %q", got, want)
	}
}

func TestLaunchArgsMapOptions(t *testing.T) {
	a := strings.Join(launchArgs(oarlock.Options{
		Model: "m", Effort: "high", Name: "n", SystemPrompt: "sp", AppendSystemPrompt: "ap",
		LaunchArgs: []string{"--x"}, AdditionalDirectories: []string{"/a", "/b"}, DisallowedTools: []string{"Bash"},
	}, "sid"), " ")
	for _, want := range []string{"--session-id sid", "--name n", "--model m", "--effort high", "--system-prompt sp",
		"--append-system-prompt ap", "--x", "--add-dir /a /b", "--disallowed-tools Bash"} {
		if !strings.Contains(a, want) {
			t.Errorf("argv lacks %q: %s", want, a)
		}
	}
	// Variadic flags go last so none swallows the host's flag.
	if strings.Index(a, "--x") > strings.Index(a, "--add-dir") {
		t.Errorf("LaunchArgs must precede the variadic flags: %s", a)
	}
}

func TestManagedLaunchArgsAreRefused(t *testing.T) {
	for _, bad := range []string{"--resume", "-r", "--session-id=abc", "--continue", "--permission-mode", "-p", "--output-format=json"} {
		err := checkLaunchArgs([]string{"--ok", bad})
		var u *oarlock.UnsupportedOption
		if !errors.As(err, &u) || u.Option != oarlock.OptionLaunchArgs {
			t.Errorf("%s: err = %v, want UnsupportedOption(launchArgs)", bad, err)
		}
	}
	if err := checkLaunchArgs([]string{"--max-turns", "3"}); err != nil {
		t.Errorf("ordinary flag refused: %v", err)
	}
}

func TestRefusedTableIsTheOneOpenChecks(t *testing.T) {
	r := Refused()
	for _, o := range oarlock.AllRefusableOptions {
		_, refused := r[o]
		if refused != (refusedTable[o] != "") {
			t.Errorf("%s: declaration and table disagree", o)
		}
	}
	for _, o := range []oarlock.Option{oarlock.OptionMCPServers, oarlock.OptionServiceTier, oarlock.OptionUserConfiguration} {
		if r[o] == "" {
			t.Errorf("%s should be refused until implemented", o)
		}
	}
	if r[oarlock.OptionLaunchArgs] != "" || r[oarlock.OptionEnv] != "" {
		t.Error("honoured options must not be refused")
	}
	if err := refusedTable.Check(oarlock.Options{MCPServers: []oarlock.MCPServer{{Name: "x", Command: "y"}}}); err == nil {
		t.Error("MCP servers are refused, never silently dropped")
	}
}

func TestSessionEnvDropsParentMarkersAndHonoursOverrides(t *testing.T) {
	v := "1"
	env := sessionEnv([]string{"PATH=/bin", "CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=cli", "KEEP=yes", "DROP=me"},
		map[string]*string{"DROP": nil, "CLAUDE_CODE_ENTRYPOINT": &v, "CLAUDECODE": &v})
	joined := "|" + strings.Join(env, "|") + "|"
	if strings.Contains(joined, "CLAUDECODE=") {
		t.Errorf("CLAUDECODE must never be set: %v", env)
	}
	if !strings.Contains(joined, "|CLAUDE_CODE_ENTRYPOINT=1|") || strings.Contains(joined, "CLAUDE_CODE_ENTRYPOINT=cli") {
		t.Errorf("host override must win over the inherited marker: %v", env)
	}
	if strings.Contains(joined, "DROP=") || !strings.Contains(joined, "|KEEP=yes|") || !strings.Contains(joined, "|PATH=/bin|") {
		t.Errorf("env = %v", env)
	}
}

// The user message we write is the recorded stdin line of a real claude run
// (testdata/steer-stdin.jsonl, from OAR's claude-plain-interrupt recording).
func TestUserMessageMatchesTheRecordedStdinLine(t *testing.T) {
	raw := loadFrames(t, "steer-stdin")
	want := sj.ParseRec(raw[0])
	line, refusal := userMessage("HOST-PROMPT: say host-answer", "6f1c2d3e-00c0-4a5b-8c7d-0000000000c0", nil)
	if refusal != nil {
		t.Fatalf("a text-only user message was refused: %+v", *refusal)
	}
	got := sj.ParseRec(line)
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(got)
	if string(a) != string(b) {
		t.Fatalf("user message\n got %s\nwant %s", b, a)
	}
	one, _ := userMessage("x", "", nil)
	escaped, _ := userMessage("a\nb", "", nil)
	if !bytes.HasSuffix(one, []byte("\n")) || bytes.Contains(escaped, []byte("\n\n")) {
		t.Fatal("a user message is exactly one LF-terminated line, newlines inside the text are escaped")
	}
	// The recorded interrupt request has the shape decideAbort writes.
	ir := sj.ParseRec(raw[1])
	if sj.Str(ir["type"]) != "control_request" || sj.Str(sj.AsRec(ir["request"])["subtype"]) != "interrupt" {
		t.Fatalf("recorded interrupt = %v", ir)
	}
}
