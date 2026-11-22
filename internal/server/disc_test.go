package server

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chinmay28/SandHarbor/internal/bluray"
	"github.com/chinmay28/SandHarbor/internal/bluray/bluraytest"
	"github.com/chinmay28/SandHarbor/internal/udf/udftest"
)

// testDisc is a Blu-ray image with a film and a trailer on it, and the
// transport stream a player should get for the film.
func testDisc() (image, film []byte) {
	trailer, _ := bluraytest.Clip(1, 300)
	clip, film := bluraytest.Clip(2, 900)
	image = bluraytest.Disc{
		Clips: map[int][]byte{0: trailer, 1: clip},
		Playlists: map[int][]bluraytest.Item{
			0:   {{Clip: 0, Seconds: 120}},
			800: {{Clip: 1, Seconds: 6000}},
		},
		// In pieces, so that a stream that read the image straight through
		// would not pass for one that followed the film.
		Layout: udftest.Options{ExtentBlocks: 7},
	}.Image()
	return image, film
}

// follow follows a stream link the way a player does: no cookie, and a Range
// header when one is given.
func (c *testClient) follow(link, byteRange string) *httptest.ResponseRecorder {
	c.t.Helper()
	req := httptest.NewRequest(http.MethodGet, link, nil)
	req.Host = "example.test"
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, req)
	return w
}

// The point of all of it: a link for a Blu-ray image is a link to the film,
// under a name and a type that any player opens.
func TestAStreamLinkForABlurayImagePlaysTheFilmInside(t *testing.T) {
	c := newTestClient(t)
	c.setup("stream-me-please", 3)
	image, film := testDisc()
	file := c.upload("Some Film (1982).iso", "/", image)

	w, body := c.json(http.MethodPost, "/api/files/"+file["id"].(string)+"/stream", nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", w.Code, w.Body.String())
	}
	link := body["url"].(string)
	if !strings.HasSuffix(link, "/Some%20Film%20%281982%29.ts") {
		t.Errorf("link %q should end in the film's name as a .ts, which is what a player picks its demuxer by", link)
	}
	if body["name"] != "Some Film (1982).ts" || body["mime"] != "video/mp2t" {
		t.Errorf("name %v, mime %v; want Some Film (1982).ts, video/mp2t", body["name"], body["mime"])
	}

	got := c.follow(link, "")
	if got.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", got.Code, got.Body.String())
	}
	if !bytes.Equal(got.Body.Bytes(), film) {
		t.Fatalf("the stream is %d bytes that are not the film's %d", got.Body.Len(), len(film))
	}
	if ct := got.Header().Get("Content-Type"); ct != "video/mp2t" {
		t.Errorf("Content-Type = %q, want video/mp2t", ct)
	}
	if cd := got.Header().Get("Content-Disposition"); !strings.Contains(cd, ".ts") || strings.Contains(cd, ".iso") {
		t.Errorf("Content-Disposition = %q, want the film's name, not the image's", cd)
	}

	// Seeking, which for a player is most of what it does.
	part := c.follow(link, "bytes=100000-100999")
	if part.Code != http.StatusPartialContent || !bytes.Equal(part.Body.Bytes(), film[100000:101000]) {
		t.Errorf("a range came back %d with %d bytes, or the wrong ones", part.Code, part.Body.Len())
	}
	if want := fmt.Sprintf("bytes 100000-100999/%d", len(film)); part.Header().Get("Content-Range") != want {
		t.Errorf("Content-Range = %q, want %q", part.Header().Get("Content-Range"), want)
	}
	tail := c.follow(link, "bytes=-188")
	if !bytes.Equal(tail.Body.Bytes(), film[len(film)-188:]) {
		t.Error("the last packet, asked for from the end, is not the film's last packet")
	}
}

// The film and the image are different bytes behind one file, and a cache
// that took one for the other would hand a player the wrong one.
func TestTheFilmAndItsImageHaveDifferentETags(t *testing.T) {
	c := newTestClient(t)
	c.setup("stream-me-please", 3)
	image, _ := testDisc()
	id := c.upload("film.iso", "/", image)["id"].(string)

	_, asFilm := c.json(http.MethodPost, "/api/files/"+id+"/stream", nil)
	_, asFile := c.json(http.MethodPost, "/api/files/"+id+"/stream?download=1", nil)
	a := c.follow(asFilm["url"].(string), "bytes=0-0").Header().Get("ETag")
	b := c.follow(asFile["url"].(string), "bytes=0-0").Header().Get("ETag")
	if a == "" || a == b {
		t.Errorf("ETags %q and %q should differ", a, b)
	}
}

// A link asked for in order to save the file is the image, whole: a download
// of a disc image has to give back the disc image.
func TestADownloadLinkForADiscImageIsTheImage(t *testing.T) {
	c := newTestClient(t)
	c.setup("stream-me-please", 3)
	image, _ := testDisc()
	file := c.upload("film.iso", "/", image)

	_, body := c.json(http.MethodPost, "/api/files/"+file["id"].(string)+"/stream?download=1", nil)
	link := body["url"].(string)
	if !strings.HasSuffix(link, "/film.iso") || body["name"] != "film.iso" {
		t.Fatalf("link %q, name %v; want the image under its own name", link, body["name"])
	}
	got := c.follow(link+"?download=1", "")
	if !bytes.Equal(got.Body.Bytes(), image) {
		t.Errorf("the download is %d bytes that are not the image's %d", got.Body.Len(), len(image))
	}
	if cd := got.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, "film.iso") {
		t.Errorf("Content-Disposition = %q", cd)
	}
}

// An .iso that is not a Blu-ray, or one with no film that can be found, gets
// the link it always got: finding no film is never a reason to refuse a file.
func TestAnImageWithNoFilmStreamsAsItself(t *testing.T) {
	clip, _ := bluraytest.Clip(1, 64)
	images := map[string][]byte{
		"installer.iso": bytes.Repeat([]byte("not a disc "), 90000),
		"data.iso":      udftest.Build(map[string][]byte{"setup.exe": clip}, udftest.Options{}),
		"encrypted.iso": bluraytest.Disc{Clips: map[int][]byte{1: bluraytest.Encrypt(clip)}}.Image(),
	}
	c := newTestClient(t)
	c.setup("stream-me-please", 3)
	for name, image := range images {
		file := c.upload(name, "/", image)
		link := c.mintStream(file["id"].(string))
		if !strings.HasSuffix(link, "/"+name) {
			t.Errorf("%s: link %q should be to the image itself", name, link)
		}
		if got := c.follow(link, ""); !bytes.Equal(got.Body.Bytes(), image) {
			t.Errorf("%s: the stream is not the image", name)
		}
	}
}

// The TV's player reads films and not discs, so the TV is given the film —
// and told why, in words, when there is none to give.
func TestATVPlaysTheFilmInADiscImage(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/films"})
	image, film := testDisc()
	clip, _ := bluraytest.Clip(5, 64)

	good := c.upload("Alien (1979).iso", "/films", image)["id"].(string)
	c.upload("Alien (1979).srt", "/films", []byte("1\n00:00:01,000 --> 00:00:02,000\nIn space\n"))
	encrypted := c.upload("Bought.iso", "/films",
		bluraytest.Disc{Clips: map[int][]byte{1: bluraytest.Encrypt(clip)}}.Image())["id"].(string)
	notADisc := c.upload("Ubuntu.iso", "/films", bytes.Repeat([]byte("installer "), 90000))["id"].(string)

	token := c.pairTV(c.mintTVCode("/films", 60))

	w, body := c.tvDo(http.MethodPost, "/api/tv/items/"+good+"/play", token, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("play: %d %v", w.Code, body)
	}
	link := body["url"].(string)
	// Marked as a Blu-ray's stream, which the TV's player has to be told to
	// read its sound correctly.
	if !strings.HasSuffix(link, ".ts?disc=bluray") || body["mime"] != "video/mp2t" {
		t.Errorf("link %q, mime %v; want a marked .ts the TV's player opens", link, body["mime"])
	}
	if got := c.follow(link, ""); !bytes.Equal(got.Body.Bytes(), film) {
		t.Errorf("the TV's stream is %d bytes that are not the film's %d", got.Body.Len(), len(film))
	}
	// The subtitle file beside the image belongs to the film in it.
	if subs, _ := body["subtitles"].([]any); len(subs) != 1 {
		t.Errorf("subtitles = %v, want the .srt beside the image", body["subtitles"])
	}

	for id, wantCode := range map[string]string{encrypted: "DISC_ENCRYPTED", notADisc: "DISC_UNPLAYABLE"} {
		w, body := c.tvDo(http.MethodPost, "/api/tv/items/"+id+"/play", token, nil)
		if w.Code != http.StatusUnprocessableEntity || body["code"] != wantCode {
			t.Errorf("play of an unplayable image = %d %v, want 422 %s", w.Code, body, wantCode)
		}
		if msg, _ := body["error"].(string); msg == "" {
			t.Errorf("an unplayable image was refused without saying why: %v", body)
		}
	}
}

// A link under a TV code dies with the code, for a film out of a disc image
// as for any other.
func TestRevokingACodeStopsADiscFilm(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/films"})
	image, _ := testDisc()
	id := c.upload("film.iso", "/films", image)["id"].(string)

	code := c.mintTVCode("/films", 60)
	token := c.pairTV(code)
	_, body := c.tvDo(http.MethodPost, "/api/tv/items/"+id+"/play", token, nil)
	link := body["url"].(string)
	if w := c.follow(link, "bytes=0-187"); w.Code != http.StatusPartialContent {
		t.Fatalf("before revoking: %d", w.Code)
	}

	if w, body := c.json(http.MethodDelete, "/api/tv/codes/"+code, nil); w.Code >= 300 {
		t.Fatalf("revoke: %d %v", w.Code, body)
	}
	if w := c.follow(link, "bytes=0-187"); w.Code != http.StatusNotFound {
		t.Errorf("after revoking: %d, want 404", w.Code)
	}
}

// Locking the vault forgets where the films lie, with everything else that
// was read out of the plaintext.
func TestLockingForgetsWhatWasFoundInDiscImages(t *testing.T) {
	c := newTestClient(t)
	c.setup("stream-me-please", 3)
	image, _ := testDisc()
	file := c.upload("film.iso", "/", image)
	c.mintStream(file["id"].(string))

	hash := file["hash"].(string)
	if _, ok := c.server.discs.get(hash); !ok {
		t.Fatal("a played disc's title was not remembered")
	}
	if w, _ := c.json(http.MethodPost, "/api/vault/lock", nil); w.Code >= 300 {
		t.Fatalf("lock: %d", w.Code)
	}
	if _, ok := c.server.discs.get(hash); ok {
		t.Error("the title outlived the lock")
	}
}

func TestDiscCacheKeepsVerdictsAndStaysBounded(t *testing.T) {
	var c discCache
	if _, ok := c.get("a"); ok {
		t.Error("an empty cache had something in it")
	}
	c.put("a", discVerdict{err: bluray.ErrEncrypted})
	c.put("b", discVerdict{title: &bluray.Title{Playlist: "00001.mpls"}})
	if v, ok := c.get("a"); !ok || !errors.Is(v.err, bluray.ErrEncrypted) {
		t.Errorf("a verdict against an image was not kept: %+v", v)
	}
	if v, ok := c.get("b"); !ok || v.title.Playlist != "00001.mpls" {
		t.Errorf("a title was not kept: %+v", v)
	}

	for i := 0; i < discCacheSize*3; i++ {
		c.put(fmt.Sprint("n", i), discVerdict{})
	}
	if len(c.found) != discCacheSize {
		t.Errorf("the cache holds %d, want it kept to %d", len(c.found), discCacheSize)
	}
	// Putting one that is already there must not push another out.
	var held string
	for k := range c.found {
		held = k
		break
	}
	c.put(held, discVerdict{})
	if len(c.found) != discCacheSize {
		t.Errorf("replacing an entry changed the size to %d", len(c.found))
	}

	c.clear()
	if _, ok := c.get("b"); ok {
		t.Error("clear left something behind")
	}
}

func TestDiscStreamName(t *testing.T) {
	for in, want := range map[string]string{
		"Film.iso":           "Film.ts",
		"Some.Film.1982.ISO": "Some.Film.1982.ts",
		"A Film (2010).iso":  "A Film (2010).ts",
	} {
		if got := discStreamName(in); got != want {
			t.Errorf("discStreamName(%q) = %q, want %q", in, got, want)
		}
	}
}
