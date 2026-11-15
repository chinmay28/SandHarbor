package brand

import (
	"image"
	"image/draw"
	"math"
)

// Resize scales src to width×height by area averaging: each output pixel is
// the mean of the source area it covers, with partly covered source pixels
// weighted by how much of them falls inside. For the large reductions icons
// need (1254px down to 48) that is the right filter — nothing is skipped, so
// fine detail turns into the colour it averages to rather than into noise —
// and it needs nothing outside the standard library.
//
// Colour is averaged premultiplied by alpha, so a transparent pixel's colour
// never bleeds into its neighbours.
func Resize(src image.Image, width, height int) *image.NRGBA {
	b := src.Bounds()
	in := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(in, in.Rect, src, b.Min, draw.Src)

	xs := weights(b.Dx(), width)
	ys := weights(b.Dy(), height)

	// Horizontal pass into a premultiplied float buffer, then vertical.
	row := make([]float64, width*b.Dy()*4)
	for y := range b.Dy() {
		for x, ws := range xs {
			var acc [4]float64
			for _, w := range ws {
				i := in.PixOffset(w.at, y)
				a := float64(in.Pix[i+3]) / 255
				acc[0] += float64(in.Pix[i]) * a * w.w
				acc[1] += float64(in.Pix[i+1]) * a * w.w
				acc[2] += float64(in.Pix[i+2]) * a * w.w
				acc[3] += a * w.w
			}
			copy(row[(y*width+x)*4:], acc[:])
		}
	}

	out := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y, ws := range ys {
		for x := range width {
			var acc [4]float64
			for _, w := range ws {
				j := (w.at*width + x) * 4
				for c := range acc {
					acc[c] += row[j+c] * w.w
				}
			}
			i := out.PixOffset(x, y)
			if acc[3] <= 0 {
				continue // fully transparent; leave it zero
			}
			out.Pix[i] = clamp8(acc[0] / acc[3])
			out.Pix[i+1] = clamp8(acc[1] / acc[3])
			out.Pix[i+2] = clamp8(acc[2] / acc[3])
			out.Pix[i+3] = clamp8(acc[3] * 255)
		}
	}
	return out
}

type weight struct {
	at int
	w  float64
}

// weights lists, for each of n output pixels along an axis of src source
// pixels, the source pixels it covers and the share of the output each one
// contributes. Every list sums to 1.
func weights(src, n int) [][]weight {
	scale := float64(src) / float64(n)
	out := make([][]weight, n)
	for i := range n {
		lo, hi := float64(i)*scale, float64(i+1)*scale
		for s := int(lo); s < src && float64(s) < hi; s++ {
			overlap := math.Min(hi, float64(s+1)) - math.Max(lo, float64(s))
			if overlap > 0 {
				out[i] = append(out[i], weight{s, overlap / scale})
			}
		}
	}
	return out
}

func clamp8(v float64) uint8 { return uint8(math.Round(math.Min(math.Max(v, 0), 255))) }
