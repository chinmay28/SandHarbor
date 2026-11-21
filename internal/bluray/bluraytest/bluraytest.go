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
// a player should be handed for it. The packets carry no picture: each is a
// sync byte and bytes that depend on seed, behind the four-byte header a
// Blu-ray puts in front of every packet.
func Clip(seed int64, packets int) (m2ts, ts []byte) {
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < packets; i++ {
		header := make([]byte, 4)
		binary.BigEndian.PutUint32(header, uint32(i)*1000&0x3fffffff)
		body := make([]byte, 188)
		rng.Read(body)
		body[0] = 0x47
		m2ts = append(append(m2ts, header...), body...)
		ts = append(ts, body...)
	}
	return m2ts, ts
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

// Playlist encodes an .mpls file playing the given items in order.
func Playlist(items ...Item) []byte {
	var body []byte
	for _, item := range items {
		entry := make([]byte, 22)
		binary.BigEndian.PutUint16(entry, 20)
		copy(entry[2:], fmt.Sprintf("%05dM2TS", item.Clip))
		entry[12] = 1 // connection condition
		const start = 600 * 45000
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
