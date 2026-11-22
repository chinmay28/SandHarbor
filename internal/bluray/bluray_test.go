package bluray_test

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"testing"
	"time"

	"github.com/chinmay28/SandHarbor/internal/bluray"
	"github.com/chinmay28/SandHarbor/internal/bluray/bluraytest"
	"github.com/chinmay28/SandHarbor/internal/udf/udftest"
)

type item = bluraytest.Item

func openTitle(t *testing.T, img []byte) *bluray.Title {
	t.Helper()
	title, err := bluray.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return title
}

// stream reads a title's whole transport stream out of its image.
func stream(t *testing.T, title *bluray.Title, img []byte) []byte {
	t.Helper()
	ts := title.TS(bytes.NewReader(img))
	if ts.Size() != title.Size() {
		t.Fatalf("the stream is %d bytes and the title says %d", ts.Size(), title.Size())
	}
	out, err := io.ReadAll(io.NewSectionReader(ts, 0, ts.Size()))
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	return out
}

// The film is the longest title on the disc, whatever order the playlists
// come in and however large the extras are.
func TestOpenPicksTheLongestTitle(t *testing.T) {
	trailer, _ := bluraytest.Clip(1, 400)
	film, filmTS := bluraytest.Clip(2, 300)
	menu, _ := bluraytest.Clip(3, 50)

	img := bluraytest.Disc{
		Clips: map[int][]byte{0: trailer, 1: film, 2: menu},
		Playlists: map[int][]item{
			0:   {{Clip: 0, Seconds: 150}},
			800: {{Clip: 1, Seconds: 7200}},
			// A menu loop: one short clip, named over and over. Counted as
			// written it would be longer than the film.
			900: {{Clip: 2, Seconds: 30}, {Clip: 2, Seconds: 30}, {Clip: 2, Seconds: 30}},
		},
	}.Image()

	title := openTitle(t, img)
	if title.Playlist != "00800.mpls" || title.Duration != 2*time.Hour {
		t.Errorf("picked %s (%s), want 00800.mpls (2h0m0s)", title.Playlist, title.Duration)
	}
	if len(title.Clips) != 1 || title.Clips[0] != "00001.m2ts" {
		t.Errorf("clips = %v, want [00001.m2ts]", title.Clips)
	}
	if got := stream(t, title, img); !bytes.Equal(got, filmTS) {
		t.Errorf("the stream is %d bytes that are not the film's %d", len(got), len(filmTS))
	}
}

// A menu that loops must not outrank the film by naming its clip many times.
func TestALoopedClipCountsOnce(t *testing.T) {
	film, _ := bluraytest.Clip(1, 100)
	loop, _ := bluraytest.Clip(2, 100)
	items := make([]item, 500)
	for i := range items {
		items[i] = item{Clip: 5, Seconds: 60}
	}
	img := bluraytest.Disc{
		Clips:     map[int][]byte{1: film, 5: loop},
		Playlists: map[int][]item{1: {{Clip: 1, Seconds: 5400}}, 2: items},
	}.Image()

	if title := openTitle(t, img); title.Playlist != "00001.mpls" {
		t.Errorf("picked %s, want the film in 00001.mpls", title.Playlist)
	}
}

// A title in several clips is those clips end to end, in the playlist's order
// rather than the clips' own.
func TestATitleOfSeveralClipsPlaysThemInOrder(t *testing.T) {
	a, aTS := bluraytest.Clip(1, 70)
	b, bTS := bluraytest.Clip(2, 5)
	c, cTS := bluraytest.Clip(3, 130)

	for name, layout := range map[string]udftest.Options{
		"contiguous": {},
		"fragmented": {ExtentBlocks: 2, ChainAfter: 3, SplitMetadata: true},
		"plain":      {Plain: true},
	} {
		t.Run(name, func(t *testing.T) {
			img := bluraytest.Disc{
				Clips:     map[int][]byte{10: a, 20: b, 30: c},
				Playlists: map[int][]item{1: {{Clip: 30, Seconds: 3000}, {Clip: 10, Seconds: 2000}, {Clip: 20, Seconds: 100}}},
				Layout:    layout,
			}.Image()

			title := openTitle(t, img)
			want := append(append(append([]byte{}, cTS...), aTS...), bTS...)
			if got := stream(t, title, img); !bytes.Equal(got, want) {
				t.Errorf("the stream is not the three clips in playlist order")
			}
			if title.Duration != 5100*time.Second {
				t.Errorf("Duration = %s, want 1h25m0s", title.Duration)
			}
		})
	}
}

// Between two titles of one length — the same film in two cuts — the pick has
// to be the same every time: the one with more behind it, then the lower number.
func TestEqualTitlesAreChosenTheSameWayEveryTime(t *testing.T) {
	small, _ := bluraytest.Clip(1, 50)
	large, _ := bluraytest.Clip(2, 90)
	same, _ := bluraytest.Clip(3, 90)
	img := bluraytest.Disc{
		Clips: map[int][]byte{1: small, 2: large, 3: same},
		Playlists: map[int][]item{
			5: {{Clip: 1, Seconds: 6000}},
			7: {{Clip: 3, Seconds: 6000}},
			6: {{Clip: 2, Seconds: 6000}},
		},
	}.Image()
	if title := openTitle(t, img); title.Playlist != "00006.mpls" {
		t.Errorf("picked %s, want 00006.mpls", title.Playlist)
	}
}

// A playlist naming a clip the disc does not have cannot be played, and a
// file that is not a playlist is not one to trip over.
func TestUnusablePlaylistsAreSkipped(t *testing.T) {
	film, filmTS := bluraytest.Clip(1, 40)
	img := bluraytest.Disc{
		Clips: map[int][]byte{1: film},
		Playlists: map[int][]item{
			1: {{Clip: 1, Seconds: 60}},
			2: {{Clip: 1, Seconds: 60}, {Clip: 99, Seconds: 90000}},
		},
		Files: map[string][]byte{
			"BDMV/PLAYLIST/00003.mpls": []byte("not a playlist at all"),
			"BDMV/PLAYLIST/00004.mpls": bluraytest.Playlist(item{Clip: 1, Seconds: 9})[:45],
		},
	}.Image()
	title := openTitle(t, img)
	if title.Playlist != "00001.mpls" {
		t.Errorf("picked %s, want 00001.mpls", title.Playlist)
	}
	if got := stream(t, title, img); !bytes.Equal(got, filmTS) {
		t.Error("the stream is not the film")
	}
}

// A disc with no playlist that works still has a film on it: the biggest clip.
func TestWithoutAPlaylistTheBiggestClipPlays(t *testing.T) {
	small, _ := bluraytest.Clip(1, 10)
	big, bigTS := bluraytest.Clip(2, 60)
	img := bluraytest.Disc{Clips: map[int][]byte{3: small, 4: big}}.Image()

	title := openTitle(t, img)
	if title.Playlist != "" || len(title.Clips) != 1 || title.Clips[0] != "00004.m2ts" {
		t.Errorf("title = %+v, want the clip 00004.m2ts and no playlist", title)
	}
	if got := stream(t, title, img); !bytes.Equal(got, bigTS) {
		t.Error("the stream is not the biggest clip")
	}
}

// A clip that ends partway through a packet loses the part, so that the clip
// after it still starts on a packet.
func TestAPartialLastPacketIsLeftOut(t *testing.T) {
	a, aTS := bluraytest.Clip(1, 20)
	b, bTS := bluraytest.Clip(2, 20)
	a = append(a, 1, 2, 3, 4, 0x47, 9, 9)
	img := bluraytest.Disc{
		Clips:     map[int][]byte{1: a, 2: b},
		Playlists: map[int][]item{1: {{Clip: 1, Seconds: 10}, {Clip: 2, Seconds: 10}}},
	}.Image()
	title := openTitle(t, img)
	if got := stream(t, title, img); !bytes.Equal(got, append(aTS, bTS...)) {
		t.Error("the second clip does not follow the first on a packet boundary")
	}
}

func TestOpenSaysWhatIsWrongWithAnImage(t *testing.T) {
	film, _ := bluraytest.Clip(1, 100)
	junk := make([]byte, 100*192)
	rand.New(rand.NewSource(9)).Read(junk)
	junk[4] = 0 // not even a first sync byte

	cases := map[string]struct {
		img  []byte
		want error
	}{
		"not a disc image": {bytes.Repeat([]byte("installer "), 100000), bluray.ErrNotBluray},
		"a data disc":      {udftest.Build(map[string][]byte{"setup.exe": film}, udftest.Options{}), bluray.ErrNotBluray},
		"a DVD":            {udftest.Build(map[string][]byte{"VIDEO_TS/VTS_01_1.VOB": film}, udftest.Options{Plain: true}), bluray.ErrNotBluray},
		"no clips":         {udftest.Build(map[string][]byte{"BDMV/STREAM/": nil}, udftest.Options{}), bluray.ErrNoTitle},
		"an encrypted disc": {bluraytest.Disc{
			Clips:     map[int][]byte{1: bluraytest.Encrypt(film)},
			Playlists: map[int][]item{1: {{Clip: 1, Seconds: 60}}},
		}.Image(), bluray.ErrEncrypted},
		"clips that are not clips": {bluraytest.Disc{Clips: map[int][]byte{1: junk}}.Image(), bluray.ErrNoTitle},
	}
	for name, c := range cases {
		if _, err := bluray.Open(bytes.NewReader(c.img), int64(len(c.img))); !errors.Is(err, c.want) {
			t.Errorf("%s: Open = %v, want %v", name, err, c.want)
		}
	}
}

// A player seeks, and asks for whatever lengths it likes. Every read has to
// be the same bytes a read of the whole stream would have put there.
func TestTSReaderAnswersAnyRange(t *testing.T) {
	m2ts, want := bluraytest.Clip(1, 2500) // more than one span of the reader
	r := bluray.NewTSReader(bytes.NewReader(m2ts), int64(len(m2ts)))
	if r.Size() != int64(len(want)) {
		t.Fatalf("Size = %d, want %d", r.Size(), len(want))
	}

	check := func(off, n int) {
		t.Helper()
		p := make([]byte, n)
		got, err := r.ReadAt(p, int64(off))
		end := min(off+n, len(want))
		if off > len(want) {
			end = off
		}
		if got != end-off || !bytes.Equal(p[:got], want[off:end]) {
			t.Fatalf("ReadAt(%d bytes at %d) read %d bytes, or the wrong ones", n, off, got)
		}
		if short := n > 0 && off+n > len(want); short && err != io.EOF || !short && err != nil {
			t.Fatalf("ReadAt(%d bytes at %d) err = %v", n, off, err)
		}
	}
	for _, off := range []int{0, 1, 187, 188, 189, 375, 376, 1024 * 188, 1024*188 - 1, len(want) - 1, len(want), len(want) + 5} {
		for _, n := range []int{0, 1, 4, 187, 188, 189, 400, 32 * 1024, 1024 * 188, 1024*188 + 1, len(want)} {
			check(off, n)
		}
	}
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 2000; i++ {
		check(rng.Intn(len(want)), rng.Intn(70000))
	}
}

// A source that ends inside a packet it promised is a damaged image, and has
// to be an error: a player told "end of file" would take the film as over.
func TestTSReaderReportsATruncatedSource(t *testing.T) {
	m2ts, _ := bluraytest.Clip(1, 10)
	r := bluray.NewTSReader(bytes.NewReader(m2ts[:192*4+50]), int64(len(m2ts)))
	if _, err := r.ReadAt(make([]byte, 188*10), 0); err != io.ErrUnexpectedEOF {
		t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

// A disc image is whatever somebody uploaded: damage has to come back as an
// error or a wrong film, never a panic or a hang.
func TestDamagedDiscsFailCleanly(t *testing.T) {
	film, _ := bluraytest.Clip(1, 60)
	clean := bluraytest.Disc{
		Clips:     map[int][]byte{1: film, 2: film[:192*9]},
		Playlists: map[int][]item{1: {{Clip: 1, Seconds: 60}, {Clip: 2, Seconds: 5}}, 2: {{Clip: 2, Seconds: 5}}},
		Layout:    udftest.Options{ExtentBlocks: 2, ChainAfter: 2},
	}.Image()
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 800; i++ {
		img := bytes.Clone(clean)
		for n := 0; n < 1+rng.Intn(4); n++ {
			at := rng.Intn(len(img))
			for img[at] == 0 {
				at = rng.Intn(len(img))
			}
			img[at] = byte(rng.Intn(256))
		}
		title, err := bluray.Open(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			continue
		}
		ts := title.TS(bytes.NewReader(img))
		io.Copy(io.Discard, io.NewSectionReader(ts, 0, ts.Size()))
	}
}

// Each clip on a disc keeps its own time, usually starting at the same ten
// minutes as every other. End to end they are a film whose clock keeps
// starting again, which Android's player will not play and no player can
// tell the length of. So a title of several clips is sent on one clock: each
// clip's times moved to where the playlist says the one before it ended.
func TestClipsOfATitleAreSentOnOneClock(t *testing.T) {
	const rate = bluraytest.TickRate
	// Three clips, each starting on its own clock: two at the usual ten
	// minutes, one somewhere else altogether.
	a, aTS := bluraytest.TimedClip(600*rate, 40)
	b, _ := bluraytest.TimedClip(600*rate, 25)
	c, _ := bluraytest.TimedClip(4000*rate, 60)

	img := bluraytest.Disc{
		Clips: map[int][]byte{1: a, 2: b, 3: c},
		Playlists: map[int][]item{1: {
			{Clip: 1, Seconds: 100},
			{Clip: 2, Seconds: 50},
			{Clip: 3, Seconds: 300, In: 4000},
		}},
		Layout: udftest.Options{ExtentBlocks: 2},
	}.Image()

	// What a player should get: the first clip as it is, the second starting
	// where the first is said to end, the third where the second is.
	_, bMoved := bluraytest.TimedClip((600+100)*rate, 25)
	_, cMoved := bluraytest.TimedClip((600+100+50)*rate, 60)
	want := append(append(append([]byte{}, aTS...), bMoved...), cMoved...)

	title := openTitle(t, img)
	got := stream(t, title, img)
	if !bytes.Equal(got, want) {
		for i := 0; i < len(want); i += 188 {
			if !bytes.Equal(got[i:i+188], want[i:i+188]) {
				t.Fatalf("packet %d is not on the title's clock:\n got % x\nwant % x", i/188, got[i:i+40], want[i:i+40])
			}
		}
	}

	// And the same bytes whichever way they are asked for: a read that starts
	// partway through a packet, or runs across the seam between two clips,
	// has to move the same clocks a read of the whole stream does.
	ts := title.TS(bytes.NewReader(img))
	rng := rand.New(rand.NewSource(5))
	for i := 0; i < 3000; i++ {
		off := rng.Intn(len(want))
		p := make([]byte, 1+rng.Intn(2000))
		n, _ := ts.ReadAt(p, int64(off))
		if !bytes.Equal(p[:n], want[off:off+n]) {
			t.Fatalf("a read of %d bytes at %d differs from the same bytes of the whole stream", len(p), off)
		}
	}
}

// A title in one clip has one clock already, and is sent as it lies on the
// disc — whatever is in its packets.
func TestASingleClipIsSentUntouched(t *testing.T) {
	clip, want := bluraytest.TimedClip(600*bluraytest.TickRate, 80)
	img := bluraytest.Disc{
		Clips:     map[int][]byte{1: clip},
		Playlists: map[int][]item{1: {{Clip: 1, Seconds: 5000}}},
	}.Image()
	if got := stream(t, openTitle(t, img), img); !bytes.Equal(got, want) {
		t.Error("a title of one clip was not sent as it is on the disc")
	}
}
