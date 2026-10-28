package exec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrShim reports that a .cmd/.bat launcher was requested but cannot be
// started safely. npm/pnpm ship a <name>.cmd wrapper next to the real
// <name>.ps1 script; the wrapper only exists to hand its arguments to
// PowerShell. Starting the wrapper means going through cmd.exe, which
// re-tokenises the raw command line (%*) and destroys arguments that contain
// whitespace, quotes or newlines — the very defect issue #16 removes. When no
// usable .ps1 sibling (or no PowerShell host) exists, Start refuses instead of
// silently switching back to cmd.exe re-parsing.
//
// Callers detect it with errors.Is(err, ErrShim); the wrapped error names the
// launcher path and the remedy.
var ErrShim = errors.New("exec: .cmd/.bat launcher has no usable PowerShell entry point")

// isShimExt reports whether path's extension marks it as a Windows shell
// wrapper (.cmd or .bat, case-insensitively). Both extensions are shims: the
// point is that the argument vector must not be handed to cmd.exe.
func isShimExt(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".cmd", ".bat":
		return true
	default:
		return false
	}
}

// resolveShimTarget is the pure .cmd/.bat → PowerShell decision. It is fully
// injectable (stat and powerShell are supplied by the caller) so it can be
// tested on any platform and so it never reads the environment itself.
//
// Behaviour: only for a path whose extension is .cmd or .bat. The candidate
// script is the sibling strings.TrimSuffix(filepath.Base(path),
// filepath.Ext(path)) + ".ps1" in filepath.Dir(path). If that is a regular
// file and powerShell() yields an executable, the launcher is replaced by
//
//	<powershell> -NoProfile -ExecutionPolicy Bypass -File <ps1> <args...>
//
// returned as exe=<powershell>, argv=[<powershell>, "-NoProfile",
// "-ExecutionPolicy", "Bypass", "-File", <ps1>, <args...>] with ok=true.
// argv[0] is the executable, matching the os/exec convention Start relies on.
//
// Otherwise ok=false. The .cmd body is never opened or parsed — the launcher
// file itself is not evidence of anything, only a real .ps1 sibling is — and
// no environment variable or PATH lookup happens here.
func resolveShimTarget(path string, args []string, stat func(string) (os.FileInfo, error), powerShell func() (string, bool)) (exe string, argv []string, ok bool) {
	if !isShimExt(path) {
		return "", nil, false
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	ps1 := filepath.Join(filepath.Dir(path), base+".ps1")
	if stat == nil {
		return "", nil, false
	}
	fi, err := stat(ps1)
	if err != nil || fi == nil || !fi.Mode().IsRegular() {
		return "", nil, false
	}
	if powerShell == nil {
		return "", nil, false
	}
	host, found := powerShell()
	if !found || host == "" {
		return "", nil, false
	}
	argv = make([]string, 0, 6+len(args))
	argv = append(argv, host, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", ps1)
	argv = append(argv, args...)
	return host, argv, true
}

// resolveInvocation decides which executable and argument vector Start should
// hand to the OS for the already-LookPath-resolved path and the caller's args
// (no argv[0]). It is implemented per platform:
//
//   - Windows (shim_windows.go): a .cmd/.bat launcher whose .ps1 sibling
//     exists is replaced by `powershell -NoProfile -ExecutionPolicy Bypass
//     -File <ps1> <args...>`, so every caller argument stays one discrete
//     token. A .cmd/.bat launcher without that escape hatch fails with an
//     error wrapping ErrShim — cmd.exe re-tokenisation (%*) is refused on
//     purpose, never used as a fallback.
//   - Everything else (shim_other.go): the identity. The returned argv is
//     [filepath.Base(path), args...], i.e. the os/exec argv[0] convention.
//
// Either way the returned argv already contains argv[0], so the caller does
// `osexec.Command(exe, argv...)` without prepending anything.
// resolveInvocation is declared in exactly one of shim_windows.go /
// shim_other.go so the seam is single-defined on every GOOS.
