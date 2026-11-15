package brand

import (
	"image"
	"image/color"
	"math"
)

// Plate says how Render shapes the background plate.
type Plate int

const (
	// Rounded keeps the plate's corners as drawn, on transparency — for
	// browsers and launchers that place an icon without masking it.
	Rounded Plate = iota
	// Square fills the whole canvas. iOS masks its own squircle over an icon
	// and composites transparency onto black, so a rounded plate there reads
	// as a shrunken sticker with a dark fringe.
	Square
)

// RenderOptions sizes and shapes one raster of a mark.
type RenderOptions struct {
	// Width is the output width in pixels. The height follows the mark's
	// aspect ratio.
	Width int
	Plate Plate
	// Inset, if non-zero, shrinks everything but the plate about the centre
	// by this factor — for maskable icons, which a launcher may crop to a
	// circle 80% of the icon's width.
	Inset float64
}

// Render rasterises m. Every edge is antialiased exactly: each shape has a
// signed distance function, and a pixel's coverage is its distance to the
// edge, clamped to one pixel.
func Render(m Mark, opt RenderOptions) *image.NRGBA {
	scale := float64(opt.Width) / m.Width
	height := int(math.Round(m.Height * scale))
	img := image.NewNRGBA(image.Rect(0, 0, opt.Width, height))
	inset := opt.Inset
	if inset == 0 {
		inset = 1
	}
	cx, cy := m.Width/2, m.Height/2

	for _, s := range m.Shapes {
		plate := m.IsPlate(s)
		k := inset
		if plate {
			k = 1
		}
		// Shrink about the canvas centre, then go from mark units to pixels.
		t := s.transformed(cx*(1-k), cy*(1-k), k).transformed(0, 0, scale)
		if plate && opt.Plate == Square {
			t.R = 0
		}

		if t.Fill != nil {
			paint(img, *t.Fill, s.Opacity, t.Distance)
		}
		if t.Stroke != nil && t.StrokeWidth > 0 {
			half := t.StrokeWidth / 2
			paint(img, *t.Stroke, s.Opacity, func(x, y float64) float64 {
				return math.Abs(t.Distance(x, y)) - half
			})
		}
	}
	return img
}

// paint composites c over img wherever dist puts a pixel inside the shape.
// Source-over, un-premultiplied, which is what the PNG stores.
func paint(img *image.NRGBA, c color.NRGBA, opacity float64, dist func(x, y float64) float64) {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			a := coverage(dist(float64(x)+0.5, float64(y)+0.5)) * opacity
			if a <= 0 {
				continue
			}
			i := img.PixOffset(x, y)
			p := img.Pix[i : i+4 : i+4]
			dstA := float64(p[3]) / 255
			outA := a + dstA*(1-a)
			mix := func(src, dst uint8) uint8 {
				return uint8(math.Round((float64(src)*a + float64(dst)*dstA*(1-a)) / outA))
			}
			p[0], p[1], p[2] = mix(c.R, p[0]), mix(c.G, p[1]), mix(c.B, p[2])
			p[3] = uint8(math.Round(outA * 255))
		}
	}
}

// coverage is how much of a pixel a shape covers, given the signed distance
// from the pixel's centre to the shape's edge.
func coverage(d float64) float64 { return math.Min(math.Max(0.5-d, 0), 1) }

// Distance is the signed distance from (x, y) to the shape's outline:
// negative inside, positive outside, in the shape's own units.
func (s Shape) Distance(x, y float64) float64 {
	if s.Kind == Polygon {
		return polygonDistance(s.Points, x, y)
	}
	return roundedRectDistance(x, y, s.X+s.W/2, s.Y+s.H/2, s.W/2, s.H/2, s.R)
}

func roundedRectDistance(px, py, cx, cy, halfW, halfH, radius float64) float64 {
	r := math.Min(radius, math.Min(halfW, halfH))
	qx := math.Abs(px-cx) - (halfW - r)
	qy := math.Abs(py-cy) - (halfH - r)
	outside := math.Hypot(math.Max(qx, 0), math.Max(qy, 0))
	return outside + math.Min(math.Max(qx, qy), 0) - r
}

// polygonDistance is the exact signed distance to a simple polygon: the
// distance to the nearest edge, negated when a crossing count puts the point
// inside.
func polygonDistance(pts []Point, px, py float64) float64 {
	d := math.Inf(1)
	inside := false
	for i, j := 0, len(pts)-1; i < len(pts); j, i = i, i+1 {
		a, b := pts[j], pts[i]
		ex, ey := b.X-a.X, b.Y-a.Y
		wx, wy := px-a.X, py-a.Y
		t := math.Max(0, math.Min(1, (wx*ex+wy*ey)/(ex*ex+ey*ey)))
		d = math.Min(d, math.Hypot(wx-ex*t, wy-ey*t))
		if (a.Y > py) != (b.Y > py) && px < a.X+(py-a.Y)*ex/ey {
			inside = !inside
		}
	}
	if inside {
		return -d
	}
	return d
}
