package main

import (
	"fmt"
	"image"
	"path"

	"github.com/chinmay28/SandHarbor/internal/brand"
)

// The artwork every SAND Harbor app wears, and what is known about it. The
// plate colour and corner radius are measured off brand/icon.png; if the
// artwork is ever redrawn, measure them again (brand/README.md says how).
const (
	artPath     = "brand/icon.png"
	plateColour = "#030b26"
	plateRadius = 0.223 // of the width
)

// icon is one derived PNG.
type icon struct {
	path string
	draw func(brand.Art) (*image.NRGBA, error)
}

// text is one derived text file.
type text struct {
	path string
	data func(brand.Art) []byte
}

// app is one SAND Harbor app and the files it carries the icon in. Every app
// uses the same artwork; what differs is the shapes and sizes its platform
// wants. Adding an app is one more entry here, usually made of the helpers
// below.
type app struct {
	name  string
	icons []icon
	texts []text
}

var apps = []app{
	{
		name: "SAND Vault, web",
		icons: append(
			webIcons("web/public"),
			// The embedded build of web/; vite copies public/ into it, and
			// keeping it current here means a Go-only build shows the icon.
			webIcons("internal/server/dist")...),
	},
	{
		name: "SAND Vault for Android",
		icons: append(launcherIcons("apps/android/app/src/main/res"),
			// The mark on the app's own screens (main and setup), 72dp at most:
			// one xxxhdpi-sized image, scaled down by the view.
			icon{"apps/android/app/src/main/res/drawable-nodpi/sand_logo.png", rounded(288)},
		),
		texts: adaptiveIcon("apps/android/app/src/main/res"),
	},
	{
		name:  "SAND TV, the page a TV opens",
		icons: []icon{{"internal/server/tvui/icon.png", rounded(256)}},
	},
	{
		name: "SAND TV for Fire TV",
		icons: append(launcherIcons("apps/firetv/app/src/main/res"),
			icon{"apps/firetv/app/src/main/assets/icon.png", rounded(256)},
			// The home-screen tile: 320×180dp, drawn at xhdpi.
			icon{"apps/firetv/app/src/main/res/drawable-xhdpi/banner.png", placed(640, 360, 0.92)},
		),
		texts: adaptiveIcon("apps/firetv/app/src/main/res"),
	},
}

func rounded(size int) func(brand.Art) (*image.NRGBA, error) {
	return func(a brand.Art) (*image.NRGBA, error) { return a.Rounded(size), nil }
}

func square(size int) func(brand.Art) (*image.NRGBA, error) {
	return func(a brand.Art) (*image.NRGBA, error) { return a.Square(size), nil }
}

func placed(w, h int, scale float64) func(brand.Art) (*image.NRGBA, error) {
	return func(a brand.Art) (*image.NRGBA, error) { return a.Placed(w, h, scale) }
}

// webIcons is what a web app needs for a phone to put it on a home screen,
// plus the icon the page itself shows.
//
//   - apple-touch-icon is full bleed: iOS masks its own squircle over an icon
//     and composites transparency onto black.
//   - icon-192 and icon-512 keep the artwork's rounded plate, for browsers
//     and launchers that show an icon unmasked. icon-192 is also the page's
//     favicon and header mark.
//   - The maskable icon pulls the scene in until it sits inside the circle a
//     launcher may crop it to (80% of the width).
func webIcons(dir string) []icon {
	return []icon{
		{path.Join(dir, "apple-touch-icon.png"), square(180)},
		{path.Join(dir, "icon-192.png"), rounded(192)},
		{path.Join(dir, "icon-512.png"), rounded(512)},
		{path.Join(dir, "icon-maskable-512.png"), placed(512, 512, 0.78)},
	}
}

// densities are Android's launcher-icon buckets, as multiples of mdpi.
var densities = []struct {
	name  string
	scale int // in quarters: mdpi is 4
}{{"mdpi", 4}, {"hdpi", 6}, {"xhdpi", 8}, {"xxhdpi", 12}, {"xxxhdpi", 16}}

// launcherIcons is an Android app's launcher icon in every density: the
// legacy 48dp icon with the rounded plate, for launchers before Android 8,
// and the 108dp foreground layer of the adaptive icon, with the scene in the
// 72dp the launcher shows.
func launcherIcons(res string) []icon {
	var out []icon
	for _, d := range densities {
		px := func(dp int) int { return dp * d.scale / 4 }
		dir := path.Join(res, "mipmap-"+d.name)
		out = append(out,
			icon{path.Join(dir, "ic_launcher.png"), rounded(px(48))},
			icon{path.Join(dir, "ic_launcher_foreground.png"), placed(px(108), px(108), 72.0/108)},
		)
	}
	return out
}

// adaptiveIcon is the Android 8+ adaptive icon that puts the foreground
// layer over a background of the plate colour.
func adaptiveIcon(res string) []text {
	return []text{
		{path.Join(res, "mipmap-anydpi-v26/ic_launcher.xml"), func(brand.Art) []byte {
			return []byte(`<?xml version="1.0" encoding="utf-8"?>
<!-- Generated from ` + artPath + ` by ` + "`make icons`" + `; edit that, not this. -->
<adaptive-icon xmlns:android="http://schemas.android.com/apk/res/android">
    <background android:drawable="@color/ic_launcher_background" />
    <foreground android:drawable="@mipmap/ic_launcher_foreground" />
</adaptive-icon>
`)
		}},
		{path.Join(res, "values/ic_launcher_background.xml"), func(a brand.Art) []byte {
			p := a.Plate()
			return fmt.Appendf(nil, `<?xml version="1.0" encoding="utf-8"?>
<!-- The icon artwork's plate colour. Generated from %s by `+"`make icons`"+`. -->
<resources>
    <color name="ic_launcher_background">#%02X%02X%02X</color>
</resources>
`, artPath, p.R, p.G, p.B)
		}},
	}
}
