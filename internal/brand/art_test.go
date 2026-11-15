package brand

import (
	"image"
	"image/color"
	"math"
	"testing"
)

var (
	navy   = color.NRGBA{3, 11, 38, 255}
	black  = color.NRGBA{0, 0, 0, 255}
	orange = color.NRGBA{228, 91, 0, 255}
)

// fakeArt draws what the real artwork is: a navy plate, rounded with the
// given radius (as a fraction of size), black outside it, and an orange
// square in the middle standing in for the scene.
func fakeArt(size int, radius float64) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	s := float64(size)
	for y := range size {
		for x := range size {
			c := black
			px, py := float64(x)+0.5, float64(y)+0.5
			if roundedRectDistance(px, py, s/2, s/2, s/2, s/2, radius*s) < 0 {
				c = navy
			}
			if math.Abs(px-s/2) < s/8 && math.Abs(py-s/2) < s/8 {
				c = orange
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

func newFake(t *testing.T) Art {
	t.Helper()
	a, err := NewArt(fakeArt(400, 0.2), navy, 0.2)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func near(a, b color.NRGBA, tol int) bool {
	d := func(x, y uint8) bool { return math.Abs(float64(x)-float64(y)) <= float64(tol) }
	return d(a.R, b.R) && d(a.G, b.G) && d(a.B, b.B) && d(a.A, b.A)
}

func TestNewArtRefusesWhatItCannotUse(t *testing.T) {
	if _, err := NewArt(image.NewNRGBA(image.Rect(0, 0, 10, 12)), navy, 0.2); err == nil {
		t.Error("accepted a non-square image")
	}
	if _, err := NewArt(image.NewNRGBA(image.Rect(0, 0, 0, 0)), navy, 0.2); err == nil {
		t.Error("accepted an empty image")
	}
	for _, r := range []float64{0, -0.1, 0.6} {
		if _, err := NewArt(fakeArt(20, 0.2), navy, r); err == nil {
			t.Errorf("accepted radius %v", r)
		}
	}
}

func TestSquareFillsTheCornersWithThePlate(t *testing.T) {
	img := newFake(t).Square(100)
	if b := img.Bounds(); b.Dx() != 100 || b.Dy() != 100 {
		t.Fatalf("bounds = %v", img.Bounds())
	}
	for _, p := range []image.Point{{0, 0}, {99, 0}, {0, 99}, {99, 99}, {5, 5}} {
		if c := img.NRGBAAt(p.X, p.Y); !near(c, navy, 1) {
			t.Errorf("corner %v = %v, want the plate %v", p, c, navy)
		}
	}
	if c := img.NRGBAAt(50, 50); !near(c, orange, 1) {
		t.Errorf("centre = %v: the scene must be left alone", c)
	}
	// Along the plate's edge, nothing darker than the plate survives: the
	// artwork's black rim is painted out with the corners.
	// (The scene starts 37.5px in; stay well clear of its own edge.)
	for i := range 30 {
		for _, p := range []image.Point{{i, 0}, {0, i}, {i, i}} {
			if c := img.NRGBAAt(p.X, p.Y); c.B < navy.B-2 {
				t.Fatalf("%v = %v: dark rim left at the plate's edge", p, c)
			}
		}
	}
}

func TestRoundedCutsTheCorners(t *testing.T) {
	img := newFake(t).Rounded(100)
	if c := img.NRGBAAt(0, 0); c.A != 0 {
		t.Errorf("corner alpha = %d, want transparent", c.A)
	}
	if c := img.NRGBAAt(50, 1); !near(c, navy, 1) {
		t.Errorf("top edge = %v, want opaque plate", c)
	}
	// Round the corner the edge is antialiased, not stepped.
	partial := 0
	for y := range 20 {
		for x := range 20 {
			if a := img.NRGBAAt(x, y).A; a > 0 && a < 255 {
				partial++
			}
		}
	}
	if partial < 10 {
		t.Errorf("%d partly covered pixels round the corner, want an antialiased edge", partial)
	}
}

func TestPlacedCentresTheArtOnThePlate(t *testing.T) {
	a := newFake(t)
	img, err := a.Placed(200, 100, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 200 || b.Dy() != 100 {
		t.Fatalf("bounds = %v", img.Bounds())
	}
	if c := img.NRGBAAt(10, 50); c != navy {
		t.Errorf("margin = %v, want the plate", c)
	}
	if c := img.NRGBAAt(100, 50); !near(c, orange, 1) {
		t.Errorf("centre = %v, want the scene", c)
	}
	// The art is 50px across, so its orange (a quarter of it) spans 6.25px
	// either side of the centre and no further.
	if c := img.NRGBAAt(100, 60); !near(c, navy, 1) {
		t.Errorf("(100,60) = %v: the art is larger than asked", c)
	}

	for _, bad := range []struct {
		w, h int
		s    float64
	}{{0, 10, 0.5}, {10, 10, 0}, {10, 10, 1.5}} {
		if _, err := a.Placed(bad.w, bad.h, bad.s); err == nil {
			t.Errorf("Placed(%d, %d, %v) accepted", bad.w, bad.h, bad.s)
		}
	}
}

func TestPlate(t *testing.T) {
	a, err := NewArt(fakeArt(10, 0.2), color.NRGBA{1, 2, 3, 0}, 0.2)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Plate(); got != (color.NRGBA{1, 2, 3, 255}) {
		t.Errorf("Plate = %v, want it opaque", got)
	}
}

func TestRoundedRectDistance(t *testing.T) {
	// A 10x10 box centred on the origin, corner radius 2.
	cases := []struct{ x, y, want float64 }{
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
