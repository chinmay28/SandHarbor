package filedate

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"
)

// --- Files, built by hand ------------------------------------------------

// byteOrder is a byte order that can write as well as read.
type byteOrder interface {
	binary.ByteOrder
	binary.AppendByteOrder
}

// exifBlock builds a TIFF-shaped EXIF block. ifd0 and sub map tags to ASCII
// values; a sub-IFD is added, and pointed at, when sub is not empty.
func exifBlock(order byteOrder, ifd0, sub map[uint16]string) []byte {
	mark := "II"
	if order == byteOrder(binary.BigEndian) {
		mark = "MM"
	}
	out := []byte(mark)
	out = order.AppendUint16(out, 42)
	out = order.AppendUint32(out, 8)

	// Each directory is laid out whole, its long values after it.
	directory := func(at int, texts map[uint16]string, pointer int) []byte {
		count := len(texts)
		if pointer > 0 {
			count++
		}
		dir := order.AppendUint16(nil, uint16(count))
		tail := []byte{}
		valuesAt := at + 2 + count*12 + 4
		for _, tag := range sorted(texts) {
			text := append([]byte(texts[tag]), 0)
			dir = order.AppendUint16(dir, tag)
			dir = order.AppendUint16(dir, 2)
			dir = order.AppendUint32(dir, uint32(len(text)))
			if len(text) <= 4 {
				dir = append(dir, append(text, make([]byte, 4-len(text))...)...)
			} else {
				dir = order.AppendUint32(dir, uint32(valuesAt+len(tail)))
				tail = append(tail, text...)
			}
		}
		if pointer > 0 {
			dir = order.AppendUint16(dir, tagExifIFD)
			dir = order.AppendUint16(dir, 4)
			dir = order.AppendUint32(dir, 1)
			dir = order.AppendUint32(dir, uint32(pointer))
		}
		dir = order.AppendUint32(dir, 0) // no next directory
		return append(dir, tail...)
	}

	if len(sub) == 0 {
		return append(out, directory(8, ifd0, 0)...)
	}
	// The length of the first directory does not depend on where the second
	// is, so it is laid out once to find that and once more to point at it.
	subAt := 8 + len(directory(8, ifd0, 1))
	out = append(out, directory(8, ifd0, subAt)...)
	return append(out, directory(subAt, sub, 0)...)
}

func sorted(m map[uint16]string) []uint16 {
	tags := make([]uint16, 0, len(m))
	for tag := range m {
		tags = append(tags, tag)
	}
	for i := range tags {
		for j := i + 1; j < len(tags); j++ {
			if tags[j] < tags[i] {
				tags[i], tags[j] = tags[j], tags[i]
			}
		}
	}
	return tags
}

// jpeg wraps segments in the start of a JPEG: an APP0 any camera writes, then
// whatever is given, then a scan.
func jpeg(segments ...[]byte) []byte {
	out := []byte{0xFF, 0xD8}
	out = append(out, segment(0xE0, []byte("JFIF\x00\x01\x02\x00\x00\x01\x00\x01\x00\x00"))...)
	for _, s := range segments {
		out = append(out, s...)
	}
	out = append(out, 0xFF, 0xDA, 0x00, 0x02)
	return append(out, bytes.Repeat([]byte{0x55}, 4096)...)
}

func segment(marker byte, body []byte) []byte {
	out := []byte{0xFF, marker}
	out = binary.BigEndian.AppendUint16(out, uint16(len(body)+2))
	return append(out, body...)
}

func exifSegment(tiff []byte) []byte {
	return segment(0xE1, append([]byte("Exif\x00\x00"), tiff...))
}

// mp4box is one box: a length, a kind, and what it holds.
func mp4box(kind string, body ...[]byte) []byte {
	all := bytes.Join(body, nil)
	out := binary.BigEndian.AppendUint32(nil, uint32(len(all)+8))
	out = append(out, kind...)
	return append(out, all...)
}

// mvhd is a movie header of either version, made `seconds` after 1904.
func mvhd(version byte, seconds uint64) []byte {
	body := []byte{version, 0, 0, 0}
	if version == 1 {
		body = binary.BigEndian.AppendUint64(body, seconds)
		body = binary.BigEndian.AppendUint64(body, seconds)
	} else {
		body = binary.BigEndian.AppendUint32(body, uint32(seconds))
		body = binary.BigEndian.AppendUint32(body, uint32(seconds))
	}
	return mp4box("mvhd", body, make([]byte, 80))
}

// since1904 is a time as a movie header counts it.
func since1904(t time.Time) uint64 {
	return uint64(t.Sub(movieEpoch) / time.Second)
}

// heic builds a HEIF picture whose EXIF item lies after its meta box, located
// by an iloc of the given version.
func heic(ilocVersion byte, tiff []byte) []byte {
	const exifID = 7
	ftyp := mp4box("ftyp", []byte("heic\x00\x00\x00\x00mif1heic"))

	infe := func(id uint16, kind string) []byte {
		body := []byte{2, 0, 0, 0}
		body = binary.BigEndian.AppendUint16(body, id)
		body = binary.BigEndian.AppendUint16(body, 0)
		body = append(body, kind...)
		return mp4box("infe", append(body, 0))
	}
	iinf := mp4box("iinf", []byte{0, 0, 0, 0, 0, 2}, infe(1, "hvc1"), infe(exifID, "Exif"))

	// The item: four bytes saying the TIFF header is six bytes further on,
	// past the "Exif\0\0" that phones write there all the same.
	item := append([]byte{0, 0, 0, 6}, append([]byte("Exif\x00\x00"), tiff...)...)

	iloc := func(itemAt uint32) []byte {
		body := []byte{ilocVersion, 0, 0, 0, 0x44, 0x00} // 4-byte offsets and lengths, no base
		entry := func(id uint16, offset, length uint32) {
			if ilocVersion >= 2 {
				body = binary.BigEndian.AppendUint32(body, uint32(id))
			} else {
				body = binary.BigEndian.AppendUint16(body, id)
			}
			if ilocVersion >= 1 {
				body = binary.BigEndian.AppendUint16(body, 0) // construction method: in the file
			}
			body = binary.BigEndian.AppendUint16(body, 0) // data reference
			body = binary.BigEndian.AppendUint16(body, 1) // one extent
			body = binary.BigEndian.AppendUint32(body, offset)
			body = binary.BigEndian.AppendUint32(body, length)
		}
		if ilocVersion >= 2 {
			body = binary.BigEndian.AppendUint32(body, 2)
		} else {
			body = binary.BigEndian.AppendUint16(body, 2)
		}
		entry(1, 9999, 10)
		entry(exifID, itemAt, uint32(len(item)))
		return mp4box("iloc", body)
	}

	meta := func(itemAt uint32) []byte {
		return mp4box("meta", []byte{0, 0, 0, 0},
			mp4box("hdlr", make([]byte, 24)), iinf, iloc(itemAt))
	}
	// The item sits in the mdat after the meta box, whose length does not
	// depend on the offset written in it.
	itemAt := len(ftyp) + len(meta(0)) + 8
	out := append(ftyp, meta(uint32(itemAt))...)
	return append(out, mp4box("mdat", item, bytes.Repeat([]byte{0x33}, 2048))...)
}

// --- A reader that counts ------------------------------------------------

// counted is a file that says how much of itself was read, since the whole
// point of reading through a ReaderAt is not reading the whole file.
type counted struct {
	data  []byte
	reads int
	bytes int
	fail  error
}

func (c *counted) ReadAt(p []byte, off int64) (int, error) {
	if c.fail != nil {
		return 0, c.fail
	}
	c.reads++
	c.bytes += len(p)
	return bytes.NewReader(c.data).ReadAt(p, off)
}

func read(t *testing.T, data []byte, name string) (Stamp, bool, *counted) {
	t.Helper()
	file := &counted{data: data}
	got, ok, err := FromContent(file, int64(len(data)), name, cest)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return got, ok, file
}

// --- JPEG ----------------------------------------------------------------

func TestJPEGDateIsWhenThePictureWasTaken(t *testing.T) {
	for _, order := range []byteOrder{binary.LittleEndian, binary.BigEndian} {
		tiff := exifBlock(order,
			map[uint16]string{0x010F: "Canon", tagDateTime: "2024:01:02 03:04:05"},
			map[uint16]string{
				tagDateTimeOriginal:  "2023:05:14 10:15:02",
				tagDateTimeDigitized: "2023:05:14 10:15:09",
			})

		got, ok, file := read(t, jpeg(exifSegment(tiff)), "IMG_1234.JPG")

		// No zone recorded, so the wall clock is the viewer's.
		want := time.Date(2023, 5, 14, 10, 15, 2, 0, cest)
		if !ok || !got.At.Equal(want) || got.DayOnly {
			t.Errorf("%v: read %+v, %v; want %s", order, got, ok, want)
		}
		if file.bytes > 1024 {
			t.Errorf("%v: read %d bytes of a %d byte picture to find its date", order, file.bytes, len(file.data))
		}
	}
}

func TestJPEGDateIsReadInTheZoneTheCameraRecorded(t *testing.T) {
	tiff := exifBlock(binary.LittleEndian, nil, map[uint16]string{
		tagDateTimeOriginal: "2023:05:14 10:15:02",
		tagOffsetOriginal:   "-07:00",
	})

	got, ok, _ := read(t, jpeg(exifSegment(tiff)), "IMG_1234.jpg")

	want := time.Date(2023, 5, 14, 17, 15, 2, 0, time.UTC)
	if !ok || !got.At.Equal(want) {
		t.Errorf("read %+v, %v; want %s", got, ok, want)
	}
}

func TestJPEGDateFallsBackThroughTheTagsItHas(t *testing.T) {
	digitized := exifBlock(binary.BigEndian, nil,
		map[uint16]string{tagDateTimeDigitized: "2021:03:04 05:06:07"})
	if got, ok, _ := read(t, jpeg(exifSegment(digitized)), "scan.jpg"); !ok ||
		!got.At.Equal(time.Date(2021, 3, 4, 5, 6, 7, 0, cest)) {
		t.Errorf("digitized only: read %+v, %v", got, ok)
	}

	// Only the time the file was written, and the zone that goes with it.
	written := exifBlock(binary.LittleEndian,
		map[uint16]string{tagDateTime: "2020-01-02 03:04:05"},
		map[uint16]string{tagOffsetTime: "+00:00"})
	if got, ok, _ := read(t, jpeg(exifSegment(written)), "edit.jpeg"); !ok ||
		!got.At.Equal(time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("file time only: read %+v, %v", got, ok)
	}

	// A camera whose clock was never set writes zeros, which is no date.
	unset := exifBlock(binary.LittleEndian, nil, map[uint16]string{
		tagDateTimeOriginal: "0000:00:00 00:00:00",
		tagDateTime:         "    :  :     :  :  ",
	})
	if got, ok, _ := read(t, jpeg(exifSegment(unset)), "unset.jpg"); ok {
		t.Errorf("unset clock: read %s, want no date", got.At)
	}
}

func TestJPEGWithoutADateSaysSo(t *testing.T) {
	for why, data := range map[string][]byte{
		"no EXIF at all":       jpeg(),
		"EXIF with no date":    jpeg(exifSegment(exifBlock(binary.LittleEndian, map[uint16]string{0x010F: "Canon"}, nil))),
		"an APP1 that is XMP":  jpeg(segment(0xE1, []byte("http://ns.adobe.com/xap/1.0/\x00<x/>"))),
		"not a JPEG":           []byte("%PDF-1.7 and so on"),
		"cut off in a segment": jpeg(exifSegment(exifBlock(binary.LittleEndian, nil, map[uint16]string{tagDateTimeOriginal: "2023:05:14 10:15:02"})))[:40],
		"empty":                {},
		"EXIF that is garbage": jpeg(exifSegment(bytes.Repeat([]byte{0xFF}, 300))),
	} {
		if got, ok, _ := read(t, data, "picture.jpg"); ok {
			t.Errorf("%s: read %s, want no date", why, got.At)
		}
	}
}

// --- Movies --------------------------------------------------------------

func TestMovieDateIsItsHeadersCreationTime(t *testing.T) {
	made := time.Date(2022, 8, 9, 18, 30, 0, 0, time.UTC)
	ftyp := mp4box("ftyp", []byte("isom\x00\x00\x02\x00isomiso2"))

	for _, version := range []byte{0, 1} {
		moov := mp4box("moov", mvhd(version, since1904(made)), mp4box("trak", make([]byte, 64)))

		// A phone writes the picture first and the index after it, so the
		// header is at the far end of a large file.
		data := append(append([]byte{}, ftyp...), mp4box("mdat", make([]byte, 1<<20))...)
		data = append(data, moov...)

		got, ok, file := read(t, data, "holiday.mp4")
		if !ok || !got.At.Equal(made) {
			t.Errorf("version %d: read %+v, %v; want %s", version, got, ok, made)
		}
		if file.bytes > 256 {
			t.Errorf("version %d: read %d bytes of a megabyte to find a header", version, file.bytes)
		}
	}
}

func TestMovieWithoutADateSaysSo(t *testing.T) {
	ftyp := mp4box("ftyp", []byte("qt  \x00\x00\x00\x00qt  "))
	for why, data := range map[string][]byte{
		"creation time never set":       append(append([]byte{}, ftyp...), mp4box("moov", mvhd(0, 0))...),
		"no movie header":               append(append([]byte{}, ftyp...), mp4box("moov", mp4box("trak", nil))...),
		"no moov":                       append(append([]byte{}, ftyp...), mp4box("mdat", make([]byte, 64))...),
		"a box longer than file":        append(append([]byte{}, ftyp...), 0, 0, 0xFF, 0xFF, 'm', 'o', 'o', 'v'),
		"a box shorter than its header": append(append([]byte{}, ftyp...), 0, 0, 0, 4, 'm', 'o', 'o', 'v'),
		"cut short":                     append(append([]byte{}, ftyp...), mp4box("moov", mvhd(0, 12345))[:20]...),
	} {
		if got, ok, _ := read(t, data, "clip.MOV"); ok {
			t.Errorf("%s: read %s, want no date", why, got.At)
		}
	}
}

// A file of empty boxes must not become a fetch per box for ever.
func TestMovieWalkIsBounded(t *testing.T) {
	data := bytes.Repeat(mp4box("free"), 100000)

	_, ok, file := read(t, data, "junk.mp4")

	if ok || file.reads > maxBoxes {
		t.Errorf("walked %d boxes (found a date: %v), want at most %d", file.reads, ok, maxBoxes)
	}
}

// --- HEIF ----------------------------------------------------------------

func TestHEICDateIsItsEXIFItems(t *testing.T) {
	tiff := exifBlock(binary.BigEndian, nil, map[uint16]string{
		tagDateTimeOriginal: "2023:05:14 10:15:02",
		tagOffsetOriginal:   "+09:00",
	})
	want := time.Date(2023, 5, 14, 1, 15, 2, 0, time.UTC)

	for _, version := range []byte{0, 1, 2} {
		got, ok, _ := read(t, heic(version, tiff), "IMG_0042.HEIC")
		if !ok || !got.At.Equal(want) {
			t.Errorf("iloc version %d: read %+v, %v; want %s", version, got, ok, want)
		}
	}
}

func TestHEICWithoutADateSaysSo(t *testing.T) {
	whole := heic(1, exifBlock(binary.BigEndian, nil,
		map[uint16]string{tagDateTimeOriginal: "2023:05:14 10:15:02"}))
	for why, data := range map[string][]byte{
		"no meta box":          mp4box("ftyp", []byte("heic\x00\x00\x00\x00")),
		"cut before the item":  whole[:len(whole)-2100],
		"cut inside the meta":  whole[:60],
		"EXIF item with none":  heic(1, exifBlock(binary.BigEndian, map[uint16]string{0x010F: "Apple"}, nil)),
		"EXIF that is garbage": heic(0, bytes.Repeat([]byte{0xAB}, 64)),
	} {
		if got, ok, _ := read(t, data, "picture.heic"); ok {
			t.Errorf("%s: read %s, want no date", why, got.At)
		}
	}
}

// --- The contract ---------------------------------------------------------

func TestReadableGoesByTheExtension(t *testing.T) {
	for name, want := range map[string]bool{
		"a.jpg": true, "a.JPEG": true, "a.heic": true, "a.avif": true,
		"a.mp4": true, "a.MOV": true, "a.3gp": true,
		"a.png": false, "a.mkv": false, "a.pdf": false, "jpg": false, "a": false,
	} {
		if got := Readable(name); got != want {
			t.Errorf("%s: readable is %v, want %v", name, got, want)
		}
	}
}

func TestFromContentReadsNothingOfAKindItDoesNotKnow(t *testing.T) {
	_, ok, file := read(t, []byte("anything"), "notes.txt")
	if ok || file.reads != 0 {
		t.Errorf("read a text file %d times and found a date: %v", file.reads, ok)
	}
}

// A read that fails is an error, which is a different answer from "no date":
// the caller should not conclude anything about the file.
func TestFromContentReportsAReadThatFailed(t *testing.T) {
	gone := errors.New("the cloud did not answer")
	for _, name := range []string{"a.jpg", "a.heic", "a.mp4"} {
		_, ok, err := FromContent(&counted{fail: gone}, 4096, name, cest)
		if ok || !errors.Is(err, gone) {
			t.Errorf("%s: got %v, %v; want the read's own error", name, ok, err)
		}
	}
}

// Arbitrary bytes under every name must come back as an answer, never a panic.
func FuzzFromContent(f *testing.F) {
	tiff := exifBlock(binary.LittleEndian, nil, map[uint16]string{tagDateTimeOriginal: "2023:05:14 10:15:02"})
	f.Add(jpeg(exifSegment(tiff)))
	f.Add(heic(2, tiff))
	f.Add(mp4box("moov", mvhd(1, 3786825600)))
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, name := range []string{"a.jpg", "a.heic", "a.mp4"} {
			_, _, err := FromContent(bytes.NewReader(data), int64(len(data)), name, time.UTC)
			if err != nil && !errors.Is(err, io.EOF) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
}
