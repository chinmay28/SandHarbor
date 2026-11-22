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
	// languages is the language of each audio and subtitle stream the item
	// lists, by the stream's PID, as the three-letter code the disc gives.
	// Nil when the item's stream table could not be read.
	languages map[int]string
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
			clip:      string(name),
			in:        binary.BigEndian.Uint32(data[at+14:]),
			out:       binary.BigEndian.Uint32(data[at+18:]),
			languages: streamLanguages(data[at : at+2+length]),
		})
		at += 2 + length
	}
	return items, nil
}

// Stream kinds, as a playlist's stream table codes them.
const (
	codingPCM            = 0x80
	codingSecondaryDTS   = 0xa2
	codingPresentationGS = 0x90
	codingInteractiveGS  = 0x91
	codingTextSubtitle   = 0x92
)

// streamLanguages reads the stream table of one play item for the language
// of each audio and subtitle stream, by PID.
//
// A transport stream off a disc does not say what language anything in it is:
// that is in the playlist, beside the PIDs. Without it a film remuxed out of
// a disc has twenty subtitle tracks and no way to tell which is which.
//
// The table is optional reading. Anything about it that does not add up
// gives back nil, or what was read before it stopped adding up — never an
// error, because the film plays the same without it.
func streamLanguages(item []byte) map[int]string {
	// The table follows the item's fixed fields: 34 bytes of them, when the
	// item has one angle. An item with several lists them first, and its
	// languages are not looked for.
	const fixed = 34
	if len(item) < fixed+16 || item[12]&0x10 != 0 {
		return nil
	}
	table := item[fixed:]
	// Its length (2), reserved (2), then how many streams of each of seven
	// kinds follow (1 each), and five bytes reserved.
	if n := int(binary.BigEndian.Uint16(table)); n+2 <= len(table) {
		table = table[:n+2]
	}
	if len(table) < 16 {
		return nil
	}
	counts := table[4:11]
	total := 0
	for _, c := range counts {
		total += int(c)
	}
	entries := table[16:]

	out := map[int]string{}
	for i := 0; i < total; i++ {
		// A stream is two length-prefixed records: where it is, and what.
		if len(entries) < 1 || len(entries) < 1+int(entries[0]) {
			return out
		}
		where := entries[1 : 1+int(entries[0])]
		entries = entries[1+len(where):]
		if len(entries) < 1 || len(entries) < 1+int(entries[0]) {
			return out
		}
		what := entries[1 : 1+int(entries[0])]
		entries = entries[1+len(what):]

		// Where: a kind, then the PID after zero, two or one bytes naming a
		// sub-path, depending on the kind.
		pidAt := map[byte]int{1: 1, 2: 3, 3: 2, 4: 2}
		if len(where) < 1 {
			continue
		}
		at, ok := pidAt[where[0]]
		if !ok || len(where) < at+2 {
			continue
		}
		pid := int(binary.BigEndian.Uint16(where[at:]))

		// What: the coding, then for sound a byte of format before the
		// language, for text subtitles a character code, for picture
		// subtitles nothing.
		if len(what) < 1 {
			continue
		}
		langAt := 0
		switch coding := what[0]; {
		case coding >= codingPCM && coding <= codingSecondaryDTS && coding != codingPresentationGS &&
			coding != codingInteractiveGS && coding != codingTextSubtitle:
			langAt = 2
		case coding == codingPresentationGS, coding == codingInteractiveGS:
			langAt = 1
		case coding == codingTextSubtitle:
			langAt = 2
		default:
			continue // video: no language
		}
		if len(what) < langAt+3 {
			continue
		}
		if lang := string(what[langAt : langAt+3]); isLanguageCode(lang) {
			out[pid] = lang
		}
	}
	return out
}

// isLanguageCode reports whether s is three lower-case letters, which is all
// a language on a disc may be.
func isLanguageCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, c := range s {
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}
