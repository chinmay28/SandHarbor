package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCommittedIconsMatchBrand fails when someone edits a mark in brand/ and
// forgets `make icons`, or hand-edits a derived icon. It is the same check as
// `make icons-check`, run wherever the Go tests run.
func TestCommittedIconsMatchBrand(t *testing.T) {
	root := filepath.Join("..", "..")
	files, err := plan(root, apps)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := staleFiles(root, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range stale {
		t.Errorf("%s does not match its mark in brand/ — run: make icons", f)
	}
}

func TestEveryAppIsDistinctAndEveryOutputHasOneOwner(t *testing.T) {
	sources := map[string]string{}
	owners := map[string]string{}
	files, err := plan(filepath.Join("..", ".."), apps)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range apps {
		if prev, ok := sources[a.source]; ok {
			t.Errorf("%s and %s share %s: each app gets a mark of its own", prev, a.name, a.source)
		}
		sources[a.source] = a.name
	}
	for _, f := range files {
		if prev, ok := owners[f.path]; ok {
			t.Errorf("%s is written twice (%s)", f.path, prev)
		}
		owners[f.path] = f.path
	}
}

const testMark = `<svg viewBox="0 0 64 64">
  <rect width="64" height="64" rx="14" fill="#0a0e17"/>
  <rect x="16" y="18" width="32" height="8" rx="2" fill="#d97706"/>
</svg>`

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPlanAndStaleness(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "brand/x.svg", testMark)
	for _, d := range []string{"web", "res"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	x := []app{{
		name:           "X",
		source:         "brand/x.svg",
		svgCopies:      []string{"web/icon.svg"},
		homeScreenDirs: []string{"web"},
		androidIcons:   []string{"res/ic_launcher.xml"},
		banners:        []banner{{path: "res/banner.xml", width: 320, height: 180, scale: 2, background: "#111827"}},
	}}

	files, err := plan(root, x)
	if err != nil {
		t.Fatal(err)
	}
	if want := 1 + len(homeScreen) + 1 + 1; len(files) != want {
		t.Fatalf("planned %d files, want %d", len(files), want)
	}

	// Nothing written yet: everything is stale.
	stale, err := staleFiles(root, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != len(files) {
		t.Fatalf("stale = %v, want all %d", stale, len(files))
	}

	for _, f := range files {
		writeFile(t, root, f.path, string(f.data))
	}
	if stale, _ := staleFiles(root, files); len(stale) != 0 {
		t.Fatalf("freshly written files reported stale: %v", stale)
	}

	// A hand edit to a text output, and a PNG that no longer decodes.
	writeFile(t, root, "res/ic_launcher.xml", "<vector/>")
	writeFile(t, root, "web/icon-192.png", "not a png")
	stale, err = staleFiles(root, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 2 || stale[0] != "web/icon-192.png" || stale[1] != "res/ic_launcher.xml" {
		t.Errorf("stale = %v, want the two edited files", stale)
	}
}

func TestPlanRefusesABadMark(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "brand/x.svg", `<svg viewBox="0 0 64 32"><rect width="1" height="1" fill="#000"/></svg>`)
	_, err := plan(root, []app{{name: "X", source: "brand/x.svg", homeScreenDirs: []string{"web"}}})
	if err == nil {
		t.Error("a non-square mark was accepted for home-screen icons")
	}
	if _, err := plan(root, []app{{name: "Y", source: "brand/missing.svg"}}); err == nil {
		t.Error("a missing mark was accepted")
	}
}
