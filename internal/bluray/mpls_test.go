package bluray

import (
	"testing"
	"time"

	"github.com/chinmay28/SandHarbor/internal/bluray/bluraytest"
)

func TestParsePlaylistReadsItsItems(t *testing.T) {
	data := bluraytest.Playlist(bluraytest.Item{Clip: 12, Seconds: 90}, bluraytest.Item{Clip: 7, Seconds: 3600})
	items, err := parsePlaylist(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].clip != "00012" || items[1].clip != "00007" {
		t.Fatalf("items = %+v", items)
	}
	if items[0].duration() != 90*time.Second || items[1].duration() != time.Hour {
		t.Errorf("durations = %s, %s", items[0].duration(), items[1].duration())
	}
}

// Every way of cutting a playlist short, and every byte of it altered, has to
// be an error or a different playlist: never a read past the end.
func TestParsePlaylistSurvivesDamage(t *testing.T) {
	data := bluraytest.Playlist(bluraytest.Item{Clip: 1, Seconds: 1}, bluraytest.Item{Clip: 2, Seconds: 2})
	for n := 0; n < len(data); n++ {
		if _, err := parsePlaylist(data[:n]); err == nil {
			t.Errorf("a playlist cut to %d of %d bytes parsed", n, len(data))
		}
	}
	for at := range data {
		for _, v := range []byte{0, 1, 0x7f, 0xff} {
			damaged := append([]byte{}, data...)
			damaged[at] = v
			parsePlaylist(damaged)
		}
	}
}

// An item whose out time is before its in time plays for no time at all,
// rather than for the thirteen hours the subtraction would wrap to.
func TestABackwardsItemHasNoDuration(t *testing.T) {
	if d := (playItem{in: 500, out: 100}).duration(); d != 0 {
		t.Errorf("duration = %s, want 0", d)
	}
}
