package files

import "testing"

func TestCaptionableImage(t *testing.T) {
	for mediaType, want := range map[string]bool{
		"image/png":                true,
		"IMAGE/JPEG":               true,
		"image/jpg":                true,
		" image/webp; charset=x ":  true,
		"image/heic":               true,
		"image/heif":               true,
		"image/gif":                false,
		"image/svg+xml":            false,
		"application/pdf":          false,
		"":                         false,
		"application/octet-stream": false,
	} {
		if got := CaptionableImage(mediaType); got != want {
			t.Errorf("CaptionableImage(%q) = %v, want %v", mediaType, got, want)
		}
	}
}
