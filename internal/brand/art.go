// Package brand turns the SAND Harbor icon artwork into every size and shape
// a platform asks for.
//
// The artwork is one square image, brand/icon.png: a scene on a navy plate
// that runs edge to edge with rounded corners, and black outside those
// corners. Platforms disagree about corners. A browser tab or an unmasked
// launcher wants the rounded plate on transparency; iOS and Android's
// maskable and adaptive icons want a full-bleed square they can cut their own
// shape out of, and show a black corner as a black corner. So the corners are
// the one thing this package redraws: it never touches the scene itself.
package brand

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
)

// Art is the master artwork and what is known about its plate.
type Art struct {
	// full is the artwork with the area outside its rounded plate repainted
	// in the plate colour: a full-bleed square.
	full  *image.NRGBA
	plate color.NRGBA
	// radius is the plate's corner radius as a fraction of the width.
	radius float64
}

// fringe is how far inside the plate's outline, in master pixels, the corner
// repaint reaches. The artwork's own edge fades from navy to black over a
// couple of pixels; painting a little way inside the line takes that dark
// rim with it, and the plate is flat colour there, so nothing else is lost.
const fringe = 3.0

// NewArt prepares master artwork. img must be square; plate is the colour of
// its background plate and radius that plate's corner radius as a fraction
// of the width (0 < radius ≤ 0.5).
func NewArt(img image.Image, plate color.NRGBA, radius float64) (Art, error) {
	b := img.Bounds()
	if b.Dx() != b.Dy() || b.Dx() == 0 {
		return Art{}, fmt.Errorf("brand: artwork is %dx%d, want a square", b.Dx(), b.Dy())
	}
	if radius <= 0 || radius > 0.5 {
		return Art{}, fmt.Errorf("brand: corner radius %v is outside (0, 0.5]", radius)
	}
	plate.A = 255

	size := b.Dx()
	full := image.NewNRGBA(image.Rect(0, 0, size, size))
	draw.Draw(full, full.Rect, img, b.Min, draw.Src)

	s := float64(size)
	half := s / 2
	r := radius * s
	for y := range size {
		for x := range size {
			d := roundedRectDistance(float64(x)+0.5, float64(y)+0.5, half, half, half-fringe, half-fringe, r-fringe)
			outside := 1 - coverage(d) // how much of this pixel lies off the plate
			if outside <= 0 {
				continue
			}
			i := full.PixOffset(x, y)
			p := full.Pix[i : i+4 : i+4]
			p[0] = lerp(p[0], plate.R, outside)
			p[1] = lerp(p[1], plate.G, outside)
			p[2] = lerp(p[2], plate.B, outside)
			p[3] = 255
		}
	}
	return Art{full: full, plate: plate, radius: radius}, nil
}

// Plate is the artwork's background colour.
func (a Art) Plate() color.NRGBA { return a.plate }

// Square is the artwork at size×size, full bleed: plate colour into the
// corners, for platforms that mask the icon to a shape of their own.
func (a Art) Square(size int) *image.NRGBA {
	return Resize(a.full, size, size)
}

// Rounded is the artwork at size×size on transparency, with the plate's own
// rounded corners — for platforms that show an icon as it is drawn. The
// corners are cut at the output size, so their antialiasing is exact.
func (a Art) Rounded(size int) *image.NRGBA {
	img := a.Square(size)
	s := float64(size)
	half := s / 2
	for y := range size {
		for x := range size {
			c := coverage(roundedRectDistance(float64(x)+0.5, float64(y)+0.5, half, half, half, half, a.radius*s))
			i := img.PixOffset(x, y) + 3
			img.Pix[i] = uint8(math.Round(float64(img.Pix[i]) * c))
		}
	}
	return img
}

// Placed is a width×height canvas of plate colour with the full-bleed
// artwork centred on it, scale times the canvas's shorter side across.
// Because the artwork's background is the plate colour, the join is
// invisible: it is how a maskable icon keeps the scene inside the safe zone,
// and how a wide launcher banner is made from a square icon.
func (a Art) Placed(width, height int, scale float64) (*image.NRGBA, error) {
	if width <= 0 || height <= 0 {
		return nil, errors.New("brand: canvas must have a positive size")
	}
	if scale <= 0 || scale > 1 {
		return nil, fmt.Errorf("brand: scale %v is outside (0, 1]", scale)
	}
	side := int(math.Round(float64(min(width, height)) * scale))
	canvas := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.Draw(canvas, canvas.Rect, image.NewUniform(a.plate), image.Point{}, draw.Src)
	at := image.Pt((width-side)/2, (height-side)/2)
	draw.Draw(canvas, image.Rectangle{Min: at, Max: at.Add(image.Pt(side, side))}, a.Square(side), image.Point{}, draw.Src)
	return canvas, nil
}

func lerp(from, to uint8, t float64) uint8 {
	return uint8(math.Round(float64(from) + (float64(to)-float64(from))*t))
}

// coverage is how much of a pixel a shape covers, given the signed distance
// from the pixel's centre to the shape's edge.
func coverage(d float64) float64 { return math.Min(math.Max(0.5-d, 0), 1) }

// roundedRectDistance is the signed distance from (px, py) to a rounded
// rectangle: negative inside, positive outside.
func roundedRectDistance(px, py, cx, cy, halfW, halfH, radius float64) float64 {
	r := math.Max(0, math.Min(radius, math.Min(halfW, halfH)))
	qx := math.Abs(px-cx) - (halfW - r)
	qy := math.Abs(py-cy) - (halfH - r)
	outside := math.Hypot(math.Max(qx, 0), math.Max(qy, 0))
	return outside + math.Min(math.Max(qx, qy), 0) - r
}
