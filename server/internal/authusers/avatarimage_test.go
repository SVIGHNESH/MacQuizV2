package authusers

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// encodePNG renders a w x h solid-color PNG for pipeline tests.
func encodePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: 200, G: 80, B: 40, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestProcessAvatarImageCropsAndResizes(t *testing.T) {
	for _, dims := range [][2]int{{400, 200}, {200, 400}, {256, 256}, {31, 47}} {
		out, err := processAvatarImage(encodePNG(t, dims[0], dims[1]))
		if err != nil {
			t.Fatalf("process %dx%d: %v", dims[0], dims[1], err)
		}
		img, err := jpeg.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatalf("decode output for %dx%d: %v", dims[0], dims[1], err)
		}
		if b := img.Bounds(); b.Dx() != avatarSize || b.Dy() != avatarSize {
			t.Fatalf("output for %dx%d is %dx%d, want %dx%d",
				dims[0], dims[1], b.Dx(), b.Dy(), avatarSize, avatarSize)
		}
	}
}

func TestProcessAvatarImageRejectsGarbage(t *testing.T) {
	for _, bad := range [][]byte{nil, []byte("not an image"), encodePNG(t, 10, 10)[:20]} {
		if _, err := processAvatarImage(bad); err == nil {
			t.Fatalf("processAvatarImage(%d bytes of garbage) = nil error, want rejection", len(bad))
		}
	}
}
