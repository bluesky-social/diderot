package docs

import (
	"archive/zip"
	"io"
	"path"
	"strings"

	"github.com/bluesky-social/diderot/internal/attachments/files"
)

// Picture is an image stored inside a document: a screenshot pasted into a
// Word file. The worker hands each one to the image caption queue as an
// image of the document's event, so what it shows is searchable even when
// the document has no text at all.
type Picture struct {
	// Name is the picture's path inside the document, unique within it.
	Name      string
	MediaType string
	Data      []byte
}

// Limits on the pictures taken from one document. Each picture costs one
// call to the caption model.
const (
	// MaxPictures is how many pictures of one document are described: the
	// first ones stored in it. A document with more is a photo album or a
	// scan, and the rest are left out.
	MaxPictures = 20
	// minPictureBytes leaves out bullets, rules and icons.
	minPictureBytes = 4 << 10
	// maxPictureBytes is the largest picture the caption model is sent.
	maxPictureBytes = 20 << 20
	// maxPicturesBytes bounds what one document's pictures may hold in
	// memory together, whatever its ZIP directory claims.
	maxPicturesBytes = 64 << 20
)

// pictureTypes are the stored picture formats the caption model reads, by
// file extension. Word also stores vector formats (EMF, WMF) and others it
// does not; those are left out.
var pictureTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".webp": "image/webp", ".heic": "image/heic", ".heif": "image/heif",
}

// docxPictures reads the pictures a Word file stores with its main text
// (word/media/), in the order the file stores them, within the limits above.
// A picture that cannot be read is left out rather than failing the
// document: its text is still worth having.
func docxPictures(zr *zip.Reader) []Picture {
	var pictures []Picture
	var total int64
	for _, f := range zr.File {
		if len(pictures) == MaxPictures {
			break
		}
		if path.Dir(f.Name) != "word/media" {
			continue
		}
		mediaType := pictureTypes[strings.ToLower(path.Ext(f.Name))]
		if !files.CaptionableImage(mediaType) {
			continue
		}
		if f.UncompressedSize64 < minPictureBytes || f.UncompressedSize64 > maxPictureBytes {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		// The declared size is the archive's claim; the limit on the read
		// is what holds. Everything read counts toward the document's
		// total, kept or not, so an archive that lies about its sizes
		// cannot make this loop expand without end.
		data, err := io.ReadAll(io.LimitReader(rc, maxPictureBytes+1))
		_ = rc.Close()
		if total += int64(len(data)); total > maxPicturesBytes {
			break
		}
		if err != nil || len(data) < minPictureBytes || len(data) > maxPictureBytes {
			continue
		}
		pictures = append(pictures, Picture{Name: f.Name, MediaType: mediaType, Data: data})
	}
	return pictures
}
