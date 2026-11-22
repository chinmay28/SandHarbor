package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/SandHarbor/internal/bluray/bluraytest"
	"github.com/chinmay28/SandHarbor/internal/remux"
)

// withRemuxTools stands in for a machine with, or without, ffmpeg.
func withRemuxTools(t *testing.T, tools remux.Tools, err error) {
	t.Helper()
	old := findRemuxTools
	findRemuxTools = func() (remux.Tools, error) { return tools, err }
	t.Cleanup(func() { findRemuxTools = old })
}

// Everything that can be refused is refused when the conversion is asked
// for, with a reason, rather than as a run that fails some minutes in.
func TestConvertingRefusesWhatItCannotDo(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	image, _ := testDisc()
	clip, _ := bluraytest.Clip(1, 64)

	disc := c.upload("Film.iso", "/", image)["id"].(string)
	film := c.upload("Other.mkv", "/", []byte("a film already"))["id"].(string)
	encrypted := c.upload("Bought.iso", "/",
		bluraytest.Disc{Clips: map[int][]byte{1: bluraytest.Encrypt(clip)}}.Image())["id"].(string)

	start := func(id string) (int, string) {
		w, body := c.json(http.MethodPost, "/api/files/"+id+"/mkv", nil)
		code, _ := body["code"].(string)
		return w.Code, code
	}

	// A machine with no ffmpeg says so, in the status a page asks before it
	// offers the button and again if asked anyway.
	withRemuxTools(t, remux.Tools{}, remux.ErrNoFFmpeg)
	if _, status := c.json(http.MethodGet, "/api/mkv", nil); status["available"] != false || status["reason"] == "" {
		t.Errorf("status without ffmpeg = %v, want unavailable with a reason", status)
	}
	if code, name := start(disc); code != http.StatusNotImplemented || name != "NO_FFMPEG" {
		t.Errorf("start without ffmpeg = %d %s, want 501 NO_FFMPEG", code, name)
	}

	withRemuxTools(t, remux.Tools{FFmpeg: "/nonexistent/ffmpeg", FFprobe: "/nonexistent/ffprobe"}, nil)
	if _, status := c.json(http.MethodGet, "/api/mkv", nil); status["available"] != true {
		t.Errorf("status with ffmpeg = %v, want available", status)
	}
	if code, name := start(film); code != http.StatusUnprocessableEntity || name != "NOT_A_DISC" {
		t.Errorf("a file that is not a disc image = %d %s, want 422 NOT_A_DISC", code, name)
	}
	if code, name := start(encrypted); code != http.StatusUnprocessableEntity || name != "DISC_ENCRYPTED" {
		t.Errorf("an encrypted disc = %d %s, want 422 DISC_ENCRYPTED", code, name)
	}

	// The name it would make is taken — in whatever case.
	c.upload("film.MKV", "/", []byte("made earlier"))
	if code, name := start(disc); code != http.StatusConflict || name != "EXISTS" {
		t.Errorf("a name already taken = %d %s, want 409 EXISTS", code, name)
	}
}

func TestMkvNameIsTheImagesOwn(t *testing.T) {
	for in, want := range map[string]string{"Film.iso": "Film.mkv", "A Film (1982).ISO": "A Film (1982).mkv", "Some.Film.2160p.iso": "Some.Film.2160p.mkv"} {
		if got := mkvName(in); got != want {
			t.Errorf("mkvName(%q) = %q, want %q", in, got, want)
		}
	}
}

// waitForMkv polls the run until it is over and returns its last status.
func (c *testClient) waitForMkv() map[string]any {
	c.t.Helper()
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		_, status := c.json(http.MethodGet, "/api/mkv", nil)
		if status["running"] != true {
			return status
		}
	}
	c.t.Fatal("the conversion did not finish")
	return nil
}

// The whole of it, with the real ffmpeg where there is one: a disc image of
// two clips goes in, and a Matroska file of the film is stored beside it.
func TestConvertingADiscImageStoresItsFilmAsMKV(t *testing.T) {
	tools, err := remux.Find()
	if err != nil {
		t.Skip("no ffmpeg on this machine")
	}
	dir := t.TempDir()
	clips := map[int][]byte{}
	for n, seconds := range map[int]string{1: "3", 2: "5"} {
		file := filepath.Join(dir, "clip.m2ts")
		make := exec.Command(tools.FFmpeg, "-v", "error", "-y",
			"-f", "lavfi", "-i", "testsrc=d="+seconds+":s=320x180:r=24",
			"-f", "lavfi", "-i", "sine=d="+seconds,
			"-c:v", "libx264", "-g", "24", "-c:a", "ac3", "-output_ts_offset", "600",
			"-f", "mpegts", "-mpegts_m2ts_mode", "1", file)
		if out, err := make.CombinedOutput(); err != nil {
			t.Skipf("this ffmpeg cannot make the fixture: %v: %s", err, out)
		}
		clips[n], _ = os.ReadFile(file)
	}
	streams := []bluraytest.Stream{{PID: 0x1011, Coding: 0x1b}, {PID: 0x1100, Coding: 0x81, Language: "fra"}}
	image := bluraytest.Disc{
		Clips: clips,
		Playlists: map[int][]bluraytest.Item{1: {
			{Clip: 1, Seconds: 3, Streams: streams},
			{Clip: 2, Seconds: 5, Streams: streams},
		}},
	}.Image()

	c := newTestClient(t)
	c.setup("pw", 3)
	c.json(http.MethodPost, "/api/folders", map[string]any{"path": "/films"})
	id := c.upload("A Film (1982).iso", "/films", image)["id"].(string)

	w, started := c.json(http.MethodPost, "/api/files/"+id+"/mkv", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("start = %d %v", w.Code, started)
	}
	// Asked again while it runs, it is the same run.
	if w, again := c.json(http.MethodPost, "/api/files/"+id+"/mkv", nil); w.Code == http.StatusOK && again["id"] != started["id"] {
		t.Errorf("a second ask joined a different run: %v", again)
	}

	status := c.waitForMkv()
	if msg, _ := status["error"].(string); msg != "" {
		t.Fatalf("the conversion failed: %s", msg)
	}
	result, _ := status["result"].(map[string]any)
	if result == nil || result["name"] != "A Film (1982).mkv" || result["dir"] != "/films" {
		t.Fatalf("result = %v", result)
	}
	if result["video"] != 1.0 || result["audio"] != 1.0 {
		t.Errorf("tracks = %v video, %v audio; want 1 and 1", result["video"], result["audio"])
	}

	// The new file is in the folder beside the image, which is untouched.
	_, listing := c.json(http.MethodGet, "/api/files?path=/films", nil)
	names := map[string]bool{}
	for _, f := range listing["files"].([]any) {
		names[f.(map[string]any)["name"].(string)] = true
	}
	if !names["A Film (1982).iso"] || !names["A Film (1982).mkv"] || len(names) != 2 {
		t.Errorf("the folder holds %v, want the image and the film", names)
	}

	// And it is the film: both clips end to end as one, starting at nought,
	// with the soundtrack in the language the disc gives it.
	got := c.do(http.MethodGet, "/api/files/"+result["id"].(string)+"/content", nil, "")
	out := filepath.Join(dir, "out.mkv")
	if err := os.WriteFile(out, got.Body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(tools.FFprobe, "-v", "error", "-show_entries",
		"format=format_name,duration,start_time:stream=codec_name:stream_tags=language", "-of", "json", out).Output()
	if err != nil {
		t.Fatalf("ffprobe of the stored file: %v", err)
	}
	var doc struct {
		Format struct {
			Name     string `json:"format_name"`
			Duration string `json:"duration"`
			Start    string `json:"start_time"`
		} `json:"format"`
		Streams []struct {
			Codec string            `json:"codec_name"`
			Tags  map[string]string `json:"tags"`
		} `json:"streams"`
	}
	json.Unmarshal(raw, &doc)
	if !strings.Contains(doc.Format.Name, "matroska") || !strings.HasPrefix(doc.Format.Duration, "8.") || !strings.HasPrefix(doc.Format.Start, "0.") {
		t.Errorf("stored file: %+v; want Matroska, about 8 seconds, from nought", doc.Format)
	}
	if len(doc.Streams) != 2 || doc.Streams[0].Codec != "h264" || doc.Streams[1].Codec != "ac3" || doc.Streams[1].Tags["language"] != "fre" && doc.Streams[1].Tags["language"] != "fra" {
		t.Errorf("stored tracks: %+v; want H.264 and French AC-3", doc.Streams)
	}
	frames, _ := exec.Command(tools.FFprobe, "-v", "error", "-select_streams", "v", "-count_frames",
		"-show_entries", "stream=nb_read_frames", "-of", "csv=p=0", out).Output()
	if strings.TrimSpace(string(frames)) != "192" {
		t.Errorf("the film has %s frames, want all 192 of its two clips", strings.TrimSpace(string(frames)))
	}

	// Nothing is left on disk beside the vault.
	left := spoolsLeft(c)
	if len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}

	// Asked a second time, the name is taken.
	if w, body := c.json(http.MethodPost, "/api/files/"+id+"/mkv", nil); w.Code != http.StatusConflict || body["code"] != "EXISTS" {
		t.Errorf("converting again = %d %v, want 409 EXISTS", w.Code, body)
	}
}

// Stopping a run stops ffmpeg and keeps nothing.
func TestStoppingAConversionKeepsNothing(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	image, _ := testDisc()
	id := c.upload("Film.iso", "/", image)["id"].(string)

	// A stand-in for ffmpeg that never finishes: the probe answers, the run hangs.
	dir := t.TempDir()
	probe := filepath.Join(dir, "ffprobe")
	hang := filepath.Join(dir, "ffmpeg")
	os.WriteFile(probe, []byte("#!/bin/sh\ncat >/dev/null\necho '{\"streams\":[{\"index\":0,\"id\":\"0x1011\",\"codec_type\":\"video\",\"codec_name\":\"h264\"}]}'\n"), 0o755)
	os.WriteFile(hang, []byte("#!/bin/sh\nexec sleep 600\n"), 0o755)
	withRemuxTools(t, remux.Tools{FFmpeg: hang, FFprobe: probe}, nil)

	if w, body := c.json(http.MethodPost, "/api/files/"+id+"/mkv", nil); w.Code != http.StatusAccepted {
		t.Fatalf("start = %d %v", w.Code, body)
	}
	if w, _ := c.json(http.MethodDelete, "/api/mkv", nil); w.Code != http.StatusOK {
		t.Fatalf("stop = %d", w.Code)
	}
	status := c.waitForMkv()
	if status["result"] != nil {
		t.Errorf("a stopped run reported a result: %v", status)
	}
	_, listing := c.json(http.MethodGet, "/api/files?path=/", nil)
	if n := len(listing["files"].([]any)); n != 1 {
		t.Errorf("the folder holds %d files after a stopped run, want only the image", n)
	}
	left := spoolsLeft(c)
	if len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

// spoolsLeft lists the temporary files beside the vault.
func spoolsLeft(c *testClient) []string {
	c.t.Helper()
	v, err := c.server.Vault()
	if err != nil {
		c.t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(v.Path()), ".sand-upload-*"))
	return left
}
