// Package udftest builds small UDF disc images in memory, for tests of
// anything that reads one.
//
// The images are laid out the way a Blu-ray is: UDF 2.50, with directories
// and file entries in a metadata partition and file contents in the physical
// partition under it. Options switch on the variations a reader has to cope
// with — a plain partition, files in several extents, descriptors continued
// into another block, contents embedded in the file entry, a metadata file
// in pieces.
//
// It writes only what a reader follows, plus enough for other tools to accept
// the image (7-Zip lists and extracts what Build makes, which is how the
// writer was checked against something other than the reader it tests).
package udftest

import (
	"encoding/binary"
	"path"
	"sort"
	"strings"
	"unicode/utf16"
)

const sector = 2048

// Options chooses how an image is laid out.
type Options struct {
	// Plain leaves out the metadata partition and writes the older kind of
	// file entry: a UDF 1.02 volume, as a DVD is.
	Plain bool
	// ExtentBlocks, when positive, records file contents in extents of at
	// most this many blocks, with a block of junk between one and the next.
	ExtentBlocks int
	// ChainAfter, when positive, is how many allocation descriptors a file
	// entry holds before the rest are continued into another block.
	ChainAfter int
	// Embed records contents short enough to fit inside their file entry.
	Embed bool
	// SplitMetadata records the metadata file in two pieces.
	SplitMetadata bool
	// PadDirectories counts a run of zeros after the last record into each
	// directory's length, as some writers do.
	PadDirectories bool
}

// Build returns a UDF image holding the given files, keyed by slash-separated
// path. Directories are made as the paths need them; a path ending in a slash
// makes an empty directory.
func Build(files map[string][]byte, o Options) []byte {
	b := &builder{o: o}
	return b.build(files)
}

type node struct {
	name     string
	dir      bool
	data     []byte
	children []*node
	parent   *node

	icb uint32 // block of its file entry, in the metadata partition
}

type builder struct {
	o    Options
	phys [][]byte // blocks of the physical partition
	meta [][]byte // blocks of the metadata partition (the physical one when Plain)
}

// metaPart is the partition reference of directories and file entries.
func (b *builder) metaPart() uint16 {
	if b.o.Plain {
		return 0
	}
	return 1
}

func (b *builder) allocMeta() uint32 {
	if b.o.Plain {
		return b.allocPhys()
	}
	b.meta = append(b.meta, make([]byte, sector))
	return uint32(len(b.meta) - 1)
}

func (b *builder) metaBlock(n uint32) []byte {
	if b.o.Plain {
		return b.phys[n]
	}
	return b.meta[n]
}

func (b *builder) allocPhys() uint32 {
	b.phys = append(b.phys, make([]byte, sector))
	return uint32(len(b.phys) - 1)
}

func (b *builder) build(files map[string][]byte) []byte {
	root := &node{dir: true}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		at := root
		parts := strings.Split(strings.Trim(path.Clean("/"+p), "/"), "/")
		for i, name := range parts {
			last := i == len(parts)-1
			var next *node
			for _, c := range at.children {
				if c.name == name {
					next = c
				}
			}
			if next == nil {
				next = &node{name: name, dir: !last || strings.HasSuffix(p, "/"), parent: at}
				at.children = append(at.children, next)
			}
			if last && !next.dir {
				next.data = files[p]
			}
			at = next
		}
	}

	// Physical block 0 is the metadata file's entry; metadata block 0 is the
	// file set descriptor. Plain volumes have only the second.
	metaICB := uint32(0)
	if !b.o.Plain {
		metaICB = b.allocPhys()
	}
	fsd := b.allocMeta()

	b.assign(root)
	b.write(root)

	fs := b.metaBlock(fsd)
	putLongAD(fs[400:], sector, root.icb, b.metaPart())
	putRegid(fs[416:], "*OSTA UDF Compliant", 0x50, 0x02)
	putTag(fs, 256, fsd, 512-16)

	// The metadata partition's blocks go into the physical partition last,
	// once everything that had to know a physical address has been written.
	if !b.o.Plain {
		var ads []byte
		half := len(b.meta)
		if b.o.SplitMetadata && len(b.meta) > 1 {
			half = len(b.meta) / 2
		}
		for start := 0; start < len(b.meta); {
			end := len(b.meta)
			if start < half {
				end = half
			}
			first := uint32(len(b.phys))
			b.phys = append(b.phys, b.meta[start:end]...)
			ads = appendShortAD(ads, uint32(end-start)*sector, first)
			if end < len(b.meta) {
				junk := b.allocPhys()
				fill(b.phys[junk], 0xEE)
			}
			start = end
		}
		b.fileEntry(b.phys[metaICB], metaICB, 250, uint64(len(b.meta))*sector, 0, ads)
	}

	return b.volume()
}

// assign gives every node the block its file entry will be written in, so
// that directories can name their children before those are written.
func (b *builder) assign(n *node) {
	n.icb = b.allocMeta()
	for _, c := range n.children {
		b.assign(c)
	}
}

func (b *builder) write(n *node) {
	if !n.dir {
		b.writeFile(n)
		return
	}

	parent := n.parent
	if parent == nil {
		parent = n
	}
	data := fid(0, "", 0x0A, parent.icb, b.metaPart())
	for _, c := range n.children {
		flags := byte(0)
		if c.dir {
			flags = 0x02
		}
		data = append(data, fid(0, c.name, flags, c.icb, b.metaPart())...)
	}

	if b.o.PadDirectories {
		data = append(data, make([]byte, 100)...)
	}

	block := b.metaBlock(n.icb)
	if b.o.Embed && len(data) <= sector-b.headerLen() {
		b.fileEntry(block, n.icb, 4, uint64(len(data)), 3, data)
	} else {
		// Directory contents live with the directory, in the metadata
		// partition, and are addressed from it by short descriptors.
		blocks := (len(data) + sector - 1) / sector
		first := uint32(0)
		for i := 0; i < blocks; i++ {
			at := b.allocMeta()
			if i == 0 {
				first = at
			}
			copy(b.metaBlock(at), data[i*sector:])
		}
		b.fileEntry(block, n.icb, 4, uint64(len(data)), 0, appendShortAD(nil, uint32(len(data)), first))
	}
	for _, c := range n.children {
		b.write(c)
	}
}

func (b *builder) writeFile(n *node) {
	block := b.metaBlock(n.icb)
	if b.o.Embed && len(n.data) <= sector-b.headerLen() {
		b.fileEntry(block, n.icb, 5, uint64(len(n.data)), 3, n.data)
		return
	}

	per := b.o.ExtentBlocks
	if per <= 0 {
		per = 1 << 18 // under the 30 bits an extent's length has
	}
	var ads [][]byte
	for rest := n.data; len(rest) > 0; {
		take := min(len(rest), per*sector)
		first := uint32(len(b.phys))
		for off := 0; off < take; off += sector {
			copy(b.phys[b.allocPhys()], rest[off:take])
		}
		ad := make([]byte, 16)
		putLongAD(ad, uint32(take), first, 0)
		ads = append(ads, ad)
		rest = rest[take:]
		if len(rest) > 0 && b.o.ExtentBlocks > 0 {
			fill(b.phys[b.allocPhys()], 0xEE)
		}
	}

	// Descriptors past ChainAfter are continued into allocation extents, each
	// holding the same number again.
	var inline []byte
	if c := b.o.ChainAfter; c > 0 && len(ads) > c {
		rest := ads[c:]
		ads = append([][]byte{}, ads[:c]...)
		next := b.allocMeta()
		link := make([]byte, 16)
		putLongAD(link, 3<<30|sector, next, b.metaPart())
		ads = append(ads, link)
		for len(rest) > 0 {
			take := min(len(rest), c)
			body := []byte{}
			for _, ad := range rest[:take] {
				body = append(body, ad...)
			}
			rest = rest[take:]
			this := next
			if len(rest) > 0 {
				next = b.allocMeta()
				link := make([]byte, 16)
				putLongAD(link, 3<<30|sector, next, b.metaPart())
				body = append(body, link...)
			}
			aed := b.metaBlock(this)
			binary.LittleEndian.PutUint32(aed[20:], uint32(len(body)))
			copy(aed[24:], body)
			putTag(aed, 258, this, 8+len(body))
		}
	}
	for _, ad := range ads {
		inline = append(inline, ad...)
	}
	b.fileEntry(block, n.icb, 5, uint64(len(n.data)), 1, inline)
}

// headerLen is the length of a file entry before its allocation descriptors.
func (b *builder) headerLen() int {
	if b.o.Plain {
		return 176
	}
	return 216
}

// fileEntry writes a file entry into block: the extended kind, or the older
// kind on a plain volume.
func (b *builder) fileEntry(block []byte, loc uint32, fileType byte, size uint64, adType uint16, ads []byte) {
	h := b.headerLen()
	if h+len(ads) > sector {
		panic("udftest: allocation descriptors do not fit their file entry; set ChainAfter")
	}
	binary.LittleEndian.PutUint16(block[20:], 4)      // strategy
	binary.LittleEndian.PutUint16(block[24:], 1)      // entries
	block[27] = fileType                              //
	binary.LittleEndian.PutUint16(block[34:], adType) // ICB flags
	binary.LittleEndian.PutUint32(block[44:], 0x14A5) // permissions
	binary.LittleEndian.PutUint16(block[48:], 1)      // links
	binary.LittleEndian.PutUint64(block[56:], size)   // information length
	id := uint16(266)
	if b.o.Plain {
		id = 261
		binary.LittleEndian.PutUint64(block[64:], (size+sector-1)/sector)
		putRegid(block[128:], "*udftest", 0, 0)
		binary.LittleEndian.PutUint32(block[172:], uint32(len(ads)))
	} else {
		binary.LittleEndian.PutUint64(block[64:], size) // object size
		binary.LittleEndian.PutUint64(block[72:], (size+sector-1)/sector)
		putRegid(block[168:], "*udftest", 0, 0)
		binary.LittleEndian.PutUint32(block[212:], uint32(len(ads)))
	}
	copy(block[h:], ads)
	putTag(block, id, loc, h+len(ads)-16)
}

// volume wraps the physical partition in the volume structures.
func (b *builder) volume() []byte {
	const partStart = 272
	total := partStart + len(b.phys) + 257
	img := make([]byte, total*sector)
	for i, blk := range b.phys {
		copy(img[(partStart+i)*sector:], blk)
	}

	// Volume recognition sequence.
	nsr := "NSR03"
	if b.o.Plain {
		nsr = "NSR02"
	}
	for i, id := range []string{"BEA01", nsr, "TEA01"} {
		at := (16 + i) * sector
		copy(img[at+1:], id)
		img[at+6] = 1
	}

	// Volume descriptor sequence at sector 32: primary, partition, logical,
	// terminator.
	desc := func(n int) []byte { return img[(32+n)*sector : (33+n)*sector] }

	pvd := desc(0)
	binary.LittleEndian.PutUint32(pvd[16:], 0)
	putDstring(pvd[24:56], "UDFTEST")
	binary.LittleEndian.PutUint16(pvd[56:], 1)
	binary.LittleEndian.PutUint16(pvd[58:], 1)
	binary.LittleEndian.PutUint16(pvd[60:], 2)
	binary.LittleEndian.PutUint16(pvd[62:], 2)
	binary.LittleEndian.PutUint32(pvd[64:], 1)
	binary.LittleEndian.PutUint32(pvd[68:], 1)
	putDstring(pvd[72:200], "UDFTEST")
	pvd[200], pvd[264] = 0, 0
	copy(pvd[201:], "OSTA Compressed Unicode")
	copy(pvd[265:], "OSTA Compressed Unicode")
	putTag(pvd, 1, 32, 512-16)

	pd := desc(1)
	binary.LittleEndian.PutUint32(pd[16:], 1)
	binary.LittleEndian.PutUint16(pd[20:], 1)
	binary.LittleEndian.PutUint16(pd[22:], 0)
	putRegid(pd[24:], "+"+nsr, 0, 0)
	binary.LittleEndian.PutUint32(pd[184:], 1) // read only
	binary.LittleEndian.PutUint32(pd[188:], partStart)
	binary.LittleEndian.PutUint32(pd[192:], uint32(len(b.phys)))
	putTag(pd, 5, 33, 512-16)

	lvd := desc(2)
	binary.LittleEndian.PutUint32(lvd[16:], 2)
	copy(lvd[21:], "OSTA Compressed Unicode")
	putDstring(lvd[84:212], "UDFTEST")
	binary.LittleEndian.PutUint32(lvd[212:], sector)
	rev := byte(0x50)
	if b.o.Plain {
		rev = 0x02
	}
	putRegid(lvd[216:], "*OSTA UDF Compliant", rev, map[bool]byte{true: 0x01, false: 0x02}[b.o.Plain])
	// The file set descriptor: block 0 of the metadata partition, or of the
	// only one.
	putLongAD(lvd[248:], sector, 0, b.metaPart())
	maps := []byte{1, 6, 1, 0, 0, 0}
	if !b.o.Plain {
		m := make([]byte, 64)
		m[0], m[1] = 2, 64
		putRegid(m[4:], "*UDF Metadata Partition", 0x50, 0x02)
		binary.LittleEndian.PutUint16(m[36:], 1)
		binary.LittleEndian.PutUint16(m[38:], 0)
		binary.LittleEndian.PutUint32(m[40:], 0)          // metadata file
		binary.LittleEndian.PutUint32(m[44:], 0xFFFFFFFF) // no mirror
		binary.LittleEndian.PutUint32(m[48:], 0xFFFFFFFF) // no bitmap
		binary.LittleEndian.PutUint32(m[52:], 32)
		binary.LittleEndian.PutUint16(m[56:], 32)
		maps = append(maps, m...)
	}
	binary.LittleEndian.PutUint32(lvd[264:], uint32(len(maps)))
	binary.LittleEndian.PutUint32(lvd[268:], uint32(1+len(maps)/64))
	putRegid(lvd[272:], "*udftest", 0, 0)
	copy(lvd[440:], maps)
	putTag(lvd, 6, 34, 440+len(maps)-16)

	putTag(desc(3), 8, 35, 512-16)

	// Anchors at sector 256 and the last sector.
	for _, at := range []int{256, total - 1} {
		avdp := img[at*sector : (at+1)*sector]
		binary.LittleEndian.PutUint32(avdp[16:], 16*sector)
		binary.LittleEndian.PutUint32(avdp[20:], 32)
		binary.LittleEndian.PutUint32(avdp[24:], 16*sector)
		binary.LittleEndian.PutUint32(avdp[28:], 32)
		putTag(avdp, 2, uint32(at), 512-16)
	}
	return img
}

// --- Encoding ------------------------------------------------------------

// fid encodes one directory record.
func fid(loc uint32, name string, flags byte, icb uint32, part uint16) []byte {
	id := encodeName(name)
	out := make([]byte, (38+len(id)+3)&^3)
	binary.LittleEndian.PutUint16(out[16:], 1)
	out[18] = flags
	out[19] = byte(len(id))
	putLongAD(out[20:], sector, icb, part)
	copy(out[38:], id)
	putTag(out, 257, loc, len(out)-16)
	return out
}

// encodeName writes a name as OSTA compressed Unicode: 8-bit when every
// character fits, 16-bit otherwise.
func encodeName(name string) []byte {
	if name == "" {
		return nil
	}
	wide := false
	for _, r := range name {
		if r > 0xFF {
			wide = true
		}
	}
	if !wide {
		out := []byte{8}
		for _, r := range name {
			out = append(out, byte(r))
		}
		return out
	}
	out := []byte{16}
	for _, u := range utf16.Encode([]rune(name)) {
		out = append(out, byte(u>>8), byte(u))
	}
	return out
}

func putLongAD(b []byte, length, block uint32, part uint16) {
	binary.LittleEndian.PutUint32(b, length)
	binary.LittleEndian.PutUint32(b[4:], block)
	binary.LittleEndian.PutUint16(b[8:], part)
}

func appendShortAD(b []byte, length, block uint32) []byte {
	b = binary.LittleEndian.AppendUint32(b, length)
	return binary.LittleEndian.AppendUint32(b, block)
}

func putRegid(b []byte, id string, suffix0, suffix1 byte) {
	copy(b[1:24], id)
	b[24], b[25] = suffix0, suffix1
}

// putDstring writes a fixed-width string field: compression ID, characters,
// and the used length in the field's last byte.
func putDstring(field []byte, s string) {
	field[0] = 8
	copy(field[1:], s)
	field[len(field)-1] = byte(len(s) + 1)
}

// putTag writes a descriptor tag over the first 16 bytes of b, with the CRC
// taken over the crcLen bytes after it.
func putTag(b []byte, id uint16, loc uint32, crcLen int) {
	binary.LittleEndian.PutUint16(b[0:], id)
	binary.LittleEndian.PutUint16(b[2:], 3)
	b[4], b[5] = 0, 0
	binary.LittleEndian.PutUint16(b[6:], 1)
	binary.LittleEndian.PutUint16(b[8:], crc(b[16:16+crcLen]))
	binary.LittleEndian.PutUint16(b[10:], uint16(crcLen))
	binary.LittleEndian.PutUint32(b[12:], loc)
	var sum byte
	for i, c := range b[:16] {
		if i != 4 {
			sum += c
		}
	}
	b[4] = sum
}

// crc is the CRC-16 a descriptor tag carries: CCITT, x^16+x^12+x^5+1.
func crc(data []byte) uint16 {
	var c uint16
	for _, d := range data {
		c ^= uint16(d) << 8
		for i := 0; i < 8; i++ {
			if c&0x8000 != 0 {
				c = c<<1 ^ 0x1021
			} else {
				c <<= 1
			}
		}
	}
	return c
}

func fill(b []byte, with byte) {
	for i := range b {
		b[i] = with
	}
}
