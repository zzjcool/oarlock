package pi

// Image input tests (issue #52). The adapter reads the caller's files itself
// and puts Pi's own ImageContent parts on the prompt/steer command, only when
// there is at least one; the record keeps the paths and never the bytes.
//
// The fake Conn is session_test.go's coreConn. It keeps the Go params value, so
// a test marshals params["images"] exactly as the real Conn does before
// comparing it with the wire shape.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/oarlock"
	"github.com/zzjcool/oarlock/internal/fold"
)

// pngFixture is a real 1x1 PNG (73 bytes): the bytes a host might send, not a
// placeholder. Only its bytes matter here; the adapter never decodes an image.
var pngFixture = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
	0xde, 0x00, 0x00, 0x00, 0x10, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9c, 0x62, 0xba, 0x64, 0x25, 0x0f,
	0x08, 0x00, 0x00, 0xff, 0xff, 0x03, 0x15, 0x01,
	0x2e, 0x6c, 0xd2, 0x36, 0x72, 0x00, 0x00, 0x00,
	0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60,
	0x82,
}

// ---- helpers ---------------------------------------------------------------

// writeImage writes b into a fresh temp dir as name and returns the path: a
// file the caller owns, like a host's screenshot.
func writeImage(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// wantImagePart is Pi's ImageContent for b, as JSON decoding yields it.
func wantImagePart(b []byte, mimeType string) []any {
	return []any{map[string]any{
		"type":     "image",
		"data":     base64.StdEncoding.EncodeToString(b),
		"mimeType": mimeType,
	}}
}

// wireImages reads params["images"] the way the real Conn puts it on the wire:
// marshal, then decode as JSON, so the comparison is against the exact
// {type,data,mimeType} members and not against a Go struct. It fails when the
// key is absent, which is the text-only shape.
func wireImages(t *testing.T, params map[string]any) []any {
	t.Helper()
	v, has := params["images"]
	if !has {
		t.Fatalf("params carry no images key: %v", params)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal params[images]: %v", err)
	}
	var out []any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("params[images] is not an image array on the wire: %s", raw)
	}
	return out
}

// assertNoImageBytes proves the base64 never reaches a record: the record keeps
// paths, not bytes.
func assertNoImageBytes(t *testing.T, records []oarlock.Record, b []byte) {
	t.Helper()
	encoded := base64.StdEncoding.EncodeToString(b)
	for _, r := range records {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal record %d: %v", r.Seq, err)
		}
		if strings.Contains(string(raw), encoded) {
			t.Fatalf("record %d carries the image bytes: %s", r.Seq, raw)
		}
	}
}

// ---- one image on the wire -------------------------------------------------

// A prompt with one PNG carries Pi's ImageContent with the file's standard
// base64 (no "data:" prefix), and the request record keeps the path and the
// media type, never the bytes.
func TestSessionPromptSendsPiImagePart(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	path := writeImage(t, "shot.png", pngFixture)

	done := make(chan oarlock.ControlOutcome, 1)
	go func() {
		out, _ := sess.Prompt(context.Background(), "what is this", oarlock.PromptOptions{
			InputOptions: oarlock.InputOptions{Images: []oarlock.InputImage{{Path: path, MediaType: "image/png"}}},
		})
		done <- out
	}()
	fc := conn.next(t, "prompt")
	if fc.params["message"] != "what is this" {
		t.Fatalf("prompt message = %v, want the text intact", fc.params["message"])
	}
	if len(fc.params) != 2 {
		t.Fatalf("prompt params = %v, want message and images only", fc.params)
	}
	if got, want := wireImages(t, fc.params), wantImagePart(pngFixture, "image/png"); !reflect.DeepEqual(got, want) {
		t.Fatalf("params[images] = %v, want %v", got, want)
	}
	fc.ok(`{"disposition":"started"}`)

	out := <-done
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt with an image = %+v, want accepted", out)
	}
	imgs := out.Request.Request.Images
	if len(imgs) != 1 || imgs[0].Path != path || imgs[0].MediaType != "image/png" {
		t.Fatalf("request Images = %+v, want the path and its media type", imgs)
	}
	records := sess.Records(oarlock.Cursor{})
	assertNoImageBytes(t, records, pngFixture)
	assertDense(t, records)
}

// Empty text with an image is structurally legal for Pi (the user content is
// text parts followed by images), so the kernel lets it through and the command
// carries message "" plus the part.
func TestSessionEmptyTextWithAnImageIsAccepted(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	path := writeImage(t, "shot.png", pngFixture)

	done := make(chan oarlock.ControlOutcome, 1)
	go func() {
		out, _ := sess.Prompt(context.Background(), "", oarlock.PromptOptions{
			InputOptions: oarlock.InputOptions{Images: []oarlock.InputImage{{Path: path}}},
		})
		done <- out
	}()
	fc := conn.next(t, "prompt")
	if msg, has := fc.params["message"]; !has || msg != "" {
		t.Fatalf("prompt message = %v (present %v), want \"\"", msg, has)
	}
	if got, want := wireImages(t, fc.params), wantImagePart(pngFixture, "image/png"); !reflect.DeepEqual(got, want) {
		t.Fatalf("params[images] = %v, want %v", got, want)
	}
	fc.ok(`{"disposition":"started"}`)
	if out := <-done; out.Kind != oarlock.ControlAccepted {
		t.Fatalf("empty-text image prompt = %+v, want accepted", out)
	}
	assertDense(t, sess.Records(oarlock.Cursor{}))
}

// A text-only command keeps its pre-#52 shape: the "images" key is absent, so
// the params are byte-for-byte {"message":...} and never {"images":null}.
func TestSessionTextOnlyPromptCarriesNoImagesKey(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)

	done := make(chan oarlock.ControlOutcome, 1)
	go func() {
		out, _ := sess.Prompt(context.Background(), "plain", oarlock.PromptOptions{})
		done <- out
	}()
	fc := conn.next(t, "prompt")
	if _, has := fc.params["images"]; has {
		t.Fatalf("a text-only prompt carries an images key: %v", fc.params)
	}
	raw, err := json.Marshal(fc.params)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"message":"plain"}` {
		t.Fatalf("text-only prompt params = %s, want exactly {\"message\":\"plain\"}", raw)
	}
	fc.ok(`{"disposition":"started"}`)
	if out := <-done; out.Kind != oarlock.ControlAccepted {
		t.Fatalf("text-only prompt = %+v, want accepted", out)
	}
}

// ---- media type resolution -------------------------------------------------

// Without an explicit MediaType the extension decides, lowercased.
func TestSessionImageMediaTypeFromExtension(t *testing.T) {
	for _, tc := range []struct{ name, file, want string }{
		{"png", "shot.png", "image/png"},
		{"png uppercase", "shot.PNG", "image/png"},
		{"jpg", "shot.jpg", "image/jpeg"},
		{"jpeg", "shot.jpeg", "image/jpeg"},
		{"gif", "shot.gif", "image/gif"},
		{"webp", "shot.webp", "image/webp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer watchdog(t, 30*time.Second)()
			sess, conn := newCoreSession(t, time.Minute)
			// The bytes are the PNG fixture for every name: only the type is
			// resolved here, and the adapter never sniffs the content.
			path := writeImage(t, tc.file, pngFixture)

			done := make(chan oarlock.ControlOutcome, 1)
			go func() {
				out, _ := sess.Prompt(context.Background(), "look", oarlock.PromptOptions{
					InputOptions: oarlock.InputOptions{Images: []oarlock.InputImage{{Path: path}}},
				})
				done <- out
			}()
			fc := conn.next(t, "prompt")
			if got, want := wireImages(t, fc.params), wantImagePart(pngFixture, tc.want); !reflect.DeepEqual(got, want) {
				t.Fatalf("params[images] = %v, want %v", got, want)
			}
			fc.ok(`{"disposition":"started"}`)
			if out := <-done; out.Kind != oarlock.ControlAccepted {
				t.Fatalf("prompt with %s = %+v, want accepted", tc.file, out)
			}
		})
	}
}

// An explicit MediaType is used verbatim and wins over the extension.
func TestSessionExplicitImageMediaTypeWins(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	path := writeImage(t, "shot.png", pngFixture) // the extension says png

	done := make(chan oarlock.ControlOutcome, 1)
	go func() {
		out, _ := sess.Prompt(context.Background(), "look", oarlock.PromptOptions{
			InputOptions: oarlock.InputOptions{Images: []oarlock.InputImage{{Path: path, MediaType: "image/webp"}}},
		})
		done <- out
	}()
	fc := conn.next(t, "prompt")
	if got, want := wireImages(t, fc.params), wantImagePart(pngFixture, "image/webp"); !reflect.DeepEqual(got, want) {
		t.Fatalf("params[images] = %v, want the explicit %v", got, want)
	}
	fc.ok(`{"disposition":"started"}`)
	out := <-done
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("prompt with an explicit media type = %+v, want accepted", out)
	}
	if imgs := out.Request.Request.Images; len(imgs) != 1 || imgs[0].MediaType != "image/webp" {
		t.Fatalf("request Images = %+v, want the explicit media type", imgs)
	}
}

// ---- refusals --------------------------------------------------------------

// A file that is not a png/jpeg/gif/webp, an explicit media type outside the
// accepted set, and an unreadable file each refuse the whole input: the typed
// code, the reason, no command on the wire and a dense stream.
func TestSessionImageRefusalSendsNothing(t *testing.T) {
	t.Run("unknown extension", func(t *testing.T) {
		defer watchdog(t, 30*time.Second)()
		sess, conn := newCoreSession(t, time.Minute)
		path := writeImage(t, "shot.bmp", pngFixture)

		out, err := sess.Prompt(context.Background(), "look", oarlock.PromptOptions{
			InputOptions: oarlock.InputOptions{Images: []oarlock.InputImage{{Path: path}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := "not a png, jpeg, gif or webp image: " + path
		if out.Kind != oarlock.ControlRejected || out.Code != oarlock.Unsupported || out.Reason != want {
			t.Fatalf("prompt with a .bmp = %+v, want rejected/unsupported %q", out, want)
		}
		if n := conn.countSent("prompt"); n != 0 {
			t.Fatalf("prompt commands = %d, want none for a refused image", n)
		}
		assertDense(t, sess.Records(oarlock.Cursor{}))
	})

	t.Run("unknown explicit media type", func(t *testing.T) {
		defer watchdog(t, 30*time.Second)()
		sess, conn := newCoreSession(t, time.Minute)
		path := writeImage(t, "shot.png", pngFixture)

		out, err := sess.Prompt(context.Background(), "look", oarlock.PromptOptions{
			InputOptions: oarlock.InputOptions{Images: []oarlock.InputImage{{Path: path, MediaType: "image/bmp"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := "not a png, jpeg, gif or webp image: " + path
		if out.Kind != oarlock.ControlRejected || out.Code != oarlock.Unsupported || out.Reason != want {
			t.Fatalf("prompt with an explicit image/bmp = %+v, want rejected/unsupported %q", out, want)
		}
		if n := conn.countSent("prompt"); n != 0 {
			t.Fatalf("prompt commands = %d, want none for a refused image", n)
		}
		assertDense(t, sess.Records(oarlock.Cursor{}))
	})

	t.Run("missing file", func(t *testing.T) {
		defer watchdog(t, 30*time.Second)()
		sess, conn := newCoreSession(t, time.Minute)
		path := filepath.Join(t.TempDir(), "gone.png")

		out, err := sess.Prompt(context.Background(), "look", oarlock.PromptOptions{
			InputOptions: oarlock.InputOptions{Images: []oarlock.InputImage{{Path: path}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := "cannot read image " + path + ": "
		if out.Kind != oarlock.ControlRejected || out.Code != oarlock.ErrorCode || !strings.HasPrefix(out.Reason, want) || len(out.Reason) == len(want) {
			t.Fatalf("prompt with a missing file = %+v, want rejected/error with reason %q plus the os error", out, want)
		}
		if n := conn.countSent("prompt"); n != 0 {
			t.Fatalf("prompt commands = %d, want none for an unreadable image", n)
		}
		assertDense(t, sess.Records(oarlock.Cursor{}))
	})

	t.Run("steer", func(t *testing.T) {
		defer watchdog(t, 30*time.Second)()
		sess, conn := newCoreSession(t, time.Minute)
		startTurn(t, sess, conn, "look")
		path := filepath.Join(t.TempDir(), "gone.png")

		out, err := sess.Steer(context.Background(), "and this", oarlock.InputOptions{
			Images: []oarlock.InputImage{{Path: path}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if out.Kind != oarlock.ControlRejected || out.Code != oarlock.ErrorCode {
			t.Fatalf("steer with a missing file = %+v, want rejected/error", out)
		}
		if n := conn.countSent("steer"); n != 0 {
			t.Fatalf("steer commands = %d, want none for an unreadable image", n)
		}
		if st := statusOf(sess); st.Kind != fold.StatusRunning {
			t.Fatalf("status after a refused image steer = %q, want running", st.Kind)
		}
		assertDense(t, sess.Records(oarlock.Cursor{}))
	})
}

// The status gate wins over the image gate, exactly as OAR orders it: a busy
// prompt is busy (not unsupported) and an idle steer is no_active_turn even
// when the image itself would also be refused.
func TestSessionImageGateComesAfterTheStatusGate(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	startTurn(t, sess, conn, "busy")

	bad := []oarlock.InputImage{{Path: filepath.Join(t.TempDir(), "gone.png")}}
	busy, err := sess.Prompt(context.Background(), "again", oarlock.PromptOptions{InputOptions: oarlock.InputOptions{Images: bad}})
	if err != nil {
		t.Fatal(err)
	}
	if busy.Kind != oarlock.ControlRejected || busy.Code != oarlock.Busy {
		t.Fatalf("prompt with a bad image while running = %+v, want rejected/busy", busy)
	}
	if n := conn.countSent("prompt"); n != 1 {
		t.Fatalf("prompt commands = %d, want only the running turn's one", n)
	}
	settle(conn, "stop", false)
	coreWait(t, "idle", func() bool { return statusOf(sess).Kind == fold.StatusIdle })

	idle, err := sess.Steer(context.Background(), "too early", oarlock.InputOptions{Images: bad})
	if err != nil {
		t.Fatal(err)
	}
	if idle.Kind != oarlock.ControlRejected || idle.Code != oarlock.NoActiveTurn {
		t.Fatalf("image steer with no turn = %+v, want rejected/no_active_turn", idle)
	}
	if n := conn.countSent("steer"); n != 0 {
		t.Fatalf("steer commands = %d, want none", n)
	}
	assertDense(t, sess.Records(oarlock.Cursor{}))
}

// ---- queue ----------------------------------------------------------------

// Images are read at queue time: the queue request keeps the paths, the drain
// sends the same parts without reading the file again, and the drained prompt
// still carries the input id.
func TestSessionQueueHoldsTheImageAndDrainsTheSamePart(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	startTurn(t, sess, conn, "A")

	path := writeImage(t, "shot.png", pngFixture)
	out, err := sess.Queue(context.Background(), "and this one?", oarlock.InputOptions{
		InputID: "in-shot",
		Images:  []oarlock.InputImage{{Path: path}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != oarlock.ControlAccepted {
		t.Fatalf("Queue with an image while busy = %+v, want accepted", out)
	}
	if imgs := out.Request.Request.Images; len(imgs) != 1 || imgs[0].Path != path {
		t.Fatalf("queue request Images = %+v, want the path", imgs)
	}
	if n := conn.countSent("prompt"); n != 1 {
		t.Fatalf("prompt commands = %d: queued input was sent while a turn was running", n)
	}

	// The queue owns the bytes from here: deleting the file must not change
	// what the drain sends.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	settle(conn, "stop", false)

	fc := conn.next(t, "prompt")
	if fc.params["message"] != "and this one?" {
		t.Fatalf("drained message = %v, want %q", fc.params["message"], "and this one?")
	}
	if got, want := wireImages(t, fc.params), wantImagePart(pngFixture, "image/png"); !reflect.DeepEqual(got, want) {
		t.Fatalf("drained params[images] = %v, want %v", got, want)
	}
	fc.ok(`{"disposition":"started"}`)

	var drained *oarlock.Record
	records := sess.Records(oarlock.Cursor{})
	for i := range records {
		r := records[i]
		if r.Kind == oarlock.KindRequest && r.Request.Action == oarlock.ActionPrompt && r.Request.Input == "and this one?" {
			drained = &r
		}
	}
	if drained == nil {
		t.Fatal("the drained prompt left no request record")
	}
	if imgs := drained.Request.Images; len(imgs) != 1 || imgs[0].Path != path {
		t.Fatalf("drained request Images = %+v, want the path", imgs)
	}
	if c := drained.Request.Carries; len(c) != 1 || c[0] != "in-shot" {
		t.Fatalf("drained request Carries = %v, want [in-shot]", c)
	}
	assertNoImageBytes(t, records, pngFixture)
	assertDense(t, records)
}

// A bad image is refused when it is queued: the typed rejection is the
// caller's straight away, and nothing is held for a later, silent failure.
func TestSessionQueueRefusesABadImageAtQueueTime(t *testing.T) {
	defer watchdog(t, 30*time.Second)()
	sess, conn := newCoreSession(t, time.Minute)
	startTurn(t, sess, conn, "A")

	path := filepath.Join(t.TempDir(), "gone.png")
	out, err := sess.Queue(context.Background(), "later", oarlock.InputOptions{
		Images: []oarlock.InputImage{{Path: path}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "cannot read image " + path + ": "
	if out.Kind != oarlock.ControlRejected || out.Code != oarlock.ErrorCode || !strings.HasPrefix(out.Reason, want) {
		t.Fatalf("Queue with a missing image = %+v, want rejected/error with %q", out, want)
	}

	settle(conn, "stop", false)
	coreWait(t, "idle", func() bool { return statusOf(sess).Kind == fold.StatusIdle })
	select {
	case fc := <-conn.cmds:
		t.Fatalf("a refused image input was held anyway: the session sent %q", fc.typ)
	case <-time.After(150 * time.Millisecond):
	}
	if n := conn.countSent("prompt"); n != 1 {
		t.Fatalf("prompt commands = %d, want only the running turn's one", n)
	}
	assertDense(t, sess.Records(oarlock.Cursor{}))
}

// ---- loadInputImages -------------------------------------------------------

// An input with no images loads to nothing at all: that is what makes the
// command's "images" key absent rather than null.
func TestLoadInputImagesEmptySliceIsNil(t *testing.T) {
	for _, in := range [][]oarlock.InputImage{nil, {}} {
		parts, refusal := loadInputImages(in)
		if parts != nil || refusal != nil {
			t.Fatalf("loadInputImages(%v) = %v, %v, want nil, nil", in, parts, refusal)
		}
	}
}
