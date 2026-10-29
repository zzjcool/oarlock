package acp

// The installation probe of the ACP family: PATH first, then the directory each
// vendor's own installer links its binary into. No process is started and no
// version is read, so an installation that exists is reported as present with
// an empty Version.

import (
	"context"
	"fmt"
	"strings"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/ops"
)

// knownInstallPaths is where each vendor installer puts its binary, relative to
// the home directory (OAR docs/spec/install.md §Per runtime, runtimes/{grok,
// kimi, opencode, qwen, qoder}/install.ts). The probe looks there after PATH
// because every install line adds its folder to the shell profile, which a host
// process started before the install never re-reads: without the fallback a
// successful install would still probe as absent. antigravity has no entry: it
// has no command-line installer at all.
var knownInstallPaths = map[string]string{
	"grok":     ".grok/bin/grok",
	"kimi":     ".kimi-code/bin/kimi",
	"opencode": ".opencode/bin/opencode",
	"qwen":     ".local/bin/qwen",
	"qoder":    ".local/bin/qoder",
}

// probe is the one installation probe a profile uses: Runtime.Probe and every
// installer's re-probe build it here, so install and probe cannot disagree
// about where the binary is looked for.
func probe(id string) pathProbe {
	p := pathProbe{command: id}
	if rel, ok := knownInstallPaths[id]; ok {
		if path := ops.FallbackPath(rel); path != "" {
			p.fallbacks = []string{path}
		}
	}
	return p
}

// pathProbe answers "is it installed" from PATH and the vendor's known install
// location: ops.Find is a LookPath plus a stat of each fallback, so no process
// is started and no version is read (Version stays empty). The empty version is
// load-bearing: opencode's Select picks its release line from it and treats an
// unprobed copy as v1.
type pathProbe struct {
	command string
	// fallbacks are absolute known install locations, tried in order after PATH.
	fallbacks []string
}

// find is the probe's own lookup as a path, so a command that has to run the
// binary (opencode's auth status) runs the one the probe reports instead of a
// second guess.
func (p pathProbe) find() (string, error) {
	path, _, ok := ops.Find(p.command, p.fallbacks...)
	if ok {
		return path, nil
	}
	if len(p.fallbacks) == 0 {
		return "", fmt.Errorf("acp: no %s executable on PATH", p.command)
	}
	return "", fmt.Errorf("acp: no %s executable on PATH or at %s", p.command, strings.Join(p.fallbacks, ", "))
}

// Installation reports where the binary was found: Source "path" for PATH and
// "known" for a fallback. Neither found is Available:false, no error.
func (p pathProbe) Installation(context.Context) (oarlock.Installation, error) {
	path, source, ok := ops.Find(p.command, p.fallbacks...)
	if !ok {
		return oarlock.Installation{}, nil
	}
	return oarlock.Installation{Available: true, Path: path, Source: source}, nil
}
