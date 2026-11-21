package udf_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"testing"

	"github.com/chinmay28/SandHarbor/internal/udf"
	"github.com/chinmay28/SandHarbor/internal/udf/udftest"
)

// content is n bytes that differ from block to block, so a read from the
// wrong place cannot pass for the right one.
func content(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func open(t *testing.T, img []byte) *udf.Volume {
	t.Helper()
	v, err := udf.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return v
}

// read returns a file's contents by way of its extents: the path a stream
// takes, rather than ReadFile's.
func read(t *testing.T, v *udf.Volume, img []byte, path string) []byte {
	t.Helper()
	e, err := v.Lookup(path)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", path, err)
	}
	extents, err := v.Extents(e)
	if err != nil {
		t.Fatalf("Extents(%q): %v", path, err)
	}
	r := udf.NewExtentReader(bytes.NewReader(img), extents)
	if r.Size() != e.Size {
		t.Fatalf("%q: extents hold %d bytes, the entry says %d", path, r.Size(), e.Size)
	}
	out, err := io.ReadAll(io.NewSectionReader(r, 0, r.Size()))
	if err != nil {
		t.Fatalf("reading %q: %v", path, err)
	}
	return out
}

// Every layout a disc image comes in has to give back the same files.
func TestReadsEveryLayout(t *testing.T) {
	files := map[string][]byte{
		"BDMV/index.bdmv":          content(1, 300),
		"BDMV/PLAYLIST/00000.mpls": content(2, 5000),
		"BDMV/STREAM/00000.m2ts":   content(3, 40*2048+77),
		"BDMV/STREAM/00001.m2ts":   content(4, 2048),
		"BDMV/STREAM/empty.m2ts":   {},
		"CERTIFICATE/":             nil,
		"Ünïcödé — 名前.txt":         content(5, 10),
	}
	layouts := map[string]udftest.Options{
		"blu-ray":            {},
		"plain":              {Plain: true},
		"fragmented":         {ExtentBlocks: 3},
		"chained":            {ExtentBlocks: 1, ChainAfter: 4},
		"embedded":           {Embed: true},
		"split metadata":     {SplitMetadata: true, ExtentBlocks: 5},
		"plain, fragmented":  {Plain: true, ExtentBlocks: 2, ChainAfter: 3},
		"padded directories": {PadDirectories: true},
		"everything at once": {ExtentBlocks: 2, ChainAfter: 2, Embed: true, SplitMetadata: true, PadDirectories: true},
	}
	for name, o := range layouts {
		t.Run(name, func(t *testing.T) {
			img := udftest.Build(files, o)
			v := open(t, img)
			for path, want := range files {
				if want == nil {
					e, err := v.Lookup(path)
					if err != nil || !e.IsDir {
						t.Errorf("Lookup(%q) = %+v, %v; want a directory", path, e, err)
					}
					continue
				}
				if got := read(t, v, img, path); !bytes.Equal(got, want) {
					t.Errorf("%q: read %d bytes that are not the %d written", path, len(got), len(want))
				}
			}
		})
	}
}

func TestReadDirListsWhatIsThere(t *testing.T) {
	img := udftest.Build(map[string][]byte{
		"BDMV/STREAM/00000.m2ts": content(1, 5000),
		"BDMV/STREAM/00001.m2ts": content(2, 10),
		"BDMV/index.bdmv":        content(3, 1),
	}, udftest.Options{})
	v := open(t, img)

	dir, err := v.Lookup("BDMV/STREAM")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := v.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := ""
	for _, e := range entries {
		got += fmt.Sprintf("%s:%d:%v ", e.Name, e.Size, e.IsDir)
	}
	if want := "00000.m2ts:5000:false 00001.m2ts:10:false "; got != want {
		t.Errorf("ReadDir = %q, want %q", got, want)
	}

	root, err := v.ReadDir(v.Root())
	if err != nil || len(root) != 1 || root[0].Name != "BDMV" || !root[0].IsDir {
		t.Errorf("root = %+v, %v; want the one directory BDMV", root, err)
	}
}

// Discs are authored in capitals and asked for in whatever case a tool likes.
func TestLookupIgnoresCase(t *testing.T) {
	img := udftest.Build(map[string][]byte{"BDMV/PLAYLIST/00000.mpls": content(1, 9)}, udftest.Options{})
	v := open(t, img)
	if _, err := v.Lookup("/bdmv/playlist/00000.MPLS"); err != nil {
		t.Errorf("Lookup in another case: %v", err)
	}
	for _, missing := range []string{"BDMV/STREAM", "BDMV/PLAYLIST/00000.mpls/x", "nope"} {
		if _, err := v.Lookup(missing); !errors.Is(err, udf.ErrNotExist) {
			t.Errorf("Lookup(%q) = %v, want ErrNotExist", missing, err)
		}
	}
}

func TestReadFileRefusesMoreThanTheLimit(t *testing.T) {
	img := udftest.Build(map[string][]byte{"big": content(1, 5000), "small": content(2, 50)}, udftest.Options{})
	v := open(t, img)
	big, _ := v.Lookup("big")
	if _, err := v.ReadFile(big, 4999); err == nil {
		t.Error("ReadFile gave back a file over its limit")
	}
	small, _ := v.Lookup("small")
	if got, err := v.ReadFile(small, 50); err != nil || !bytes.Equal(got, content(2, 50)) {
		t.Errorf("ReadFile at exactly the limit: %v", err)
	}
}

func TestOpenRefusesWhatIsNotUDF(t *testing.T) {
	for name, img := range map[string][]byte{
		"empty":    {},
		"short":    make([]byte, 1000),
		"zeros":    make([]byte, 600*2048),
		"iso 9660": append(make([]byte, 16*2048), append([]byte("\x01CD001"), make([]byte, 600*2048)...)...),
	} {
		if _, err := udf.Open(bytes.NewReader(img), int64(len(img))); !errors.Is(err, udf.ErrNotUDF) {
			t.Errorf("%s: Open = %v, want ErrNotUDF", name, err)
		}
	}
}

// The anchor at sector 256 is the one a scratched or cut-off start loses, and
// the copy at the end is there for that.
func TestOpenFallsBackToTheLastAnchor(t *testing.T) {
	want := content(1, 3000)
	img := udftest.Build(map[string][]byte{"a/b": want}, udftest.Options{})
	clear(img[256*2048 : 257*2048])
	v := open(t, img)
	if got := read(t, v, img, "a/b"); !bytes.Equal(got, want) {
		t.Error("contents differ when opened by the last anchor")
	}
}

// An image is whatever somebody uploaded. Damage anywhere in it must come
// back as an error or as wrong bytes — never a panic, a hang, or a read
// outside the image.
func TestDamagedImagesFailCleanly(t *testing.T) {
	files := map[string][]byte{
		"BDMV/PLAYLIST/00000.mpls": content(1, 500),
		"BDMV/STREAM/00000.m2ts":   content(2, 9*2048),
	}
	for _, o := range []udftest.Options{{}, {Plain: true}, {ExtentBlocks: 1, ChainAfter: 2, Embed: true}} {
		clean := udftest.Build(files, o)
		rng := rand.New(rand.NewSource(7))
		for i := 0; i < 500; i++ {
			img := bytes.Clone(clean)
			// Corrupt a few bytes of the structures: everything that is not
			// all zeros in the clean image is a structure or file data.
			for n := 0; n < 1+rng.Intn(4); n++ {
				at := rng.Intn(len(img))
				for img[at] == 0 {
					at = rng.Intn(len(img))
				}
				img[at] = byte(rng.Intn(256))
			}
			walk(bounded{bytes.NewReader(img), int64(len(img)), t}, int64(len(img)))
		}
	}
}

// walk reads everything a volume will give, ignoring errors.
func walk(r io.ReaderAt, size int64) {
	v, err := udf.Open(r, size)
	if err != nil {
		return
	}
	var visit func(e udf.Entry, depth int)
	visit = func(e udf.Entry, depth int) {
		if !e.IsDir {
			if extents, err := v.Extents(e); err == nil {
				er := udf.NewExtentReader(r, extents)
				io.Copy(io.Discard, io.NewSectionReader(er, 0, min(er.Size(), 1<<20)))
			}
			return
		}
		if depth > 8 {
			return
		}
		children, _ := v.ReadDir(e)
		for _, c := range children {
			visit(c, depth+1)
		}
	}
	visit(v.Root(), 0)
}

// bounded fails the test on a read outside the image.
type bounded struct {
	r    io.ReaderAt
	size int64
	t    *testing.T
}

func (b bounded) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > b.size {
		b.t.Errorf("read of %d bytes at %d, outside an image of %d", len(p), off, b.size)
	}
	return b.r.ReadAt(p, off)
}

func TestExtentReaderReadsAcrossExtentsAndHoles(t *testing.T) {
	img := []byte("0123456789abcdefghij")
	r := udf.NewExtentReader(bytes.NewReader(img), []udf.Extent{
		{Offset: 10, Length: 4},   // abcd
		{Length: 3, Sparse: true}, // three zeros
		{Offset: 0, Length: 2},    // 01
		{Offset: 18, Length: 2},   // ij
	})
	want := []byte("abcd\x00\x00\x0001ij")
	if r.Size() != int64(len(want)) {
		t.Fatalf("Size = %d, want %d", r.Size(), len(want))
	}
	for off := 0; off <= len(want); off++ {
		for n := 0; n <= len(want)-off+2; n++ {
			p := make([]byte, n)
			got, err := r.ReadAt(p, int64(off))
			end := min(off+n, len(want))
			if !bytes.Equal(p[:got], want[off:end]) {
				t.Fatalf("ReadAt(%d bytes at %d) = %q, want %q", n, off, p[:got], want[off:end])
			}
			if short := off+n > len(want); short && err != io.EOF || !short && n > 0 && err != nil {
				t.Fatalf("ReadAt(%d bytes at %d) err = %v", n, off, err)
			}
		}
	}
}

// An extent that claims more than the image holds is a truncated image, and a
// reader told only "EOF" would take it for the end of the film.
func TestExtentReaderReportsATruncatedImage(t *testing.T) {
	r := udf.NewExtentReader(bytes.NewReader([]byte("short")), []udf.Extent{{Offset: 0, Length: 50}})
	if _, err := r.ReadAt(make([]byte, 20), 0); err != io.ErrUnexpectedEOF {
		t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}
