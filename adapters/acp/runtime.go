package acp

import "github.com/zzjcool/oarlock"

// Profiles are the six runtimes of the family. Each profile file states only
// what its vendor does differently.
func Profiles() []Profile {
	return []Profile{Grok, Kimi, OpenCode, Qwen, Qoder, Antigravity}
}

// Runtimes returns the six oarlock.Runtime values keyed by id.
func Runtimes() map[string]oarlock.Runtime {
	out := map[string]oarlock.Runtime{}
	for _, p := range Profiles() {
		out[p.ID] = p.Runtime()
	}
	return out
}
