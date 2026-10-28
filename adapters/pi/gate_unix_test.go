//go:build unix

package pi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// readPID polls path until it holds a pid.
func readPID(t *testing.T, path, what string) int {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the %s never recorded its pid", what)
		}
	}
}

// Acceptance 3 of the issue: a `--version` that never exits is killed, with the
// whole process group, and the caller is not left waiting. The proof is the pids,
// not the goroutines: the leader and a grandchild in the same group must be gone.
func TestCheckVersionTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	defer watchdog(t, 60*time.Second)()
	dir := t.TempDir()
	leaderFile := filepath.Join(dir, "leader.pid")
	grandchildFile := filepath.Join(dir, "grandchild.pid")

	c := fakeVersionCheck(t, "hang-tree", leaderEnv+"="+leaderFile, grandchildEnv+"="+grandchildFile)
	c.Timeout = 2 * time.Second // long enough for both pids to be written

	type result struct {
		version string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		v, err := CheckVersion(context.Background(), c)
		done <- result{v, err}
	}()

	leader := readPID(t, leaderFile, "leader")
	grandchild := readPID(t, grandchildFile, "grandchild")
	if leader == grandchild || pidGone(leader) || pidGone(grandchild) {
		t.Fatalf("before the timeout: leader %d, grandchild %d, want two distinct live processes", leader, grandchild)
	}

	var r result
	select {
	case r = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("CheckVersion did not return: a hung --version hung the caller")
	}
	var pe *VersionProbeError
	if !errors.As(r.err, &pe) || !pe.Timeout || r.version != "" {
		t.Fatalf("CheckVersion = %q, %v; want a timed-out *VersionProbeError", r.version, r.err)
	}
	for deadline := time.Now().Add(5 * time.Second); !pidGone(leader) || !pidGone(grandchild); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("after the timeout: leader %d gone=%v, grandchild %d gone=%v, want both gone",
				leader, pidGone(leader), grandchild, pidGone(grandchild))
		}
	}
	noLeaks(t)
}

// The same through Open: the probe's tree is gone when Open returns its error,
// and no session was started.
func TestOpenVersionTimeoutKillsTheProbeTree(t *testing.T) {
	defer watchdog(t, 60*time.Second)()
	dir := t.TempDir()
	leaderFile := filepath.Join(dir, "leader.pid")
	grandchildFile := filepath.Join(dir, "grandchild.pid")

	e, _ := newGateEnv(t, "hang-tree")
	e.env[leaderEnv] = &leaderFile
	e.env[grandchildEnv] = &grandchildFile
	spec := e.spec(t)
	spec.versionTimeout = 2 * time.Second
	_, err := open(context.Background(), spec, e.options(""))
	var pe *VersionProbeError
	if !errors.As(err, &pe) || !pe.Timeout {
		t.Fatalf("open = %v; want a timed-out *VersionProbeError", err)
	}
	leader := readPID(t, leaderFile, "leader")
	grandchild := readPID(t, grandchildFile, "grandchild")
	for deadline := time.Now().Add(5 * time.Second); !pidGone(leader) || !pidGone(grandchild); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the probe tree outlived Open's error: leader %d gone=%v, grandchild %d gone=%v",
				leader, pidGone(leader), grandchild, pidGone(grandchild))
		}
	}
	if e.started() {
		t.Fatal("a session process was started although the probe timed out")
	}
	noLeaks(t)
}
