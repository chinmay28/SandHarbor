// Package bluraytest builds small Blu-ray disc images in memory, for tests of
// anything that plays one.
package bluraytest

import (
	"encoding/binary"
	"fmt"
	"math/rand"

	"github.com/chinmay28/SandHarbor/internal/udf/udftest"
)

// Item is one play item of a playlist: a clip by number, and how long the
// playlist plays it for, in seconds.
type Item struct {
	Clip    int
	Seconds int
	// In is the time on the clip's own clock that the item starts at, in
	// seconds; zero means the ten minutes most clips on a disc start at.
	In int
	// Streams is the item's stream table: what is in the clip, and in which
	// language. Empty writes an item without one.
	Streams []Stream
}

// Stream is one stream a play item lists.
type Stream struct {
	PID int
	// Coding is the kind of stream as a disc codes it: 0x1b H.264, 0x24
	// HEVC, 0x81 AC-3, 0x86 DTS-HD MA, 0x90 picture subtitles, and so on.
	Coding byte
	// Language is its three-letter language, for sound and subtitles.
	Language string
}

// Disc is the contents of a disc image: clips and playlists by number, and
// any other files by path.
type Disc struct {
	// Clips are the BDMV/STREAM files: clip number → its bytes, which Clip
	// makes.
	Clips map[int][]byte
	// Playlists are the BDMV/PLAYLIST files: playlist number → its items.
	Playlists map[int][]Item
	// Files are added as they are, by path.
	Files map[string][]byte
	// Layout is how the image is laid out; the zero value is a Blu-ray's.
	Layout udftest.Options
}

// Image returns the disc as a UDF image.
func (d Disc) Image() []byte {
	files := map[string][]byte{"BDMV/index.bdmv": []byte("INDX0200")}
	for n, clip := range d.Clips {
		files[fmt.Sprintf("BDMV/STREAM/%05d.m2ts", n)] = clip
	}
	for n, items := range d.Playlists {
		files[fmt.Sprintf("BDMV/PLAYLIST/%05d.mpls", n)] = Playlist(items...)
	}
	for path, data := range d.Files {
		files[path] = data
	}
	return udftest.Build(files, d.Layout)
}

// Clip makes a clip of the given number of packets, and the transport stream
// a player should be handed for it. The packets carry no picture and no
// clock: each is the header of a packet from the middle of a stream and bytes
// that depend on seed, behind the four-byte header a Blu-ray puts in front
// of every packet. TimedClip makes one whose packets do carry a clock.
func Clip(seed int64, packets int) (m2ts, ts []byte) {
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < packets; i++ {
		header := make([]byte, 4)
		binary.BigEndian.PutUint32(header, uint32(i)*1000&0x3fffffff)
		body := make([]byte, 188)
		rng.Read(body)
		body[0] = 0x47
		body[1] &^= 0x40              // no unit starts here
		body[3] = 0x10 | byte(i)&0x0f // payload only
		m2ts = append(append(m2ts, header...), body...)
		ts = append(ts, body...)
	}
	return m2ts, ts
}

// TickRate is how many ticks of a transport stream's clock make a second.
const TickRate = 90000

// frameTicks is how far apart TimedClip's packets are in time.
const frameTicks = 3003

// TimedClip makes a clip whose every packet carries all three of a transport
// stream's timestamps — a clock reference, and the presentation and decode
// times of an elementary stream packet starting in it — counting up from
// start, in ticks. Its last packet's times are start plus Span(packets).
func TimedClip(start uint64, packets int) (m2ts, ts []byte) {
	for i := 0; i < packets; i++ {
		now := (start + uint64(i)*frameTicks) & (1<<33 - 1)
		body := make([]byte, 188)
		body[0] = 0x47
		body[1], body[2] = 0x40|0x10, 0x11 // a unit starts; PID 0x1011
		body[3] = 0x30 | byte(i)&0x0f      // adaptation field and payload
		body[4] = 7                        // adaptation field: flags and a clock reference
		body[5] = 0x10
		body[6], body[7], body[8], body[9] = byte(now>>25), byte(now>>17), byte(now>>9), byte(now>>1)
		body[10] = byte(now<<7) | 0x7e
		body[11] = 0x55 // the 27 MHz remainder, which nothing should touch

		pes := body[12:]
		pes[2], pes[3] = 1, 0xe0
		pes[6], pes[7], pes[8] = 0x80, 0xc0, 10
		putPESTime(pes[9:], 0x3, now+2*frameTicks) // presented two frames after it is decoded
		putPESTime(pes[14:], 0x1, now)
		for j := 19; j < len(pes); j++ {
			pes[j] = byte(i + j)
		}

		header := make([]byte, 4)
		binary.BigEndian.PutUint32(header, uint32(i)*1000&0x3fffffff)
		m2ts = append(append(m2ts, header...), body...)
		ts = append(ts, body...)
	}
	return m2ts, ts
}

// Span is how many ticks pass between the first and last packets of a
// TimedClip of the given length.
func Span(packets int) uint64 { return uint64(packets-1) * frameTicks }

func putPESTime(f []byte, prefix byte, t uint64) {
	t &= 1<<33 - 1
	f[0] = prefix<<4 | byte(t>>30)<<1 | 1
	f[1] = byte(t >> 22)
	f[2] = byte(t>>15)<<1 | 1
	f[3] = byte(t >> 7)
	f[4] = byte(t)<<1 | 1
}

// Encrypt scrambles a clip the way an undecrypted disc is: in units of 32
// packets, everything after the first sixteen bytes of each unit.
func Encrypt(m2ts []byte) []byte {
	out := append([]byte{}, m2ts...)
	for unit := 0; unit < len(out); unit += 6144 {
		for i := unit + 16; i < unit+6144 && i < len(out); i++ {
			out[i] ^= 0xA5
		}
	}
	return out
}

// streamTable encodes a play item's stream table: video, then sound, then
// picture subtitles, as a disc orders them.
func streamTable(streams []Stream) []byte {
	kind := func(s Stream) int {
		switch {
		case s.Coding == 0x90:
			return 2
		case s.Coding >= 0x80:
			return 1
		}
		return 0
	}
	counts := make([]byte, 7)
	var entries []byte
	for k := 0; k < 3; k++ {
		for _, s := range streams {
			if kind(s) != k {
				continue
			}
			counts[k]++
			// Where it is: 9 bytes, of which a kind and a PID are used.
			entries = append(entries, 9, 1, byte(s.PID>>8), byte(s.PID), 0, 0, 0, 0, 0, 0)
			// What it is: 5 bytes — the coding, and by kind a format byte
			// and a language.
			what := []byte{5, s.Coding, 0, 0, 0, 0}
			switch k {
			case 1:
				what[2] = 0x61
				copy(what[3:], s.Language)
			case 2:
				copy(what[2:], s.Language)
			default:
				what[2] = 0x61
			}
			entries = append(entries, what...)
		}
	}
	table := make([]byte, 16)
	copy(table[4:], counts)
	table = append(table, entries...)
	binary.BigEndian.PutUint16(table, uint16(len(table)-2))
	return table
}

// Playlist encodes an .mpls file playing the given items in order.
func Playlist(items ...Item) []byte {
	var body []byte
	for _, item := range items {
		entry := make([]byte, 22)
		if len(item.Streams) > 0 {
			// The rest of the fixed fields, then the table.
			entry = append(entry, make([]byte, 12)...)
			entry = append(entry, streamTable(item.Streams)...)
		}
		binary.BigEndian.PutUint16(entry, uint16(len(entry)-2))
		copy(entry[2:], fmt.Sprintf("%05dM2TS", item.Clip))
		entry[12] = 1 // connection condition
		start := uint32(600 * 45000)
		if item.In != 0 {
			start = uint32(item.In) * 45000
		}
		binary.BigEndian.PutUint32(entry[14:], start)
		binary.BigEndian.PutUint32(entry[18:], start+uint32(item.Seconds)*45000)
		body = append(body, entry...)
	}

	const headerLen = 40
	out := make([]byte, headerLen+10)
	copy(out, "MPLS0200")
	binary.BigEndian.PutUint32(out[8:], headerLen)
	binary.BigEndian.PutUint32(out[12:], uint32(headerLen+10+len(body)))
	binary.BigEndian.PutUint32(out[headerLen:], uint32(6+len(body)))
	binary.BigEndian.PutUint16(out[headerLen+6:], uint16(len(items)))
	return append(out, body...)
}
