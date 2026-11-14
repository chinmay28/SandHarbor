package grind

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenUnderKeepsToTheFolder(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "share")
	outside := filepath.Join(dir, "secret.txt")
	os.MkdirAll(filepath.Join(root, "films"), 0o755)
	os.WriteFile(filepath.Join(root, "films", "a.mkv"), []byte("film"), 0o644)
	os.WriteFile(outside, []byte("secret"), 0o644)
	os.Symlink(outside, filepath.Join(root, "escape"))
	os.Symlink(filepath.Join(root, "films", "a.mkv"), filepath.Join(root, "inside"))
	os.WriteFile(filepath.Join(dir, "sharewide.txt"), nil, 0o644)

	f, info, err := openUnder(root, "films/a.mkv")
	if err != nil {
		t.Fatalf("a file in the folder: %v", err)
	}
	f.Close()
	if info.Size() != 4 {
		t.Fatalf("size = %d", info.Size())
	}

	if f, _, err := openUnder(root, "inside"); err != nil {
		t.Fatalf("a link that stays inside: %v", err)
	} else {
		f.Close()
	}

	for _, rel := range []string{"../secret.txt", "films/../../secret.txt", "escape", "films", "../sharewide.txt"} {
		if f, _, err := openUnder(root, rel); err == nil {
			f.Close()
			t.Errorf("%q was opened", rel)
		}
	}
}

// The folder itself may be reached through a link — /home/me/share pointing
// at a disk — and that is not an escape.
func TestOpenUnderARootThatIsALink(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk")
	os.MkdirAll(disk, 0o755)
	os.WriteFile(filepath.Join(disk, "a.txt"), []byte("a"), 0o644)
	link := filepath.Join(dir, "share")
	os.Symlink(disk, link)

	f, _, err := openUnder(link, "a.txt")
	if err != nil {
		t.Fatalf("through a linked root: %v", err)
	}
	f.Close()
}
