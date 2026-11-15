package brand

import (
	"image"
	"image/color"
	"math"
	"testing"
)

var (
	navy  = color.NRGBA{0x0a, 0x0e, 0x17, 0xff}
	amber = color.NRGBA{0xd9, 0x77, 0x06, 0xff}
)

// plateMark is a 64-unit plate with rounded corners and an amber square in
// the middle, 16 units across.
func plateMark() Mark {
	return Mark{Width: 64, Height: 64, Shapes: []Shape{
		{Kind: Rect, W: 64, H: 64, R: 14, Fill: &navy, Opacity: 1},
		{Kind: Rect, X: 24, Y: 24, W: 16, H: 16, Fill: &amber, Opacity: 1},
	}}
}

func at(img *image.NRGBA, x, y int) color.NRGBA { return img.NRGBAAt(x, y) }

func TestRenderRoundedKeepsCornersTransparent(t *testing.T) {
	img := Render(plateMark(), RenderOptions{Width: 64})
	if b := img.Bounds(); b.Dx() != 64 || b.Dy() != 64 {
		t.Fatalf("bounds = %v", b)
	}
	if c := at(img, 0, 0); c.A != 0 {
		t.Errorf("corner = %v, want transparent", c)
	}
	if c := at(img, 32, 4); c != navy {
		t.Errorf("plate = %v, want %v", c, navy)
	}
	if c := at(img, 32, 32); c != amber {
		t.Errorf("centre = %v, want %v", c, amber)
	}
}

func TestRenderSquareFillsTheCanvas(t *testing.T) {
	img := Render(plateMark(), RenderOptions{Width: 128, Plate: Square})
	for _, p := range []image.Point{{0, 0}, {127, 0}, {0, 127}, {127, 127}} {
		if c := at(img, p.X, p.Y); c != navy {
			t.Errorf("corner %v = %v, want opaque plate", p, c)
		}
	}
}

func TestRenderInsetShrinksAllButThePlate(t *testing.T) {
	// At 64px one unit is one pixel; the amber square spans 24..40. Inset by
	// half about the centre, it spans 28..36.
	img := Render(plateMark(), RenderOptions{Width: 64, Plate: Square, Inset: 0.5})
	if c := at(img, 26, 32); c != navy {
		t.Errorf("(26,32) = %v, want plate: the square should have shrunk away from it", c)
	}
	if c := at(img, 30, 32); c != amber {
		t.Errorf("(30,32) = %v, want the shrunk square", c)
	}
	if c := at(img, 0, 0); c != navy {
		t.Errorf("corner = %v: the plate must not shrink", c)
	}
}

func TestRenderAntialiasesEdges(t *testing.T) {
	half := Shape{Kind: Rect, X: 0, Y: 0, W: 2.5, H: 4, Fill: &amber, Opacity: 1}
	img := Render(Mark{Width: 4, Height: 4, Shapes: []Shape{half}}, RenderOptions{Width: 4})
	if c := at(img, 1, 1); c.A != 255 {
		t.Errorf("inside alpha = %d, want 255", c.A)
	}
	if c := at(img, 2, 1); c.A < 120 || c.A > 135 || c.R != amber.R {
		t.Errorf("half-covered pixel = %v, want amber at about half alpha", c)
	}
	if c := at(img, 3, 1); c.A != 0 {
		t.Errorf("outside alpha = %d, want 0", c.A)
	}
}

func TestRenderOpacityAndStroke(t *testing.T) {
	ring := Shape{Kind: Rect, X: 2, Y: 2, W: 12, H: 12, Stroke: &amber, StrokeWidth: 2, Opacity: 1}
	faint := Shape{Kind: Rect, X: 6, Y: 6, W: 4, H: 4, Fill: &amber, Opacity: 0.5}
	img := Render(Mark{Width: 16, Height: 16, Shapes: []Shape{ring, faint}}, RenderOptions{Width: 16})

	if c := at(img, 2, 8); c != amber {
		t.Errorf("on the stroke = %v, want amber", c)
	}
	if c := at(img, 4, 8); c.A != 0 {
		t.Errorf("inside the ring = %v, want unpainted", c)
	}
	if c := at(img, 7, 7); c.A != 128 {
		t.Errorf("half-opacity fill alpha = %d, want 128", c.A)
	}
}

func TestRenderFollowsTheAspectRatio(t *testing.T) {
	m := Mark{Width: 320, Height: 180, Shapes: []Shape{{Kind: Rect, W: 320, H: 180, Fill: &navy, Opacity: 1}}}
	if b := Render(m, RenderOptions{Width: 160}).Bounds(); b.Dx() != 160 || b.Dy() != 90 {
		t.Errorf("bounds = %v, want 160x90", b)
	}
}

func TestPolygonDistance(t *testing.T) {
	tri := []Point{{0, 0}, {10, 0}, {0, 10}}
	cases := []struct {
		x, y, want float64
	}{
		{1, 1, -1},               // inside, nearest edges are the axes
		{-3, 5, 3},               // outside, left of the vertical edge
		{5, 5, 0},                // on the hypotenuse
		{10, 10, 5 * math.Sqrt2}, // outside, off the hypotenuse
		{-3, -4, 5},              // outside, nearest a vertex
		{2, 2, -2},               // inside
	}
	for _, c := range cases {
		if got := polygonDistance(tri, c.x, c.y); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("distance(%g,%g) = %g, want %g", c.x, c.y, got, c.want)
		}
	}
}

func TestRoundedRectDistance(t *testing.T) {
	// A 10x10 box centred on the origin, corner radius 2.
	cases := []struct {
		x, y, want float64
	}{
		{0, 0, -5},
		{7, 0, 2},
		{0, -6, 1},
		{5, 5, 2*math.Sqrt2 - 2}, // out past the rounded corner
	}
	for _, c := range cases {
		if got := roundedRectDistance(c.x, c.y, 0, 0, 5, 5, 2); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("distance(%g,%g) = %g, want %g", c.x, c.y, got, c.want)
		}
	}
}
