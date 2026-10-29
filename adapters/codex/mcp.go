package codex

// Options.MCPServers as codex's own `mcp_servers` config override (issue #54),
// and the credential values a session knows.
//
// oarlock attaches no server itself and filters nothing: the entries are native
// codex configuration, sent as `config.mcp_servers` on thread/start AND
// thread/resume (codex remembers no server for a thread). codex merges a config
// override into the user's own config.toml field by field, so an entry named
// like one of the user's is merged into it: `args` (an array is replaced whole)
// and `enabled: true` are written so the user's `args` or `enabled = false`
// cannot drop or change the session's server; the user's other fields of that
// entry (more env keys, env_vars, cwd, timeouts, tool filters) stay. An entry of
// the other transport fails the open ("url is not supported for stdio").
//
// Nested under one `mcp_servers` key rather than dotted `mcp_servers.<name>`
// keys, which a `.` in a name would split.
//
// Ported from OAR runtimes/codex/open.ts codexMcpServersConfig and
// shared/mcp-servers.ts. Live-unverified: no logged-in codex exists here; the
// shape is the pinned RawMcpServerConfig{command,args,env,url,http_headers,
// enabled,disabled_tools} of `codex app-server generate-json-schema` 0.165.0
// (docs/runtimes/codex.md).

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/zzjcool/oarlock"
)

// reservedMCPServerNames are names some runtimes drop without a word: object
// members their configuration parsers do not keep (shared/mcp-servers.ts
// RESERVED_MCP_SERVER_NAMES). Refused on every runtime, so one rule holds.
var reservedMCPServerNames = map[string]bool{
	"__proto__":        true,
	"__defineGetter__": true,
	"__defineSetter__": true,
	"__lookupGetter__": true,
	"__lookupSetter__": true,
}

// codexMCPServerName is the name codex starts an MCP server under; any other
// fails that server's startup after the thread opened.
var codexMCPServerName = regexp.MustCompile(`^[\w:@/.-]+$`)

// mcpConfig is the `config.mcp_servers` override built from opt.MCPServers, or
// nil when the session attaches no server of its own.
//
// Refusals are *oarlock.UnsupportedOption on OptionMCPServers: an empty name, a
// name given twice (each runtime keys its servers by name, so the later entry
// would silently replace the earlier one), a reserved name, a name codex
// cannot start, and a missing url (http) or command (stdio).
func mcpConfig(servers []oarlock.MCPServer) (map[string]any, error) {
	if len(servers) == 0 {
		return nil, nil
	}
	refuse := func(format string, args ...any) (map[string]any, error) {
		return nil, &oarlock.UnsupportedOption{Option: oarlock.OptionMCPServers, Reason: fmt.Sprintf(format, args...)}
	}
	// The name rules first, for every entry: a list codex cannot key is refused
	// before any entry is built.
	seen := make(map[string]bool, len(servers))
	for _, s := range servers {
		switch {
		case s.Name == "":
			return refuse("mcpServers has an entry with an empty name; a server is keyed by its name")
		case seen[s.Name]:
			return refuse("mcpServers names %q twice; names are unique within a session", s.Name)
		case reservedMCPServerNames[s.Name]:
			return refuse("mcpServers name %q is reserved: some runtimes drop a server so named without a word", s.Name)
		}
		seen[s.Name] = true
	}
	for _, s := range servers {
		if !codexMCPServerName.MatchString(s.Name) {
			return refuse("codex starts no MCP server named %q: its names match %s", s.Name, codexMCPServerName.String())
		}
	}
	out := make(map[string]any, len(servers))
	for _, s := range servers {
		if s.IsHTTP() {
			// codex starts one entry as one transport; the other transport's
			// fields on it would be dropped without a word.
			if s.Command != "" || len(s.Args) > 0 || len(s.Env) > 0 {
				return refuse("mcpServers entry %q is an http server (type %q) and also carries command/args/env; an accepted entry is never partly dropped", s.Name, s.Type)
			}
			if s.URL == "" {
				return refuse("mcpServers entry %q is an http server and has no url; codex starts an http MCP server only with one", s.Name)
			}
			entry := map[string]any{"url": s.URL, "enabled": true}
			if len(s.Headers) > 0 {
				entry["http_headers"] = copyStringMap(s.Headers)
			}
			out[s.Name] = entry
			continue
		}
		if s.URL != "" || len(s.Headers) > 0 {
			return refuse("mcpServers entry %q is a stdio server (type %q) and also carries url/headers; an accepted entry is never partly dropped", s.Name, s.Type)
		}
		if s.Command == "" {
			return refuse("mcpServers entry %q is a stdio server and has no command; codex starts a stdio MCP server only with one", s.Name)
		}
		// args is always present (an empty array when there are none): codex
		// replaces an array whole, so the user's own args of that name cannot
		// leak into this session's server.
		entry := map[string]any{"command": s.Command, "args": append([]string{}, s.Args...), "enabled": true}
		if len(s.Env) > 0 {
			entry["env"] = copyStringMap(s.Env)
		}
		out[s.Name] = entry
	}
	return out, nil
}

// sessionSecrets is every credential value this session knows (Options.Env
// values, every MCP server env value and header value). The kernel redactor
// replaces them in every retained record and error: an MCP credential travels to
// codex inside the config override, so it must never be readable in a record.
// Ordered by key, so the same options always give the same list.
func sessionSecrets(opt oarlock.Options) []string {
	var out []string
	envKeys := make([]string, 0, len(opt.Env))
	for k := range opt.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		if v := opt.Env[k]; v != nil {
			out = append(out, *v)
		}
	}
	for _, s := range opt.MCPServers {
		out = append(out, sortedMapValues(s.Env)...)
		out = append(out, sortedMapValues(s.Headers)...)
	}
	return out
}

// copyStringMap keeps the caller's map out of the request this session sends.
func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// sortedMapValues is the map's values in key order (a deterministic list).
func sortedMapValues(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(m))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}
