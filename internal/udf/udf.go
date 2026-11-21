// Package udf reads the directory tree of a UDF volume: the filesystem inside
// a Blu-ray or DVD disc image.
//
// It answers two questions and nothing else: what is in this directory, and
// where in the image do this file's bytes lie. The second is the reason the
// package exists. A film inside a disc image is tens of gigabytes that are
// already stored, in order, inside a file the vault can read at any offset —
// so playing it is a matter of knowing which ranges of the image to hand
// over, not of copying anything out.
//
// What is read:
//
//   - UDF 1.02 to 2.60 on 2048-byte sectors, which is every optical disc image.
//   - Plain (type 1) partitions and the metadata partition a Blu-ray keeps its
//     directories in.
//   - Short, long and extended allocation descriptors, data embedded in the
//     file entry, and descriptors continued into further extents.
//
// What is not: virtual and sparable partitions (rewritable and write-once
// media, never a pressed disc or an image of one), strategy 4096 file
// entries, named streams and extended attributes.
//
// The image is untrusted input. Every length and location in it is checked
// against the size of the image before it is followed, and directories, the
// chains they are read through and the names in them are all bounded, so a
// crafted image costs a refused read rather than memory or time.
package udf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
)

// SectorSize is the sector and logical block size of every optical disc image.
const SectorSize = 2048

// ErrNotUDF is returned by Open for an image that holds no UDF volume.
var ErrNotUDF = errors.New("udf: not a UDF volume")

// ErrUnsupported is returned for a UDF volume laid out in a way this package
// does not read.
var ErrUnsupported = errors.New("udf: unsupported volume layout")

// ErrCorrupt is returned when the volume's own structures contradict
// themselves or point outside the image.
var ErrCorrupt = errors.New("udf: corrupt volume")

// ErrNotExist is returned by Lookup for a path the volume does not hold.
var ErrNotExist = errors.New("udf: no such file or directory")

// Bounds on what an image may ask this package to hold or to walk.
const (
	maxDirBytes   = 16 << 20 // one directory's records
	maxExtents    = 1 << 16  // one file's allocation descriptors
	maxADChain    = 4096     // continuation extents of one file's descriptors
	maxVDSSectors = 256      // volume descriptor sequence
	maxPartitions = 16       // partition maps of the logical volume
)

// Extent is a run of a file's bytes as they lie in the image.
type Extent struct {
	// Offset is where the run starts in the image, in bytes. It means nothing
	// when Sparse is set.
	Offset int64
	// Length is how many bytes the run holds.
	Length int64
	// Sparse marks a run that is not recorded in the image and reads as zeros.
	Sparse bool
}

// Entry is one file or directory of the volume.
type Entry struct {
	// Name is the entry's name in its directory, empty for the root.
	Name string
	// IsDir reports whether the entry is a directory.
	IsDir bool
	// Size is the length of the entry's contents in bytes.
	Size int64

	// icb is where the entry's file entry is recorded.
	icb longAD
}

// Volume is an open UDF volume. It is safe for concurrent use so long as the
// reader it was opened on is.
type Volume struct {
	r    io.ReaderAt
	size int64

	parts []partition
	root  longAD
}

// partition is one entry of the logical volume's partition map: a place that
// block numbers are counted from.
type partition struct {
	// start is the partition's first byte in the image, for a physical one.
	start int64
	// length is how many bytes the partition holds.
	length int64
	// meta is set for a metadata partition, whose blocks are the contents of
	// a file: runs of the physical partition, in order.
	meta []Extent
}

// Open reads the volume structures of the UDF image in r, which is size bytes
// long. Nothing but those structures is read: a few sectors, wherever the
// image keeps them.
func Open(r io.ReaderAt, size int64) (*Volume, error) {
	v := &Volume{r: r, size: size}

	vds, err := v.anchor()
	if err != nil {
		return nil, err
	}
	fileSet, err := v.readVolumeSequence(vds)
	if err != nil {
		return nil, err
	}

	// The file set descriptor names the root directory.
	block, err := v.readBlock(fileSet.part, fileSet.block)
	if err != nil {
		return nil, err
	}
	if err := checkTag(block, tagFileSet); err != nil {
		return nil, err
	}
	v.root = parseLongAD(block[400:])
	return v, nil
}

// Root returns the volume's root directory.
func (v *Volume) Root() Entry {
	return Entry{IsDir: true, icb: v.root}
}

// Lookup resolves a slash-separated path from the root. Names are matched
// without regard to case, as every system that reads these discs matches
// them; a path that is not there is ErrNotExist.
func (v *Volume) Lookup(path string) (Entry, error) {
	entry := v.Root()
	for _, name := range strings.Split(path, "/") {
		if name == "" {
			continue
		}
		if !entry.IsDir {
			return Entry{}, ErrNotExist
		}
		children, err := v.ReadDir(entry)
		if err != nil {
			return Entry{}, err
		}
		found := false
		for _, child := range children {
			if strings.EqualFold(child.Name, name) {
				entry, found = child, true
				break
			}
		}
		if !found {
			return Entry{}, ErrNotExist
		}
	}
	return entry, nil
}

// ReadDir lists a directory, in the order the volume records it. Deleted
// entries and the entry for the parent are left out.
func (v *Volume) ReadDir(dir Entry) ([]Entry, error) {
	if !dir.IsDir {
		return nil, fmt.Errorf("udf: %q is not a directory", dir.Name)
	}
	fe, err := v.fileEntry(dir.icb)
	if err != nil {
		return nil, err
	}
	if fe.size > maxDirBytes {
		return nil, fmt.Errorf("%w: a directory of %d bytes", ErrUnsupported, fe.size)
	}
	data := make([]byte, fe.size)
	if err := readExtents(v.r, fe.extents, data); err != nil {
		return nil, err
	}

	var out []Entry
	for len(data) >= fidFixed {
		// Zeros where a record would begin are the rest of a block some
		// writers count into the directory's length: the end of the listing,
		// not damage.
		if binary.LittleEndian.Uint16(data) == 0 {
			break
		}
		if err := checkTag(data, tagFileIdentifier); err != nil {
			return nil, err
		}
		flags := data[18]
		nameLen := int(data[19])
		icb := parseLongAD(data[20:])
		useLen := int(binary.LittleEndian.Uint16(data[36:]))
		total := (fidFixed + useLen + nameLen + 3) &^ 3
		if total > len(data) {
			return nil, fmt.Errorf("%w: a directory record runs past its directory", ErrCorrupt)
		}
		name := decodeName(data[fidFixed+useLen : fidFixed+useLen+nameLen])
		data = data[total:]

		const deleted, parent = 1 << 2, 1 << 3
		if flags&(deleted|parent) != 0 || name == "" {
			continue
		}
		// The size is the file entry's to say, and one more read each: asked
		// for here so that a listing is complete, which for a disc's handful
		// of small directories is what every caller wants.
		child, err := v.fileEntry(icb)
		if err != nil {
			return nil, err
		}
		out = append(out, Entry{Name: name, IsDir: child.dir, Size: child.size, icb: icb})
	}
	return out, nil
}

// Extents reports where an entry's contents lie in the image, in order. The
// lengths add up to the entry's Size.
func (v *Volume) Extents(e Entry) ([]Extent, error) {
	fe, err := v.fileEntry(e.icb)
	if err != nil {
		return nil, err
	}
	return fe.extents, nil
}

// ReadFile returns an entry's contents, refusing one longer than limit bytes.
func (v *Volume) ReadFile(e Entry, limit int64) ([]byte, error) {
	fe, err := v.fileEntry(e.icb)
	if err != nil {
		return nil, err
	}
	if fe.size > limit {
		return nil, fmt.Errorf("udf: %q is %d bytes, more than the %d allowed", e.Name, fe.size, limit)
	}
	data := make([]byte, fe.size)
	if err := readExtents(v.r, fe.extents, data); err != nil {
		return nil, err
	}
	return data, nil
}

// --- Volume structures ---------------------------------------------------

// Descriptor tag identifiers (ECMA-167 3/7.2.1 and 4/7.2.1).
const (
	tagAnchor           = 2
	tagVolumePointer    = 3
	tagPartition        = 5
	tagLogicalVolume    = 6
	tagTerminating      = 8
	tagFileSet          = 256
	tagFileIdentifier   = 257
	tagAllocationExtent = 258
	tagFileEntry        = 261
	tagExtendedFile     = 266
)

// fidFixed is the length of a file identifier descriptor before its
// implementation use and name.
const fidFixed = 38

// checkTag verifies that b opens with a descriptor tag of the wanted kind.
// The tag's own checksum is verified; the CRC over the descriptor body is
// not, because writers disagree about how much of the body it covers and a
// reader that insisted would refuse discs every player accepts.
func checkTag(b []byte, want uint16) error {
	if len(b) < 16 {
		return fmt.Errorf("%w: a descriptor cut short", ErrCorrupt)
	}
	if got := binary.LittleEndian.Uint16(b); got != want {
		return fmt.Errorf("%w: descriptor %d where %d was expected", ErrCorrupt, got, want)
	}
	var sum byte
	for i, c := range b[:16] {
		if i != 4 {
			sum += c
		}
	}
	if sum != b[4] {
		return fmt.Errorf("%w: descriptor %d fails its checksum", ErrCorrupt, want)
	}
	return nil
}

// longAD is a long allocation descriptor: a run of blocks in a named partition.
type longAD struct {
	length uint32 // bytes, with the extent type in the top two bits
	block  uint32
	part   uint16
}

func parseLongAD(b []byte) longAD {
	return longAD{
		length: binary.LittleEndian.Uint32(b),
		block:  binary.LittleEndian.Uint32(b[4:]),
		part:   binary.LittleEndian.Uint16(b[8:]),
	}
}

// anchor finds the anchor volume descriptor pointer and returns the extent of
// the main volume descriptor sequence. The anchor is at sector 256, and again
// near the end of the volume for a disc whose start is unreadable.
func (v *Volume) anchor() (Extent, error) {
	last := v.size/SectorSize - 1
	sector := make([]byte, SectorSize)
	for _, at := range []int64{256, last - 256, last} {
		if at < 0 || (at+1)*SectorSize > v.size {
			continue
		}
		if _, err := v.r.ReadAt(sector, at*SectorSize); err != nil {
			return Extent{}, fmt.Errorf("udf: reading the volume anchor: %w", err)
		}
		if checkTag(sector, tagAnchor) != nil {
			continue
		}
		ext := Extent{
			Length: int64(binary.LittleEndian.Uint32(sector[16:])),
			Offset: int64(binary.LittleEndian.Uint32(sector[20:])) * SectorSize,
		}
		if err := v.inImage(ext.Offset, ext.Length); err != nil {
			return Extent{}, err
		}
		return ext, nil
	}
	return Extent{}, ErrNotUDF
}

// inImage checks that a run of bytes lies inside the image.
func (v *Volume) inImage(offset, length int64) error {
	if offset < 0 || length < 0 || offset > v.size || length > v.size-offset {
		return fmt.Errorf("%w: a structure points outside the image", ErrCorrupt)
	}
	return nil
}

// physical is a partition descriptor: where a numbered partition lies.
type physical struct {
	number        uint16
	start, length int64
}

// readVolumeSequence walks the volume descriptor sequence for the partitions
// and the logical volume, builds the partition map, and returns where the
// file set descriptor is.
func (v *Volume) readVolumeSequence(vds Extent) (longAD, error) {
	var (
		physicals []physical
		logical   []byte
	)
	sectors := vds.Length / SectorSize
	if sectors > maxVDSSectors {
		sectors = maxVDSSectors
	}
	at := vds.Offset
	for i := int64(0); i < sectors; i++ {
		sector := make([]byte, SectorSize)
		if _, err := v.r.ReadAt(sector, at); err != nil {
			return longAD{}, fmt.Errorf("udf: reading the volume descriptors: %w", err)
		}
		at += SectorSize

		done := false
		switch binary.LittleEndian.Uint16(sector) {
		case tagPartition:
			if checkTag(sector, tagPartition) != nil {
				continue
			}
			p := physical{
				number: binary.LittleEndian.Uint16(sector[22:]),
				start:  int64(binary.LittleEndian.Uint32(sector[188:])) * SectorSize,
				length: int64(binary.LittleEndian.Uint32(sector[192:])) * SectorSize,
			}
			if err := v.inImage(p.start, 0); err != nil {
				return longAD{}, err
			}
			// A partition may be declared longer than a truncated image; what
			// lies past the end is refused when something reads it.
			if p.length > v.size-p.start {
				p.length = v.size - p.start
			}
			physicals = append(physicals, p)
		case tagLogicalVolume:
			if checkTag(sector, tagLogicalVolume) == nil {
				logical = sector
			}
		case tagVolumePointer:
			// The sequence continues elsewhere.
			if checkTag(sector, tagVolumePointer) != nil {
				continue
			}
			next := Extent{
				Length: int64(binary.LittleEndian.Uint32(sector[20:])),
				Offset: int64(binary.LittleEndian.Uint32(sector[24:])) * SectorSize,
			}
			if next.Length == 0 {
				continue
			}
			if err := v.inImage(next.Offset, next.Length); err != nil {
				return longAD{}, err
			}
			at = next.Offset
		case tagTerminating, 0:
			done = true
		}
		if done {
			break
		}
	}
	if logical == nil || len(physicals) == 0 {
		return longAD{}, fmt.Errorf("%w: no logical volume or no partition", ErrCorrupt)
	}
	if bs := binary.LittleEndian.Uint32(logical[212:]); bs != SectorSize {
		return longAD{}, fmt.Errorf("%w: %d-byte blocks", ErrUnsupported, bs)
	}

	if err := v.buildPartitions(logical, physicals); err != nil {
		return longAD{}, err
	}
	// Logical volume contents use: the file set descriptor's extent.
	return parseLongAD(logical[248:]), nil
}

// buildPartitions turns the logical volume's partition maps into the table
// block addresses are resolved against.
func (v *Volume) buildPartitions(logical []byte, physicals []physical) error {
	tableLen := int(binary.LittleEndian.Uint32(logical[264:]))
	count := int(binary.LittleEndian.Uint32(logical[268:]))
	if count > maxPartitions || tableLen > len(logical)-440 {
		return fmt.Errorf("%w: a partition map of %d entries in %d bytes", ErrUnsupported, count, tableLen)
	}
	maps := logical[440 : 440+tableLen]

	find := func(number uint16) (physical, bool) {
		for _, p := range physicals {
			if p.number == number {
				return p, true
			}
		}
		return physical{}, false
	}

	// Metadata partitions are files inside a physical one, so they are filled
	// in on a second pass, once every physical partition is in the table.
	type pending struct {
		index    int
		physical int
		block    uint32
	}
	var metas []pending

	for i := 0; i < count; i++ {
		if len(maps) < 2 || int(maps[1]) < 2 || int(maps[1]) > len(maps) {
			return fmt.Errorf("%w: a partition map cut short", ErrCorrupt)
		}
		kind, length := maps[0], int(maps[1])
		entry := maps[:length]
		maps = maps[length:]

		switch {
		case kind == 1 && length >= 6:
			p, ok := find(binary.LittleEndian.Uint16(entry[4:]))
			if !ok {
				return fmt.Errorf("%w: a map names a partition that is not described", ErrCorrupt)
			}
			v.parts = append(v.parts, partition{start: p.start, length: p.length})

		case kind == 2 && length >= 64 && strings.HasPrefix(string(entry[5:28]), "*UDF Metadata Partition"):
			number := binary.LittleEndian.Uint16(entry[38:])
			// The map says which partition number the metadata file is in;
			// that number is found among the maps already read.
			phys := -1
			if p, ok := find(number); ok {
				for j, existing := range v.parts {
					if existing.meta == nil && existing.start == p.start {
						phys = j
					}
				}
			}
			if phys < 0 {
				return fmt.Errorf("%w: a metadata partition with no partition under it", ErrCorrupt)
			}
			metas = append(metas, pending{index: len(v.parts), physical: phys, block: binary.LittleEndian.Uint32(entry[40:])})
			v.parts = append(v.parts, partition{})

		default:
			id := ""
			if kind == 2 && length >= 28 {
				id = strings.TrimRight(string(entry[5:28]), "\x00")
			}
			return fmt.Errorf("%w: partition map type %d %s", ErrUnsupported, kind, id)
		}
	}

	for _, m := range metas {
		fe, err := v.fileEntry(longAD{block: m.block, part: uint16(m.physical)})
		if err != nil {
			return fmt.Errorf("udf: reading the metadata file: %w", err)
		}
		var total int64
		for _, e := range fe.extents {
			total += e.Length
		}
		v.parts[m.index] = partition{length: total, meta: fe.extents}
		if fe.extents == nil {
			return fmt.Errorf("%w: an empty metadata partition", ErrCorrupt)
		}
	}
	return nil
}

// resolve turns a run of bytes at a block of a partition into runs of the
// image. A run in a physical partition is one extent; one in a metadata
// partition is as many as the metadata file is in pieces.
func (v *Volume) resolve(part uint16, block uint32, length int64) ([]Extent, error) {
	if int(part) >= len(v.parts) {
		return nil, fmt.Errorf("%w: partition %d of %d", ErrCorrupt, part, len(v.parts))
	}
	p := v.parts[part]
	offset := int64(block) * SectorSize
	if length < 0 || offset > p.length || length > p.length-offset {
		return nil, fmt.Errorf("%w: an extent runs past its partition", ErrCorrupt)
	}
	if p.meta == nil {
		if err := v.inImage(p.start+offset, length); err != nil {
			return nil, err
		}
		return []Extent{{Offset: p.start + offset, Length: length}}, nil
	}

	var out []Extent
	for _, e := range p.meta {
		if length == 0 {
			break
		}
		if offset >= e.Length {
			offset -= e.Length
			continue
		}
		n := min(e.Length-offset, length)
		out = append(out, Extent{Offset: e.Offset + offset, Length: n, Sparse: e.Sparse})
		offset, length = 0, length-n
	}
	return out, nil
}

// readBlock reads one whole block of a partition.
func (v *Volume) readBlock(part uint16, block uint32) ([]byte, error) {
	extents, err := v.resolve(part, block, SectorSize)
	if err != nil {
		return nil, err
	}
	data := make([]byte, SectorSize)
	if err := readExtents(v.r, extents, data); err != nil {
		return nil, err
	}
	return data, nil
}

// --- File entries --------------------------------------------------------

// fileEntry is what a file entry says about its file.
type fileEntry struct {
	dir     bool
	size    int64
	extents []Extent
}

// fileEntry reads the file entry at an ICB and resolves its allocation
// descriptors into extents of the image.
func (v *Volume) fileEntry(icb longAD) (*fileEntry, error) {
	block, err := v.readBlock(icb.part, icb.block)
	if err != nil {
		return nil, err
	}

	// The two kinds of file entry differ only in where the fields sit.
	var eaLenAt, adLenAt, bodyAt int
	switch binary.LittleEndian.Uint16(block) {
	case tagFileEntry:
		eaLenAt, adLenAt, bodyAt = 168, 172, 176
	case tagExtendedFile:
		eaLenAt, adLenAt, bodyAt = 208, 212, 216
	default:
		return nil, fmt.Errorf("%w: descriptor %d where a file entry was expected",
			ErrCorrupt, binary.LittleEndian.Uint16(block))
	}
	if err := checkTag(block, binary.LittleEndian.Uint16(block)); err != nil {
		return nil, err
	}
	if strategy := binary.LittleEndian.Uint16(block[20:]); strategy != 4 {
		return nil, fmt.Errorf("%w: ICB strategy %d", ErrUnsupported, strategy)
	}

	fe := &fileEntry{dir: block[27] == 4}
	size := binary.LittleEndian.Uint64(block[56:])
	if size > 1<<62 {
		return nil, fmt.Errorf("%w: an impossible file length", ErrCorrupt)
	}
	fe.size = int64(size)

	eaLen := int64(binary.LittleEndian.Uint32(block[eaLenAt:]))
	adLen := int64(binary.LittleEndian.Uint32(block[adLenAt:]))
	adAt := int64(bodyAt) + eaLen
	if adAt+adLen > SectorSize {
		return nil, fmt.Errorf("%w: a file entry longer than its block", ErrCorrupt)
	}
	ads := block[adAt : adAt+adLen]

	adType := binary.LittleEndian.Uint16(block[34:]) & 7
	if adType == 3 {
		// The contents are in the file entry itself, where the descriptors
		// would be.
		if fe.size > adLen {
			return nil, fmt.Errorf("%w: embedded data shorter than its file", ErrCorrupt)
		}
		where, err := v.resolve(icb.part, icb.block, SectorSize)
		if err != nil {
			return nil, err
		}
		if fe.size > 0 {
			if len(where) != 1 {
				// One block is one run, unless the metadata file is in pieces
				// smaller than a block, which nothing writes.
				return nil, fmt.Errorf("%w: a file entry split across extents", ErrUnsupported)
			}
			fe.extents = []Extent{{Offset: where[0].Offset + adAt, Length: fe.size}}
		}
		return fe, nil
	}

	fe.extents, err = v.allocation(icb.part, adType, ads, fe.size)
	if err != nil {
		return nil, err
	}
	return fe, nil
}

// allocation resolves a file's allocation descriptors into extents covering
// exactly size bytes. part is the partition the file entry is in, which is
// the partition a short descriptor counts its blocks from.
func (v *Volume) allocation(part uint16, adType uint16, ads []byte, size int64) ([]Extent, error) {
	var width int
	switch adType {
	case 0:
		width = 8
	case 1:
		width = 16
	case 2:
		width = 20
	default:
		return nil, fmt.Errorf("%w: allocation descriptor type %d", ErrUnsupported, adType)
	}

	var out []Extent
	remaining := size
	chained := 0
	for len(ads) >= width && remaining > 0 {
		raw := binary.LittleEndian.Uint32(ads)
		kind, length := raw>>30, int64(raw&0x3fffffff)

		adPart, block := part, uint32(0)
		switch adType {
		case 0:
			block = binary.LittleEndian.Uint32(ads[4:])
		case 1:
			block = binary.LittleEndian.Uint32(ads[4:])
			adPart = binary.LittleEndian.Uint16(ads[8:])
		case 2:
			block = binary.LittleEndian.Uint32(ads[12:])
			adPart = binary.LittleEndian.Uint16(ads[16:])
		}
		ads = ads[width:]

		if length == 0 {
			break
		}
		if kind == 3 {
			// The descriptors carry on in another extent.
			if chained++; chained > maxADChain {
				return nil, fmt.Errorf("%w: an endless chain of allocation extents", ErrCorrupt)
			}
			next, err := v.readBlock(adPart, block)
			if err != nil {
				return nil, err
			}
			if err := checkTag(next, tagAllocationExtent); err != nil {
				return nil, err
			}
			n := int64(binary.LittleEndian.Uint32(next[20:]))
			if n > SectorSize-24 {
				return nil, fmt.Errorf("%w: an allocation extent longer than its block", ErrCorrupt)
			}
			ads = next[24 : 24+n]
			continue
		}

		// The last extent is recorded in whole blocks; the file ends inside it.
		length = min(length, remaining)
		remaining -= length
		if kind == 0 {
			runs, err := v.resolve(adPart, block, length)
			if err != nil {
				return nil, err
			}
			out = append(out, runs...)
		} else {
			// Allocated but never written, or not allocated at all: zeros.
			out = append(out, Extent{Length: length, Sparse: true})
		}
		if len(out) > maxExtents {
			return nil, fmt.Errorf("%w: a file in more than %d extents", ErrUnsupported, maxExtents)
		}
	}
	if remaining != 0 {
		return nil, fmt.Errorf("%w: a file's extents fall %d bytes short of its length", ErrCorrupt, remaining)
	}
	return out, nil
}

// decodeName reads a file identifier: OSTA compressed Unicode, which is one
// byte saying whether the rest is 8-bit or 16-bit characters.
func decodeName(b []byte) string {
	if len(b) < 2 {
		return ""
	}
	switch b[0] {
	case 8:
		runes := make([]rune, len(b)-1)
		for i, c := range b[1:] {
			runes[i] = rune(c)
		}
		return string(runes)
	case 16:
		units := make([]uint16, (len(b)-1)/2)
		for i := range units {
			units[i] = binary.BigEndian.Uint16(b[1+2*i:])
		}
		return string(utf16.Decode(units))
	}
	return ""
}
