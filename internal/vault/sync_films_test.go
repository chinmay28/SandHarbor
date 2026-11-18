package vault

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/chinmay28/SandHarbor/internal/movie"
)

// A phone running the vault never looks films up: the primary does, and the
// details and posters reach the phone in the shared index. These are the
// paths that carry them.

// matchOn records a film match on v the way a sweep does: the details in the
// index, the poster as the file's thumbnail.
func matchOn(t *testing.T, v *Vault, e *Entry, title string, poster []byte) {
	t.Helper()
	if err := v.SetMovie(e.ID, &movie.Info{TMDBID: 1, Title: title, Year: 1995}); err != nil {
		t.Fatalf("%s: SetMovie: %v", v.Instance(), err)
	}
	if err := v.SetThumb(context.Background(), e.ID, poster); err != nil {
		t.Fatalf("%s: SetThumb: %v", v.Instance(), err)
	}
	v.AwaitBackupSync()
}

func thumbOn(t *testing.T, v *Vault, e *Entry) []byte {
	t.Helper()
	data, err := v.Thumb(context.Background(), e.ID)
	if err != nil {
		t.Fatalf("%s: Thumb(%s): %v", v.Instance(), e.Name, err)
	}
	return data
}

// The phone had the films folder open — its pictures in memory — before the
// primary matched a film in it. The pull must bring the poster, not leave the
// phone drawing the folder as it was.
func TestAPosterMatchedOnThePrimaryReachesAPhoneThatHadTheFolderOpen(t *testing.T) {
	pi, _ := newTestVaultOn(t, "pi", 3)
	phone := secondMachine(t, pi, "phone")
	phone.SetPhone(true)

	matched := storeAt(t, pi, "Ronin.1998.mkv", "one film")
	film := storeAt(t, pi, "Heat.1995.mkv", "another film")
	matchOn(t, pi, matched, "Ronin", []byte("ronin poster"))

	pull(t, phone)
	if got := thumbOn(t, phone, matched); string(got) != "ronin poster" {
		t.Fatalf("before: the phone draws %q", got)
	}
	if _, err := phone.Thumb(context.Background(), film.ID); !errors.Is(err, ErrNoThumb) {
		t.Fatalf("before: Heat has a picture on the phone already: %v", err)
	}

	matchOn(t, pi, film, "Heat", []byte("heat poster"))
	pull(t, phone)

	if info := phone.Movie(film.ID); info == nil || info.Title != "Heat" {
		t.Fatalf("the phone's details for Heat: %+v", info)
	}
	if got := thumbOn(t, phone, film); string(got) != "heat poster" {
		t.Errorf("the phone draws Heat as %q", got)
	}
	if got := thumbOn(t, phone, matched); string(got) != "ronin poster" {
		t.Errorf("the phone draws Ronin as %q", got)
	}
}

// A poster replacing a picture the phone drew itself — a frame from the
// video, stored before the folder was matched — shows once the phone pulls.
func TestAPosterReplacesThePhonesOwnPictureAfterAPull(t *testing.T) {
	pi, _ := newTestVaultOn(t, "pi", 3)
	phone := secondMachine(t, pi, "phone")
	phone.SetPhone(true)

	film := storeAt(t, pi, "Heat.1995.mkv", "a film")
	pull(t, phone)
	if err := phone.SetThumb(context.Background(), film.ID, []byte("a frame")); err != nil {
		t.Fatalf("phone: SetThumb: %v", err)
	}
	phone.AwaitBackupSync()

	pull(t, pi)
	matchOn(t, pi, film, "Heat", []byte("heat poster"))
	pull(t, phone)

	if got := thumbOn(t, phone, film); string(got) != "heat poster" {
		t.Errorf("the phone draws Heat as %q", got)
	}
}

// Details matched on the primary while the phone was writing too — the two
// indexes diverged, and are combined rather than one taken whole — still
// arrive, and the phone's own file survives with them.
func TestFilmDetailsSurviveACombinedIndex(t *testing.T) {
	pi, _ := newTestVaultOn(t, "pi", 3)
	phone := secondMachine(t, pi, "phone")
	phone.SetPhone(true)

	film := storeAt(t, pi, "Heat.1995.mkv", "a film")
	pull(t, phone)

	matchOn(t, pi, film, "Heat", []byte("heat poster"))
	storeAt(t, phone, "from-the-phone.jpg", "a photo")

	// The phone's push pulls first, so the combine may already have happened
	// by now; this pull makes sure the latest of the primary's is in.
	pull(t, phone)
	if info := phone.Movie(film.ID); info == nil || info.Title != "Heat" {
		t.Fatalf("the phone's details for Heat: %+v", info)
	}
	if got := thumbOn(t, phone, film); string(got) != "heat poster" {
		t.Errorf("the phone draws Heat as %q", got)
	}
	if _, err := phone.EntryByPath(MainScope, "/from-the-phone.jpg"); err != nil {
		t.Errorf("the phone's own file was lost in the combine: %v", err)
	}
}

func TestChangedPacks(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	pack := func(key string, when time.Time) *ThumbPack {
		return &ThumbPack{UpdatedAt: when, Shards: []Shard{{ProviderID: "p1", Key: key}}}
	}
	same := pack("a", at)

	for _, tc := range []struct {
		name          string
		before, after map[string]*ThumbPack
		want          []string
	}{
		{"both empty", nil, nil, nil},
		{"the same record", map[string]*ThumbPack{"/f": same}, map[string]*ThumbPack{"/f": same}, nil},
		{"an equal copy", map[string]*ThumbPack{"/f": same}, map[string]*ThumbPack{"/f": pack("a", at)}, nil},
		{"rewritten", map[string]*ThumbPack{"/f": same}, map[string]*ThumbPack{"/f": pack("b", at.Add(time.Second))}, []string{"/f"}},
		{"new parts, same time", map[string]*ThumbPack{"/f": same}, map[string]*ThumbPack{"/f": pack("b", at)}, []string{"/f"}},
		{"added", nil, map[string]*ThumbPack{"/f": same}, []string{"/f"}},
		{"dropped", map[string]*ThumbPack{"/f": same, "/g": same}, map[string]*ThumbPack{"/g": same}, []string{"/f"}},
		{"sorted", nil, map[string]*ThumbPack{"/z": same, "/a": same}, []string{"/a", "/z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := changedPacks(tc.before, tc.after); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("changedPacks = %v, want %v", got, tc.want)
			}
		})
	}
}
