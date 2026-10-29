package claude

// Tests for the image blocks on the stream-json user line (issue #53 §1): the
// accepted mime set, the refusals that name the path and never the bytes, and
// the exact wire shape — image blocks first, text block last and omitted when
// there is no text, no uuid key when there is no uuid.
//
// The wire shape is read with the same parser the folder uses (sj.ParseRec) and
// compared as decoded maps. Comparing the string would pin key order, which
// JSON does not, and would miss a second key spelled the same way.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzjcool/oarlock"
	sj "github.com/zzjcool/oarlock/internal/streamjson"
)

// ---- helpers ---------------------------------------------------------------

// imgFile writes data to name in a fresh temp dir and returns the path.
func imgFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// imgBlock is the decoded image block the wire must carry for these bytes.
func imgBlock(media string, data []byte) map[string]any {
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": media,
			"data":       base64.StdEncoding.EncodeToString(data),
		},
	}
}

// imgTextBlock is the decoded text block the wire must carry.
func imgTextBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

// imgLine decodes one LF-terminated line and asserts it is exactly one JSON
// object: nothing a path or a prompt carries can start a second line.
func imgLine(t *testing.T, line []byte) sj.Rec {
	t.Helper()
	if len(line) == 0 || line[len(line)-1] != '\n' {
		t.Fatalf("not an LF-terminated line: %q", line)
	}
	body := line[:len(line)-1]
	if bytes.IndexByte(body, '\n') >= 0 {
		t.Fatalf("the line carries an unescaped newline: %q", line)
	}
	rec := sj.ParseRec(body)
	if rec == nil {
		t.Fatalf("the line is not one JSON object: %q", line)
	}
	return rec
}

// imgContent returns message.content of a decoded user line.
func imgContent(t *testing.T, rec sj.Rec) []any {
	t.Helper()
	return sj.AsList(sj.AsRec(rec["message"])["content"])
}

// imgEqual compares decoded JSON values exactly.
func imgEqual(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		t.Fatalf("%s\n got %s\nwant %s", what, g, w)
	}
}

// imgPayload decodes one image block back to its mime type and exact bytes.
func imgPayload(t *testing.T, block any) (string, []byte) {
	t.Helper()
	b := sj.AsRec(block)
	if sj.Str(b["type"]) != "image" {
		t.Fatalf("not an image block: %v", block)
	}
	src := sj.AsRec(b["source"])
	if sj.Str(src["type"]) != "base64" {
		t.Fatalf("image source is not base64: %v", src)
	}
	data, err := base64.StdEncoding.DecodeString(sj.Str(src["data"]))
	if err != nil {
		t.Fatalf("image payload is not standard base64: %v", err)
	}
	return sj.Str(src["media_type"]), data
}

// ---- media type ------------------------------------------------------------

// The accepted set is a whitelist: an explicit MediaType wins over the
// extension, but only one of the four accepted types can win.
func TestMediaTypeOfPrefersTheExplicitTypeAndWhitelistsExtensions(t *testing.T) {
	cases := []struct {
		name string
		img  oarlock.InputImage
		want string
	}{
		{"png", oarlock.InputImage{Path: "/shots/a.png"}, "image/png"},
		{"jpg", oarlock.InputImage{Path: "/shots/a.jpg"}, "image/jpeg"},
		{"jpeg", oarlock.InputImage{Path: "/shots/a.jpeg"}, "image/jpeg"},
		{"gif", oarlock.InputImage{Path: "/shots/a.gif"}, "image/gif"},
		{"webp", oarlock.InputImage{Path: "/shots/a.webp"}, "image/webp"},
		{"extension is case-insensitive", oarlock.InputImage{Path: "/shots/A.JpEg"}, "image/jpeg"},
		{"explicit wins over the extension", oarlock.InputImage{Path: "/shots/a.jpg", MediaType: "image/png"}, "image/png"},
		{"explicit rescues an unknown extension", oarlock.InputImage{Path: "/shots/a.txt", MediaType: "image/webp"}, "image/webp"},
		{"explicit outside the set is not accepted", oarlock.InputImage{Path: "/shots/a.png", MediaType: "image/tiff"}, ""},
		{"explicit is not a free pass", oarlock.InputImage{Path: "/shots/a.png", MediaType: "application/pdf"}, ""},
		{"unknown extension", oarlock.InputImage{Path: "/shots/a.tiff"}, ""},
		{"no extension", oarlock.InputImage{Path: "/shots/image"}, ""},
		{"empty path", oarlock.InputImage{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mediaTypeOf(c.img); got != c.want {
				t.Fatalf("mediaTypeOf(%+v) = %q, want %q", c.img, got, c.want)
			}
		})
	}
}

// ---- refusals --------------------------------------------------------------

// A type outside the set is refused unsupported with the path in the reason; an
// unreadable file of an accepted type is refused error. Neither reason carries
// file content.
func TestContentBlocksRefuseUnsupportedOrUnreadableImages(t *testing.T) {
	const marker = "SECRET-BYTES-MUST-NOT-LEAK"
	png := imgFile(t, "ok.png", []byte(marker))
	txt := imgFile(t, "notes.txt", []byte(marker))
	tiff := imgFile(t, "scan.tiff", []byte(marker))
	dir := filepath.Join(t.TempDir(), "folder.png")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "gone.png")

	cases := []struct {
		name     string
		img      oarlock.InputImage
		code     oarlock.Code
		want     string // the whole frozen sentence
		wantOpen string // the frozen opener; the OS text follows
	}{
		{
			name: "a .txt file", img: oarlock.InputImage{Path: txt},
			code: oarlock.Unsupported, want: "not a png, jpeg, gif or webp image: " + txt,
		},
		{
			name: "an unknown extension", img: oarlock.InputImage{Path: tiff},
			code: oarlock.Unsupported, want: "not a png, jpeg, gif or webp image: " + tiff,
		},
		{
			name: "an explicit type outside the set", img: oarlock.InputImage{Path: png, MediaType: "image/tiff"},
			code: oarlock.Unsupported, want: "not a png, jpeg, gif or webp image: " + png,
		},
		{
			name: "a missing file", img: oarlock.InputImage{Path: missing},
			code: oarlock.ErrorCode, wantOpen: "cannot read image " + missing + ": ",
		},
		{
			name: "a directory", img: oarlock.InputImage{Path: dir},
			code: oarlock.ErrorCode, wantOpen: "cannot read image " + dir + ": ",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blocks, refused := contentBlocks("look", []oarlock.InputImage{c.img})
			if refused == nil {
				t.Fatalf("contentBlocks(%+v) was not refused, blocks = %v", c.img, blocks)
			}
			if blocks != nil {
				t.Fatalf("a refusal must not carry blocks: %v", blocks)
			}
			if refused.Kind != oarlock.ResponseRejected || refused.Code != c.code {
				t.Fatalf("refusal = %+v, want rejected/%s", refused, c.code)
			}
			switch {
			case c.want != "":
				if refused.Reason != c.want {
					t.Fatalf("reason = %q, want %q", refused.Reason, c.want)
				}
			case !strings.HasPrefix(refused.Reason, c.wantOpen):
				t.Fatalf("reason = %q, want the opener %q", refused.Reason, c.wantOpen)
			}
			if strings.Contains(refused.Reason, marker) {
				t.Fatalf("the reason leaked file bytes: %q", refused.Reason)
			}
			// A refusal never yields a line: the input is still the caller's.
			line, refusedLine := userMessage("look", "in-1", []oarlock.InputImage{c.img})
			if line != nil || refusedLine == nil || refusedLine.Code != c.code {
				t.Fatalf("userLine = %q, %+v; want no line and the refusal", line, refusedLine)
			}
		})
	}
}

// One bad image refuses the whole line: nothing is half-sent for an input the
// caller keeps.
func TestContentBlocksRefuseTheWholeInputOnTheSecondImage(t *testing.T) {
	png := imgFile(t, "ok.png", []byte("\x89PNG"))
	txt := imgFile(t, "notes.txt", []byte("nope"))

	blocks, refused := contentBlocks("look", []oarlock.InputImage{{Path: png}, {Path: txt}})
	if refused == nil || refused.Code != oarlock.Unsupported {
		t.Fatalf("contentBlocks = %v, %+v; want the .txt refused", blocks, refused)
	}
	if !strings.Contains(refused.Reason, txt) {
		t.Fatalf("reason = %q, want it to name %q", refused.Reason, txt)
	}
	if blocks != nil {
		t.Fatalf("a refusal must not carry the first image either: %v", blocks)
	}
}

// ---- ordering --------------------------------------------------------------

// Image blocks precede the text block and keep the order the caller gave.
func TestContentBlocksImagesFirstThenTextInOrder(t *testing.T) {
	pngBytes := []byte("\x89PNG-one")
	webpBytes := []byte("RIFF-two")
	png := imgFile(t, "one.png", pngBytes)
	webp := imgFile(t, "two.webp", webpBytes)

	line, refused := userMessage("what is this?", "", []oarlock.InputImage{{Path: png}, {Path: webp}})
	if refused != nil {
		t.Fatalf("refused: %+v", refused)
	}
	content := imgContent(t, imgLine(t, line))
	imgEqual(t, "content", content, []any{
		imgBlock("image/png", pngBytes),
		imgBlock("image/webp", webpBytes),
		imgTextBlock("what is this?"),
	})

	if media, data := imgPayload(t, content[0]); media != "image/png" || !bytes.Equal(data, pngBytes) {
		t.Fatalf("first image = %q %q, want the png bytes back", media, data)
	}
	if media, data := imgPayload(t, content[1]); media != "image/webp" || !bytes.Equal(data, webpBytes) {
		t.Fatalf("second image = %q %q, want the webp bytes back", media, data)
	}
}

// Image-only input (the kernel already allows it when text is empty) carries no
// text block at all, not an empty one.
func TestContentBlocksImageOnlyHasNoTextBlock(t *testing.T) {
	pngBytes := []byte("\x89PNG-only")
	png := imgFile(t, "only.png", pngBytes)

	line, refused := userMessage("", "in-only", []oarlock.InputImage{{Path: png}})
	if refused != nil {
		t.Fatalf("refused: %+v", refused)
	}
	imgEqual(t, "user line", any(imgLine(t, line)), any(map[string]any{
		"type": "user",
		"uuid": "in-only",
		"message": map[string]any{"role": "user", "content": []any{
			imgBlock("image/png", pngBytes),
		}},
	}))
}

// No text and no images is empty content, not a refusal: the kernel refuses
// empty input before the adapter is asked, so this is the shape of a case that
// cannot arrive, spelled out so it cannot quietly become an error.
func TestContentBlocksWithNoInputAreEmpty(t *testing.T) {
	blocks, refused := contentBlocks("", nil)
	if refused != nil {
		t.Fatalf("refused: %+v", refused)
	}
	if len(blocks) != 0 {
		t.Fatalf("blocks = %v, want none", blocks)
	}
}

// 64 KiB (well past any inline small-image case) round-trips byte for byte:
// std base64 of the exact file, no truncation and no size limit.
func TestContentBlocksLargeImageRoundTrips(t *testing.T) {
	data := make([]byte, 64<<10)
	for i := range data {
		data[i] = byte(i*31 + 7)
	}
	png := imgFile(t, "big.png", data)

	blocks, refused := contentBlocks("read this", []oarlock.InputImage{{Path: png}})
	if refused != nil {
		t.Fatalf("refused: %+v", refused)
	}
	media, got := imgPayload(t, blocks[0])
	if media != "image/png" || !bytes.Equal(got, data) {
		t.Fatalf("payload = %q of %d bytes, want image/png of the exact %d bytes", media, len(got), len(data))
	}
}

// ---- the wire shape --------------------------------------------------------

// The whole line, decoded: type/user, uuid only when one was given, image
// blocks then the text block.
func TestUserLineWireShape(t *testing.T) {
	pngBytes := []byte("\x89PNG\r\n\x1a\nwire")
	png := imgFile(t, "shot.png", pngBytes)

	cases := []struct {
		name   string
		text   string
		uuid   string
		images []oarlock.InputImage
		want   map[string]any
	}{
		{
			name: "text only, no uuid",
			text: "hello",
			want: map[string]any{"type": "user", "message": map[string]any{
				"role": "user", "content": []any{imgTextBlock("hello")},
			}},
		},
		{
			name: "text with uuid",
			text: "hello",
			uuid: "in-7",
			want: map[string]any{"type": "user", "uuid": "in-7", "message": map[string]any{
				"role": "user", "content": []any{imgTextBlock("hello")},
			}},
		},
		{
			name:   "image before text",
			text:   "what is this",
			uuid:   "in-8",
			images: []oarlock.InputImage{{Path: png}},
			want: map[string]any{"type": "user", "uuid": "in-8", "message": map[string]any{
				"role": "user", "content": []any{imgBlock("image/png", pngBytes), imgTextBlock("what is this")},
			}},
		},
		{
			name:   "image only, no uuid",
			images: []oarlock.InputImage{{Path: png, MediaType: "image/png"}},
			want: map[string]any{"type": "user", "message": map[string]any{
				"role": "user", "content": []any{imgBlock("image/png", pngBytes)},
			}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line, refused := userMessage(c.text, c.uuid, c.images)
			if refused != nil {
				t.Fatalf("refused: %+v", refused)
			}
			rec := imgLine(t, line)
			imgEqual(t, "user line", any(rec), any(c.want))
			if _, has := rec["uuid"]; has != (c.uuid != "") {
				t.Fatalf("uuid key present = %v, want %v: %v", has, c.uuid != "", rec)
			}
		})
	}
}

// A prompt or a path that spells JSON cannot escape the object it rides in.
func TestUserLineTextAndPathCannotInjectJSON(t *testing.T) {
	nasty := "a\nb\"}\n{\"type\":\"user\",\"message\":{\"role\":\"user\"}}\n"
	png := imgFile(t, "we{ird} name.png", []byte("\x89PNG"))

	line, refused := userMessage(nasty, "", []oarlock.InputImage{{Path: png}})
	if refused != nil {
		t.Fatalf("refused: %+v", refused)
	}
	content := imgContent(t, imgLine(t, line))
	if len(content) != 2 {
		t.Fatalf("blocks = %d, want the image and the text: %v", len(content), content)
	}
	imgEqual(t, "the text block is the text verbatim", content[1], imgTextBlock(nasty))
}

// The no-image line is byte-for-byte the recorded stdin line of a real claude
// run (testdata/steer-stdin.jsonl), so turning images on changed nothing for a
// text-only prompt.
func TestUserLineMatchesTheRecordedStdinLine(t *testing.T) {
	raw := loadFrames(t, "steer-stdin")
	want := sj.ParseRec(raw[0])
	if want == nil {
		t.Fatal("the fixture's first line is not JSON")
	}
	line, refused := userMessage("HOST-PROMPT: say host-answer", "6f1c2d3e-00c0-4a5b-8c7d-0000000000c0", nil)
	if refused != nil {
		t.Fatalf("refused: %+v", refused)
	}
	imgEqual(t, "recorded stdin line", any(imgLine(t, line)), any(want))
}
