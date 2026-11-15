// Command sand-icons derives every SAND Harbor app icon from the one piece of
// artwork in brand/icon.png.
//
// Each platform wants the icon in its own sizes and shapes — rounded or full
// bleed, square or a wide banner, PNGs for home screens and launcher mipmaps
// for Android — and this writes all of them. The outputs are committed;
// replace the artwork, run `make icons`, commit what falls out, and never
// edit an output by hand.
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
	"image/color"
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

	if err := run(*root, *check); err != nil {
		fmt.Fprintln(os.Stderr, "sand-icons:", err)
		os.Exit(1)
	}
}

func run(root string, check bool) error {
	files, err := plan(root, apps)
	if err != nil {
		return err
	}

	if check {
		stale, err := staleFiles(root, files)
		if err != nil {
			return err
		}
		for _, f := range stale {
			fmt.Fprintf(os.Stderr, "stale: %s\n", f)
		}
		if len(stale) > 0 {
			return fmt.Errorf("%d icon file(s) out of date — run: make icons", len(stale))
		}
		fmt.Println("icons match", artPath)
		return nil
	}

	for _, f := range files {
		p := filepath.Join(root, f.path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, f.data, 0o644); err != nil {
			return err
		}
		fmt.Printf("%-72s %6.1f KB\n", f.path, float64(len(f.data))/1024)
	}
	return nil
}

// output is one derived file: where it goes and what it holds.
type output struct {
	path string
	data []byte
	// png outputs are compared by their pixels, not their bytes, so a change
	// in the encoder's compression alone never reads as a stale icon.
	png bool
}

// loadArt reads the artwork and prepares it for drawing.
func loadArt(root string) (brand.Art, error) {
	f, err := os.Open(filepath.Join(root, artPath))
	if err != nil {
		return brand.Art{}, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return brand.Art{}, fmt.Errorf("%s: %w", artPath, err)
	}
	plate, err := parseHex(plateColour)
	if err != nil {
		return brand.Art{}, err
	}
	return brand.NewArt(img, plate, plateRadius)
}

// plan renders every output for apps without writing anything.
func plan(root string, apps []app) ([]output, error) {
	art, err := loadArt(root)
	if err != nil {
		return nil, err
	}
	var out []output
	for _, a := range apps {
		for _, ic := range a.icons {
			img, err := ic.draw(art)
			if err != nil {
				return nil, fmt.Errorf("%s: %s: %w", a.name, ic.path, err)
			}
			data, err := encodePNG(img)
			if err != nil {
				return nil, err
			}
			out = append(out, output{path: ic.path, data: data, png: true})
		}
		for _, t := range a.texts {
			out = append(out, output{path: t.path, data: t.data(art)})
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
			if same, err = samePixels(have, f.data); err != nil {
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

// parseHex reads an opaque "#rrggbb" colour.
func parseHex(v string) (color.NRGBA, error) {
	var c color.NRGBA
	if _, err := fmt.Sscanf(v, "#%02x%02x%02x", &c.R, &c.G, &c.B); err != nil || len(v) != 7 {
		return color.NRGBA{}, fmt.Errorf("colour %q is not #rrggbb", v)
	}
	c.A = 255
	return c, nil
}
