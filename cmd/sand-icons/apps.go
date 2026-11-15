package main

import "github.com/chinmay28/SandHarbor/internal/brand"

// app is one SAND Harbor app: the mark it is drawn from in brand/, and every
// file in the repository that is derived from that mark. Paths are relative
// to the repository root.
//
// Adding an app to the family is a new SVG in brand/ (see brand/README.md)
// and one more entry in apps.
type app struct {
	name   string
	source string // the mark, e.g. "brand/vault.svg"

	// svgCopies are byte-for-byte copies of the source, for pages that load
	// the mark as an SVG.
	svgCopies []string
	// homeScreenDirs each get the full set of homeScreen PNGs.
	homeScreenDirs []string
	// androidIcons are vector drawables of the mark, for launcher icons.
	androidIcons []string
	// banners are wide launcher tiles with the mark centred on them.
	banners []banner
}

// banner is a launcher tile — Fire TV wants 320×180 — with the app's mark
// placed in the middle of it.
type banner struct {
	path          string
	width, height float64
	scale         float64 // of the mark, in banner units per mark unit
	background    string  // a theme colour
}

// raster is one home-screen PNG and the reason it is shaped as it is; see
// brand.Plate and brand.RenderOptions.Inset.
type raster struct {
	file string
	opt  brand.RenderOptions
}

// homeScreen is what a web app needs for a phone to put it on a home screen:
// a touch icon for iOS, and the manifest's plain and maskable icons for
// Android. The maskable icon pulls the artwork in until the ring's corners
// clear the launcher's circular safe zone with room to spare.
var homeScreen = []raster{
	{"apple-touch-icon.png", brand.RenderOptions{Width: 180, Plate: brand.Square}},
	{"icon-192.png", brand.RenderOptions{Width: 192, Plate: brand.Rounded}},
	{"icon-512.png", brand.RenderOptions{Width: 512, Plate: brand.Rounded}},
	{"icon-maskable-512.png", brand.RenderOptions{Width: 512, Plate: brand.Square, Inset: 0.78}},
}

var apps = []app{
	{
		name:   "SAND Vault",
		source: "brand/vault.svg",
		svgCopies: []string{
			"web/public/icon.svg",
			// The embedded build of web/; vite copies public/ into it, and
			// keeping it current here means a Go-only build shows the new mark.
			"internal/server/dist/icon.svg",
		},
		homeScreenDirs: []string{"web/public", "internal/server/dist"},
		androidIcons:   []string{"apps/android/app/src/main/res/drawable/ic_launcher.xml"},
	},
	{
		name:   "SAND TV",
		source: "brand/tv.svg",
		svgCopies: []string{
			"internal/server/tvui/icon.svg",
			"apps/firetv/app/src/main/assets/icon.svg",
		},
		androidIcons: []string{"apps/firetv/app/src/main/res/drawable/ic_launcher.xml"},
		banners: []banner{{
			path:       "apps/firetv/app/src/main/res/drawable/banner.xml",
			width:      320,
			height:     180,
			scale:      2.4,
			background: "#111827", // theme surface: the plate stands off it
		}},
	},
}
