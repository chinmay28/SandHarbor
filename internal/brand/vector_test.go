package brand

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestAndroidVector(t *testing.T) {
	m := Mark{Width: 64, Height: 32, Shapes: []Shape{
		{Kind: Rect, W: 64, H: 32, Fill: &navy, Opacity: 1},
		{Kind: Rect, X: 16, Y: 18, W: 32, H: 8, R: 2, Fill: &amber, Opacity: 0.42},
		{Kind: Rect, X: 8.5, Y: 8.5, W: 47, H: 15, R: 9, Stroke: &navy, StrokeWidth: 3, Opacity: 1},
		{Kind: Polygon, Points: []Point{{28.5, 22}, {28.5, 35}, {39.5, 28.5}}, Fill: &navy, Opacity: 1},
	}}
	out := string(AndroidVector(m, "made from a -- test"))

	// It has to be well-formed XML, or aapt refuses the whole build.
	if err := xml.Unmarshal([]byte(out), new(struct{})); err != nil {
		t.Fatalf("not XML: %v\n%s", err, out)
	}
	for _, want := range []string{
		`<!-- made from a — test -->`,
		`android:width="64dp"`,
		`android:viewportHeight="32"`,
		`android:fillColor="#FF0A0E17" android:pathData="M0,0h64v32h-64z"`,
		// Opacity folds into alpha: round(255 × 0.42) = 107 = 0x6B.
		`android:fillColor="#6BD97706" android:pathData="M18,18h28a2,2 0,0 1,2,2v4a2,2 0,0 1,-2,2h-28a2,2 0,0 1,-2,-2v-4a2,2 0,0 1,2,-2z"`,
		`android:strokeColor="#FF0A0E17" android:strokeWidth="3"`,
		`android:pathData="M28.5,22L28.5,35L39.5,28.5z"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	if strings.Contains(out, "fillColor=\"#FF0A0E17\" android:pathData=\"M17.5") {
		t.Error("a stroke-only shape was also filled")
	}
}

func TestPathDataClampsTheRadius(t *testing.T) {
	// A radius larger than half the short side draws a pill, as SVG does.
	got := pathData(Shape{Kind: Rect, W: 10, H: 4, R: 9})
	want := "M2,0h6a2,2 0,0 1,2,2v0a2,2 0,0 1,-2,2h-6a2,2 0,0 1,-2,-2v0a2,2 0,0 1,2,-2z"
	if got != want {
		t.Errorf("pathData = %s\nwant       %s", got, want)
	}
}
