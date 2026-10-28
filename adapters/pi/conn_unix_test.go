//go:build unix

package pi

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

// pidGone reports whether pid no longer exists. A zombie (state Z or X in
// /proc) has exited and cannot run, so it counts as gone: a grandchild that was
// reparented to a PID 1 which does not reap would otherwise stay visible forever.
func pidGone(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return errors.Is(err, syscall.ESRCH)
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return false
	}
	fields := strings.Fields(s[i+1:])
	return len(fields) > 0 && (fields[0] == "Z" || fields[0] == "X")
}

// Acceptance: the timeout kills the whole process group, not just the pid that
// was started.
func TestTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	marker := filepath.Join(t.TempDir(), "grandchild.pid")
	p := launch(t, "silent-grandchild", Options{CommandTimeout: 300 * time.Millisecond}, markerEnv+"="+marker)

	var grandchild int
	for deadline := time.Now().Add(10 * time.Second); grandchild == 0; time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(marker); err == nil {
			grandchild, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if grandchild == 0 && time.Now().After(deadline) {
			t.Fatal("the grandchild never recorded its pid")
		}
	}
	leader := p.Pid()
	if grandchild == leader || pidGone(grandchild) || pidGone(leader) {
		t.Fatalf("before the timeout: leader %d, grandchild %d, want two distinct live processes", leader, grandchild)
	}

	err := p.Do(context.Background(), Command{Type: "get_state"}, nil)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Do = %v, want ErrTimeout", err)
	}
	for deadline := time.Now().Add(5 * time.Second); !pidGone(leader) || !pidGone(grandchild); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("after the timeout: leader %d gone=%v, grandchild %d gone=%v, want both gone",
				leader, pidGone(leader), grandchild, pidGone(grandchild))
		}
	}
	finish(t, p)
}
