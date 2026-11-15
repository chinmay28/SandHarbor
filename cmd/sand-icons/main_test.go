package main

import (
	"bytes"
	"image"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

const repoRoot = "../.."

// planned renders the repository's icons once for every test that needs them.
var planned = sync.OnceValues(func() ([]output, error) { return plan(repoRoot, apps) })

// TestCommittedIconsMatchTheArtwork fails when the artwork changes without a
// `make icons`, or when someone hand-edits a derived icon. It is the same
// check as `make icons-check`, run wherever the Go tests run.
func TestCommittedIconsMatchTheArtwork(t *testing.T) {
	files, err := planned()
	if err != nil {
		t.Fatal(err)
	}
	stale, err := staleFiles(repoRoot, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range stale {
		t.Errorf("%s does not match %s — run: make icons", f, artPath)
	}
}

func TestEveryOutputHasOneOwnerAndTheRightSize(t *testing.T) {
	files, err := planned()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range files {
		if seen[f.path] {
			t.Errorf("%s is written twice", f.path)
		}
		seen[f.path] = true
		if !f.png {
			continue
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(f.data))
		if err != nil {
			t.Fatalf("%s: %v", f.path, err)
		}
		// Launcher mipmaps must be exactly their density's size, or Android
		// scales them again.
		for _, d := range densities {
			if !strings.Contains(f.path, "/mipmap-"+d.name+"/") {
				continue
			}
			dp := 48
			if strings.HasSuffix(f.path, "_foreground.png") {
				dp = 108
			}
			if want := dp * d.scale / 4; cfg.Width != want || cfg.Height != want {
				t.Errorf("%s is %dx%d, want %d", f.path, cfg.Width, cfg.Height, want)
			}
		}
	}
}

func TestEveryAndroidAppHasAnAdaptiveIcon(t *testing.T) {
	for _, res := range []string{"apps/android/app/src/main/res", "apps/firetv/app/src/main/res"} {
		manifest, err := os.ReadFile(filepath.Join(repoRoot, filepath.Dir(res), "AndroidManifest.xml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(manifest), `android:icon="@mipmap/ic_launcher"`) {
			t.Errorf("%s/../AndroidManifest.xml does not use @mipmap/ic_launcher", res)
		}
		for _, f := range []string{"mipmap-anydpi-v26/ic_launcher.xml", "values/ic_launcher_background.xml", "mipmap-mdpi/ic_launcher.png"} {
			if _, err := os.Stat(filepath.Join(repoRoot, res, f)); err != nil {
				t.Errorf("%s: %v", res, err)
			}
		}
	}
}

func TestStaleness(t *testing.T) {
	root := t.TempDir()
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	data, err := encodePNG(img)
	if err != nil {
		t.Fatal(err)
	}
	files := []output{
		{path: "a.png", data: data, png: true},
		{path: "b.xml", data: []byte("<b/>")},
		{path: "c.png", data: data, png: true},
	}
	if stale, _ := staleFiles(root, files); len(stale) != 3 {
		t.Fatalf("nothing written, but stale = %v", stale)
	}

	write := func(p string, b []byte) {
		if err := os.WriteFile(filepath.Join(root, p), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// a.png: the same pixels, encoded differently (no compression). Not stale.
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.NoCompression}).Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	write("a.png", buf.Bytes())
	write("b.xml", []byte("<b>edited</b>"))
	write("c.png", []byte("not a png"))

	stale, err := staleFiles(root, files)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(stale, ",") != "b.xml,c.png" {
		t.Errorf("stale = %v, want b.xml and c.png", stale)
	}
}

func TestParseHex(t *testing.T) {
	c, err := parseHex("#030b26")
	if err != nil || c.R != 3 || c.G != 11 || c.B != 38 || c.A != 255 {
		t.Errorf("parseHex = %v, %v", c, err)
	}
	for _, bad := range []string{"030b26", "#030b2", "#030b266", "#zzzzzz"} {
		if _, err := parseHex(bad); err == nil {
			t.Errorf("parseHex(%q) accepted", bad)
		}
	}
}

// TestAndroidIconReferencesResolve fails when an Android app refers to a
// drawable or mipmap that no longer exists — which is what replacing an icon
// leaves behind, and which only a Gradle build would otherwise notice.
// References into the platform (android.R.drawable, @android:drawable) are
// not the app's to resolve and are skipped.
func TestAndroidIconReferencesResolve(t *testing.T) {
	ref := regexp.MustCompile(`(?:@|\bR\.)(drawable|mipmap)[/.]([a-z0-9_]+)`)
	for _, app := range []string{"apps/android/app/src/main", "apps/firetv/app/src/main"} {
		dir := filepath.Join(repoRoot, app)
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if ext := filepath.Ext(p); ext != ".kt" && ext != ".xml" && ext != ".java" {
				return nil
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			for _, m := range ref.FindAllSubmatchIndex(src, -1) {
				// Skip android.R.drawable.x and @android:drawable/x.
				if m[0] >= 8 && string(src[m[0]-8:m[0]]) == "android." {
					continue
				}
				kind, name := string(src[m[2]:m[3]]), string(src[m[4]:m[5]])
				found, _ := filepath.Glob(filepath.Join(dir, "res", kind+"*", name+".*"))
				if len(found) == 0 {
					rel, _ := filepath.Rel(repoRoot, p)
					t.Errorf("%s refers to %s/%s, which no res/%s* directory has", rel, kind, name, kind)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
