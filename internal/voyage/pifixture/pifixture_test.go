package pifixture

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/redact"
)

const piDir = "../../../testdata/pi"

func mustBuild(t *testing.T, name string) Trace {
	t.Helper()
	tr, err := Build(piDir, name, redact.New(nil))
	if err != nil {
		t.Fatalf("Build(%s): %v", name, err)
	}
	return tr
}

func TestBuildIsDeterministic(t *testing.T) {
	for _, sc := range Scenarios() {
		a, b := mustBuild(t, sc), mustBuild(t, sc)
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		if !bytes.Equal(ja, jb) {
			t.Fatalf("%s: two builds differ", sc)
		}
		for i, r := range a.Records {
			if r.Seq != uint64(i+1) {
				t.Fatalf("%s: record %d has seq %d, want dense from 1", sc, i, r.Seq)
			}
		}
	}
}

func TestEveryFrameRetainedVerbatim(t *testing.T) {
	for _, sc := range Scenarios() {
		lines, err := readLines(filepath.Join(piDir, sc+".frames.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		var frames []oarlock.Record
		for _, r := range mustBuild(t, sc).Records {
			if r.Kind == oarlock.KindFrame {
				frames = append(frames, r)
			}
		}
		if len(frames) != len(lines) {
			t.Fatalf("%s: %d frame records for %d Pi lines (none dropped, none synthesized)", sc, len(frames), len(lines))
		}
		for i := range lines {
			if !bytes.Equal(frames[i].Native, lines[i]) {
				t.Fatalf("%s: frame %d Native is not the raw Pi line", sc, i+1)
			}
		}
	}
}

func lastUsageTokens(tr Trace) *oarlock.TokenTotals {
	var out *oarlock.TokenTotals
	for _, r := range tr.Records {
		for _, e := range r.Events {
			if e.Kind == oarlock.EventUsage && e.Usage != nil && e.Usage.Tokens != nil {
				out = e.Usage.Tokens
			}
		}
	}
	return out
}

func lastOutcome(tr Trace) *oarlock.TurnOutcome {
	var out *oarlock.TurnOutcome
	for _, r := range tr.Records {
		for _, e := range r.Events {
			if e.Kind == oarlock.EventTurnEnded {
				out = e.Outcome
			}
		}
	}
	return out
}

func TestUsageCountsCacheBackIntoInput(t *testing.T) {
	// normal-turn assistant usage: input 254, cacheRead 256, cacheWrite 0, output 2.
	tok := lastUsageTokens(mustBuild(t, "normal-turn"))
	if tok == nil || tok.Input != 510 || tok.Output != 2 || tok.CacheRead == nil || *tok.CacheRead != 256 {
		t.Fatalf("usage tokens = %+v, want input 510 output 2 cacheRead 256", tok)
	}
}

func TestSettledOutcome(t *testing.T) {
	if o := lastOutcome(mustBuild(t, "normal-turn")); o == nil || o.Kind != oarlock.OutcomeCompleted {
		t.Fatalf("normal-turn outcome = %+v, want completed", o)
	}
	if o := lastOutcome(mustBuild(t, "abort-turn")); o == nil || o.Kind != oarlock.OutcomeAborted {
		t.Fatalf("abort-turn outcome = %+v, want aborted", o)
	}
}

func TestNameAndScenarios(t *testing.T) {
	if got := Name("normal-turn"); got != "pi-1.4.0-normal-turn" {
		t.Fatalf("Name = %q", got)
	}
	if s := Scenarios(); len(s) != 2 || s[0] != "abort-turn" || s[1] != "normal-turn" {
		t.Fatalf("Scenarios = %v", s)
	}
}
