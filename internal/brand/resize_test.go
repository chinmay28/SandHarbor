package brand

import (
	"image"
	"image/color"
	"math"
	"testing"
)

func TestWeightsCoverEverySourcePixelOnce(t *testing.T) {
	for _, c := range []struct{ src, n int }{{1254, 48}, {1254, 512}, {10, 3}, {3, 10}, {7, 7}} {
		ws := weights(c.src, c.n)
		if len(ws) != c.n {
			t.Fatalf("%d→%d: %d outputs", c.src, c.n, len(ws))
		}
		used := make([]float64, c.src)
		for i, list := range ws {
			sum := 0.0
			for _, w := range list {
				sum += w.w
				used[w.at] += w.w * float64(c.src) / float64(c.n)
			}
			if math.Abs(sum-1) > 1e-9 {
				t.Errorf("%d→%d: output %d weights sum to %v", c.src, c.n, i, sum)
			}
		}
		for s, u := range used {
			if math.Abs(u-1) > 1e-9 {
				t.Errorf("%d→%d: source %d used %v times", c.src, c.n, s, u)
			}
		}
	}
}

func TestResizeAverages(t *testing.T) {
	// Alternating black and white columns average to mid grey.
	src := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := range 8 {
		for x := range 8 {
			v := uint8(0)
			if x%2 == 1 {
				v = 255
			}
			src.SetNRGBA(x, y, color.NRGBA{v, v, v, 255})
		}
	}
	out := Resize(src, 2, 2)
	for y := range 2 {
		for x := range 2 {
			if c := out.NRGBAAt(x, y); c.R < 127 || c.R > 128 || c.A != 255 {
				t.Errorf("(%d,%d) = %v, want mid grey", x, y, c)
			}
		}
	}
}

func TestResizeDoesNotBleedTransparentColour(t *testing.T) {
	// Half opaque red, half fully transparent "green": the average is red at
	// half alpha, with no green in it.
	src := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	src.SetNRGBA(0, 0, color.NRGBA{255, 0, 0, 255})
	src.SetNRGBA(1, 0, color.NRGBA{0, 255, 0, 0})
	c := Resize(src, 1, 1).NRGBAAt(0, 0)
	if c.R != 255 || c.G != 0 || c.A < 127 || c.A > 128 {
		t.Errorf("got %v, want red at half alpha", c)
	}
}

func TestResizeKeepsFlatColourExact(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 37, 37))
	for i := 0; i < len(src.Pix); i += 4 {
		copy(src.Pix[i:], []uint8{3, 11, 38, 255})
	}
	for _, n := range []int{1, 5, 36, 100} {
		out := Resize(src, n, n)
		for y := range n {
			for x := range n {
				if c := out.NRGBAAt(x, y); c != (color.NRGBA{3, 11, 38, 255}) {
					t.Fatalf("37→%d: (%d,%d) = %v", n, x, y, c)
				}
			}
		}
	}
}
