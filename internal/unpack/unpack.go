// Package unpack reads the files out of an archive, one at a time.
//
// It knows two formats — zip, and a gzipped tar — and does one thing with
// them: hand each entry to the caller, in the order the archive holds them,
// without ever writing anything anywhere itself. Where an entry goes, and
// whether its path is one worth honouring, is the caller's business; an
// archive is somebody else's word about what a tree looks like, and this
// package passes that word on verbatim rather than vouching for it.
//
// What it does decide is what counts as a file. A symbolic link, a device, a
// hard link and a password-protected entry are none of them bytes that can be
// stored under a name, so they are skipped and reported rather than guessed
// at, and the bookkeeping an archiver leaves behind for itself (macOS resource
// forks) is dropped without comment.
package unpack

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"
)

// Format is a kind of archive this package can read.
type Format int

const (
	Zip Format = iota + 1
	TarGz
)

func (f Format) String() string {
	switch f {
	case Zip:
		return "zip"
	case TarGz:
		return "tar.gz"
	}
	return "unknown"
}

// suffixes maps the endings a filename can have to the format they name,
// longest first so ".tar.gz" is not read as a ".gz" of something.
var suffixes = []struct {
	ext    string
	format Format
}{
	{".tar.gz", TarGz},
	{".tgz", TarGz},
	{".zip", Zip},
}

// FormatOf says which format a file of this name is, going by its extension
// alone. The second result is false for a name that is not an archive this
// package reads — including one that is nothing but the extension, since
// there would be nothing left to call the folder it unpacks into.
func FormatOf(name string) (Format, bool) {
	lower := strings.ToLower(name)
	for _, s := range suffixes {
		if strings.HasSuffix(lower, s.ext) && len(name) > len(s.ext) {
			return s.format, true
		}
	}
	return 0, false
}

// FolderName is what an archive of this name unpacks into: the name without
// its archive extension, so "photos.tar.gz" is "photos". A name FormatOf does
// not recognise is returned as it is.
func FolderName(name string) string {
	lower := strings.ToLower(name)
	for _, s := range suffixes {
		if strings.HasSuffix(lower, s.ext) && len(name) > len(s.ext) {
			return name[:len(name)-len(s.ext)]
		}
	}
	return name
}

// Entry is one file or folder of an archive.
type Entry struct {
	// Path is the entry's path as the archive wrote it, with forward slashes
	// and no trailing one. It is not checked: it may be absolute, climb with
	// "..", or hold names nothing would accept. The caller must decide.
	Path string
	// Dir is true for a folder, which has no contents of its own.
	Dir bool
	// Size is the length of a file's contents once unpacked.
	Size int64
	// ModTime is when the archive says the entry was last changed, or the
	// zero time if it does not say.
	ModTime time.Time
}

// Skip is an entry that was passed over, and why.
type Skip struct {
	Path   string
	Reason string
}

// Limits bounds what Walk will unpack. An archive is a few megabytes that can
// claim to be anything: both limits are there so a small upload cannot become
// an unbounded amount of work. Zero means no limit.
type Limits struct {
	// MaxEntries caps how many entries the archive may hold, of any kind.
	MaxEntries int
	// MaxBytes caps the total unpacked size of its files.
	MaxBytes int64
}

// ErrTooManyEntries and ErrTooLarge are what Walk's error wraps when an
// archive is over one of its Limits.
var (
	ErrTooManyEntries = errors.New("archive holds too many entries")
	ErrTooLarge       = errors.New("archive unpacks to too much")
)

// Visitor is called once per entry. For a file, body yields exactly its
// contents and is only valid until the visitor returns; for a folder it is
// nil. Returning an error stops the walk, and Walk returns that error.
//
// A visitor that does not read a file's body to the end has not hurt
// anything: the walk moves on to the next entry regardless.
type Visitor func(entry Entry, body io.Reader) error

// Walk reads the archive in src, which is size bytes long, and calls visit
// for each file and folder in it, in archive order. It returns the entries it
// passed over, and the first error that stopped it.
//
// A zip is checked against the limits before the first visit, because its
// directory says up front what it holds: over the limit, nothing is visited.
// A tar only reveals itself as it is read, so it is visited until the entry
// that crosses a limit and stops there. Either way an error from Walk means
// the archive was not read to its end, and whatever was visited was visited.
func Walk(format Format, src io.ReaderAt, size int64, limits Limits, visit Visitor) ([]Skip, error) {
	switch format {
	case Zip:
		return walkZip(src, size, limits, visit)
	case TarGz:
		return walkTarGz(io.NewSectionReader(src, 0, size), limits, visit)
	}
	return nil, fmt.Errorf("unpack: unknown archive format %d", int(format))
}

// budget counts entries and bytes against the limits.
type budget struct {
	limits  Limits
	entries int
	bytes   int64
}

func (b *budget) entry() error {
	b.entries++
	if b.limits.MaxEntries > 0 && b.entries > b.limits.MaxEntries {
		return fmt.Errorf("%w: more than %d", ErrTooManyEntries, b.limits.MaxEntries)
	}
	return nil
}

func (b *budget) add(size int64) error {
	// A negative or wrapping size is a lie either way.
	if size < 0 || b.bytes+size < b.bytes {
		return fmt.Errorf("%w: an entry claims an impossible size", ErrTooLarge)
	}
	b.bytes += size
	if b.limits.MaxBytes > 0 && b.bytes > b.limits.MaxBytes {
		return fmt.Errorf("%w: more than %d bytes", ErrTooLarge, b.limits.MaxBytes)
	}
	return nil
}

// junk reports whether a path is an archiver's own bookkeeping rather than
// anything somebody put in the archive: the "__MACOSX" tree macOS adds to
// every zip it makes, holding a resource fork per file.
func junk(path string) bool {
	return path == "__MACOSX" || strings.HasPrefix(path, "__MACOSX/")
}

// clean puts an archive's path in the one shape Entry promises: forward
// slashes, nothing trailing. It repairs nothing else.
func clean(name string) string {
	return strings.TrimRight(strings.ReplaceAll(name, "\\", "/"), "/")
}

func walkZip(src io.ReaderAt, size int64, limits Limits, visit Visitor) ([]Skip, error) {
	zr, err := zip.NewReader(src, size)
	if err != nil {
		return nil, fmt.Errorf("not a readable zip archive: %w", err)
	}

	// The whole of it is known before any of it is read, so the limits are
	// settled before anything is visited. The sizes are the directory's own
	// claim, and a claim that is enough: the reader will not yield more than
	// an entry declares, so a file cannot unpack past what was counted here.
	b := budget{limits: limits}
	for _, f := range zr.File {
		if err := b.entry(); err != nil {
			return nil, err
		}
		if !f.FileInfo().IsDir() {
			if err := b.add(int64(f.UncompressedSize64)); err != nil {
				return nil, err
			}
		}
	}

	var skipped []Skip
	for _, f := range zr.File {
		path := clean(f.Name)
		if path == "" || junk(path) {
			continue
		}
		mode := f.Mode()
		entry := Entry{Path: path, ModTime: f.Modified}

		switch {
		case mode.IsDir() || strings.HasSuffix(f.Name, "/"):
			entry.Dir = true
			if err := visit(entry, nil); err != nil {
				return skipped, err
			}
			continue
		case mode&fs.ModeSymlink != 0:
			skipped = append(skipped, Skip{path, "a symbolic link"})
			continue
		case !mode.IsRegular():
			skipped = append(skipped, Skip{path, "not an ordinary file"})
			continue
		case f.Flags&0x1 != 0:
			// Bit 0 is "encrypted". Read anyway it would be noise that
			// fails its checksum at the very end, after being stored.
			skipped = append(skipped, Skip{path, "password-protected"})
			continue
		}

		body, err := f.Open()
		if err != nil {
			skipped = append(skipped, Skip{path, fmt.Sprintf("could not be opened: %v", err)})
			continue
		}
		entry.Size = int64(f.UncompressedSize64)
		err = visit(entry, body)
		body.Close()
		if err != nil {
			return skipped, err
		}
	}
	return skipped, nil
}

func walkTarGz(src io.Reader, limits Limits, visit Visitor) ([]Skip, error) {
	gz, err := gzip.NewReader(src)
	if err != nil {
		return nil, fmt.Errorf("not a readable tar.gz archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	b := budget{limits: limits}
	var skipped []Skip
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return skipped, nil
		}
		if err != nil {
			return skipped, fmt.Errorf("not a readable tar.gz archive: %w", err)
		}
		if err := b.entry(); err != nil {
			return skipped, err
		}

		path := clean(hdr.Name)
		if path == "" || path == "." || junk(path) {
			continue
		}
		entry := Entry{Path: path, ModTime: hdr.ModTime}

		switch hdr.Typeflag {
		case tar.TypeDir:
			entry.Dir = true
			if err := visit(entry, nil); err != nil {
				return skipped, err
			}
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // old archives still write it
			if err := b.add(hdr.Size); err != nil {
				return skipped, err
			}
			entry.Size = hdr.Size
			if err := visit(entry, tr); err != nil {
				return skipped, err
			}
		case tar.TypeSymlink:
			skipped = append(skipped, Skip{path, "a symbolic link"})
		case tar.TypeLink:
			skipped = append(skipped, Skip{path, "a hard link"})
		case tar.TypeXGlobalHeader:
			// Metadata about the archive, not something in it.
		default:
			skipped = append(skipped, Skip{path, "not an ordinary file"})
		}
	}
}
