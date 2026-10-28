package voyage_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
	"github.com/zzjcool/oarlock/internal/redact"
	"github.com/zzjcool/oarlock/internal/voyage"
	"github.com/zzjcool/oarlock/internal/voyage/pifixture"
)

const (
	goldenPiDir  = "../../testdata/pi"
	goldenOutDir = "../../testdata/voyage"
)

type goldenAnswers struct {
	Status  fold.QueryResult[fold.Status]
	Usage   fold.QueryResult[fold.Usage]
	Context fold.QueryResult[*fold.ContextReport]
	Conv    fold.QueryResult[[]fold.Message]
}

func goldenOf(f *fold.Fold) goldenAnswers {
	return goldenAnswers{Status: f.Status(), Usage: f.Usage(), Context: f.ContextUsage(), Conv: f.Conversation()}
}

func goldenFoldAll(recs []oarlock.Record) goldenAnswers { return goldenOf(fold.Of(recs)) }

func goldenFeed(recs []oarlock.Record) goldenAnswers {
	f := fold.New()
	for _, r := range recs {
		f.Feed(r)
	}
	return goldenOf(f)
}

func goldenJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func goldenEqual(t *testing.T, what string, a, b goldenAnswers) {
	t.Helper()
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("%s differ:\n a=%s\n b=%s", what, goldenJSON(t, a), goldenJSON(t, b))
	}
}

func goldenWrite(t *testing.T, scenario string, tr pifixture.Trace, red *redact.Redactor) string {
	t.Helper()
	dir := t.TempDir()
	if err := pifixture.WriteTrace(dir, scenario, tr, red); err != nil {
		t.Fatalf("write: %v", err)
	}
	return filepath.Join(dir, pifixture.Name(scenario)+".jsonl")
}

func goldenI64(v int64) *int64 { return &v }

// goldenPinned are the literals derived BY HAND from the real frames in
// testdata/pi (not from the stand-in code).
//
// normal-turn message_end(assistant).usage = {input 254, output 2, cacheRead
// 256, cacheWrite 0, reasoning 0, totalTokens 512}; model contextWindow
// 1000000. Input = 254+256+0 = 510; context = 512/1000000*100 = 0.0512.
// abort-turn message_end(assistant).usage = all zeros (no reasoning key).
func goldenPinned(t *testing.T, scenario string, a goldenAnswers) {
	t.Helper()
	wantKind := oarlock.OutcomeCompleted
	wantTotal := &oarlock.TokenTotals{Input: 510, Output: 2, Reasoning: goldenI64(0), CacheRead: goldenI64(256), CacheWrite: goldenI64(0)}
	var wantTokens int64 = 512
	wantPct := 0.0512
	if scenario == "abort-turn" {
		wantKind = oarlock.OutcomeAborted
		wantTotal = &oarlock.TokenTotals{Input: 0, Output: 0, CacheRead: goldenI64(0), CacheWrite: goldenI64(0)}
		wantTokens, wantPct = 0, 0
	}
	if a.Status.Value.Kind != fold.StatusIdle {
		t.Fatalf("%s: status = %q, want idle", scenario, a.Status.Value.Kind)
	}
	if o := a.Status.Value.LastTurnOutcome; o == nil || o.Kind != wantKind {
		t.Fatalf("%s: last outcome = %+v, want %s", scenario, o, wantKind)
	}
	if !reflect.DeepEqual(a.Usage.Value.Total, wantTotal) {
		t.Fatalf("%s: usage total = %s, want %s", scenario, goldenJSON(t, a.Usage.Value.Total), goldenJSON(t, wantTotal))
	}
	c := a.Context.Value
	if c == nil || c.Usage.Tokens == nil || c.Usage.ContextWindow == nil || c.Usage.Percent == nil {
		t.Fatalf("%s: context usage missing: %s", scenario, goldenJSON(t, c))
	}
	if *c.Usage.Tokens != wantTokens || *c.Usage.ContextWindow != 1000000 || math.Abs(*c.Usage.Percent-wantPct) > 1e-9 {
		t.Fatalf("%s: context = %s", scenario, goldenJSON(t, c))
	}
}

func goldenFramesWith(t *testing.T, rec oarlock.Record, needle string) bool {
	t.Helper()
	return bytes.Contains(rec.Native, []byte(needle))
}

func TestGoldenReplayEqualsLive(t *testing.T) {
	for _, sc := range pifixture.Scenarios() {
		t.Run(sc, func(t *testing.T) {
			tr, err := pifixture.Build(goldenPiDir, sc, redact.New(nil))
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			live := goldenFoldAll(tr.Records)
			path := goldenWrite(t, sc, tr, redact.New(nil))
			replayed, err := voyage.Replay(path)
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			if goldenJSON(t, tr.Records) != goldenJSON(t, replayed) {
				t.Fatal("replayed records differ from live kernel records")
			}
			goldenEqual(t, "live vs replayed fold", live, goldenFoldAll(replayed))
			goldenPinned(t, sc, live)
			goldenPinned(t, sc, goldenFoldAll(replayed))
		})
	}
}

func TestGoldenDifferentialFeedAndResume(t *testing.T) {
	for _, sc := range pifixture.Scenarios() {
		t.Run(sc, func(t *testing.T) {
			tr, err := pifixture.Build(goldenPiDir, sc, redact.New(nil))
			if err != nil {
				t.Fatal(err)
			}
			replayed, err := voyage.Replay(goldenWrite(t, sc, tr, redact.New(nil)))
			if err != nil {
				t.Fatal(err)
			}
			oneShot := goldenFoldAll(replayed)
			goldenEqual(t, "fold.Of vs Feed", oneShot, goldenFeed(replayed))
			goldenEqual(t, "live Feed vs replayed Of", goldenFeed(tr.Records), oneShot)
			for k := 0; k <= len(replayed); k++ {
				f := fold.Of(replayed[:k])
				for _, r := range replayed[k:] {
					f.Feed(r)
				}
				goldenEqual(t, "resume at k", oneShot, goldenOf(f))
			}
		})
	}
}

func TestGoldenRequestsPlacement(t *testing.T) {
	tr, err := pifixture.Build(goldenPiDir, "abort-turn", redact.New(nil))
	if err != nil {
		t.Fatal(err)
	}
	idx := map[string]int{}
	for i, r := range tr.Records {
		switch {
		case r.Kind == oarlock.KindRequest && r.Request != nil && r.Request.Action == oarlock.ActionPrompt && r.ID == "probe-2":
			idx["prompt"] = i
		case r.Kind == oarlock.KindRequest && r.Request != nil && r.Request.Action == oarlock.ActionAbort && r.ID == "probe-3":
			idx["abort"] = i
		case goldenFramesWith(t, r, `"type":"message_update"`) && idx["update"] == 0:
			idx["update"] = i
		case goldenFramesWith(t, r, `"type":"agent_settled"`):
			idx["settled"] = i
		case goldenFramesWith(t, r, `"command":"abort"`):
			idx["abortResp"] = i
		}
	}
	for _, k := range []string{"prompt", "abort", "update", "settled", "abortResp"} {
		if _, ok := idx[k]; !ok {
			t.Fatalf("missing %s record: %v", k, idx)
		}
	}
	if !(idx["abort"] > idx["update"] && idx["abortResp"] > idx["settled"]) {
		t.Fatalf("abort ordering wrong: %v", idx)
	}
}

func TestGoldenCommittedFixtures(t *testing.T) {
	for _, sc := range pifixture.Scenarios() {
		t.Run(sc, func(t *testing.T) {
			committed := filepath.Join(goldenOutDir, pifixture.Name(sc)+".jsonl")
			recs, err := voyage.Replay(committed)
			if err != nil {
				t.Fatalf("replay committed: %v", err)
			}
			goldenPinned(t, sc, goldenFoldAll(recs))
			tr, err := pifixture.Build(goldenPiDir, sc, redact.New(nil))
			if err != nil {
				t.Fatal(err)
			}
			rebuilt, err := os.ReadFile(goldenWrite(t, sc, tr, redact.New(nil)))
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(committed)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(rebuilt, want) {
				t.Fatalf("%s drifted from testdata/pi: run `go generate ./internal/voyage`", committed)
			}
		})
	}
}

func TestGoldenTruncation(t *testing.T) {
	for _, sc := range pifixture.Scenarios() {
		t.Run(sc, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(goldenOutDir, pifixture.Name(sc)+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			lines := bytes.SplitAfter(data, []byte("\n"))
			lines = lines[:len(lines)-1] // trailing empty element
			cuts := map[string][]byte{
				"no-end":   bytes.Join(lines[:len(lines)-1], nil),
				"mid-line": data[:len(data)-7],
			}
			for name, cut := range cuts {
				p := filepath.Join(t.TempDir(), "cut.jsonl")
				if err := os.WriteFile(p, cut, 0o644); err != nil {
					t.Fatal(err)
				}
				recs, err := voyage.Replay(p)
				if !errors.Is(err, voyage.ErrTruncated) || recs != nil {
					t.Fatalf("%s: Replay = %d records, %v; want nil + ErrTruncated", name, len(recs), err)
				}
				log, err := voyage.Read(p)
				if !errors.Is(err, voyage.ErrTruncated) || log == nil || log.Complete() {
					t.Fatalf("%s: Read err=%v log=%v", name, err, log)
				}
				full, _ := voyage.Replay(filepath.Join(goldenOutDir, pifixture.Name(sc)+".jsonl"))
				n := len(log.Records)
				if n == 0 || n > len(full) {
					t.Fatalf("%s: salvaged %d of %d", name, n, len(full))
				}
				goldenEqual(t, name+" prefix", goldenFoldAll(full[:n]), goldenFoldAll(log.Records))
			}
		})
	}
}

func TestGoldenRedactionOnRealPipeline(t *testing.T) {
	// Assembled at runtime so no scannable credential literal sits in source.
	fake := "sk-" + strings.Repeat("Zq7", 12)
	frames := goldenLines(t, filepath.Join(goldenPiDir, "normal-turn.frames.jsonl"))
	requests := goldenLines(t, filepath.Join(goldenPiDir, "normal-turn.requests.jsonl"))
	extra := []byte(`{"type":"extension_ui_request","id":"x1","method":"notify","message":"leak ` + fake + `"}`)
	frames = append(frames[:len(frames)-1], extra, frames[len(frames)-1])

	red := redact.New([]string{fake})
	tr, err := pifixture.BuildFrom(frames, requests, red)
	if err != nil {
		t.Fatal(err)
	}
	path := goldenWrite(t, "normal-turn", tr, red)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(fake)) || bytes.Contains(raw, []byte("sk-Zq7")) {
		t.Fatal("credential reached the voyage file")
	}
	if !bytes.Contains(raw, []byte("[redacted]")) {
		t.Fatal("expected [redacted] marker in the voyage file")
	}
	if _, err := voyage.Replay(path); err != nil {
		t.Fatalf("redacted fixture does not replay: %v", err)
	}
}

func goldenLines(t *testing.T, path string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for _, l := range bytes.Split(data, []byte("\n")) {
		if l = bytes.TrimSpace(l); len(l) > 0 {
			out = append(out, l)
		}
	}
	return out
}

// TestGoldenClockOrdering pins the header clock: startedAt is on the same
// (synthetic) clock as receivedAt and end.at, so startedAt <= every receivedAt
// <= end.at, for both a fresh build and the committed fixtures.
func TestGoldenClockOrdering(t *testing.T) {
	for _, sc := range pifixture.Scenarios() {
		t.Run(sc, func(t *testing.T) {
			tr, err := pifixture.Build(goldenPiDir, sc, redact.New(nil))
			if err != nil {
				t.Fatal(err)
			}
			paths := map[string]string{
				"built":     goldenWrite(t, sc, tr, redact.New(nil)),
				"committed": filepath.Join(goldenOutDir, pifixture.Name(sc)+".jsonl"),
			}
			for name, path := range paths {
				log, err := voyage.Read(path)
				if err != nil || !log.Complete() || len(log.Records) == 0 {
					t.Fatalf("%s: read: %v", name, err)
				}
				first := log.Records[0].ReceivedAt.UnixMilli()
				last := log.Records[len(log.Records)-1].ReceivedAt.UnixMilli()
				if log.Header.StartedAt > first || last > log.End.At {
					t.Fatalf("%s: startedAt %d, first receivedAt %d, last receivedAt %d, end.at %d: want startedAt <= first <= last <= end.at",
						name, log.Header.StartedAt, first, last, log.End.At)
				}
			}
		})
	}
}
