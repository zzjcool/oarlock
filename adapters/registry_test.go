package adapters_test

import (
	"testing"

	"github.com/zzjcool/oarlock"
	_ "github.com/zzjcool/oarlock/adapters/acp"
	_ "github.com/zzjcool/oarlock/adapters/claude"
	_ "github.com/zzjcool/oarlock/adapters/codex"
	_ "github.com/zzjcool/oarlock/adapters/cursor"
	_ "github.com/zzjcool/oarlock/adapters/pi"
)

func TestAllAdaptersRegisterWithTheRootRegistry(t *testing.T) {
	for _, id := range []string{"pi", "claude", "codex", "cursor", "grok", "kimi", "opencode", "qwen", "qoder", "antigravity"} {
		rt, err := oarlock.Require(id)
		if err != nil {
			t.Errorf("Require(%q): %v", id, err)
			continue
		}
		if rt.ID != id || rt.Session == nil {
			t.Errorf("Require(%q) = %+v", id, rt)
		}
	}
}
