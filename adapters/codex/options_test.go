package codex

// Issue #54 §G-W-C: the codex option surface — MCP servers, tool denials,
// userConfiguration=exclude and the credentials that must never reach a record.
//
// Everything below is asserted from the two places a host can see: the wire the
// fake app-server recorded (fake_test.go, contract §F) and the stream the
// Session retained. No test reads a Session field a host cannot read.
//
// Live-unverified: there is no logged-in codex here, so the fake app-server is
// the only executor; docs/runtimes/codex.md carries the markers.

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/redact"
)

// ---- wire helpers ----------------------------------------------------------------

// configSent returns params["config"], the config overrides a request carried.
func configSent(t *testing.T, params map[string]any) map[string]any {
	t.Helper()
	config, ok := params["config"].(map[string]any)
	if !ok {
		t.Fatalf("request carried no config override: %#v", params)
	}
	return config
}

// serversSent returns params.config.mcp_servers as it went over the wire.
func serversSent(t *testing.T, params map[string]any) map[string]any {
	t.Helper()
	servers, ok := configSent(t, params)["mcp_servers"].(map[string]any)
	if !ok {
		t.Fatalf("request carried no config.mcp_servers: %#v", params)
	}
	return servers
}

// startLine is the launch line the fake app-server logged (its own argv).
func startLine(t *testing.T, e *fakeEnv) string {
	t.Helper()
	for _, line := range e.wire() {
		if strings.HasPrefix(line, "START ") || line == "START" {
			return line
		}
	}
	t.Fatalf("no launch line on the wire: %q", e.wire())
	return ""
}

// codexHomeSeen is the CODEX_HOME the session process itself reported: the home
// the adapter really handed to codex, not the one it meant to.
func codexHomeSeen(t *testing.T, e *fakeEnv) string {
	t.Helper()
	for _, line := range e.wire() {
		if home, ok := strings.CutPrefix(line, codexHomeEnv+"="); ok {
			return home
		}
	}
	t.Fatalf("the session process never reported its %s: %q", codexHomeEnv, e.wire())
	return ""
}

// hasMethod reports whether the wire carries a method.
func hasMethod(methods []string, method string) bool {
	return slices.Contains(methods, method)
}

// ---- option fixtures -------------------------------------------------------------

// optionProbe is one Options value that exercises a single frozen option, for
// the table tests that open or refuse every option of the runtime.
func optionProbe(t *testing.T, option oarlock.Option) oarlock.Options {
	t.Helper()
	value := "probe-value"
	opt := oarlock.Options{Cwd: t.TempDir()}
	switch option {
	case oarlock.OptionSystemPrompt:
		opt.SystemPrompt = "be brief"
	case oarlock.OptionAppendSystemPrompt:
		opt.AppendSystemPrompt = "be kind"
	case oarlock.OptionEnv:
		opt.Env = map[string]*string{"OARLOCK_PROBE": &value}
	case oarlock.OptionMCPServers:
		opt.MCPServers = []oarlock.MCPServer{{Name: "probe", Command: "probe-mcp"}}
	case oarlock.OptionMCPServerCredentials:
		opt.MCPServers = []oarlock.MCPServer{{Name: "probe", Command: "probe-mcp", Env: map[string]string{"PROBE_TOKEN": "probe-token-value"}}}
	case oarlock.OptionDisallowedTools:
		opt.DisallowedTools = []string{"mcp__a__b"}
	case oarlock.OptionLaunchArgs:
		opt.LaunchArgs = []string{"-c", "probe=1"}
	case oarlock.OptionServiceTier:
		opt.ServiceTier = "priority"
	case oarlock.OptionAdditionalDirectories:
		opt.AdditionalDirectories = []string{t.TempDir()}
	case oarlock.OptionOutputSchema:
		opt.OutputSchema = json.RawMessage(`{"type":"object"}`)
	case oarlock.OptionName:
		opt.Name = "probe"
	case oarlock.OptionUserConfiguration:
		opt.UserConfiguration = oarlock.UserConfigurationExclude
	}
	return opt
}

// ---- §G-W-C 1: the mcp_servers override --------------------------------------------

// A session's own servers reach thread/start — and thread/resume — as the
// config.mcp_servers override: args always, env and http_headers only when
// non-empty, enabled pinned true, and no `-c` on the launch line (the override
// is one JSON-RPC config, not CLI arguments a shell has to re-parse).
func TestMCPServersReachThreadStartAsAConfigOverride(t *testing.T) {
	servers := []oarlock.MCPServer{
		{Name: "alpha", Command: "npx", Args: []string{"-y", "pkg"}, Env: map[string]string{"ALPHA_TOKEN": "alpha-token-value"}},
		{Name: "plain", Command: "plain-mcp"},
		{Name: "remote", Type: "http", URL: "https://mcp.example/v1", Headers: map[string]string{"Authorization": "Bearer remote-token-value"}},
	}
	want := map[string]any{
		"alpha": map[string]any{
			"command": "npx",
			"args":    []any{"-y", "pkg"},
			"env":     map[string]any{"ALPHA_TOKEN": "alpha-token-value"},
			"enabled": true,
		},
		"plain": map[string]any{
			// args is always present ([] when nil): a same-named entry of the
			// user's config.toml cannot drop it.
			"command": "plain-mcp",
			"args":    []any{},
			"enabled": true,
		},
		"remote": map[string]any{
			"url":          "https://mcp.example/v1",
			"http_headers": map[string]any{"Authorization": "Bearer remote-token-value"},
			"enabled":      true,
		},
	}
	for _, tc := range []struct {
		name   string
		resume string
		method string
	}{
		{name: "new thread", method: "thread/start"},
		{name: "resumed thread", resume: "thread-old", method: "thread/resume"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFake(t, "")
			e.mustOpen(oarlock.Options{Resume: tc.resume, MCPServers: servers}, Config{})

			if tc.resume != "" && hasMethod(e.methods(), "thread/start") {
				t.Errorf("wire = %v; a resume must not start a new thread", e.methods())
			}
			params := e.sent(tc.method, 0)
			if got := serversSent(t, params); !reflect.DeepEqual(got, want) {
				t.Errorf("%s config.mcp_servers =\n %#v\nwant\n %#v", tc.method, got, want)
			}
			// One config source: the servers are JSON-RPC params, never `-c` on
			// the launch line of the app-server.
			line := startLine(t, e)
			for _, field := range strings.Fields(line) {
				if field == "-c" {
					t.Errorf("the launch line passes -c: %q", line)
				}
			}
		})
	}
}

// ---- §G-W-C 2: refusals ---------------------------------------------------------------

// A server codex cannot express is refused as mcpServers, and nothing reached
// the wire: no probe, no process, no thread.
func TestMCPServerRefusalsNameTheOptionAndStartNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		servers []oarlock.MCPServer
		reason  string // a value the refusal must name ("" = only the rule)
	}{
		{name: "empty name", servers: []oarlock.MCPServer{{Command: "echo"}}},
		{name: "duplicate name", servers: []oarlock.MCPServer{{Name: "dup", Command: "a"}, {Name: "dup", Command: "b"}}, reason: "dup"},
		{name: "reserved name", servers: []oarlock.MCPServer{{Name: "__proto__", Command: "x"}}, reason: "__proto__"},
		{name: "invalid character", servers: []oarlock.MCPServer{{Name: "bad name!", Command: "x"}}, reason: "bad name!"},
		{name: "http without url", servers: []oarlock.MCPServer{{Name: "h", Type: "http"}}},
		{name: "stdio without command", servers: []oarlock.MCPServer{{Name: "s"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFake(t, "")
			s, err := e.open(oarlock.Options{MCPServers: tc.servers}, Config{})
			if s != nil {
				_ = s.Dispose(bg)
				t.Fatal("a session was opened for an mcpServers the adapter cannot express")
			}
			var un *oarlock.UnsupportedOption
			if !errors.As(err, &un) || un.Option != oarlock.OptionMCPServers {
				t.Fatalf("err = %v; want *UnsupportedOption{Option: %s}", err, oarlock.OptionMCPServers)
			}
			if un.Reason == "" {
				t.Error("the refusal names no reason")
			}
			if tc.reason != "" && !strings.Contains(un.Reason, tc.reason) {
				t.Errorf("reason %q does not name the value %q", un.Reason, tc.reason)
			}
			if got := e.methods(); len(got) != 0 {
				t.Errorf("wire = %v; a refused option costs nothing, not even a probe", got)
			}
		})
	}
}

// ---- §G-W-C 3: tool denials ------------------------------------------------------------

// sessionEntries are the config entries this session's own servers must keep on
// thread/start while denials are merged in (mcp.go, contract §A), field by
// field.
func sessionEntries() map[string]map[string]any {
	return map[string]map[string]any{
		"alpha": {
			"command": "alpha-mcp",
			"args":    []any{},
			"env":     map[string]any{"ALPHA_TOKEN": "alpha-token-value"},
			"enabled": true,
		},
		"search": {
			"url":     "https://search.example/mcp",
			"enabled": true,
		},
	}
}

// disallowedTools resolves names against codex's own config/read table and this
// session's servers alike, and lands each server's disabled_tools on
// thread/start without disturbing the session's own MCP launch config.
func TestDisallowedToolsReachThreadStartAsServerConfig(t *testing.T) {
	servers := []oarlock.MCPServer{
		{Name: "alpha", Command: "alpha-mcp", Env: map[string]string{"ALPHA_TOKEN": "alpha-token-value"}},
		{Name: "search", Type: "http", URL: "https://search.example/mcp"},
	}
	for _, tc := range []struct {
		name     string
		scenario string
		denied   []string
		want     map[string][]string // server → the disabled_tools array to send
	}{
		{
			name: "user and session servers",
			// One name twice: the array is de-duplicated, not appended blind.
			denied: []string{"mcp__a__b", "mcp__a__b", "mcp__echo__y", "mcp__alpha__run", "mcp__search__fetch"},
			want: map[string][]string{
				"a":      {"b"},
				"echo":   {"y"},
				"alpha":  {"run"},
				"search": {"fetch"},
			},
		},
		{
			name:     "unions with an existing array",
			scenario: "config-denials",
			denied:   []string{"mcp__a__bbb", "mcp__a__aaa"},
			want:     map[string][]string{"a": {"aaa", "bbb", "zzz"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFake(t, tc.scenario)
			dir := t.TempDir()
			e.mustOpen(oarlock.Options{Cwd: dir, MCPServers: servers, DisallowedTools: tc.denied}, Config{})

			// codex's own effective config decides which server a name belongs
			// to, so it is read first, in the session's directory.
			methods := e.methods()
			read, start := slices.Index(methods, "config/read"), slices.Index(methods, "thread/start")
			if read < 0 || start < 0 || read > start {
				t.Fatalf("wire = %v; the denials need config/read before thread/start", methods)
			}
			if got := e.sent("config/read", 0)["cwd"]; got != dir {
				t.Errorf("config/read cwd = %v, want %q", got, dir)
			}

			sent := serversSent(t, e.sent("thread/start", 0))
			for server, wantTools := range tc.want {
				entry, ok := sent[server].(map[string]any)
				if !ok {
					t.Fatalf("no config.mcp_servers[%q] on thread/start: %#v", server, sent)
				}
				tools := make([]any, 0, len(wantTools))
				for _, tool := range wantTools {
					tools = append(tools, tool)
				}
				if !reflect.DeepEqual(entry["disabled_tools"], tools) {
					t.Errorf("%s disabled_tools = %#v, want %#v", server, entry["disabled_tools"], tools)
				}
			}
			// The session's own launch config is still there, field by field:
			// a denial may add disabled_tools, never replace a server.
			for server, wantFields := range sessionEntries() {
				entry, ok := sent[server].(map[string]any)
				if !ok {
					t.Fatalf("the session's own server %q was dropped: %#v", server, sent)
				}
				for field, value := range wantFields {
					if !reflect.DeepEqual(entry[field], value) {
						t.Errorf("%s.%s = %#v, want %#v (entry %#v)", server, field, entry[field], value, entry)
					}
				}
			}
		})
	}
}

// ---- §G-W-C 4: denial refusals -----------------------------------------------------------

// A denial this adapter cannot express refuses the open as disallowedTools and
// never reaches thread/start; the ones codex cannot even express are refused
// before a session process exists.
func TestDisallowedToolRefusalsReachNoThreadStart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scenario string
		denied   []string
		read     bool // the server is only known from codex's own config/read table
		preOpen  bool // the name alone is enough: no process at all
	}{
		{name: "unqualified name", denied: []string{"b"}, preOpen: true},
		{name: "unknown server", denied: []string{"mcp__nope__x"}, read: true},
		// A denial for a reserved server name would be dropped by codex, so it
		// refuses. Which server mcp____proto____x names (__proto__ is one
		// reading) is decided by codex's own keys, so the refusal may come before
		// or after config/read; pinned here is that it refuses at all.
		{name: "reserved server name", denied: []string{"mcp____proto____x"}},
		{name: "existing disabled_tools is not an array", scenario: "config-bad-denials", denied: []string{"mcp__a__b"}, read: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFake(t, tc.scenario)
			s, err := e.open(oarlock.Options{DisallowedTools: tc.denied}, Config{})
			if s != nil {
				_ = s.Dispose(bg)
				t.Fatal("a session was opened for a refusal codex cannot express")
			}
			var un *oarlock.UnsupportedOption
			if !errors.As(err, &un) || un.Option != oarlock.OptionDisallowedTools {
				t.Fatalf("err = %v; want *UnsupportedOption{Option: %s}", err, oarlock.OptionDisallowedTools)
			}
			methods := e.methods()
			if hasMethod(methods, "thread/start") || hasMethod(methods, "thread/resume") {
				t.Errorf("wire = %v; a refused denial still opened a thread", methods)
			}
			if tc.read && !hasMethod(methods, "config/read") {
				t.Errorf("wire = %v; the server of %v can only be resolved from config/read", methods, tc.denied)
			}
			if tc.preOpen && hasMethod(methods, "START") {
				t.Errorf("wire = %v; a name codex cannot express is refused before the session process starts", methods)
			}
		})
	}
}

// ---- §G-W-C 5: userConfiguration=exclude -------------------------------------------------

// Exclude runs codex on an isolated home: the login travels, the configuration
// does not, and the user's own home is only ever read.
func TestUserConfigurationExcludeRunsOnAnIsolatedHome(t *testing.T) {
	e := newFake(t, "")
	home := e.home // the CODEX_HOME newFake gave the fake app-server
	login := []byte(`{"tokens":{"access_token":"user-login"}}`)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), login, 0o600); err != nil {
		t.Fatalf("write the user's login: %v", err)
	}
	userConfig := []byte("[model]\nmodel = \"gpt-5.5\"\n")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), userConfig, 0o600); err != nil {
		t.Fatalf("write the user's config: %v", err)
	}
	before := dirFiles(t, home)

	// Contract §D: a sibling of the user's home, so the isolated home persists
	// per user home instead of per session.
	isolated := filepath.Join(filepath.Dir(home), filepath.Base(home)+".oarlock-exclude")
	if _, err := os.Stat(isolated); err == nil {
		t.Fatalf("the isolated home %q exists before the open", isolated)
	}

	e.mustOpen(oarlock.Options{UserConfiguration: oarlock.UserConfigurationExclude}, Config{})

	if got := codexHomeSeen(t, e); got != isolated {
		t.Fatalf("the session process ran with %s=%q, want %q", codexHomeEnv, got, isolated)
	}
	info, err := os.Stat(isolated)
	if err != nil || !info.IsDir() {
		t.Fatalf("the isolated home %q is not a directory: %v", isolated, err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Errorf("the isolated home is %v; it must be 0700", info.Mode().Perm())
	}

	// The login travels: the session stays signed in without the user's config.
	copied, err := os.ReadFile(filepath.Join(isolated, "auth.json"))
	if err != nil || string(copied) != string(login) {
		t.Errorf("the isolated login = %q, %v; want the user's %q", copied, err, login)
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(filepath.Join(isolated, "auth.json")); err == nil && st.Mode().Perm() != 0o600 {
			t.Errorf("the isolated login is %v; it must be 0600", st.Mode().Perm())
		}
	}
	// The configuration does not: the adapter never creates or copies one.
	if _, err := os.Stat(filepath.Join(isolated, "config.toml")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the isolated home holds a config.toml (stat: %v); it must hold none", err)
	}

	// The user's own home is read, never written.
	if after := dirFiles(t, home); !reflect.DeepEqual(after, before) {
		t.Errorf("the user's own codex home changed: %q → %q", before, after)
	}
	if got := os.Getenv(codexHomeEnv); got != home {
		t.Errorf("the open changed the process's %s to %q, want %q", codexHomeEnv, got, home)
	}
}

// dirFiles snapshots a directory (name → content) so a test can prove an open
// wrote nothing there.
func dirFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		out[entry.Name()] = string(content)
	}
	return out
}

// ---- §G-W-C 6: credentials never reach a record -------------------------------------------

// MCP credentials the session knows (an env value and a header value) never
// reach a retained record, not even when codex itself says them back: the
// record carries the placeholder instead.
func TestMCPCredentialsNeverReachARecord(t *testing.T) {
	const envSecret = "mcp-env-secret-value"
	const headerSecret = "Bearer mcp-header-secret"
	e := newFake(t, "echo-secrets")
	s := e.mustOpen(oarlock.Options{MCPServers: []oarlock.MCPServer{
		{Name: "alpha", Command: "alpha-mcp", Env: map[string]string{"ALPHA_TOKEN": envSecret}},
		{Name: "remote", Type: "http", URL: "https://mcp.example/v1", Headers: map[string]string{"Authorization": headerSecret}},
	}}, Config{})
	prompt(t, s, "hi", "in-1")
	waitIdle(t, s)

	recs := s.Records(oarlock.Cursor{})
	var echoed *oarlock.Record
	for i := range recs {
		if strings.Contains(string(recs[i].Native), "item/agentMessage/delta") {
			echoed = &recs[i]
		}
	}
	if echoed == nil {
		t.Fatalf("the echoed delta is not in the stream: %q", skeleton(recs))
	}
	native := string(echoed.Native)
	// The fake really said the credentials back (and not just "ok"), so the
	// placeholder below is the redactor's work and not an absent echo.
	if !strings.Contains(native, "secrets:") {
		t.Fatalf("the fake did not echo the credentials: %s", native)
	}
	if !strings.Contains(native, redact.RedactedPlaceholder) {
		t.Errorf("the echoed frame carries no %s: %s", redact.RedactedPlaceholder, native)
	}
	for _, r := range recs {
		for _, secret := range []string{envSecret, headerSecret} {
			if strings.Contains(string(r.Native), secret) {
				t.Errorf("record %d leaks %q: %s", r.Seq, secret, r.Native)
			}
		}
	}
}

// ---- §G-W-C 7: the option table ------------------------------------------------------------

// The four options issue #54 adds open a session, and no option the table still
// refuses reaches anything: the refusal is a typed error and the wire stays
// empty.
func TestHonouredOptionsOpenAndRefusalsStayOffTheWire(t *testing.T) {
	honoured := []oarlock.Option{
		oarlock.OptionMCPServers,
		oarlock.OptionMCPServerCredentials,
		oarlock.OptionDisallowedTools,
		oarlock.OptionUserConfiguration,
	}
	for _, option := range honoured {
		t.Run(string(option), func(t *testing.T) {
			if reason, refused := Refused()[option]; refused {
				t.Fatalf("%s is still refused: %s", option, reason)
			}
			e := newFake(t, "")
			s := e.mustOpen(optionProbe(t, option), Config{})
			if s.ThreadID() == "" {
				t.Error("opened without a thread")
			}
			if got := e.methods(); !hasMethod(got, "thread/start") {
				t.Errorf("wire = %v; thread/start was never sent", got)
			}
		})
	}
	// Nothing the table refuses may cost a process, a probe or an RPC.
	table := Refused()
	for _, option := range oarlock.AllRefusableOptions {
		if _, refused := table[option]; !refused {
			continue
		}
		t.Run("refused "+string(option), func(t *testing.T) {
			e := newFake(t, "")
			s, err := e.open(optionProbe(t, option), Config{})
			if s != nil {
				_ = s.Dispose(bg)
				t.Fatalf("%s opened a session although the table refuses it", option)
			}
			var un *oarlock.UnsupportedOption
			if !errors.As(err, &un) || un.Option != option {
				t.Fatalf("err = %v; want *UnsupportedOption{Option: %s}", err, option)
			}
			if got := e.methods(); len(got) != 0 {
				t.Errorf("wire = %v; a refused option reached the wire", got)
			}
		})
	}
}
