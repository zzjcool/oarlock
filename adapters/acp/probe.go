package acp

import (
	"context"
	"os/exec"

	"github.com/zzjcool/oarlock"
)

// pathProbe answers "is it installed" from PATH alone: no process is started
// and no account or network I/O happens, so Version stays empty (unprobed).
type pathProbe struct{ command string }

func (p pathProbe) Installation(context.Context) (oarlock.Installation, error) {
	path, err := exec.LookPath(p.command)
	if err != nil {
		return oarlock.Installation{}, nil
	}
	return oarlock.Installation{Available: true, Path: path, Source: "path"}, nil
}
