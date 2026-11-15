package brand

import (
	"image/color"
	"strings"
	"testing"
)

func TestParseReadsShapesInPaintOrder(t *testing.T) {
	m, err := Parse([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 32">
  <!-- comments are ignored -->
  <rect width="64" height="32" rx="4" fill="#0a0e17"/>
  <g fill="#d97706">
    <rect x="1" y="2" width="3" height="4" rx="0.5" opacity="0.5"/>
    <g>
      <rect x="5" y="6" width="7" height="8"/>
    </g>
  </g>
  <rect x="8.5" y="8.5" width="10" height="10" fill="none" stroke="#1e2d3d" stroke-width="3"/>
  <polygon points="1,1 9,1 5,7" fill="#fff"/>
</svg>`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Width != 64 || m.Height != 32 {
		t.Fatalf("size = %gx%g, want 64x32", m.Width, m.Height)
	}
	if len(m.Shapes) != 5 {
		t.Fatalf("got %d shapes, want 5", len(m.Shapes))
	}

	amber := color.NRGBA{0xd9, 0x77, 0x06, 0xff}
	s := m.Shapes[1]
	if s.Kind != Rect || s.X != 1 || s.Y != 2 || s.W != 3 || s.H != 4 || s.R != 0.5 || s.Opacity != 0.5 {
		t.Errorf("rect geometry = %+v", s)
	}
	if s.Fill == nil || *s.Fill != amber {
		t.Errorf("group fill not inherited: %v", s.Fill)
	}
	if f := m.Shapes[2].Fill; f == nil || *f != amber {
		t.Errorf("fill not inherited through a nested group: %v", f)
	}
	if s := m.Shapes[3]; s.Fill != nil || s.Stroke == nil || s.StrokeWidth != 3 {
		t.Errorf("stroked rect = %+v", s)
	}
	poly := m.Shapes[4]
	if poly.Kind != Polygon || len(poly.Points) != 3 || poly.Points[2] != (Point{5, 7}) {
		t.Errorf("polygon = %+v", poly)
	}
	if *poly.Fill != (color.NRGBA{255, 255, 255, 255}) {
		t.Errorf("#fff = %v", *poly.Fill)
	}
}

func TestParseRefusesWhatItCannotDraw(t *testing.T) {
	wrap := func(body string) string {
		return `<svg viewBox="0 0 64 64">` + body + `</svg>`
	}
	cases := map[string]string{
		"unknown element": wrap(`<circle cx="1" cy="1" r="1" fill="#000"/>`),
		"path":            wrap(`<path d="M0,0h1v1z" fill="#000"/>`),
		"named colour":    wrap(`<rect width="1" height="1" fill="red"/>`),
		"no paint":        wrap(`<rect width="1" height="1"/>`),
		"zero size":       wrap(`<rect width="0" height="1" fill="#000"/>`),
		"bad number":      wrap(`<rect width="1" height="1" rx="two" fill="#000"/>`),
		"opacity > 1":     wrap(`<rect width="1" height="1" opacity="2" fill="#000"/>`),
		"short polygon":   wrap(`<polygon points="0,0 1,1" fill="#000"/>`),
		"empty":           wrap(``),
		"no viewBox":      `<svg><rect width="1" height="1" fill="#000"/></svg>`,
		"offset viewBox":  `<svg viewBox="1 0 64 64"><rect width="1" height="1" fill="#000"/></svg>`,
		"no svg":          `<rect width="1" height="1" fill="#000"/>`,
		"malformed":       `<svg viewBox="0 0 64 64"><rect`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(src)); err == nil {
				t.Errorf("Parse accepted %s", src)
			} else if !strings.HasPrefix(err.Error(), "brand:") {
				t.Errorf("error %q does not say where it came from", err)
			}
		})
	}
}

func TestIsPlate(t *testing.T) {
	m := Mark{Width: 64, Height: 64}
	if !m.IsPlate(Shape{Kind: Rect, W: 64, H: 64}) {
		t.Error("full-canvas rect is not the plate")
	}
	if m.IsPlate(Shape{Kind: Rect, X: 1, W: 64, H: 64}) {
		t.Error("offset rect taken for the plate")
	}
	if m.IsPlate(Shape{Kind: Polygon, Points: []Point{{0, 0}, {64, 0}, {64, 64}}}) {
		t.Error("polygon taken for the plate")
	}
}

func TestPlacedCentresTheMark(t *testing.T) {
	fill := color.NRGBA{1, 2, 3, 255}
	m := Mark{Width: 10, Height: 10, Shapes: []Shape{
		{Kind: Rect, W: 10, H: 10, R: 2, Fill: &fill, Opacity: 1},
		{Kind: Polygon, Points: []Point{{0, 0}, {10, 0}, {5, 10}}, Fill: &fill, Opacity: 1},
	}}
	bg := color.NRGBA{9, 9, 9, 255}
	p := m.Placed(100, 50, 2, bg)

	if p.Width != 100 || p.Height != 50 || len(p.Shapes) != 3 {
		t.Fatalf("placed = %gx%g with %d shapes", p.Width, p.Height, len(p.Shapes))
	}
	if !p.IsPlate(p.Shapes[0]) || *p.Shapes[0].Fill != bg {
		t.Error("first shape is not the new background")
	}
	if p.IsPlate(p.Shapes[1]) {
		t.Error("the mark's own plate still covers the whole banner")
	}
	if s := p.Shapes[1]; s.X != 40 || s.Y != 15 || s.W != 20 || s.H != 20 || s.R != 4 {
		t.Errorf("placed rect = %+v, want 20x20 at (40,15) r4", s)
	}
	if got := p.Shapes[2].Points[2]; got != (Point{50, 35}) {
		t.Errorf("placed polygon tip = %v, want (50,35)", got)
	}
	if m.Shapes[1].Points[2] != (Point{5, 10}) {
		t.Error("Placed changed the original mark")
	}
}

func TestParseHex(t *testing.T) {
	for in, want := range map[string]color.NRGBA{
		"#d97706": {0xd9, 0x77, 0x06, 0xff},
		"#0A0E17": {0x0a, 0x0e, 0x17, 0xff},
		"#abc":    {0xaa, 0xbb, 0xcc, 0xff},
	} {
		if got, err := ParseHex(in); err != nil || got != want {
			t.Errorf("ParseHex(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "d97706", "#d9770", "#ggg", "#d9770600"} {
		if _, err := ParseHex(bad); err == nil {
			t.Errorf("ParseHex(%q) accepted", bad)
		}
	}
}
