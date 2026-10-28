//go:build !windows

package exec

import "path/filepath"

// resolveInvocation is the identity on non-Windows platforms: there is no
// .cmd/.bat wrapper to step around, so the resolved executable is started as
// given and argv[0] is the executable's base name (the os/exec convention).
func resolveInvocation(path string, args []string) (exe string, argv []string, err error) {
	return path, append([]string{filepath.Base(path)}, args...), nil
}
