//go:build !unix

package pi

// liveInputPidLiveness is the platform half of the live dispose proof's "no
// leftover process" check (issue #64). This platform has neither kill(0) nor
// /proc, so it cannot answer, and it says so instead of guessing: ok=false, and
// the live test falls back to the stream-only residue proof (dispose response
// recorded, no records appended afterwards, a post-dispose Prompt refused and
// recorded) and logs this reason.
func liveInputPidLiveness(pid int) (gone, ok bool, why string) {
	return false, false, "pid liveness needs unix (kill(0) + /proc); this platform has neither, so the live test proves no residue through the stream instead"
}
