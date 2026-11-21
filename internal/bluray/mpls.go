package bluray

import (
	"encoding/binary"
	"errors"
	"time"
)

// playItem is one entry of a playlist: a clip, and the part of it to play.
type playItem struct {
	// clip is the clip's five-digit name, as in "00001.m2ts".
	clip string
	// in and out are the presentation times the item plays between, in the
	// 45 kHz ticks a playlist counts in.
	in, out uint32
}

// duration is how long the item plays for.
func (p playItem) duration() time.Duration {
	if p.out <= p.in {
		return 0
	}
	return time.Duration(p.out-p.in) * time.Second / 45000
}

// errBadPlaylist is returned for a playlist file that is not one.
var errBadPlaylist = errors.New("bluray: malformed playlist")

// maxPlayItems bounds how many items a playlist is read for. A film is a few;
// a disc with seamless branching, a few hundred.
const maxPlayItems = 4096

// parsePlaylist reads the play items of an .mpls file: the clips a title is
// made of, in the order they play.
//
// Only the main path is read. Sub-paths (picture-in-picture, a second audio
// stream out of another file) and the marks that make chapters are left where
// they are.
func parsePlaylist(data []byte) ([]playItem, error) {
	if len(data) < 20 || string(data[:4]) != "MPLS" {
		return nil, errBadPlaylist
	}
	start := int(binary.BigEndian.Uint32(data[8:]))
	// The playlist block: its length (4), reserved (2), then how many play
	// items and sub-paths there are (2 each).
	if start < 20 || start > len(data)-10 {
		return nil, errBadPlaylist
	}
	count := int(binary.BigEndian.Uint16(data[start+6:]))
	if count > maxPlayItems {
		return nil, errBadPlaylist
	}

	items := make([]playItem, 0, count)
	at := start + 10
	for i := 0; i < count; i++ {
		// An item: its length (2), the clip's name (5) and codec (4), flags
		// (2), a clock reference (1), then the in and out times (4 each).
		if at > len(data)-22 {
			return nil, errBadPlaylist
		}
		length := int(binary.BigEndian.Uint16(data[at:]))
		if length < 20 || at+2+length > len(data) {
			return nil, errBadPlaylist
		}
		name := data[at+2 : at+7]
		for _, c := range name {
			if c < '0' || c > '9' {
				return nil, errBadPlaylist
			}
		}
		items = append(items, playItem{
			clip: string(name),
			in:   binary.BigEndian.Uint32(data[at+14:]),
			out:  binary.BigEndian.Uint32(data[at+18:]),
		})
		at += 2 + length
	}
	return items, nil
}
