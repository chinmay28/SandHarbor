// Package bluray finds the film inside a Blu-ray disc image and hands it out
// as an ordinary transport stream.
//
// A disc image is a filesystem, not a film: the picture is in clips under
// BDMV/STREAM, and playlists under BDMV/PLAYLIST say which clips make a title
// and in what order. A player that opens discs follows all of that and draws
// the menus besides. A player that is given an address follows none of it.
//
// So this package does the one part of a disc player's work that matters for
// watching the film. Open reads the playlists, picks the main title — the
// longest — and works out where that title's clips lie in the image. The
// result is a Title: a list of byte ranges of the image, which TS turns into
// a seekable stream that any player takes for a .ts file. Nothing is copied
// and nothing is converted; a title of forty gigabytes costs a few sectors
// to find and then exactly the bytes a player asks for.
//
// What a Title is not: the disc. There are no menus, no extras and no
// chapters; a playlist that plays part of a clip gets the whole clip; and a
// title assembled from many clips plays them end to end, with their
// timestamps moved onto one clock (see ts.go), so that a player sees one
// film of the right length rather than a clock that keeps starting again.
//
// A disc image copied straight off a bought disc is encrypted, and Open says
// so (ErrEncrypted) rather than handing out noise.
package bluray

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/chinmay28/SandHarbor/internal/udf"
)

// ErrNotBluray is returned for an image that is not a Blu-ray disc: not a UDF
// volume at all, or one with no BDMV directory on it.
var ErrNotBluray = errors.New("bluray: not a Blu-ray disc image")

// ErrNoTitle is returned for a Blu-ray image with no playable title in it.
var ErrNoTitle = errors.New("bluray: no playable title on this disc")

// ErrEncrypted is returned for a disc whose clips are still encrypted, as
// they are on an image copied from a bought disc without decrypting it.
var ErrEncrypted = errors.New("bluray: this disc image is encrypted")

// Bounds on what a disc may ask to be read while a title is looked for.
const (
	maxPlaylistBytes = 4 << 20
	maxPlaylists     = 4096
)

// Title is one title of a disc: the film, as a run of clips.
type Title struct {
	// Playlist is the name of the playlist the title was read from, such as
	// "00800.mpls", or empty for a disc that had to be played without one.
	Playlist string
	// Duration is how long the playlist says the title runs, or zero where
	// it does not say.
	Duration time.Duration
	// Clips are the clip files the title plays, in order.
	Clips []string
	// Languages is the language of each audio and subtitle stream of the
	// title, by the stream's PID in the transport stream, as the three-letter
	// code the disc gives ("eng", "fra"). Empty where the disc does not say.
	Languages map[int]string

	// extents is where the title's clips lie in the image, end to end, each
	// clip cut to a whole number of packets.
	extents []udf.Extent
	size    int64
	// shifts puts the clips' separate clocks onto one line. See ts.go.
	shifts []clockShift
}

// Size is the length in bytes of the transport stream TS returns.
func (t *Title) Size() int64 { return t.size / sourcePacket * tsPacket }

// TS returns the title as a transport stream of 188-byte packets, read out
// of image, which must be the image the title was found in.
func (t *Title) TS(image io.ReaderAt) *TSReader {
	r := NewTSReader(udf.NewExtentReader(image, t.extents), t.size)
	r.shifts = t.shifts
	return r
}

// Open finds the main title of the Blu-ray disc image in r, which is size
// bytes long.
//
// It reads the volume's structures, the playlists, and the first packets of
// the title it picks: a few hundred kilobytes of a typical disc, from near
// the start of the image.
func Open(r io.ReaderAt, size int64) (*Title, error) {
	vol, err := udf.Open(r, size)
	if err != nil {
		if errors.Is(err, udf.ErrNotUDF) {
			return nil, ErrNotBluray
		}
		return nil, unplayable(err)
	}
	title, err := openVolume(vol, r)
	return title, unplayable(err)
}

// unplayable files an error from reading the volume under the three things
// that can be wrong with an image, where it is one of them. A volume that
// contradicts itself, or is laid out in a way that is not read, is a disc
// with no title that can be played; what is left over is a failure to read
// the image at all, and is passed on as it came.
func unplayable(err error) error {
	if errors.Is(err, udf.ErrCorrupt) || errors.Is(err, udf.ErrUnsupported) {
		return fmt.Errorf("%w: %w", ErrNoTitle, err)
	}
	return err
}

// openVolume is Open once the image is known to hold a volume.
func openVolume(vol *udf.Volume, r io.ReaderAt) (*Title, error) {
	streamDir, err := vol.Lookup("BDMV/STREAM")
	if err != nil || !streamDir.IsDir {
		if err == nil || errors.Is(err, udf.ErrNotExist) {
			return nil, ErrNotBluray
		}
		return nil, err
	}

	// The clips there are to play, by five-digit name.
	entries, err := vol.ReadDir(streamDir)
	if err != nil {
		return nil, err
	}
	clips := map[string]udf.Entry{}
	for _, e := range entries {
		name := strings.ToLower(e.Name)
		if !e.IsDir && strings.HasSuffix(name, ".m2ts") && e.Size >= sourcePacket {
			clips[strings.TrimSuffix(name, ".m2ts")] = e
		}
	}
	if len(clips) == 0 {
		return nil, ErrNoTitle
	}

	title, items, err := mainPlaylist(vol, clips)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, item := range items {
		names = append(names, item.clip)
	}
	if len(items) > 0 {
		// Every item of a title lists the same streams; the first speaks for
		// them all.
		title.Languages = items[0].languages
	}
	if title == nil {
		// No playlist that could be read names a clip that is there. The
		// biggest clip is the film on any disc this simple.
		title = &Title{}
		largest := ""
		for name, e := range clips {
			if largest == "" || e.Size > clips[largest].Size || e.Size == clips[largest].Size && name < largest {
				largest = name
			}
		}
		names = []string{largest}
	}

	// Where the title has got to, in the playlist's 45 kHz ticks, counted
	// from where its first clip's own clock starts.
	var elapsed uint64
	for i, name := range names {
		entry := clips[name]
		extents, err := vol.Extents(entry)
		if err != nil {
			return nil, err
		}
		whole := entry.Size - entry.Size%sourcePacket
		if len(items) > 1 {
			// The clip's clock reads its own in time where the title's should
			// read the first clip's in time plus everything played so far. A
			// timestamp ticks twice for each of a playlist's.
			item := items[i]
			want := uint64(items[0].in) + elapsed
			title.shifts = append(title.shifts, clockShift{
				first: title.size / sourcePacket,
				ticks: (want - uint64(item.in)) * 2 & tsClockMask,
			})
			if item.out > item.in {
				elapsed += uint64(item.out - item.in)
			}
		}
		title.extents = append(title.extents, trim(extents, whole)...)
		title.size += whole
		title.Clips = append(title.Clips, entry.Name)
	}

	if err := checkReadable(udf.NewExtentReader(r, title.extents)); err != nil {
		return nil, err
	}
	return title, nil
}

// mainPlaylist picks the playlist that is the film and returns it with the
// clips it plays, or nil when the disc has no usable playlist.
//
// The film is the longest title, which is the rule every tool that rips a
// disc starts from. Two things are done to keep it honest. A clip is counted
// once however often a playlist names it, because a playlist that loops one
// short clip for hours is a menu background, not a film. And between titles
// of the same length, the one with more bytes behind it wins, then the one
// with the lower number: the same film in two cuts or two languages is very
// often two playlists a second apart, and a disc has to be picked from the
// same way every time.
func mainPlaylist(vol *udf.Volume, clips map[string]udf.Entry) (*Title, []playItem, error) {
	dir, err := vol.Lookup("BDMV/PLAYLIST")
	if errors.Is(err, udf.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	entries, err := vol.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	if len(entries) > maxPlaylists {
		entries = entries[:maxPlaylists]
	}

	var (
		best      *Title
		bestClips []playItem
		bestBytes int64
	)
	for _, e := range entries {
		if e.IsDir || !strings.HasSuffix(strings.ToLower(e.Name), ".mpls") || e.Size > maxPlaylistBytes {
			continue
		}
		data, err := vol.ReadFile(e, maxPlaylistBytes)
		if err != nil {
			return nil, nil, err
		}
		items, err := parsePlaylist(data)
		if err != nil {
			// One unreadable playlist among many is a disc with one fewer.
			continue
		}

		var (
			names    []playItem
			seen     = map[string]bool{}
			duration time.Duration
			bytes    int64
			missing  bool
		)
		for _, item := range items {
			clip, ok := clips[item.clip]
			if !ok {
				missing = true
				break
			}
			if seen[item.clip] {
				continue
			}
			seen[item.clip] = true
			names = append(names, item)
			duration += item.duration()
			bytes += clip.Size
		}
		if missing || len(names) == 0 {
			continue
		}
		if best == nil || duration > best.Duration || duration == best.Duration && bytes > bestBytes {
			best, bestClips, bestBytes = &Title{Playlist: e.Name, Duration: duration}, names, bytes
		}
	}
	return best, bestClips, nil
}

// trim cuts a run of extents down to its first size bytes.
func trim(extents []udf.Extent, size int64) []udf.Extent {
	var out []udf.Extent
	for _, e := range extents {
		if size <= 0 {
			break
		}
		if e.Length > size {
			e.Length = size
		}
		out = append(out, e)
		size -= e.Length
	}
	return out
}

// probePackets is how many packets are looked at to tell a readable clip from
// an encrypted one: one of the 6144-byte units a disc is encrypted in.
const probePackets = 32

// checkReadable looks at the first packets of a title for the sync byte each
// must begin with.
//
// An encrypted disc is encrypted in units of 32 packets, with the first
// sixteen bytes of each unit left clear. So the first packet of an encrypted
// clip has its sync byte and the thirty-one after it have noise where theirs
// should be, which is what tells a disc nobody decrypted from a file that is
// simply not a clip.
func checkReadable(src *udf.ExtentReader) error {
	count := int(min(src.Size()/sourcePacket, probePackets))
	buf := make([]byte, count*sourcePacket)
	if _, err := src.ReadAt(buf, 0); err != nil {
		return fmt.Errorf("bluray: reading the start of the title: %w", err)
	}
	synced := 0
	for i := 0; i < count; i++ {
		if buf[i*sourcePacket+packetExtra] == syncByte {
			synced++
		}
	}
	switch {
	case synced == count:
		return nil
	case buf[packetExtra] == syncByte:
		return ErrEncrypted
	default:
		return ErrNoTitle
	}
}
