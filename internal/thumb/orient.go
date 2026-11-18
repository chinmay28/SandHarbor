package thumb

import (
	"encoding/binary"
	"image"
)

// A camera stores a picture the way its sensor read it and says, in EXIF, how
// to turn it for viewing. The browser honours that; the standard library's
// JPEG decoder ignores it. These two functions are the difference: find the
// tag, and turn the (already small) thumbnail to match.

// jpegOrientation reads the EXIF orientation of a JPEG: 1 (as stored) to 8.
// Anything it cannot find or read is 1.
func jpegOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	i := 2
	for i+4 <= len(data) {
		if data[i] != 0xFF {
			return 1
		}
		marker := data[i+1]
		// Start of scan: the picture itself follows, and no EXIF after it.
		if marker == 0xDA || marker == 0xD9 {
			return 1
		}
		size := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if size < 2 || i+2+size > len(data) {
			return 1
		}
		segment := data[i+4 : i+2+size]
		if marker == 0xE1 && len(segment) > 6 && string(segment[:6]) == "Exif\x00\x00" {
			return tiffOrientation(segment[6:])
		}
		i += 2 + size
	}
	return 1
}

// tiffOrientation finds tag 0x0112 in the first IFD of a TIFF header.
func tiffOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	ifd := int(order.Uint32(tiff[4:8]))
	if ifd < 8 || ifd+2 > len(tiff) {
		return 1
	}
	count := int(order.Uint16(tiff[ifd : ifd+2]))
	for n := 0; n < count; n++ {
		entry := ifd + 2 + n*12
		if entry+12 > len(tiff) {
			return 1
		}
		if order.Uint16(tiff[entry:entry+2]) != 0x0112 {
			continue
		}
		// A SHORT, stored in the first two bytes of the value field.
		if o := int(order.Uint16(tiff[entry+8 : entry+10])); o >= 1 && o <= 8 {
			return o
		}
		return 1
	}
	return 1
}

// orientate turns an image the way EXIF orientation o says to show it.
func orientate(src image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	// 5 to 8 swap the sides.
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2: // mirrored
				dx, dy = w-1-x, y
			case 3: // turned 180°
				dx, dy = w-1-x, h-1-y
			case 4: // mirrored, upside down
				dx, dy = x, h-1-y
			case 5: // mirrored, then turned
				dx, dy = y, x
			case 6: // turned 90° clockwise to view
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8: // turned 90° anticlockwise to view
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
