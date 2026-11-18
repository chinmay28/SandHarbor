package thumb

// Thumbnails made on the server, from the file itself.
//
// Normalize (thumb.go) re-encodes a small picture somebody else drew. This is
// the other way to a thumbnail: start from the stored file and draw it here —
// which is what a background job needs, since nobody's browser is there to do
// it. JPEG (turned the way its EXIF says), PNG and GIF are the standard
// library's; HEIC, what an iPhone takes, is github.com/gen2brain/heic, a
// decoder run as WebAssembly in pure Go so that the build stays free of C; a
// PDF's first page is drawn by poppler's pdftoppm, when the machine has it.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gen2brain/heic"
)

// MaxSourceBytes is the largest file a thumbnail is made from. Drawing a
// picture larger than this costs more than its thumbnail is worth; the
// browser's own limit is the same.
const MaxSourceBytes = 80 << 20

// maxSourcePixels caps a source picture's area before it is decoded, so a
// small file claiming to be enormous cannot make the server allocate for it.
// A 48-megapixel phone photograph fits with room to spare.
const maxSourcePixels = 120_000_000

// pdfTimeout bounds drawing one PDF's first page.
const pdfTimeout = time.Minute

// The kinds of file a thumbnail can be made from on the server.
const (
	KindImage = "image"
	KindHEIC  = "heic"
	KindPDF   = "pdf"
)

// ErrNoPDFRenderer is returned for a PDF on a machine without pdftoppm.
var ErrNoPDFRenderer = errors.New("drawing a PDF needs pdftoppm (poppler-utils), and this machine does not have it")

// SourceKind says which kind of file this is for making a thumbnail from, by
// its type and name, or "" when the server cannot make one from it.
func SourceKind(mime, name string) string {
	mime = strings.ToLower(strings.TrimSpace(mime))
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case mime == "image/heic" || mime == "image/heif" || ext == ".heic" || ext == ".heif":
		return KindHEIC
	case mime == "image/jpeg" || mime == "image/png" || mime == "image/gif",
		ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif":
		return KindImage
	case mime == "application/pdf" || ext == ".pdf":
		return KindPDF
	}
	return ""
}

// FromSource draws a stored thumbnail — a JPEG no larger than Size on its
// longest edge — from a whole file of the kind SourceKind names.
func FromSource(ctx context.Context, data []byte, mime, name string) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("the file is empty")
	}
	if len(data) > MaxSourceBytes {
		return nil, fmt.Errorf("the file is larger than %d MB", MaxSourceBytes>>20)
	}

	var (
		src    image.Image
		orient = 1
		err    error
	)
	switch SourceKind(mime, name) {
	case KindHEIC:
		// The decoder turns the picture the way the file says itself.
		if cfg, cerr := heic.DecodeConfig(bytes.NewReader(data)); cerr == nil {
			if err := checkArea(cfg); err != nil {
				return nil, err
			}
		}
		src, err = heic.Decode(bytes.NewReader(data))
	case KindImage:
		src, err = decodeStandard(data)
		orient = jpegOrientation(data)
	case KindPDF:
		var page []byte
		if page, err = pdfFirstPage(ctx, data); err == nil {
			src, err = decodeStandard(page)
		}
	default:
		return nil, fmt.Errorf("a thumbnail cannot be made from %s on the server", describe(mime, name))
	}
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, orientate(shrink(src, Size), orient), &jpeg.Options{Quality: Quality}); err != nil {
		return nil, fmt.Errorf("encoding the thumbnail: %w", err)
	}
	return out.Bytes(), nil
}

func decodeStandard(data []byte) (image.Image, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("this is not a picture the server can read: %w", err)
	}
	if err := checkArea(cfg); err != nil {
		return nil, err
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("this is not a picture the server can read: %w", err)
	}
	return src, nil
}

func checkArea(cfg image.Config) error {
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return errors.New("this picture has no size")
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxSourcePixels {
		return fmt.Errorf("this picture is %dx%d, too large to draw", cfg.Width, cfg.Height)
	}
	return nil
}

func describe(mime, name string) string {
	if ext := filepath.Ext(name); ext != "" {
		return "a " + strings.ToLower(ext) + " file"
	}
	if mime != "" {
		return mime
	}
	return "this file"
}

// pdfFirstPage draws a PDF's first page as a PNG, with pdftoppm, at about
// twice the thumbnail's size so the box filter has something to average.
func pdfFirstPage(ctx context.Context, data []byte) ([]byte, error) {
	tool, err := exec.LookPath("pdftoppm")
	if err != nil {
		return nil, ErrNoPDFRenderer
	}
	dir, err := os.MkdirTemp("", "sand-pdf-thumb-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(in, data, 0o600); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, pdfTimeout)
	defer cancel()
	outBase := filepath.Join(dir, "page")
	cmd := exec.CommandContext(ctx, tool,
		"-f", "1", "-l", "1", "-singlefile", "-png",
		"-scale-to", fmt.Sprint(Size*2), in, outBase)
	if msg, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("pdftoppm could not draw the first page: %s", firstLine(msg, err))
	}
	return os.ReadFile(outBase + ".png")
}

func firstLine(msg []byte, err error) string {
	if line, _, _ := strings.Cut(strings.TrimSpace(string(msg)), "\n"); line != "" {
		return line
	}
	return err.Error()
}
