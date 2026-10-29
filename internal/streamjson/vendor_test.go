package streamjson

// Tests for the vendor extensions a near-dialect runtime needs (cursor,
// issue #22): a Profile that declares its own top-level vocabulary, projects
// assistant content itself, and reads its own frames; and a transport that
// accepts a free-text prompt on stdin. claude's Profile implements none of
// them, so claude's behaviour is what these tests pin as unchanged.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
)

// vendorProfile is the smallest vendor half: one vendor frame type, its own
// content projection, and a tally of what it saw.
type vendorProfile struct {
	vendorFrames []string
	other        []string
	content      int
}

func (p *vendorProfile) Assistant(Rec, bool) {}
func (p *vendorProfile) Result(Rec, []string, bool) (oarlock.TurnOutcome, []oarlock.Event, []oarlock.Event) {
	return oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted}, nil, nil
}
func (p *vendorProfile) Init(Rec, bool) []oarlock.Event { return nil }
func (p *vendorProfile) Compacted(bool)                 {}
func (p *vendorProfile) System(Rec, []string) []oarlock.Event {
	return nil
}
func (p *vendorProfile) Other(m Rec)     { p.other = append(p.other, Str(m["type"])) }
func (p *vendorProfile) Types() []string { return []string{"vendorx"} }
func (p *vendorProfile) VendorFrame(m Rec, _ []string) []oarlock.Event {
	p.vendorFrames = append(p.vendorFrames, Str(m["subtype"]))
	return []oarlock.Event{{Kind: oarlock.EventReasoning, Text: Str(m["text"])}}
}
func (p *vendorProfile) Content(_ []byte, m Rec, _ []string) []oarlock.Event {
	p.content++
	return []oarlock.Event{{Kind: oarlock.EventTextDelta, Text: "vendor:" + Str(AsRec(m["message"])["id"])}}
}

var (
	_ Profile       = (*vendorProfile)(nil)
	_ VendorTypes   = (*vendorProfile)(nil)
	_ FrameReader   = (*vendorProfile)(nil)
	_ ContentReader = (*vendorProfile)(nil)
)

// TestVendorFramesAreKnownAndRead pins both halves of the extension: a declared
// vendor type is not tallied as unknown, its events land in the record, and a
// type nobody declared is still tallied (the MUL-5231 signal stays loud).
func TestVendorFramesAreKnownAndRead(t *testing.T) {
	p := &vendorProfile{}
	f := NewFolder(p)
	fr := f.Fold([]byte(`{"type":"vendorx","subtype":"delta","text":"thinking out loud"}`))
	if got := fr.Events; len(got) != 1 || got[0].Kind != oarlock.EventReasoning || got[0].Text != "thinking out loud" {
		t.Fatalf("vendor frame events = %+v, want one reasoning event", got)
	}
	if unknown := f.Unknown(); len(unknown) != 0 {
		t.Fatalf("unknown tally after a declared vendor type = %v, want empty", unknown)
	}

	f.Fold([]byte(`{"type":"mystery","subtype":"later"}`))
	if unknown := f.Unknown(); unknown["mystery"] != 1 {
		t.Fatalf("unknown tally = %v, want mystery:1", unknown)
	}
	// The vendor hook sees every frame the fold does not handle, the undeclared
	// one included: knowing which types are the vendor's is the vendor's job,
	// the tally above is the fold's.
	if len(p.vendorFrames) != 2 || p.vendorFrames[0] != "delta" || p.vendorFrames[1] != "later" {
		t.Fatalf("vendor hook saw %v, want both unhandled frames in order", p.vendorFrames)
	}
}

// TestVendorContentReplacesClaudeProjection pins that a ContentReader is
// authoritative for assistant frames: the shared projector (which would emit
// the frame's text) does not also run.
func TestVendorContentReplacesClaudeProjection(t *testing.T) {
	p := &vendorProfile{}
	f := NewFolder(p)
	raw := []byte(`{"type":"assistant","message":{"id":"m-1","content":[{"type":"text","text":"hello"}]}}`)
	fr := f.Fold(raw)
	if p.content != 1 {
		t.Fatalf("vendor content hook called %d times, want 1", p.content)
	}
	if len(fr.Events) != 1 || fr.Events[0].Text != "vendor:m-1" {
		t.Fatalf("events = %+v, want the vendor projection only", fr.Events)
	}
}

// claudeProfile is a Profile with no extensions: the control group.
type claudeProfile struct{ other int }

func (p *claudeProfile) Assistant(Rec, bool) {}
func (p *claudeProfile) Result(Rec, []string, bool) (oarlock.TurnOutcome, []oarlock.Event, []oarlock.Event) {
	return oarlock.TurnOutcome{Kind: oarlock.OutcomeCompleted}, nil, nil
}
func (p *claudeProfile) Init(Rec, bool) []oarlock.Event       { return nil }
func (p *claudeProfile) Compacted(bool)                       {}
func (p *claudeProfile) System(Rec, []string) []oarlock.Event { return nil }
func (p *claudeProfile) Other(Rec)                            { p.other++ }

// TestProfileWithoutExtensionsKeepsClaudeBehaviour pins that the extensions are
// optional: an undeclared type is tallied, and assistant content goes through
// the shared projector.
func TestProfileWithoutExtensionsKeepsClaudeBehaviour(t *testing.T) {
	p := &claudeProfile{}
	f := NewFolder(p)
	if unknown := f.Unknown(); len(unknown) != 0 {
		t.Fatalf("fresh folder unknown tally = %v, want empty", unknown)
	}
	fr := f.Fold([]byte(`{"type":"vendorx","subtype":"delta"}`))
	if len(fr.Events) != 0 {
		t.Fatalf("undeclared frame events = %+v, want none", fr.Events)
	}
	if unknown := f.Unknown(); unknown["vendorx"] != 1 {
		t.Fatalf("unknown tally = %v, want vendorx:1", unknown)
	}
	if p.other != 1 {
		t.Fatalf("Other called %d times, want 1", p.other)
	}
	fr = f.Fold([]byte(`{"type":"assistant","message":{"id":"m-1","content":[{"type":"text","text":"hello"}]}}`))
	if len(fr.Events) != 1 || fr.Events[0].Text != "hello" {
		t.Fatalf("claude projection events = %+v, want the frame's text", fr.Events)
	}
}

// ---- free-text stdin (WriteRaw + CloseStdin) --------------------------------

const rawModeEnv = "STREAMJSON_RAW_HELPER_MODE"

// TestRawHelperProcess reads stdin to EOF — cursor-agent's prompt contract — and
// reports what it got as one frame.
func TestRawHelperProcess(t *testing.T) {
	if os.Getenv(rawModeEnv) == "" {
		return
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(90)
	}
	sum := sha256.Sum256(data)
	os.Stdout.WriteString(`{"type":"raw","n":` + itoa(len(data)) + `,"sha":"` + hex.EncodeToString(sum[:]) + `"}` + "\n")
	os.Exit(0)
}

// TestWriteRawThenCloseStdinIsEOF pins the cursor prompt path: arbitrary bytes
// (newlines included), then EOF. A 1 MiB prompt must not deadlock against a
// child that has not read anything yet.
func TestWriteRawThenCloseStdinIsEOF(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p, err := Launch(ctx, Options{
		Command:   exe,
		Args:      []string{"-test.run=TestRawHelperProcess$"},
		Env:       append(os.Environ(), "GORACE=atexit_sleep_ms=0", rawModeEnv+"=1"),
		KillGrace: time.Second, WaitDelay: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	prompt := []byte(strings.Repeat("line of a large prompt\n", 1<<16)) // > 1 MiB, many LFs
	if err := p.WriteRaw(ctx, prompt); err != nil {
		t.Fatalf("WriteRaw: %v", err)
	}
	if err := p.CloseStdin(); err != nil {
		t.Fatalf("CloseStdin: %v", err)
	}
	if err := p.CloseStdin(); err != nil {
		t.Fatalf("CloseStdin is not idempotent: %v", err)
	}
	sum := sha256.Sum256(prompt)

	select {
	case m := <-p.Events():
		rec := ParseRec(m.Raw)
		if got, _ := AsNum(rec["n"]); int(got) != len(prompt) {
			t.Fatalf("child read %v bytes, want %d", rec["n"], len(prompt))
		}
		if Str(rec["sha"]) != hex.EncodeToString(sum[:]) {
			t.Fatalf("child got different bytes than were written")
		}
	case <-ctx.Done():
		t.Fatal("no frame from the child: stdin EOF or the large write deadlocked")
	}

	// An empty prompt is legal: EOF alone starts the run.
	if err := p.WriteRaw(ctx, nil); err != nil {
		t.Fatalf("WriteRaw(empty): %v", err)
	}
}
