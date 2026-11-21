package udf

import (
	"fmt"
	"io"
	"sort"
)

// ExtentReader reads a sequence of extents of an image as one run of bytes:
// a file of the volume, or several of them end to end, without copying
// anything out of the image.
//
// It is an io.ReaderAt, and as safe for concurrent use as the reader under it.
type ExtentReader struct {
	r       io.ReaderAt
	extents []Extent
	// starts[i] is where extents[i] begins in the run; one entry more than
	// there are extents, the last being the whole length.
	starts []int64
}

// NewExtentReader reads the given extents of r, in order, as one run.
func NewExtentReader(r io.ReaderAt, extents []Extent) *ExtentReader {
	starts := make([]int64, len(extents)+1)
	for i, e := range extents {
		starts[i+1] = starts[i] + e.Length
	}
	return &ExtentReader{r: r, extents: extents, starts: starts}
}

// Size is the length of the run in bytes.
func (e *ExtentReader) Size() int64 { return e.starts[len(e.extents)] }

// ReadAt follows io.ReaderAt: it fills p from off, and returns io.EOF with
// whatever it read when the run ends first.
func (e *ExtentReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("udf: read at negative offset %d", off)
	}
	if off >= e.Size() {
		return 0, io.EOF
	}
	// The extent holding off: the last whose start is not past it.
	i := sort.Search(len(e.extents), func(i int) bool { return e.starts[i+1] > off })

	read := 0
	for read < len(p) && i < len(e.extents) {
		ext := e.extents[i]
		within := off + int64(read) - e.starts[i]
		n := int(min(int64(len(p)-read), ext.Length-within))
		if n > 0 {
			if ext.Sparse {
				clear(p[read : read+n])
			} else if got, err := e.r.ReadAt(p[read:read+n], ext.Offset+within); got < n {
				if err == nil || err == io.EOF {
					// The image ended inside an extent it declared.
					err = io.ErrUnexpectedEOF
				}
				return read + got, err
			}
			read += n
		}
		i++
	}
	if read < len(p) {
		return read, io.EOF
	}
	return read, nil
}

// readExtents fills p from the start of a run of extents, which must hold at
// least len(p) bytes.
func readExtents(r io.ReaderAt, extents []Extent, p []byte) error {
	if len(p) == 0 {
		return nil
	}
	if _, err := NewExtentReader(r, extents).ReadAt(p, 0); err != nil {
		if err == io.EOF {
			return fmt.Errorf("%w: a structure shorter than it says", ErrCorrupt)
		}
		return fmt.Errorf("udf: reading the image: %w", err)
	}
	return nil
}
