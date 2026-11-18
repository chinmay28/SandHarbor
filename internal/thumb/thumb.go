// Package thumb makes the thumbnails the vault stores: a JPEG of a known size.
//
// They come two ways. At upload, the tab drew the picture already, and
// Normalize re-encodes it rather than trusting it: that caps the dimensions,
// strips whatever metadata the source carried, and guarantees the stored bytes
// really are an image of the size claimed. For a file that arrived without
// one, FromSource (source.go) draws it on the server from the file itself.
package thumb

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"

	// Registered for their decoders: a browser may hand us any of the three.
	_ "image/gif"
	_ "image/png"
)

// Size is the longest edge of a stored thumbnail, in pixels. It covers the
// 52px tile in the list at three times density with room to spare, and keeps a
// typical picture near 10 KB.
const Size = 256

// Quality is the JPEG quality stored thumbnails are encoded at. Above about
// 80 the file grows faster than the picture improves at this size.
const Quality = 80

// MaxInputBytes is the largest payload accepted for normalizing. A 256px JPEG
// is a few kilobytes; anything approaching this is not one.
const MaxInputBytes = 2 << 20

// maxInputPixels caps the source dimensions before anything is decoded, so a
// small file that claims to be enormous cannot make the server allocate for
// it. Decoding is bounded by width × height, not by the bytes on the wire.
const maxInputPixels = 8192

// Normalize decodes an image and re-encodes it as a JPEG no larger than Size
// on its longest edge.
func Normalize(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("thumbnail is empty")
	}
	if len(data) > MaxInputBytes {
		return nil, fmt.Errorf("thumbnail is larger than %d bytes", MaxInputBytes)
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("this is not an image the vault can read: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("this image has no size")
	}
	if cfg.Width > maxInputPixels || cfg.Height > maxInputPixels {
		return nil, fmt.Errorf("this image is %dx%d, too large for a thumbnail", cfg.Width, cfg.Height)
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("this is not an image the vault can read: %w", err)
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, shrink(src, Size), &jpeg.Options{Quality: Quality}); err != nil {
		return nil, fmt.Errorf("encoding the thumbnail: %w", err)
	}
	return out.Bytes(), nil
}

// shrink scales an image down so that neither side exceeds max, and returns it
// untouched when it is already small enough.
//
// The filter is a box average: each destination pixel is the mean of the
// source pixels it covers. It is what a downscale of this size wants anyway —
// the alternative is a resampling dependency for an image that ends up 52
// pixels wide on a phone.
func shrink(src image.Image, max int) image.Image {
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= max && h <= max {
		return src
	}

	dw, dh := w, h
	if w >= h {
		dw = max
		dh = h * max / w
	} else {
		dh = max
		dw = w * max / h
	}
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}

	// Work in RGBA so the averaging below reads uniform 8-bit channels
	// whatever the source's own model is — but one band of source rows at a
	// time, the rows that make up one row of the thumbnail, rather than the
	// whole picture at once. A 48-megapixel photograph is 190 MB as RGBA; a
	// band of it is a few hundred kilobytes.
	band := image.NewRGBA(image.Rect(0, 0, w, h/dh+2))
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		y0 := bounds.Min.Y + y*h/dh
		y1 := bounds.Min.Y + (y+1)*h/dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		rows := image.Rect(0, 0, w, y1-y0)
		if rows.Dy() > band.Bounds().Dy() {
			band = image.NewRGBA(rows)
		}
		draw.Draw(band, rows, src, image.Pt(bounds.Min.X, y0), draw.Src)

		for x := 0; x < dw; x++ {
			x0 := x * w / dw
			x1 := (x + 1) * w / dw
			if x1 <= x0 {
				x1 = x0 + 1
			}

			var r, g, b, a, n uint32
			for sy := 0; sy < y1-y0; sy++ {
				row := band.PixOffset(x0, sy)
				for sx := x0; sx < x1; sx++ {
					p := band.Pix[row : row+4 : row+4]
					r += uint32(p[0])
					g += uint32(p[1])
					b += uint32(p[2])
					a += uint32(p[3])
					n++
					row += 4
				}
			}
			if n == 0 {
				continue
			}

			o := dst.PixOffset(x, y)
			dst.Pix[o] = uint8(r / n)
			dst.Pix[o+1] = uint8(g / n)
			dst.Pix[o+2] = uint8(b / n)
			dst.Pix[o+3] = uint8(a / n)
		}
	}
	return dst
}
