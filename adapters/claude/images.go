package claude

// Images for the stream-json user line (issue #53 §1). Claude takes images
// natively: one base64 source block each, up to the four mime types OAR's
// input-images table names. The record keeps the path, the wire keeps the
// bytes, and the bytes reach nothing else — not a reason, not a log, not an
// error.
//
// The line is built with encoding/json and never with string formatting, so a
// path or a prompt cannot close the JSON object it rides in.

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzjcool/oarlock"
)

// imageMediaTypes is the accepted set: exactly the four image/* types OAR
// accepts (input-images.ts:15-24), keyed by the lower-case extension that
// stands for them.
var imageMediaTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// mediaTypeOf: explicit MediaType, else the extension table; "" when not
// accepted. An explicit type wins over the extension, but it must be one of
// the four accepted types to win: the accepted set is a whitelist, not a hint,
// so anything else stays "" and is refused by the caller.
func mediaTypeOf(img oarlock.InputImage) string {
	if img.MediaType != "" {
		for _, mt := range imageMediaTypes {
			if img.MediaType == mt {
				return mt
			}
		}
		return ""
	}
	return imageMediaTypes[strings.ToLower(filepath.Ext(img.Path))]
}

// contentBlocks: image blocks then the text block (nil when text == ""). A
// refusal is returned as a response body the caller records; the blocks are
// then nil, because nothing is sent for an input that was not taken over.
//
// The mime type is checked before the file is read: a type outside the
// accepted set is refused without touching the disk, and a readable file of an
// accepted type is the only thing that can fail the read.
func contentBlocks(text string, images []oarlock.InputImage) ([]any, *oarlock.ResponseBody) {
	blocks := make([]any, 0, len(images)+1)
	for _, img := range images {
		media := mediaTypeOf(img)
		if media == "" {
			return nil, refusedImage(oarlock.Unsupported, "not a png, jpeg, gif or webp image: "+img.Path)
		}
		data, err := os.ReadFile(img.Path)
		if err != nil {
			return nil, refusedImage(oarlock.ErrorCode, "cannot read image "+img.Path+": "+err.Error())
		}
		blocks = append(blocks, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": media,
				"data":       base64.StdEncoding.EncodeToString(data),
			},
		})
	}
	if text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	return blocks, nil
}

// userMessage is one stream-json user line with its native image blocks first
// and the text block last (omitted when text is empty). A refusal is returned
// as a response body the caller records; err is nil in that case.
//
// The uuid key is omitted when uuid is empty: claude's own replay echo matches
// on it, and an empty id must not be spelled as a real one.
func userMessage(text, uuid string, images []oarlock.InputImage) ([]byte, *oarlock.ResponseBody) {
	blocks, refused := contentBlocks(text, images)
	if refused != nil {
		return nil, refused
	}
	msg := map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": blocks},
	}
	if uuid != "" {
		msg["uuid"] = uuid
	}
	b, err := json.Marshal(msg)
	if err != nil {
		// Unreachable: the map holds strings and nested maps of strings only.
		// A caller that gets a line must be able to write it, so a partial one
		// is never returned.
		return nil, refusedImage(oarlock.ErrorCode, "cannot marshal the user message: "+err.Error())
	}
	return append(b, '\n'), nil
}

// userLine is userMessage under the name this file's task spelled. Both are
// kept while the siblings land so either caller compiles; the contract's name
// is userMessage.
func userLine(text, uuid string, images []oarlock.InputImage) ([]byte, *oarlock.ResponseBody) {
	return userMessage(text, uuid, images)
}

// refusedImage is the response body a caller records: the input is still the
// caller's, and the reason names the path and the problem, never the bytes.
func refusedImage(code oarlock.Code, reason string) *oarlock.ResponseBody {
	body := rejected(code, reason)
	return &body
}
