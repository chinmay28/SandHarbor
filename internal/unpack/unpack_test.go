package unpack

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// member is one thing to put in a test archive. A body of "" with a path
// ending in "/" is a folder; link makes it a symbolic link instead.
type member struct {
	path string
	body string
	link string
	mod  time.Time
}

func makeZip(t *testing.T, members ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range members {
		hdr := &zip.FileHeader{Name: m.path, Method: zip.Deflate, Modified: m.mod}
		if m.link != "" {
			hdr.SetMode(0o777 | 1<<27) // fs.ModeSymlink
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, m.body+m.link)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeTarGz(t *testing.T, members ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		hdr := &tar.Header{Name: m.path, Mode: 0o644, Size: int64(len(m.body)), ModTime: m.mod, Typeflag: tar.TypeReg}
		switch {
		case m.link != "":
			hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeSymlink, m.link, 0
		case strings.HasSuffix(m.path, "/"):
			hdr.Typeflag, hdr.Mode = tar.TypeDir, 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		io.WriteString(tw, m.body)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// seen is what a walk visited, as "path=contents" for a file and "path/" for
// a folder, in order.
func walk(t *testing.T, format Format, raw []byte, limits Limits) (seen []string, skipped []Skip, err error) {
	t.Helper()
	skipped, err = Walk(format, bytes.NewReader(raw), int64(len(raw)), limits, func(e Entry, body io.Reader) error {
		if e.Dir {
			if body != nil {
				t.Errorf("%s: a folder was given a body", e.Path)
			}
			seen = append(seen, e.Path+"/")
			return nil
		}
		data, rerr := io.ReadAll(body)
		if rerr != nil {
			return rerr
		}
		if int64(len(data)) != e.Size {
			t.Errorf("%s: Size = %d, but the body held %d bytes", e.Path, e.Size, len(data))
		}
		seen = append(seen, e.Path+"="+string(data))
		return nil
	})
	return seen, skipped, err
}

// Both formats are held to the same contract, so most tests run over both.
var builders = map[Format]func(*testing.T, ...member) []byte{
	Zip:   makeZip,
	TarGz: makeTarGz,
}

func TestFormatOf(t *testing.T) {
	cases := map[string]Format{
		"photos.zip":       Zip,
		"PHOTOS.ZIP":       Zip,
		"src.tar.gz":       TarGz,
		"src.TGZ":          TarGz,
		"a.b.tar.gz":       TarGz,
		"notes.txt":        0,
		"backup.tar":       0,
		"report.gz":        0,
		"zip":              0,
		".zip":             0,
		".tar.gz":          0,
		"archive.zip.part": 0,
	}
	for name, want := range cases {
		got, ok := FormatOf(name)
		if got != want || ok != (want != 0) {
			t.Errorf("FormatOf(%q) = %v, %v; want %v", name, got, ok, want)
		}
	}
}

func TestFolderName(t *testing.T) {
	cases := map[string]string{
		"photos.zip":      "photos",
		"Photos 2024.ZIP": "Photos 2024",
		"src-1.0.tar.gz":  "src-1.0",
		"src.tgz":         "src",
		"notes.txt":       "notes.txt",
		".zip":            ".zip",
	}
	for name, want := range cases {
		if got := FolderName(name); got != want {
			t.Errorf("FolderName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestWalkVisitsFilesAndFoldersInOrder(t *testing.T) {
	for format, build := range builders {
		t.Run(format.String(), func(t *testing.T) {
			raw := build(t,
				member{path: "top.txt", body: "one"},
				member{path: "docs/"},
				member{path: "docs/deep/note.txt", body: "two"},
				member{path: "empty.txt"},
			)
			seen, skipped, err := walk(t, format, raw, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			want := "top.txt=one docs/ docs/deep/note.txt=two empty.txt="
			if got := strings.Join(seen, " "); got != want {
				t.Errorf("visited %q, want %q", got, want)
			}
			if len(skipped) != 0 {
				t.Errorf("skipped %v, want nothing", skipped)
			}
		})
	}
}

func TestWalkReportsTheModifiedTime(t *testing.T) {
	when := time.Date(2019, 6, 1, 12, 30, 0, 0, time.UTC)
	for format, build := range builders {
		t.Run(format.String(), func(t *testing.T) {
			raw := build(t, member{path: "old.txt", body: "x", mod: when})
			var got time.Time
			_, err := Walk(format, bytes.NewReader(raw), int64(len(raw)), Limits{}, func(e Entry, _ io.Reader) error {
				got = e.ModTime
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(when) {
				t.Errorf("ModTime = %v, want %v", got, when)
			}
		})
	}
}

func TestWalkSkipsLinksAndSaysSo(t *testing.T) {
	for format, build := range builders {
		t.Run(format.String(), func(t *testing.T) {
			raw := build(t,
				member{path: "real.txt", body: "kept"},
				member{path: "passwd", link: "/etc/passwd"},
			)
			seen, skipped, err := walk(t, format, raw, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(seen, " "); got != "real.txt=kept" {
				t.Errorf("visited %q — a link is not a file to hand over", got)
			}
			if len(skipped) != 1 || skipped[0].Path != "passwd" || skipped[0].Reason != "a symbolic link" {
				t.Errorf("skipped = %v, want the link named", skipped)
			}
		})
	}
}

func TestWalkDropsArchiverBookkeeping(t *testing.T) {
	for format, build := range builders {
		t.Run(format.String(), func(t *testing.T) {
			raw := build(t,
				member{path: "__MACOSX/"},
				member{path: "__MACOSX/._photo.jpg", body: "fork"},
				member{path: "photo.jpg", body: "pixels"},
			)
			seen, skipped, err := walk(t, format, raw, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(seen, " "); got != "photo.jpg=pixels" {
				t.Errorf("visited %q", got)
			}
			if len(skipped) != 0 {
				t.Errorf("skipped = %v — bookkeeping is dropped without comment", skipped)
			}
		})
	}
}

// The path is the archive's word and is passed on as written: refusing it is
// the caller's job, and a caller cannot refuse what it was never shown.
func TestWalkPassesHostilePathsThroughUnrepaired(t *testing.T) {
	for format, build := range builders {
		t.Run(format.String(), func(t *testing.T) {
			raw := build(t,
				member{path: "../escape.txt", body: "a"},
				member{path: "/abs.txt", body: "b"},
				member{path: `win\style.txt`, body: "c"},
			)
			seen, _, err := walk(t, format, raw, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			want := "../escape.txt=a /abs.txt=b win/style.txt=c"
			if got := strings.Join(seen, " "); got != want {
				t.Errorf("visited %q, want %q", got, want)
			}
		})
	}
}

func TestWalkStopsWhenTheVisitorSaysSo(t *testing.T) {
	stop := errors.New("enough")
	for format, build := range builders {
		t.Run(format.String(), func(t *testing.T) {
			raw := build(t, member{path: "a", body: "1"}, member{path: "b", body: "2"})
			visits := 0
			_, err := Walk(format, bytes.NewReader(raw), int64(len(raw)), Limits{}, func(Entry, io.Reader) error {
				visits++
				return stop
			})
			if !errors.Is(err, stop) || visits != 1 {
				t.Errorf("err = %v after %d visits, want the visitor's error after one", err, visits)
			}
		})
	}
}

// A body left unread must not derail the entries after it.
func TestWalkSurvivesAVisitorThatDoesNotRead(t *testing.T) {
	for format, build := range builders {
		t.Run(format.String(), func(t *testing.T) {
			raw := build(t, member{path: "a", body: strings.Repeat("x", 5000)}, member{path: "b", body: "tail"})
			var last string
			_, err := Walk(format, bytes.NewReader(raw), int64(len(raw)), Limits{}, func(e Entry, body io.Reader) error {
				if e.Path == "b" {
					data, _ := io.ReadAll(body)
					last = string(data)
				}
				return nil
			})
			if err != nil || last != "tail" {
				t.Errorf("last = %q, err = %v", last, err)
			}
		})
	}
}

func TestWalkEnforcesTheEntryLimit(t *testing.T) {
	for format, build := range builders {
		t.Run(format.String(), func(t *testing.T) {
			raw := build(t, member{path: "a", body: "1"}, member{path: "b", body: "2"}, member{path: "c", body: "3"})
			if _, _, err := walk(t, format, raw, Limits{MaxEntries: 3}); err != nil {
				t.Errorf("at the limit: %v", err)
			}
			_, _, err := walk(t, format, raw, Limits{MaxEntries: 2})
			if !errors.Is(err, ErrTooManyEntries) {
				t.Errorf("err = %v, want ErrTooManyEntries", err)
			}
		})
	}
}

func TestWalkEnforcesTheByteLimit(t *testing.T) {
	for format, build := range builders {
		t.Run(format.String(), func(t *testing.T) {
			// Highly compressible, which is the point: the limit is on what
			// comes out, not on what was uploaded.
			raw := build(t, member{path: "a", body: strings.Repeat("0", 600)}, member{path: "b", body: strings.Repeat("0", 600)})
			if _, _, err := walk(t, format, raw, Limits{MaxBytes: 1200}); err != nil {
				t.Errorf("at the limit: %v", err)
			}
			_, _, err := walk(t, format, raw, Limits{MaxBytes: 1199})
			if !errors.Is(err, ErrTooLarge) {
				t.Errorf("err = %v, want ErrTooLarge", err)
			}
		})
	}
}

// A zip says what it holds up front, so one over the limit is refused whole.
func TestAZipOverTheLimitIsNotVisitedAtAll(t *testing.T) {
	raw := makeZip(t, member{path: "small", body: "1"}, member{path: "big", body: strings.Repeat("0", 1000)})
	seen, _, err := walk(t, Zip, raw, Limits{MaxBytes: 500})
	if !errors.Is(err, ErrTooLarge) || len(seen) != 0 {
		t.Errorf("visited %v with err %v, want nothing visited", seen, err)
	}
}

func TestWalkSkipsAPasswordProtectedZipEntry(t *testing.T) {
	raw := makeZip(t, member{path: "secret.txt", body: "hidden"}, member{path: "open.txt", body: "plain"})
	// Set the "encrypted" bit on the first entry, in its local header and in
	// the directory, which is what a password-protected zip looks like before
	// anything is decrypted.
	for _, sig := range []string{"PK\x03\x04", "PK\x01\x02"} {
		at := bytes.Index(raw, []byte(sig))
		flags := at + 6
		if sig == "PK\x01\x02" {
			flags = at + 8
		}
		raw[flags] |= 0x1
	}
	seen, skipped, err := walk(t, Zip, raw, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(seen, " "); got != "open.txt=plain" {
		t.Errorf("visited %q", got)
	}
	if len(skipped) != 1 || skipped[0].Reason != "password-protected" {
		t.Errorf("skipped = %v", skipped)
	}
}

func TestWalkRefusesWhatIsNotAnArchive(t *testing.T) {
	raw := []byte("just some text pretending")
	for _, format := range []Format{Zip, TarGz} {
		visited := false
		_, err := Walk(format, bytes.NewReader(raw), int64(len(raw)), Limits{}, func(Entry, io.Reader) error {
			visited = true
			return nil
		})
		if err == nil || visited {
			t.Errorf("%v: err = %v, visited = %v", format, err, visited)
		}
	}
	if _, err := Walk(Format(99), bytes.NewReader(raw), int64(len(raw)), Limits{}, nil); err == nil {
		t.Error("an unknown format was accepted")
	}
}

// A tar cut short has still handed over what came before the cut, and says
// that it did not reach the end.
func TestATruncatedTarGzFailsAfterWhatItCouldRead(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"first", "second"} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 4, Typeflag: tar.TypeReg})
		io.WriteString(tw, "data")
	}
	tw.Close()
	// Cut inside the second entry's header, then gzip what is left.
	var gzbuf bytes.Buffer
	gz := gzip.NewWriter(&gzbuf)
	gz.Write(buf.Bytes()[:1024+100])
	gz.Close()

	seen, _, err := walk(t, TarGz, gzbuf.Bytes(), Limits{})
	if err == nil {
		t.Fatal("a truncated archive read as a whole one")
	}
	if got := strings.Join(seen, " "); got != "first=data" {
		t.Errorf("visited %q, want the entry before the cut", got)
	}
}
