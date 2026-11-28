package filedate

import (
	"encoding/binary"
	"errors"
	"io"
	"path"
	"strings"
	"time"
)

// The date inside a file.
//
// Three containers cover what a phone or a camera makes:
//
//   - JPEG keeps EXIF in an APP1 segment near its start.
//   - HEIC, HEIF and AVIF keep the same EXIF block as an item of the file's
//     meta box, found through the item table (iinf) and its locations (iloc).
//   - MP4, MOV, M4V and 3GP keep a creation time in the movie header (mvhd),
//     in seconds since 1904, in UTC.
//
// Everything is read through an io.ReaderAt, a few bytes at a time and only
// where the container says to look, because the file behind it is in pieces on
// other people's computers and every read is a fetch. A JPEG costs its first
// few kilobytes. A video costs a header per top-level box and then its movie
// header, wherever in the file that is.
//
// A file that is not what its name says, or that is cut short, or that simply
// carries no date, is "no date here" and not an error. An error is a read that
// failed: the bytes could not be had, which says nothing about what is in them.

// Readable reports whether a file of this name is a kind FromContent can read
// a date out of. It goes by the extension alone, since it is asked of files
// nobody has fetched yet.
func Readable(name string) bool {
	return kindOf(name) != ""
}

func kindOf(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg", ".jpe":
		return "jpeg"
	case ".heic", ".heif", ".avif":
		return "heif"
	case ".mp4", ".m4v", ".mov", ".qt", ".3gp", ".3g2":
		return "movie"
	}
	return ""
}

// FromContent reads the date a picture or video carries inside it: when the
// picture was taken, or when the recording was made.
//
// An EXIF time is a wall clock. It is read in the zone the camera recorded
// beside it when there is one, and in loc when there is not. A movie header's
// time is UTC already.
//
// It reports false, with no error, for a file with no date in it or of a kind
// it does not read (see Readable). The error is a failed read of r.
func FromContent(r io.ReaderAt, size int64, name string, loc *time.Location) (Stamp, bool, error) {
	var (
		at  time.Time
		ok  bool
		err error
	)
	switch kindOf(name) {
	case "jpeg":
		at, ok, err = jpegDate(r, size, loc)
	case "heif":
		at, ok, err = heifDate(r, size, loc)
	case "movie":
		at, ok, err = movieDate(r, size)
	}
	// Running off the end is a file cut short, which is a file with no date
	// to read rather than a read that failed.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return Stamp{}, false, nil
	}
	if err != nil || !ok || at.Before(earliest) {
		return Stamp{}, false, err
	}
	return Stamp{At: at.UTC()}, true, nil
}

// readAt is r.ReadAt for exactly n bytes at off, refusing anything outside the
// file before asking for it.
func readAt(r io.ReaderAt, size, off int64, n int) ([]byte, error) {
	if off < 0 || n < 0 || off+int64(n) > size {
		return nil, io.ErrUnexpectedEOF
	}
	buf := make([]byte, n)
	if _, err := r.ReadAt(buf, off); err != nil {
		if int64(n) == size-off && errors.Is(err, io.EOF) {
			// ReadAt may report EOF beside a full read that ends the file.
			return buf, nil
		}
		return nil, err
	}
	return buf, nil
}

// --- JPEG ----------------------------------------------------------------

// maxSegments is how many JPEG segments are walked looking for EXIF. It is
// always among the first few; this only bounds a file built to waste reads.
const maxSegments = 64

func jpegDate(r io.ReaderAt, size int64, loc *time.Location) (time.Time, bool, error) {
	head, err := readAt(r, size, 0, 2)
	if err != nil {
		return time.Time{}, false, err
	}
	if head[0] != 0xFF || head[1] != 0xD8 {
		return time.Time{}, false, nil
	}

	off := int64(2)
	for n := 0; n < maxSegments; n++ {
		seg, err := readAt(r, size, off, 4)
		if err != nil {
			return time.Time{}, false, err
		}
		// Start of scan or end of image: the picture follows, and no EXIF.
		if seg[0] != 0xFF || seg[1] == 0xDA || seg[1] == 0xD9 {
			return time.Time{}, false, nil
		}
		length := int(binary.BigEndian.Uint16(seg[2:4]))
		if length < 2 {
			return time.Time{}, false, nil
		}
		if seg[1] == 0xE1 && length >= 8 {
			body, err := readAt(r, size, off+4, length-2)
			if err != nil {
				return time.Time{}, false, err
			}
			if string(body[:6]) == "Exif\x00\x00" {
				at, ok := exifDate(body[6:], loc)
				return at, ok, nil
			}
		}
		off += 2 + int64(length)
	}
	return time.Time{}, false, nil
}

// --- EXIF ----------------------------------------------------------------

// The tags a date is read from. The first three are in the EXIF sub-IFD, with
// the zone each was recorded in beside it; DateTime is in the first IFD and is
// when the file was last written, which is the date taken until somebody
// edits the picture.
const (
	tagDateTime          = 0x0132
	tagExifIFD           = 0x8769
	tagDateTimeOriginal  = 0x9003
	tagDateTimeDigitized = 0x9004
	tagOffsetTime        = 0x9010
	tagOffsetOriginal    = 0x9011
	tagOffsetDigitized   = 0x9012
)

// exifDate reads when a picture was taken out of a TIFF-shaped EXIF block:
// the original time if it is there, then the digitized one, then the file's.
func exifDate(tiff []byte, loc *time.Location) (time.Time, bool) {
	if len(tiff) < 8 {
		return time.Time{}, false
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return time.Time{}, false
	}

	first := ifdTexts(tiff, order, int64(order.Uint32(tiff[4:8])))
	var sub map[uint16]string
	if at, ok := first.pointers[tagExifIFD]; ok {
		sub = ifdTexts(tiff, order, int64(at)).texts
	}

	for _, pair := range []struct {
		when, zone string
	}{
		{sub[tagDateTimeOriginal], sub[tagOffsetOriginal]},
		{sub[tagDateTimeDigitized], sub[tagOffsetDigitized]},
		{first.texts[tagDateTime], sub[tagOffsetTime]},
	} {
		if at, ok := exifTime(pair.when, pair.zone, loc); ok {
			return at, true
		}
	}
	return time.Time{}, false
}

// ifd is what one image file directory holds that a date needs: its ASCII
// values, and its LONG values, which is how one directory points at another.
type ifd struct {
	texts    map[uint16]string
	pointers map[uint16]uint32
}

// ifdTexts reads one directory. Anything out of bounds ends the reading where
// it stands and keeps what was read before it.
func ifdTexts(tiff []byte, order binary.ByteOrder, at int64) ifd {
	out := ifd{texts: map[uint16]string{}, pointers: map[uint16]uint32{}}
	if at < 8 || at+2 > int64(len(tiff)) {
		return out
	}
	count := int64(order.Uint16(tiff[at : at+2]))
	for n := int64(0); n < count; n++ {
		entry := at + 2 + n*12
		if entry+12 > int64(len(tiff)) {
			return out
		}
		tag := order.Uint16(tiff[entry : entry+2])
		kind := order.Uint16(tiff[entry+2 : entry+4])
		length := int64(order.Uint32(tiff[entry+4 : entry+8]))
		value := tiff[entry+8 : entry+12]

		switch kind {
		case 4: // LONG
			out.pointers[tag] = order.Uint32(value)
		case 2: // ASCII, in the entry itself when it fits and elsewhere when not
			text := value
			if length > 4 {
				start := int64(order.Uint32(value))
				if start < 0 || length > 64 || start+length > int64(len(tiff)) {
					continue
				}
				text = tiff[start : start+length]
			} else {
				text = value[:length]
			}
			out.texts[tag] = strings.TrimRight(string(text), "\x00 ")
		}
	}
	return out
}

// exifTime reads "2023:05:14 10:15:02", in the zone "+02:00" names when the
// camera recorded one and in loc when it did not.
func exifTime(when, zone string, loc *time.Location) (time.Time, bool) {
	if len(when) < 19 {
		return time.Time{}, false
	}
	// Colons by the standard, hyphens by a few cameras that did not read it.
	fields := [6]int{}
	for i, span := range [6][2]int{{0, 4}, {5, 7}, {8, 10}, {11, 13}, {14, 16}, {17, 19}} {
		for _, c := range when[span[0]:span[1]] {
			if c < '0' || c > '9' {
				return time.Time{}, false
			}
			fields[i] = fields[i]*10 + int(c-'0')
		}
	}
	if z, err := time.Parse("-07:00", strings.TrimSpace(zone)); err == nil {
		loc = z.Location()
	}
	return civil(fields[0], fields[1], fields[2], fields[3], fields[4], fields[5], loc)
}

// --- ISO base media: HEIF pictures and MP4 movies --------------------------

// box is one box of an ISO base media file: its four-letter kind, and where
// its contents begin and end.
type box struct {
	kind      string
	body, end int64
}

// maxBoxes is how many sibling boxes are walked before giving up: far more
// than any real file has at one level, and few enough that a file built of
// empty boxes cannot turn into a million fetches.
const maxBoxes = 256

// maxMeta is the most of a picture's meta box, or of its EXIF item, that is
// read into memory. Both are a few kilobytes in practice.
const maxMeta = 4 << 20

// boxAt reads the header of the box at off, within the region ending at end.
func boxAt(r io.ReaderAt, size, off, end int64) (box, error) {
	head, err := readAt(r, size, off, 8)
	if err != nil {
		return box{}, err
	}
	length := int64(binary.BigEndian.Uint32(head[:4]))
	b := box{kind: string(head[4:8]), body: off + 8}
	switch length {
	case 0: // to the end of what holds it
		b.end = end
	case 1: // a 64-bit length follows the kind
		wide, err := readAt(r, size, off+8, 8)
		if err != nil {
			return box{}, err
		}
		b.body = off + 16
		length = int64(binary.BigEndian.Uint64(wide))
		b.end = off + length
	default:
		b.end = off + length
	}
	if b.end < b.body || b.end > end {
		return box{}, io.ErrUnexpectedEOF
	}
	return b, nil
}

// findBox walks the sibling boxes between off and end for the first of a kind.
func findBox(r io.ReaderAt, size, off, end int64, kind string) (box, bool, error) {
	for n := 0; n < maxBoxes && off+8 <= end; n++ {
		b, err := boxAt(r, size, off, end)
		if err != nil {
			return box{}, false, err
		}
		if b.kind == kind {
			return b, true, nil
		}
		off = b.end
	}
	return box{}, false, nil
}

// movieEpoch is where a movie header counts its seconds from.
var movieEpoch = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)

// movieDate reads the creation time in a movie's header. A recorder that never
// set it leaves zero, which is 1904 and is "no date".
func movieDate(r io.ReaderAt, size int64) (time.Time, bool, error) {
	moov, ok, err := findBox(r, size, 0, size, "moov")
	if err != nil || !ok {
		return time.Time{}, false, err
	}
	mvhd, ok, err := findBox(r, size, moov.body, moov.end, "mvhd")
	if err != nil || !ok {
		return time.Time{}, false, err
	}
	head, err := readAt(r, size, mvhd.body, 12)
	if err != nil {
		return time.Time{}, false, err
	}
	var seconds uint64
	if head[0] == 1 {
		seconds = binary.BigEndian.Uint64(head[4:12])
	} else {
		seconds = uint64(binary.BigEndian.Uint32(head[4:8]))
	}
	// Past any year a file could be from, which also keeps the sum in range.
	if seconds == 0 || seconds > 1<<33 {
		return time.Time{}, false, nil
	}
	return movieEpoch.Add(time.Duration(seconds) * time.Second), true, nil
}

// heifDate reads the EXIF item of a HEIF picture: the meta box names the
// item (iinf) and says where in the file it lies (iloc).
func heifDate(r io.ReaderAt, size int64, loc *time.Location) (time.Time, bool, error) {
	meta, ok, err := findBox(r, size, 0, size, "meta")
	if err != nil || !ok {
		return time.Time{}, false, err
	}
	// A full box: a version and flags come before what it holds.
	if meta.end-meta.body < 4 || meta.end-meta.body > maxMeta {
		return time.Time{}, false, nil
	}
	body, err := readAt(r, size, meta.body+4, int(meta.end-meta.body-4))
	if err != nil {
		return time.Time{}, false, err
	}

	id, ok := exifItem(child(body, "iinf"))
	if !ok {
		return time.Time{}, false, nil
	}
	start, length, ok := itemExtent(child(body, "iloc"), id)
	if !ok || length < 8 || length > maxMeta {
		return time.Time{}, false, nil
	}
	item, err := readAt(r, size, start, int(length))
	if err != nil {
		return time.Time{}, false, err
	}
	// The item opens with how far into it the TIFF header is, which skips
	// the "Exif\0\0" a JPEG would have had there.
	skip := int64(binary.BigEndian.Uint32(item[:4]))
	if skip > int64(len(item))-4 {
		return time.Time{}, false, nil
	}
	at, ok := exifDate(item[4+skip:], loc)
	return at, ok, nil
}

// child is the contents of the first box of a kind among the boxes in data,
// or nil.
func child(data []byte, kind string) []byte {
	for n := 0; n < maxBoxes && len(data) >= 8; n++ {
		length := int64(binary.BigEndian.Uint32(data[:4]))
		head := int64(8)
		switch length {
		case 0:
			length = int64(len(data))
		case 1:
			if len(data) < 16 {
				return nil
			}
			length = int64(binary.BigEndian.Uint64(data[8:16]))
			head = 16
		}
		if length < head || length > int64(len(data)) {
			return nil
		}
		if string(data[4:8]) == kind {
			return data[head:length]
		}
		data = data[length:]
	}
	return nil
}

// cursor reads big-endian fields off the front of a box, and remembers having
// run out rather than making every read answer for it.
type cursor struct {
	data  []byte
	short bool
}

func (c *cursor) uint(width int) uint64 {
	if width < 0 || width > 8 || len(c.data) < width {
		c.short = true
		c.data = nil
		return 0
	}
	var v uint64
	for _, b := range c.data[:width] {
		v = v<<8 | uint64(b)
	}
	c.data = c.data[width:]
	return v
}

// exifItem finds the ID of the item of type "Exif" in an item information box.
func exifItem(iinf []byte) (uint32, bool) {
	c := &cursor{data: iinf}
	version := c.uint(1)
	c.uint(3)
	if version == 0 {
		c.uint(2)
	} else {
		c.uint(4)
	}
	if c.short {
		return 0, false
	}

	rest := c.data
	for n := 0; n < maxBoxes && len(rest) >= 8; n++ {
		length := int(binary.BigEndian.Uint32(rest[:4]))
		if length < 8 || length > len(rest) {
			return 0, false
		}
		if string(rest[4:8]) == "infe" {
			e := &cursor{data: rest[8:length]}
			v := e.uint(1)
			e.uint(3)
			// Versions 0 and 1 have no item type to be "Exif".
			if v >= 2 {
				width := 2
				if v >= 3 {
					width = 4
				}
				id := uint32(e.uint(width))
				e.uint(2) // protection index
				kind := e.uint(4)
				if !e.short && kind == 0x45786966 { // "Exif"
					return id, true
				}
			}
		}
		rest = rest[length:]
	}
	return 0, false
}

// itemExtent finds where in the file an item lies: its first extent, which for
// an EXIF block is the only one. An item kept anywhere but at an offset in the
// file itself is not found.
func itemExtent(iloc []byte, want uint32) (start, length int64, ok bool) {
	c := &cursor{data: iloc}
	version := c.uint(1)
	c.uint(3)
	sizes := c.uint(2)
	offsetSize, lengthSize := int(sizes>>12&0xF), int(sizes>>8&0xF)
	baseSize, indexSize := int(sizes>>4&0xF), int(sizes&0xF)
	if version == 0 {
		indexSize = 0
	}

	idWidth, countWidth := 2, 2
	if version >= 2 {
		idWidth, countWidth = 4, 4
	}
	items := c.uint(countWidth)
	for n := uint64(0); n < items && !c.short; n++ {
		id := uint32(c.uint(idWidth))
		method := uint64(0)
		if version >= 1 {
			method = c.uint(2) & 0xF
		}
		c.uint(2) // data reference index
		base := c.uint(baseSize)
		extents := c.uint(2)
		for x := uint64(0); x < extents && !c.short; x++ {
			c.uint(indexSize)
			offset := c.uint(offsetSize)
			size := c.uint(lengthSize)
			if id != want || x != 0 || c.short {
				continue
			}
			if method != 0 || base+offset > 1<<62 || size > 1<<62 {
				return 0, 0, false
			}
			return int64(base + offset), int64(size), true
		}
	}
	return 0, 0, false
}
