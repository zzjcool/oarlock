package scripted

import (
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
)

// TestScriptedPassesSuite is acceptance 1 for the scripted runtime: the shared
// behaviour suite (RunSuite) passes on it in every run of go test. The same
// suite runs on a live Pi in pi_live_test.go.
func TestScriptedPassesSuite(t *testing.T) {
	RunSuite(t, Target{
		Name: "scripted",
		Open: func(t *testing.T, plan ...Behaviour) oarlock.Session {
			t.Helper()
			turns := make([]Turn, len(plan))
			for i, b := range plan {
				switch b {
				case Quick:
					turns[i] = Turn{Say("ok")}
				case Long:
					turns[i] = Turn{Say("working"), Hang()}
				default:
					t.Fatalf("suite asked for an unknown behaviour %d", b)
				}
			}
			// A short grace keeps a take-over (which the suite must never need)
			// from hiding behind the default second.
			s, err := NewWithOptions(Options{AbortGrace: 200 * time.Millisecond}, turns...)
			if err != nil {
				t.Fatalf("NewWithOptions: %v", err)
			}
			return s
		},
		Prompt: func(b Behaviour) string {
			if b == Long {
				return "run a long turn"
			}
			return "run a quick turn"
		},
	})
}
