package codex

// Options.DisallowedTools as codex's own per-server `disabled_tools` config
// overrides (issue #54).
//
// codex app-server has no builtin deny channel: only MCP tools, named
// `mcp__server__tool`, can be filtered, and the filter is native config on the
// server entry. Which server a name belongs to is decided by codex's own
// effective config (`config/read`, includeLayers=false), because the user's
// config.toml servers are attached by codex itself, never by oarlock.
//
// A name codex cannot express is refused before the thread opens: dropping it
// would leave a tool the caller disabled callable. Nothing oarlock-side filters
// a call — the runtime's own deny channel does.
//
// Ported from OAR runtimes/codex/tool-denials.ts. Live-unverified: no logged-in
// codex exists here; `disabled_tools` is a field of the pinned
// RawMcpServerConfig of `codex app-server generate-json-schema` 0.165.0.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/zzjcool/oarlock"
)

// codexToolDenial is the only shape of tool name codex can disable: an MCP tool
// qualified with its server (`mcp__server__tool`).
var codexToolDenial = regexp.MustCompile(`^mcp__[\w]+__.+$`)

// codexWordName is a server name codex's native tool namespace carries
// unmodified; any other name cannot be addressed by a denial.
var codexWordName = regexp.MustCompile(`^\w+$`)

// validateToolDenials is the pre-open check (the same rules as toolDenials, with
// no config/read), so a name codex cannot express is refused before anything
// starts.
func validateToolDenials(names []string) error {
	for _, name := range names {
		if server, ok := reservedServerOf(name); ok {
			return &oarlock.UnsupportedOption{Option: oarlock.OptionDisallowedTools,
				Reason: fmt.Sprintf("codex cannot disable tools of the MCP server %q: the name is reserved and the denial would be dropped", server)}
		}
	}
	unsupported := unexpressible(names)
	if len(unsupported) == 0 {
		return nil
	}
	return &oarlock.UnsupportedOption{Option: oarlock.OptionDisallowedTools,
		Reason: "codex only filters MCP tools qualified as mcp__server__tool; built-in or unqualified names have no native deny channel: " + quotedList(unsupported)}
}

// reservedServerOf is the reserved server a denial addresses, if any: a name
// `mcp__<reserved>__<tool>` loses its denial when codex merges the config into a
// table that drops such a member, so the name is refused before anything starts.
func reservedServerOf(name string) (string, bool) {
	var hits []string
	for reserved := range reservedMCPServerNames {
		if strings.HasPrefix(name, "mcp__"+reserved+"__") {
			hits = append(hits, reserved)
		}
	}
	if len(hits) == 0 {
		return "", false
	}
	// A name can prefix two reserved servers; name one of them, always the same.
	sort.Strings(hits)
	return hits[0], true
}

// toolDenials is the per-server `{"disabled_tools":[…]}` override for
// opt.DisallowedTools. effective is the mcp_servers table of codex's config/read
// reply (nil when absent); session is opt.MCPServers.
//
// A name is resolved against the union of both server sets — the user's own
// servers and this session's — and must name exactly one of them with an
// unmodified native namespace: two servers sharing a prefix ("a" and "a_b" both
// claim mcp__a__b__x) make the name unexpressible, and guessing one of two
// possible servers is not an option.
func toolDenials(names []string, effective map[string]any, session []oarlock.MCPServer) (map[string]map[string]any, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if err := validateToolDenials(names); err != nil {
		return nil, err
	}
	servers := knownServers(effective, session)
	groups := make(map[string][]string, len(servers))
	var unsupported []string
	for _, name := range names {
		server, tool, ok := denialServer(name, servers)
		if !ok {
			unsupported = append(unsupported, name)
			continue
		}
		// The pre-open check above already refuses a reserved server from the name
		// alone; the rule holds here too, so whichever way the server was found, a
		// denial that codex's merge would drop is never sent. A server of the
		// user's own config named after an object member would vanish from the
		// merged mcp_servers, its denial with it.
		if reservedMCPServerNames[server] {
			return nil, &oarlock.UnsupportedOption{Option: oarlock.OptionDisallowedTools,
				Reason: fmt.Sprintf("codex cannot disable tools of the MCP server %q: the name is reserved and the denial would be dropped", server)}
		}
		groups[server] = append(groups[server], tool)
	}
	if len(unsupported) > 0 {
		return nil, &oarlock.UnsupportedOption{Option: oarlock.OptionDisallowedTools,
			Reason: "codex cannot identify one configured MCP server with an unmodified native namespace for: " + quotedList(unsupported)}
	}
	out := make(map[string]map[string]any, len(groups))
	for _, server := range sortedServers(groups) {
		tools := groups[server]
		prior, ok := priorDenials(effective, server)
		if !ok {
			return nil, &oarlock.UnsupportedOption{Option: oarlock.OptionDisallowedTools,
				Reason: fmt.Sprintf("codex cannot preserve the existing disabled_tools for server %q while disabling %s", server, quotedList(unionSorted(tools)))}
		}
		out[server] = map[string]any{"disabled_tools": unionSorted(prior, tools)}
	}
	return out, nil
}

// mergeDenials folds toolDenials output into params["config"]["mcp_servers"],
// keeping every other config override and every other field of the server
// entries: a denial is additional native config on the session's own launch
// config, never a replacement for it.
func mergeDenials(params map[string]any, denials map[string]map[string]any) {
	if len(denials) == 0 {
		return
	}
	config, _ := params["config"].(map[string]any)
	if config == nil {
		config = map[string]any{}
	}
	servers, _ := config["mcp_servers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	for server, denied := range denials {
		entry, _ := servers[server].(map[string]any)
		merged := make(map[string]any, len(entry)+len(denied))
		for k, v := range entry {
			merged[k] = v
		}
		for k, v := range denied {
			merged[k] = v
		}
		servers[server] = merged
	}
	config["mcp_servers"] = servers
	params["config"] = config
}

// knownServers is keys(effective) ∪ {session server names}, sorted: the servers
// a denial may name.
func knownServers(effective map[string]any, session []oarlock.MCPServer) []string {
	seen := make(map[string]bool, len(effective)+len(session))
	out := make([]string, 0, len(effective)+len(session))
	for name := range effective {
		seen[name] = true
		out = append(out, name)
	}
	for _, s := range session {
		if !seen[s.Name] {
			seen[s.Name] = true
			out = append(out, s.Name)
		}
	}
	sort.Strings(out)
	return out
}

// denialServer resolves the one server a name belongs to, and the tool within
// it. codex's own namespace is `mcp__<server>__<tool>`, so a server whose name
// is not a plain word cannot be addressed, and two matching candidates mean the
// name is ambiguous rather than guessable.
func denialServer(name string, servers []string) (server, tool string, ok bool) {
	matches := 0
	for _, candidate := range servers {
		if !codexWordName.MatchString(candidate) {
			continue
		}
		prefix := "mcp__" + candidate + "__"
		if !strings.HasPrefix(name, prefix) || len(name) == len(prefix) {
			continue
		}
		server, tool = candidate, name[len(prefix):]
		matches++
	}
	if matches != 1 {
		return "", "", false
	}
	return server, tool, true
}

// priorDenials reads a server entry's own disabled_tools out of codex's
// effective config: absent or null is none; anything but an array of strings is
// unreadable, so the caller refuses rather than replacing a filter it cannot
// preserve.
func priorDenials(effective map[string]any, server string) ([]string, bool) {
	entry, _ := effective[server].(map[string]any)
	if entry == nil {
		return nil, true
	}
	raw, has := entry["disabled_tools"]
	if !has || raw == nil {
		return nil, true
	}
	switch list := raw.(type) {
	case []string:
		return list, true
	case []any:
		out := make([]string, 0, len(list))
		for _, v := range list {
			s, ok := v.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// unionSorted is the deterministic disabled_tools value: the server's own
// entries plus the new names, de-duplicated and sorted.
func unionSorted(lists ...[]string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, list := range lists {
		for _, s := range list {
			if seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// unexpressible is every name in the list codex has no way to express.
func unexpressible(names []string) []string {
	var out []string
	for _, name := range names {
		if !codexToolDenial.MatchString(name) {
			out = append(out, name)
		}
	}
	return out
}

// sortedServers is the servers of a per-server table in order, so a refusal
// names the same one twice running.
func sortedServers(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// quotedList renders values the way the refusal names them: ["a","b"].
func quotedList(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	b, err := json.Marshal(values)
	if err != nil {
		return fmt.Sprintf("%q", values)
	}
	return string(b)
}
