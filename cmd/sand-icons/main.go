// Command sand-icons draws every SAND Harbor app icon from its mark in brand/.
//
// Each app's mark is drawn once, as an SVG, and everything a platform needs is
// derived from it: copies of the SVG for the web pages that load it, PNGs for
// home screens (iOS will only take a raster touch icon, and a web manifest
// wants known pixel sizes), and vector drawables for the Android apps. The
// outputs are committed; change the SVG, run `make icons`, commit what falls
// out, and never edit an output by hand.
//
// Usage:
//
//	sand-icons [-root DIR]          # write every output
//	sand-icons [-root DIR] -check   # exit 1 if any output is stale
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/chinmay28/SandHarbor/internal/brand"
)

func main() {
	root := flag.String("root", ".", "repository root")
	check := flag.Bool("check", false, "report stale outputs instead of writing them")
	flag.Parse()

	files, err := plan(*root, apps)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sand-icons:", err)
		os.Exit(1)
	}

	if *check {
		stale, err := staleFiles(*root, files)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sand-icons:", err)
			os.Exit(1)
		}
		for _, f := range stale {
			fmt.Fprintf(os.Stderr, "stale: %s\n", f)
		}
		if len(stale) > 0 {
			fmt.Fprintf(os.Stderr, "\n%d icon file(s) out of date — run: make icons\n", len(stale))
			os.Exit(1)
		}
		fmt.Println("icons match brand/")
		return
	}

	for _, f := range files {
		path := filepath.Join(*root, f.path)
		if err := os.WriteFile(path, f.data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "sand-icons:", err)
			os.Exit(1)
		}
		fmt.Printf("%-64s %6.1f KB\n", f.path, float64(len(f.data))/1024)
	}
}

// output is one derived file: where it goes and what it holds.
type output struct {
	path string
	data []byte
	// png outputs are compared by their pixels, not their bytes, so a change
	// in the encoder's compression alone never reads as a stale icon.
	png bool
}

// plan works out every output for apps without touching the disk beyond
// reading each app's mark.
func plan(root string, apps []app) ([]output, error) {
	var out []output
	for _, a := range apps {
		src, err := os.ReadFile(filepath.Join(root, a.source))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", a.name, err)
		}
		mark, err := brand.Parse(src)
		if err != nil {
			return nil, fmt.Errorf("%s (%s): %w", a.name, a.source, err)
		}
		note := fmt.Sprintf("The %s mark. Generated from %s by `make icons`; edit that, not this.", a.name, a.source)

		for _, p := range a.svgCopies {
			out = append(out, output{path: p, data: src})
		}
		if len(a.homeScreenDirs) > 0 {
			if mark.Width != mark.Height {
				return nil, fmt.Errorf("%s: home-screen icons need a square mark, %s is %gx%g",
					a.name, a.source, mark.Width, mark.Height)
			}
			for _, r := range homeScreen {
				data, err := encodePNG(brand.Render(mark, r.opt))
				if err != nil {
					return nil, err
				}
				for _, dir := range a.homeScreenDirs {
					out = append(out, output{path: filepath.ToSlash(filepath.Join(dir, r.file)), data: data, png: true})
				}
			}
		}
		for _, p := range a.androidIcons {
			out = append(out, output{path: p, data: brand.AndroidVector(mark, note)})
		}
		for _, b := range a.banners {
			bg, err := brand.ParseHex(b.background)
			if err != nil {
				return nil, fmt.Errorf("%s banner: %w", a.name, err)
			}
			placed := mark.Placed(b.width, b.height, b.scale, bg)
			bnote := fmt.Sprintf("The %s launcher banner, %gx%g. Generated from %s by `make icons`; edit that, not this.",
				a.name, b.width, b.height, a.source)
			out = append(out, output{path: b.path, data: brand.AndroidVector(placed, bnote)})
		}
	}
	return out, nil
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// staleFiles lists the outputs whose committed file is missing or differs.
func staleFiles(root string, files []output) ([]string, error) {
	var stale []string
	for _, f := range files {
		have, err := os.ReadFile(filepath.Join(root, f.path))
		if errors.Is(err, fs.ErrNotExist) {
			stale = append(stale, f.path)
			continue
		}
		if err != nil {
			return nil, err
		}
		same := bytes.Equal(have, f.data)
		if !same && f.png {
			same, err = samePixels(have, f.data)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f.path, err)
			}
		}
		if !same {
			stale = append(stale, f.path)
		}
	}
	return stale, nil
}

// samePixels reports whether two PNGs decode to the same image. A committed
// file that will not decode is simply stale.
func samePixels(have, want []byte) (bool, error) {
	w, err := png.Decode(bytes.NewReader(want))
	if err != nil {
		return false, err
	}
	h, err := png.Decode(bytes.NewReader(have))
	if err != nil {
		return false, nil
	}
	// The encoder drops the alpha channel from a fully opaque image, so the
	// two can decode to different types; compare them in one colour model.
	hn, wn := toNRGBA(h), toNRGBA(w)
	return hn.Rect == wn.Rect && slices.Equal(hn.Pix, wn.Pix), nil
}

func toNRGBA(img image.Image) *image.NRGBA {
	b := img.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Rect, img, b.Min, draw.Src)
	return out
}
