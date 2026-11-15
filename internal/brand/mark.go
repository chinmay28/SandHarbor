// Package brand reads a SAND Harbor app mark and draws it in the forms each
// platform wants: PNGs for home screens, and vector drawables for Android.
//
// A mark is drawn exactly once, as an SVG in brand/, and everything else is
// derived from it. The SVG is restricted to the handful of shapes the family
// actually uses — rounded rectangles (bricks, studs, the plate, the ring) and
// straight-edged polygons (glyphs such as play) — because every one of those
// has an exact distance function. That makes rasterising arithmetic rather
// than a dependency, and it makes the Android conversion lossless. Anything
// outside that subset is an error, never a silent approximation: a mark that
// no longer renders as drawn is worse than one that does not render at all.
package brand

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"image/color"
	"io"
	"strconv"
	"strings"
)

// Kind says which geometry a Shape carries.
type Kind int

const (
	// Rect is a rectangle with an optional corner radius.
	Rect Kind = iota
	// Polygon is a closed polygon with straight edges.
	Polygon
)

// Point is a position in the mark's own units (its viewBox).
type Point struct{ X, Y float64 }

// Shape is one painted element of a mark. Shapes are painted in order.
type Shape struct {
	Kind Kind

	// Rect geometry.
	X, Y, W, H, R float64

	// Polygon geometry, in drawing order. At least three points.
	Points []Point

	// Fill is the fill colour, or nil for none.
	Fill *color.NRGBA
	// Stroke is the stroke colour, or nil for none. A stroke straddles the
	// outline: half of StrokeWidth falls either side of it.
	Stroke      *color.NRGBA
	StrokeWidth float64

	// Opacity scales both fill and stroke, 0..1.
	Opacity float64
}

// Mark is a parsed mark: a canvas size and the shapes painted onto it.
type Mark struct {
	Width, Height float64
	Shapes        []Shape
}

// IsPlate reports whether s is the mark's background plate: a rectangle that
// covers the whole canvas. Renderers reshape the plate (square corners for
// iOS, full bleed for maskable icons) and leave everything else as drawn.
func (m Mark) IsPlate(s Shape) bool {
	return s.Kind == Rect && s.X <= 0 && s.Y <= 0 &&
		s.X+s.W >= m.Width && s.Y+s.H >= m.Height
}

// Placed returns m drawn at scale, centred on a new width×height canvas whose
// first shape is a square-cornered background in bg. It is how a launcher
// banner reuses the app's mark rather than redrawing it.
func (m Mark) Placed(width, height, scale float64, bg color.NRGBA) Mark {
	dx := (width - m.Width*scale) / 2
	dy := (height - m.Height*scale) / 2
	out := Mark{Width: width, Height: height}
	out.Shapes = append(out.Shapes, Shape{Kind: Rect, W: width, H: height, Fill: &bg, Opacity: 1})
	for _, s := range m.Shapes {
		out.Shapes = append(out.Shapes, s.transformed(dx, dy, scale))
	}
	return out
}

// transformed returns s scaled by k about the origin, then moved by (dx, dy).
func (s Shape) transformed(dx, dy, k float64) Shape {
	t := s
	t.X, t.Y = s.X*k+dx, s.Y*k+dy
	t.W, t.H, t.R = s.W*k, s.H*k, s.R*k
	t.StrokeWidth = s.StrokeWidth * k
	if s.Points != nil {
		t.Points = make([]Point, len(s.Points))
		for i, p := range s.Points {
			t.Points[i] = Point{p.X*k + dx, p.Y*k + dy}
		}
	}
	return t
}

// Parse reads a mark from SVG source. It accepts <svg viewBox="0 0 W H">,
// <g fill="…"> (fill is the only property a group passes down), <rect> and
// <polygon>, with fill, stroke, stroke-width and opacity. Anything else is an
// error naming the element.
func Parse(src []byte) (Mark, error) {
	dec := xml.NewDecoder(bytes.NewReader(src))
	var (
		m      Mark
		seen   bool
		groups []*color.NRGBA // inherited fills, innermost last
	)

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Mark{}, fmt.Errorf("brand: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			a := attrs(t.Attr)
			switch t.Name.Local {
			case "svg":
				if seen {
					return Mark{}, errors.New("brand: nested <svg>")
				}
				seen = true
				w, h, err := parseViewBox(a["viewBox"])
				if err != nil {
					return Mark{}, err
				}
				m.Width, m.Height = w, h
			case "g":
				fill, err := inheritedFill(a, groups)
				if err != nil {
					return Mark{}, err
				}
				groups = append(groups, fill)
			case "rect", "polygon":
				if !seen {
					return Mark{}, fmt.Errorf("brand: <%s> outside <svg>", t.Name.Local)
				}
				s, err := parseShape(t.Name.Local, a, groups)
				if err != nil {
					return Mark{}, err
				}
				m.Shapes = append(m.Shapes, s)
			default:
				return Mark{}, fmt.Errorf("brand: <%s> is not a shape a mark may use", t.Name.Local)
			}
		case xml.EndElement:
			if t.Name.Local == "g" && len(groups) > 0 {
				groups = groups[:len(groups)-1]
			}
		}
	}

	if !seen {
		return Mark{}, errors.New("brand: no <svg> element")
	}
	if len(m.Shapes) == 0 {
		return Mark{}, errors.New("brand: nothing to draw")
	}
	return m, nil
}

func attrs(list []xml.Attr) map[string]string {
	a := make(map[string]string, len(list))
	for _, at := range list {
		a[at.Name.Local] = at.Value
	}
	return a
}

func parseViewBox(v string) (w, h float64, err error) {
	f := strings.Fields(strings.ReplaceAll(v, ",", " "))
	if len(f) != 4 || f[0] != "0" || f[1] != "0" {
		return 0, 0, fmt.Errorf("brand: viewBox %q must be \"0 0 W H\"", v)
	}
	if w, err = positive(f[2]); err != nil {
		return 0, 0, fmt.Errorf("brand: viewBox width: %w", err)
	}
	if h, err = positive(f[3]); err != nil {
		return 0, 0, fmt.Errorf("brand: viewBox height: %w", err)
	}
	return w, h, nil
}

func positive(s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("%q is not a positive number", s)
	}
	return v, nil
}

func inheritedFill(a map[string]string, groups []*color.NRGBA) (*color.NRGBA, error) {
	if v, ok := a["fill"]; ok {
		return parsePaint(v)
	}
	if len(groups) > 0 {
		return groups[len(groups)-1], nil
	}
	return nil, nil
}

func parseShape(kind string, a map[string]string, groups []*color.NRGBA) (Shape, error) {
	s := Shape{Opacity: 1}
	var err error

	if s.Fill, err = inheritedFill(a, groups); err != nil {
		return Shape{}, err
	}
	if v, ok := a["stroke"]; ok {
		if s.Stroke, err = parsePaint(v); err != nil {
			return Shape{}, err
		}
	}
	nums := map[string]*float64{"stroke-width": &s.StrokeWidth, "opacity": &s.Opacity}

	switch kind {
	case "rect":
		s.Kind = Rect
		for k, p := range map[string]*float64{"x": &s.X, "y": &s.Y, "width": &s.W, "height": &s.H, "rx": &s.R} {
			nums[k] = p
		}
	case "polygon":
		s.Kind = Polygon
		if s.Points, err = parsePoints(a["points"]); err != nil {
			return Shape{}, err
		}
	}

	for k, p := range nums {
		v, ok := a[k]
		if !ok {
			continue
		}
		if *p, err = strconv.ParseFloat(v, 64); err != nil {
			return Shape{}, fmt.Errorf("brand: <%s %s=%q>: not a number", kind, k, v)
		}
	}

	switch {
	case s.Kind == Rect && (s.W <= 0 || s.H <= 0):
		return Shape{}, fmt.Errorf("brand: <rect> needs a positive width and height")
	case s.R < 0 || s.StrokeWidth < 0:
		return Shape{}, fmt.Errorf("brand: <%s> has a negative size", kind)
	case s.Opacity < 0 || s.Opacity > 1:
		return Shape{}, fmt.Errorf("brand: <%s> opacity %v is outside 0..1", kind, s.Opacity)
	case s.Fill == nil && (s.Stroke == nil || s.StrokeWidth == 0):
		return Shape{}, fmt.Errorf("brand: <%s> paints nothing", kind)
	}
	return s, nil
}

func parsePoints(v string) ([]Point, error) {
	f := strings.Fields(strings.ReplaceAll(v, ",", " "))
	if len(f) < 6 || len(f)%2 != 0 {
		return nil, fmt.Errorf("brand: <polygon points=%q> needs at least three x,y pairs", v)
	}
	pts := make([]Point, 0, len(f)/2)
	for i := 0; i < len(f); i += 2 {
		x, errX := strconv.ParseFloat(f[i], 64)
		y, errY := strconv.ParseFloat(f[i+1], 64)
		if errX != nil || errY != nil {
			return nil, fmt.Errorf("brand: <polygon points=%q>: not a number", v)
		}
		pts = append(pts, Point{x, y})
	}
	return pts, nil
}

// parsePaint reads "none", "#rgb" or "#rrggbb". Named colours are refused on
// purpose: every colour in a mark should be one of the theme's, written the
// way theme.js writes it.
func parsePaint(v string) (*color.NRGBA, error) {
	if v == "none" {
		return nil, nil
	}
	c, err := ParseHex(v)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ParseHex reads an opaque "#rgb" or "#rrggbb" colour.
func ParseHex(v string) (color.NRGBA, error) {
	h, ok := strings.CutPrefix(v, "#")
	if ok && len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if !ok || len(h) != 6 {
		return color.NRGBA{}, fmt.Errorf("brand: colour %q is not #rgb or #rrggbb", v)
	}
	n, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return color.NRGBA{}, fmt.Errorf("brand: colour %q is not hex", v)
	}
	return color.NRGBA{R: uint8(n >> 16), G: uint8(n >> 8), B: uint8(n), A: 255}, nil
}
