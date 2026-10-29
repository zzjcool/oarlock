package pi

// Image input (issue #52). Pi's RPC prompt and steer commands take
// images?: ImageContent[], and ImageContent is
// {type:"image", data:string, mimeType:string}, with data plain standard base64
// and no "data:" prefix. The adapter reads the caller's files itself: the
// record keeps the paths (oarlock.InputImage), never the bytes, and the base64
// appears only in the native frame Pi writes.
//
// A refusal here is the whole input's: the caller still owns it and nothing is
// written to the Conn. The status gate comes before this file (see
// session.go), so busy keeps its meaning; OAR does the same.

import (
	"encoding/base64"
	"os"
	"path"
	"strings"

	"github.com/zzjcool/oarlock"
)

// nativeImage is Pi's own image part: what goes on the wire, never what the
// record keeps.
type nativeImage struct {
	Type     string `json:"type"`     // always "image"
	Data     string `json:"data"`     // std base64 of the file bytes
	MimeType string `json:"mimeType"` // resolved media type
}

// imageMediaTypeByExtension is the extension table: how a path without an
// explicit MediaType names its type.
var imageMediaTypeByExtension = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// acceptedImageMediaTypes is the frozen accepted set: exactly the values of
// imageMediaTypeByExtension.
var acceptedImageMediaTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// loadInputImages reads every image. It returns (nil, nil) for an empty slice,
// the loaded parts otherwise, and a *ResponseBody rejection (already in record
// shape) for the first image that cannot go.
//
// The order is: resolve the media type, then read the bytes, for one image
// before the next. Any failure refuses the input as a whole, so it is never
// delivered without the images it came with.
func loadInputImages(images []oarlock.InputImage) ([]nativeImage, *oarlock.ResponseBody) {
	if len(images) == 0 {
		return nil, nil
	}
	parts := make([]nativeImage, 0, len(images))
	for _, img := range images {
		mediaType := imageMediaType(img)
		if !acceptedImageMediaTypes[mediaType] {
			return nil, imageRejected(oarlock.Unsupported, "not a png, jpeg, gif or webp image: "+img.Path)
		}
		data, err := os.ReadFile(img.Path)
		if err != nil {
			// The file is the caller's, not the transport's: an unreadable one
			// is an error code, not a runtime refusal.
			return nil, imageRejected(oarlock.ErrorCode, "cannot read image "+img.Path+": "+err.Error())
		}
		parts = append(parts, nativeImage{
			Type:     "image",
			Data:     base64.StdEncoding.EncodeToString(data),
			MimeType: mediaType,
		})
	}
	return parts, nil
}

// imageMediaType resolves one image's media type: the caller's explicit value
// used verbatim when it gave one, otherwise the file extension lowercased. It
// returns "" when nothing resolves, which is never in the accepted set.
func imageMediaType(img oarlock.InputImage) string {
	if img.MediaType != "" {
		return img.MediaType
	}
	return imageMediaTypeByExtension[strings.ToLower(path.Ext(img.Path))]
}

// imageRejected is a refusal in record shape, ready for the kernel.
func imageRejected(code oarlock.Code, reason string) *oarlock.ResponseBody {
	return &oarlock.ResponseBody{Kind: oarlock.ResponseRejected, Code: code, Reason: reason}
}
