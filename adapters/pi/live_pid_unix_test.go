//go:build unix

package pi

// liveInputPidLiveness is the platform half of the live dispose proof's "no
// leftover process" check (issue #64): the unix answer reuses pidGone from
// conn_unix_test.go, so the live test and the timeout tests share one rule for
// what "gone" means (kill(0) plus the /proc state, a zombie counts as gone).
//
// The live test itself carries no build tag, so the platform-specific half lives
// here and in live_pid_other_test.go: the same two functions, one per GOOS
// family, keep `GOOS=windows go vet ./...` and the offline suite compiling.

// liveInputPidLiveness answers whether pid is gone and whether this platform can
// answer at all. ok=false is never a pass: the live test then proves no residue
// through the stream and says so in its log.
func liveInputPidLiveness(pid int) (gone, ok bool, why string) {
	return pidGone(pid), true, ""
}
