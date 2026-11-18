package thumb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func decodeJPEG(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("the thumbnail is not a JPEG: %v", err)
	}
	return img
}

func fitsSize(t *testing.T, img image.Image) {
	t.Helper()
	if b := img.Bounds(); b.Dx() > Size || b.Dy() > Size || b.Dx() < 1 || b.Dy() < 1 {
		t.Errorf("thumbnail is %dx%d, want within %d", b.Dx(), b.Dy(), Size)
	}
}

func TestSourceKind(t *testing.T) {
	for _, c := range []struct{ mime, name, want string }{
		{"image/jpeg", "a.jpg", KindImage},
		{"", "IMG_3157.HEIC", KindHEIC},
		{"image/heif", "x", KindHEIC},
		{"image/png", "", KindImage},
		{"", "scan.PDF", KindPDF},
		{"video/mp4", "clip.mp4", ""},
		{"image/webp", "a.webp", ""},
		{"", "notes.txt", ""},
	} {
		if got := SourceKind(c.mime, c.name); got != c.want {
			t.Errorf("SourceKind(%q, %q) = %q, want %q", c.mime, c.name, got, c.want)
		}
	}
}

// An iPhone photograph, decoded in pure Go.
func TestFromSourceDrawsAHEIC(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "tests", "sample.heic"))
	if err != nil {
		t.Fatalf("reading the sample: %v", err)
	}
	out, err := FromSource(context.Background(), data, "", "sample.heic")
	if err != nil {
		t.Fatalf("FromSource: %v", err)
	}
	fitsSize(t, decodeJPEG(t, out))
}

func TestFromSourceDrawsAPNG(t *testing.T) {
	out, err := FromSource(context.Background(), sourcePNG(t, 900, 300), "image/png", "wide.png")
	if err != nil {
		t.Fatalf("FromSource: %v", err)
	}
	img := decodeJPEG(t, out)
	if b := img.Bounds(); b.Dx() != Size || b.Dy() != Size/3 {
		t.Errorf("thumbnail is %dx%d, want %dx%d", b.Dx(), b.Dy(), Size, Size/3)
	}
}

// withOrientation puts an EXIF block saying orientation o into a JPEG, after
// its start-of-image marker, as a camera writes it.
func withOrientation(t *testing.T, jpg []byte, o uint16, order binary.ByteOrder) []byte {
	t.Helper()
	tiff := new(bytes.Buffer)
	if order == binary.LittleEndian {
		tiff.WriteString("II")
	} else {
		tiff.WriteString("MM")
	}
	binary.Write(tiff, order, uint16(42))
	binary.Write(tiff, order, uint32(8)) // the first IFD straight after
	binary.Write(tiff, order, uint16(1)) // one entry
	binary.Write(tiff, order, uint16(0x0112))
	binary.Write(tiff, order, uint16(3)) // SHORT
	binary.Write(tiff, order, uint32(1))
	binary.Write(tiff, order, o)
	binary.Write(tiff, order, uint16(0))
	binary.Write(tiff, order, uint32(0)) // no next IFD

	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	out := append([]byte{}, jpg[:2]...)
	out = append(out, seg...)
	out = append(out, payload...)
	return append(out, jpg[2:]...)
}

// A portrait photograph stored on its side, as phones store them, comes out
// standing up: the corner the sensor read first ends up at the top right.
func TestFromSourceTurnsAJPEGTheWayItsExifSays(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 80, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 80; x++ {
			src.Set(x, y, color.RGBA{0, 0, 255, 255})
		}
	}
	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			src.Set(x, y, color.RGBA{255, 0, 0, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}

	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		data := withOrientation(t, buf.Bytes(), 6, order)
		if got := jpegOrientation(data); got != 6 {
			t.Fatalf("jpegOrientation = %d, want 6", got)
		}
		out, err := FromSource(context.Background(), data, "image/jpeg", "IMG_1.JPG")
		if err != nil {
			t.Fatalf("FromSource: %v", err)
		}
		img := decodeJPEG(t, out)
		if b := img.Bounds(); b.Dx() != 40 || b.Dy() != 80 {
			t.Fatalf("turned thumbnail is %dx%d, want 40x80", b.Dx(), b.Dy())
		}
		r, _, bl, _ := img.At(35, 4).RGBA()
		if r>>8 < 200 || bl>>8 > 80 {
			t.Errorf("the top right is not the red corner: r=%d b=%d", r>>8, bl>>8)
		}
	}

	if got := jpegOrientation(buf.Bytes()); got != 1 {
		t.Errorf("a JPEG without EXIF reads as orientation %d", got)
	}
}

func TestOrientateSwapsTheSidesForTheTurns(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 2))
	for o := 1; o <= 8; o++ {
		b := orientate(src, o).Bounds()
		wantW, wantH := 4, 2
		if o >= 5 {
			wantW, wantH = 2, 4
		}
		if b.Dx() != wantW || b.Dy() != wantH {
			t.Errorf("orientation %d: %dx%d, want %dx%d", o, b.Dx(), b.Dy(), wantW, wantH)
		}
	}
}

func TestFromSourceDrawsAPDFsFirstPage(t *testing.T) {
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		t.Skip("pdftoppm is not installed here")
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "tests", "design.pdf"))
	if err != nil {
		t.Fatalf("reading the sample: %v", err)
	}
	out, err := FromSource(context.Background(), data, "application/pdf", "design.pdf")
	if err != nil {
		t.Fatalf("FromSource: %v", err)
	}
	fitsSize(t, decodeJPEG(t, out))
}

func TestFromSourceSaysWhenThereIsNoPDFRenderer(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := FromSource(context.Background(), []byte("%PDF-1.4"), "application/pdf", "a.pdf")
	if !errors.Is(err, ErrNoPDFRenderer) {
		t.Errorf("err = %v, want ErrNoPDFRenderer", err)
	}
}

func TestFromSourceRefusesWhatItCannotDraw(t *testing.T) {
	ctx := context.Background()
	if _, err := FromSource(ctx, nil, "image/png", "a.png"); err == nil {
		t.Error("an empty file was drawn")
	}
	if _, err := FromSource(ctx, []byte("not a picture"), "image/jpeg", "a.jpg"); err == nil {
		t.Error("garbage was drawn")
	}
	if _, err := FromSource(ctx, []byte("....ftypmp42"), "video/mp4", "a.mp4"); err == nil {
		t.Error("a video was drawn")
	}
	big := make([]byte, MaxSourceBytes+1)
	if _, err := FromSource(ctx, big, "image/png", "a.png"); err == nil {
		t.Error("a file over the limit was drawn")
	}

	// A small file claiming to be enormous is refused before it is decoded.
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	huge := buf.Bytes()
	binary.BigEndian.PutUint32(huge[16:20], 20000) // IHDR width
	binary.BigEndian.PutUint32(huge[20:24], 20000) // IHDR height
	if _, err := FromSource(ctx, huge, "image/png", "a.png"); err == nil {
		t.Error("a 20000x20000 picture was decoded")
	}
}
