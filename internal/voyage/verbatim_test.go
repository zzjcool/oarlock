package voyage_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/voyage"
	"github.com/zzjcool/oarlock/internal/voyage/pifixture"
)

// record-stream.md: Native is the runtime's frame verbatim. encoding/json
// escapes <, > and & by default, so a naive writer changes the bytes while
// keeping the JSON value; the log must not.
func TestNativeSurvivesTheLogByteForByte(t *testing.T) {
	native := json.RawMessage(`{"type":"x","t":"a<b>&c","n":1.50,"big":12345678901234567890}`)
	path := filepath.Join(t.TempDir(), "v.jsonl")
	w, err := voyage.Create(path, voyage.Header{Runtime: "pi", SessionID: "s"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(oarlock.Record{Seq: 1, Kind: oarlock.KindFrame, SessionID: "s", Native: native}); err != nil {
		t.Fatal(err)
	}
	if err := w.End("test"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `\u003c`) || strings.Contains(string(raw), `\u0026`) {
		t.Fatalf("log rewrote an HTML character inside Native:\n%s", raw)
	}
	recs, err := voyage.Replay(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recs[0].Native, native) {
		t.Fatalf("Native changed:\n got %s\nwant %s", recs[0].Native, native)
	}
}

// The real-wire version of the same claim: every frame record replayed from a
// committed fixture carries exactly the bytes of the matching Pi line in
// testdata/pi (host paths and credentials were normalised when that fixture was
// recorded, not here).
func TestCommittedFixtureNativeEqualsThePiWireLines(t *testing.T) {
	for _, sc := range pifixture.Scenarios() {
		t.Run(sc, func(t *testing.T) {
			wire, err := os.ReadFile(filepath.Join("..", "..", "testdata", "pi", sc+".frames.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			// A Windows checkout may have turned LF into CRLF (core.autocrlf); the
			// Pi line is the JSON object, not its terminator.
			lines := strings.Split(strings.TrimSuffix(string(wire), "\n"), "\n")
			for i := range lines {
				lines[i] = strings.TrimSuffix(lines[i], "\r")
			}
			recs, err := voyage.Replay(filepath.Join("..", "..", "testdata", "voyage", pifixture.Name(sc)+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, r := range recs {
				if r.Kind != oarlock.KindFrame {
					continue
				}
				if n >= len(lines) {
					t.Fatalf("more frame records than Pi lines (%d)", len(lines))
				}
				if string(r.Native) != lines[n] {
					t.Fatalf("frame %d (seq %d) is not the Pi line verbatim:\n got %.200s\nwant %.200s", n+1, r.Seq, r.Native, lines[n])
				}
				n++
			}
			if n != len(lines) {
				t.Fatalf("%d frame records for %d Pi lines: a frame was dropped or synthesized", n, len(lines))
			}
		})
	}
}
