//go:build unix

package streamjson

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func pidGone(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return errors.Is(err, syscall.ESRCH)
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	f := strings.Fields(s[i+1:])
	return len(f) > 0 && (f[0] == "Z" || f[0] == "X")
}

// The process tree is owned: closing kills a grandchild too, not just the pid we started.
func TestCloseKillsTheWholeProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "gc.pid")
	exe, _ := os.Executable()
	p, err := Launch(context.Background(), Options{
		Command: "/bin/sh",
		Args:    []string{"-c", exe + ` -test.run=TestHelperProcess$ & echo $! > ` + marker + `; wait`},
		Env:     append(os.Environ(), modeEnv+"=deaf"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var gc int
	for deadline := time.Now().Add(10 * time.Second); gc == 0; time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(marker); err == nil {
			gc, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if gc == 0 && time.Now().After(deadline) {
			t.Fatal("grandchild never started")
		}
	}
	if pidGone(gc) {
		t.Fatal("grandchild is not running before Close")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !pidGone(gc); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived Close: the process group was not killed", gc)
		}
	}
}
