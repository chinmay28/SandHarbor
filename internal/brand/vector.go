package brand

import (
	"fmt"
	"image/color"
	"math"
	"strconv"
	"strings"
)

// AndroidVector writes m as an Android vector drawable, one <path> per paint
// (a shape with both a fill and a stroke becomes two). Opacity is folded into
// each colour's alpha. The note, if any, goes in a comment at the top — the
// place to say which file the drawable was generated from.
func AndroidVector(m Mark, note string) []byte {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n")
	if note != "" {
		fmt.Fprintf(&b, "<!-- %s -->\n", strings.ReplaceAll(note, "--", "—"))
	}
	fmt.Fprintf(&b, `<vector xmlns:android="http://schemas.android.com/apk/res/android"
    android:width="%[1]sdp"
    android:height="%[2]sdp"
    android:viewportWidth="%[1]s"
    android:viewportHeight="%[2]s">
`, num(m.Width), num(m.Height))

	for _, s := range m.Shapes {
		data := pathData(s)
		if s.Fill != nil {
			fmt.Fprintf(&b, "    <path android:fillColor=\"%s\" android:pathData=\"%s\" />\n",
				argb(*s.Fill, s.Opacity), data)
		}
		if s.Stroke != nil && s.StrokeWidth > 0 {
			fmt.Fprintf(&b, "    <path android:strokeColor=\"%s\" android:strokeWidth=\"%s\" android:pathData=\"%s\" />\n",
				argb(*s.Stroke, s.Opacity), num(s.StrokeWidth), data)
		}
	}
	b.WriteString("</vector>\n")
	return []byte(b.String())
}

// pathData is the outline of s as SVG path data, which is what a vector
// drawable's pathData takes.
func pathData(s Shape) string {
	if s.Kind == Polygon {
		parts := make([]string, len(s.Points))
		for i, p := range s.Points {
			cmd := "L"
			if i == 0 {
				cmd = "M"
			}
			parts[i] = cmd + num(p.X) + "," + num(p.Y)
		}
		return strings.Join(parts, "") + "z"
	}

	r := math.Min(s.R, math.Min(s.W, s.H)/2)
	if r == 0 {
		return fmt.Sprintf("M%s,%sh%sv%sh%sz", num(s.X), num(s.Y), num(s.W), num(s.H), num(-s.W))
	}
	arc := func(dx, dy float64) string {
		return fmt.Sprintf("a%[1]s,%[1]s 0,0 1,%s,%s", num(r), num(dx), num(dy))
	}
	return "M" + num(s.X+r) + "," + num(s.Y) +
		"h" + num(s.W-2*r) + arc(r, r) +
		"v" + num(s.H-2*r) + arc(-r, r) +
		"h" + num(-(s.W - 2*r)) + arc(-r, -r) +
		"v" + num(-(s.H - 2*r)) + arc(r, -r) + "z"
}

func argb(c color.NRGBA, opacity float64) string {
	a := uint8(math.Round(float64(c.A) * opacity))
	return fmt.Sprintf("#%02X%02X%02X%02X", a, c.R, c.G, c.B)
}

// num formats a coordinate as briefly as it can be written exactly.
func num(v float64) string {
	if v == 0 {
		return "0" // and never "-0"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}
