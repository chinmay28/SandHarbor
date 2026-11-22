package bluray

import (
	"fmt"
	"io"
	"sort"
	"sync"
)

// A Blu-ray clip is an MPEG transport stream with four bytes in front of every
// packet: 192 bytes where a transport stream has 188. The four are a copy
// permission flag and the time the packet is due off the disc, which a drive's
// read-ahead needs and nothing reading a file does.
//
// They are also the whole difference between a file every player opens and one
// several do not. A player that looks for a sync byte every 188 bytes finds
// none in a clip, and gives up before it has read a frame — Android's own
// media library among them, which is what SAND TV plays through. So the
// stream handed out is the clip with those four bytes left out of each packet.
// Nothing is decoded or rewritten: byte n of the stream is a fixed place in
// the clip, which is what lets a seek be answered without reading anything
// before it.
const (
	sourcePacket = 192 // a packet as a Blu-ray clip stores it
	tsPacket     = 188 // a packet of an ordinary transport stream
	packetExtra  = sourcePacket - tsPacket
	syncByte     = 0x47
)

// spanPackets is how many packets one read of the source covers at most: the
// size of the buffer a read works in, about 190 KB.
const spanPackets = 1024

var spanPool = sync.Pool{New: func() any {
	b := make([]byte, spanPackets*sourcePacket)
	return &b
}}

// TSReader reads Blu-ray clips as a plain MPEG transport stream of 188-byte
// packets. It is an io.ReaderAt, and as safe for concurrent use as the reader
// it was made over.
type TSReader struct {
	src     io.ReaderAt
	packets int64
	// shifts moves the clocks of the packets from each one's first packet
	// on, in the order of first; empty when no packet's clock is moved.
	shifts []clockShift
}

// clockShift moves the clocks in a run of packets: every timestamp in the
// packets from first on is advanced by ticks of the 90 kHz clock, modulo the
// 33 bits a timestamp has.
type clockShift struct {
	first int64
	ticks uint64
}

// NewTSReader reads the 192-byte packets in the first size bytes of src as a
// transport stream. A trailing part of a packet is left out.
func NewTSReader(src io.ReaderAt, size int64) *TSReader {
	return &TSReader{src: src, packets: size / sourcePacket}
}

// Size is the length of the transport stream in bytes.
func (t *TSReader) Size() int64 { return t.packets * tsPacket }

// ReadAt follows io.ReaderAt: it fills p from off, and returns io.EOF with
// whatever it read when the stream ends first.
func (t *TSReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("bluray: read at negative offset %d", off)
	}
	bufp := spanPool.Get().(*[]byte)
	defer spanPool.Put(bufp)

	read := 0
	for read < len(p) {
		at := off + int64(read)
		packet, within := at/tsPacket, int(at%tsPacket)
		if packet >= t.packets {
			return read, io.EOF
		}
		// Enough whole packets to cover what is left of p, up to a span.
		want := (int64(within) + int64(len(p)-read) + tsPacket - 1) / tsPacket
		count := int(min(want, t.packets-packet, spanPackets))
		span := (*bufp)[:count*sourcePacket]
		if n, err := t.src.ReadAt(span, packet*sourcePacket); n < len(span) {
			if err == nil || err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return read, err
		}
		shift := t.shiftAt(packet)
		for i := 0; i < count && read < len(p); i++ {
			body := span[i*sourcePacket+packetExtra : (i+1)*sourcePacket]
			if shift >= 0 {
				// The next run's shift takes over at its first packet.
				for shift+1 < len(t.shifts) && t.shifts[shift+1].first <= packet+int64(i) {
					shift++
				}
				if ticks := t.shifts[shift].ticks; ticks != 0 {
					shiftClocks(body, ticks)
				}
			}
			read += copy(p[read:], body[within:])
			within = 0
		}
	}
	return read, nil
}

// shiftAt returns the index of the shift a packet falls under, or -1 when
// there is none.
func (t *TSReader) shiftAt(packet int64) int {
	// The last shift whose first packet is not past this one.
	return sort.Search(len(t.shifts), func(i int) bool { return t.shifts[i].first > packet }) - 1
}

// Making one clock out of several.
//
// Every clip on a disc keeps its own time: its timestamps start wherever its
// author left them, usually at the same ten minutes as every other clip's. A
// disc player knows where one clip ends and resets its clock for the next.
// A player reading the clips end to end as one file knows nothing of the
// kind: it sees a film whose clock jumps back to the start every few
// minutes, and takes the length of the film, and where to seek to in it,
// from a first and a last timestamp that have nothing to do with each other.
// Some players ride that out; Android's stops playing.
//
// So a title of several clips is sent with each clip's timestamps moved onto
// one line: where the playlist says the clip before it ended. Only the
// timestamps change, in place — the three a transport stream carries, each a
// fixed-width field at a place that can be found from the packet alone — so
// the stream stays the same length, and a seek is still arithmetic.

const tsClockMask = 1<<33 - 1

// shiftClocks advances the timestamps in one 188-byte packet by ticks: the
// clock reference in its adaptation field, and the presentation and decode
// times of an elementary stream packet that begins in it.
func shiftClocks(packet []byte, ticks uint64) {
	if packet[0] != syncByte {
		return
	}
	pid := int(packet[1]&0x1f)<<8 | int(packet[2])
	if pid == 0 || pid == 0x1fff {
		// The program table, and padding: neither carries a clock.
		return
	}
	hasAdaptation, hasPayload := packet[3]&0x20 != 0, packet[3]&0x10 != 0

	payload := 4
	if hasAdaptation {
		length := int(packet[4])
		payload = 5 + length
		if payload > tsPacket {
			return
		}
		// The clock reference: 33 bits of 90 kHz, six reserved, and nine of
		// a 27 MHz remainder that a shift by whole ticks leaves alone.
		if length >= 7 && packet[5]&0x10 != 0 {
			f := packet[6:12]
			base := uint64(f[0])<<25 | uint64(f[1])<<17 | uint64(f[2])<<9 | uint64(f[3])<<1 | uint64(f[4])>>7
			base = (base + ticks) & tsClockMask
			f[0], f[1], f[2], f[3] = byte(base>>25), byte(base>>17), byte(base>>9), byte(base>>1)
			f[4] = byte(base<<7) | f[4]&0x7f
		}
	}

	// An elementary stream packet starts here when the packet says a unit
	// starts in it and the payload opens with the start code. Its header
	// says which of the two times it carries.
	if !hasPayload || packet[1]&0x40 == 0 {
		return
	}
	pes := packet[payload:]
	if len(pes) < 14 || pes[0] != 0 || pes[1] != 0 || pes[2] != 1 || pes[6]&0xc0 != 0x80 {
		return
	}
	switch id := pes[3]; {
	case id == 0xbc, id == 0xbe, id == 0xbf, id == 0xf0, id == 0xf1, id == 0xf2, id == 0xf8, id == 0xff:
		// Stream kinds whose packets have no header to carry a time.
		return
	}
	flags, headerLen := pes[7]>>6, int(pes[8])
	if flags&2 != 0 && headerLen >= 5 {
		shiftPESTime(pes[9:14], ticks)
		if flags == 3 && headerLen >= 10 && len(pes) >= 19 {
			shiftPESTime(pes[14:19], ticks)
		}
	}
}

// shiftPESTime advances one presentation or decode time: 33 bits spread over
// five bytes between marker bits, which are left as they are.
func shiftPESTime(f []byte, ticks uint64) {
	t := uint64(f[0]>>1&7)<<30 | uint64(f[1])<<22 | uint64(f[2]>>1)<<15 | uint64(f[3])<<7 | uint64(f[4]>>1)
	t = (t + ticks) & tsClockMask
	f[0] = f[0]&0xf1 | byte(t>>30)<<1
	f[1] = byte(t >> 22)
	f[2] = f[2]&1 | byte(t>>15)<<1
	f[3] = byte(t >> 7)
	f[4] = f[4]&1 | byte(t)<<1
}
