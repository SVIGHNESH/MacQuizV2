package authusers

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"

	// The upload pipeline accepts what students actually have on disk: the
	// stdlib formats register via side effect, WebP comes from x/image.
	_ "image/gif"
	_ "image/png"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// MaxAvatarBytes bounds a single avatar-upload request body. 2 MB accepts
// any reasonable photo while keeping the buffered re-encode pipeline cheap.
const MaxAvatarBytes = 2 << 20

// avatarSize is the stored square edge: 256px covers the largest render
// (56px profile identity) at greater than 4x density.
const avatarSize = 256

// maxAvatarSourcePixels rejects decompression bombs before the full decode:
// a tiny compressed file can declare enormous dimensions, and decoding one
// allocates width*height*4 bytes. 40 MP admits any real camera photo.
const maxAvatarSourcePixels = 40 << 20

// ErrBadAvatarImage marks an upload that is not a decodable PNG, JPEG,
// WebP, or GIF (or one with absurd dimensions); the handler maps it to 422.
var ErrBadAvatarImage = errors.New("not a decodable image")

// processAvatarImage turns a raw upload into the canonical stored form:
// decoded (an animated upload keeps only its first frame), center-cropped
// to a square, downscaled to avatarSize, and re-encoded as JPEG. The
// original bytes are never stored, so every stored avatar is a small
// sanitized image no matter what was uploaded.
func processAvatarImage(raw []byte) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, ErrBadAvatarImage
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxAvatarSourcePixels {
		return nil, ErrBadAvatarImage
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, ErrBadAvatarImage
	}

	b := src.Bounds()
	side := min(b.Dx(), b.Dy())
	x0 := b.Min.X + (b.Dx()-side)/2
	y0 := b.Min.Y + (b.Dy()-side)/2

	dst := image.NewRGBA(image.Rect(0, 0, avatarSize, avatarSize))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, image.Rect(x0, y0, x0+side, y0+side), draw.Src, nil)

	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, fmt.Errorf("encode avatar jpeg: %w", err)
	}
	return out.Bytes(), nil
}
