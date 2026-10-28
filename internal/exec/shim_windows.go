//go:build windows

package exec

import (
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
)

// shimRemedy is appended to ErrShim so the failure is actionable: it names
// what to install and why we refuse the obvious workaround.
const shimRemedy = "install with npm/pnpm so a .ps1 or .exe exists; cmd.exe would re-tokenise %* and is refused on purpose"

// resolveInvocation steps around the .cmd/.bat wrapper that npm/pnpm generate.
// A launcher whose <name>.ps1 sibling exists is run as
//
//	powershell -NoProfile -ExecutionPolicy Bypass -File <name>.ps1 <args...>
//
// so Go hands the child one discrete token per caller argument; nothing is
// ever re-tokenised by cmd.exe's %* expansion, which is what mangles
// multi-line prompts (#16).
//
// A .cmd/.bat launcher without that escape hatch (no .ps1 sibling, or no
// PowerShell host) fails with an error that wraps ErrShim and names the path
// plus the remedy. We never fall back to cmd.exe re-parsing: a silently
// re-tokenised prompt is worse than a loud failure.
func resolveInvocation(path string, args []string) (string, []string, error) {
	if exe, argv, ok := resolveShimTarget(path, args, os.Stat, powerShellPath); ok {
		return exe, argv, nil
	}
	if isShimExt(path) {
		return "", nil, fmt.Errorf("%w: %q: %s", ErrShim, path, shimRemedy)
	}
	return path, append([]string{filepath.Base(path)}, args...), nil
}

// powerShellPath resolves the PowerShell host used for .ps1 launchers:
// pwsh.exe from PATH first (PowerShell 7+, which honours -NoProfile and
// -ExecutionPolicy identically), then powershell.exe from PATH, then the
// Windows PowerShell 1.0 installation shipped inside %SystemRoot%. The last
// step is a stat'd absolute path, not a PATH lookup, so it still works when
// PATH is empty (services, sandboxes).
func powerShellPath() (string, bool) {
	for _, name := range []string{"pwsh.exe", "powershell.exe"} {
		if p, err := osexec.LookPath(name); err == nil && p != "" {
			return p, true
		}
	}
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	candidate := filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if fi, err := os.Stat(candidate); err == nil && fi.Mode().IsRegular() {
		return candidate, true
	}
	return "", false
}
