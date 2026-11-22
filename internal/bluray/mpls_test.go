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

// A stream on a disc says its language only in the playlist, beside its PID.
func TestParsePlaylistReadsStreamLanguages(t *testing.T) {
	streams := []bluraytest.Stream{
		{PID: 0x1011, Coding: 0x24},
		{PID: 0x1100, Coding: 0x86, Language: "eng"},
		{PID: 0x1101, Coding: 0x81, Language: "fra"},
		{PID: 0x1102, Coding: 0x83, Language: "deu"},
		{PID: 0x1200, Coding: 0x90, Language: "eng"},
		{PID: 0x1201, Coding: 0x90, Language: "spa"},
	}
	data := bluraytest.Playlist(
		bluraytest.Item{Clip: 1, Seconds: 60, Streams: streams},
		bluraytest.Item{Clip: 2, Seconds: 60},
	)
	items, err := parsePlaylist(data)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{0x1100: "eng", 0x1101: "fra", 0x1102: "deu", 0x1200: "eng", 0x1201: "spa"}
	got := items[0].languages
	if len(got) != len(want) {
		t.Fatalf("languages = %v, want %v", got, want)
	}
	for pid, lang := range want {
		if got[pid] != lang {
			t.Errorf("PID %#x: language %q, want %q", pid, got[pid], lang)
		}
	}
	// The item after it still parses, and one without a table has none.
	if items[1].clip != "00002" || len(items[1].languages) != 0 {
		t.Errorf("second item = %+v", items[1])
	}
}

// The stream table is optional reading: damage to it must never cost the
// playlist, only the languages.
func TestADamagedStreamTableCostsOnlyTheLanguages(t *testing.T) {
	data := bluraytest.Playlist(bluraytest.Item{Clip: 1, Seconds: 60, Streams: []bluraytest.Stream{
		{PID: 0x1011, Coding: 0x1b},
		{PID: 0x1100, Coding: 0x81, Language: "eng"},
		{PID: 0x1200, Coding: 0x90, Language: "eng"},
	}})
	clean, err := parsePlaylist(data)
	if err != nil || len(clean) != 1 {
		t.Fatalf("clean playlist: %v, %v", clean, err)
	}
	// Every byte of the item past its fixed fields, set every which way.
	for at := 50 + 34; at < len(data); at++ {
		for _, v := range []byte{0, 1, 3, 0x7f, 0x80, 0xff} {
			damaged := append([]byte{}, data...)
			damaged[at] = v
			items, err := parsePlaylist(damaged)
			if err != nil || len(items) != 1 || items[0].clip != "00001" {
				t.Fatalf("byte %d set to %#x cost the playlist: %v, %v", at, v, items, err)
			}
			for pid, lang := range items[0].languages {
				if !isLanguageCode(lang) {
					t.Fatalf("byte %d set to %#x gave PID %#x the language %q", at, v, pid, lang)
				}
			}
		}
	}
}
