package bluray

import (
	"fmt"
	"io"
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
		for i := 0; i < count && read < len(p); i++ {
			body := span[i*sourcePacket+packetExtra : (i+1)*sourcePacket]
			read += copy(p[read:], body[within:])
			within = 0
		}
	}
	return read, nil
}
