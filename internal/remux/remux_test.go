package remux

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A UHD disc's main title, as ffprobe describes it.
var discStreams = []Stream{
	{Index: 0, PID: 0x1011, Kind: "video", Codec: "hevc"},
	{Index: 1, PID: 0x1100, Kind: "audio", Codec: "dts"},
	{Index: 2, PID: 0x1101, Kind: "audio", Codec: "truehd"},
	{Index: 3, PID: 0x1101, Kind: "audio", Codec: "ac3"},
	{Index: 4, PID: 0x1200, Kind: "subtitle", Codec: "hdmv_pgs_subtitle"},
	{Index: 5, PID: 0x1102, Kind: "audio", Codec: "pcm_bluray", Bits: 24},
	{Index: 6, PID: 0x1201, Kind: "subtitle", Codec: "hdmv_pgs_subtitle"},
	{Index: 7, PID: 0x1015, Kind: "video", Codec: "hevc"},
	{Index: 8, PID: 0x1a00, Kind: "audio", Codec: "none"},
	{Index: 9, PID: 0x1f00, Kind: "data", Codec: "bin_data"},
}

var discLanguages = map[int]string{0x1100: "eng", 0x1101: "fra", 0x1102: "jpn", 0x1200: "eng", 0x1201: "spa", 0x1011: "und"}

func TestArgsCopiesEveryTrackAndLabelsIt(t *testing.T) {
	plan, err := Args(discStreams, discLanguages, "A Film (1982)", "/tmp/out")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(plan.Args, " ")

	// Every stream ffmpeg understands, video then sound then subtitles, each
	// kind in the disc's own order.
	if want := "-map 0:0 -map 0:7 -map 0:1 -map 0:2 -map 0:3 -map 0:5 -map 0:4 -map 0:6 -c copy"; !strings.Contains(got, want) {
		t.Errorf("maps = %q, want them to contain %q", got, want)
	}
	if plan.Video != 2 || plan.Audio != 4 || plan.Subtitles != 2 {
		t.Errorf("tracks = %d video, %d audio, %d subtitles; want 2, 4, 2", plan.Video, plan.Audio, plan.Subtitles)
	}
	if len(plan.Dropped) != 2 {
		t.Errorf("dropped = %v, want the stream with no codec and the data stream", plan.Dropped)
	}

	// Languages by the track's place in the output, not in the input: the
	// TrueHD track and the AC-3 core inside it share a PID and a language.
	for _, want := range []string{
		"-metadata:s:a:0 language=eng",
		"-metadata:s:a:1 language=fra",
		"-metadata:s:a:2 language=fra",
		"-metadata:s:a:3 language=jpn",
		"-metadata:s:s:0 language=eng",
		"-metadata:s:s:1 language=spa",
		"-metadata title=A Film (1982)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("args lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "-metadata:s:v") {
		t.Errorf("a video track was given a language:\n%s", got)
	}

	// The one track that cannot be copied as it is.
	if !strings.Contains(got, "-c:a:3 pcm_s24le") || len(plan.Rewritten) != 1 {
		t.Errorf("the disc's PCM was not rewritten as plain PCM: %v\n%s", plan.Rewritten, got)
	}
	// Read from standard input, written as Matroska whatever the file is called.
	if !strings.Contains(got, "-i pipe:0") || !strings.HasSuffix(got, "-f matroska /tmp/out") {
		t.Errorf("args do not read a pipe and write Matroska:\n%s", got)
	}
}

func TestArgsKeepsPCMAtItsOwnDepth(t *testing.T) {
	for bits, want := range map[int]string{16: "pcm_s16le", 20: "pcm_s24le", 24: "pcm_s24le", 0: "pcm_s24le"} {
		plan, err := Args([]Stream{
			{Index: 0, Kind: "video", Codec: "h264"},
			{Index: 1, Kind: "audio", Codec: "pcm_bluray", Bits: bits},
		}, nil, "", "out")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(plan.Args, " "); !strings.Contains(got, "-c:a:0 "+want) {
			t.Errorf("%d-bit PCM: args %q, want %s", bits, got, want)
		}
	}
}

func TestArgsRefusesAStreamWithNoVideo(t *testing.T) {
	_, err := Args([]Stream{{Index: 0, Kind: "audio", Codec: "ac3"}}, nil, "", "out")
	if !errors.Is(err, ErrNothingToCopy) {
		t.Errorf("err = %v, want ErrNothingToCopy", err)
	}
}

func TestParseProbeReadsFFprobesAnswer(t *testing.T) {
	streams, err := parseProbe([]byte(`{"programs": [], "streams": [
		{"index": 0, "codec_name": "hevc", "codec_type": "video", "id": "0x1011", "bits_per_raw_sample": "10"},
		{"index": 1, "codec_name": "pcm_bluray", "codec_type": "audio", "id": "0x1100", "sample_fmt": "s32", "bits_per_raw_sample": "24"},
		{"index": 4, "codec_name": "pcm_bluray", "codec_type": "audio", "id": "0x1101", "sample_fmt": "s16"},
		{"index": 2, "codec_type": "subtitle", "codec_name": "hdmv_pgs_subtitle", "id": "0x1200"},
		{"index": 3, "codec_type": "data"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Stream{
		{Index: 0, PID: 0x1011, Kind: "video", Codec: "hevc", Bits: 10},
		{Index: 1, PID: 0x1100, Kind: "audio", Codec: "pcm_bluray", Bits: 24},
		{Index: 2, PID: 0x1200, Kind: "subtitle", Codec: "hdmv_pgs_subtitle"},
		{Index: 3, Kind: "data"},
		{Index: 4, PID: 0x1101, Kind: "audio", Codec: "pcm_bluray", Bits: 16},
	}
	// ffprobe lists them in its own order; compare by index.
	sort.Slice(streams, func(i, j int) bool { return streams[i].Index < streams[j].Index })
	if len(streams) != len(want) {
		t.Fatalf("streams = %+v", streams)
	}
	for i := range want {
		if streams[i] != want[i] {
			t.Errorf("stream %d = %+v, want %+v", i, streams[i], want[i])
		}
	}
	if _, err := parseProbe([]byte("not json")); err == nil {
		t.Error("nonsense parsed")
	}
}

func TestFindFollowsTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvFFmpeg, filepath.Join(dir, "ffmpeg"))
	if _, err := Find(); !errors.Is(err, ErrNoFFmpeg) {
		t.Errorf("a path with nothing at it: err = %v, want ErrNoFFmpeg", err)
	}
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tools, err := Find()
	if err != nil || tools.FFmpeg != filepath.Join(dir, "ffmpeg") || tools.FFprobe != filepath.Join(dir, "ffprobe") {
		t.Errorf("Find = %+v, %v", tools, err)
	}
}

func TestTailKeepsTheEnd(t *testing.T) {
	var buf bytes.Buffer
	w := &tail{max: 10, buf: &buf}
	for _, s := range []string{"the beginning ", "is long, and ", "the end"} {
		w.Write([]byte(s))
	}
	if got := buf.String(); got != "d the end" && got != "nd the end" {
		t.Errorf("tail = %q, want the last ten bytes", got)
	}
}

// The whole thing against the real ffmpeg, where there is one: a clip as a
// disc stores it, with the three kinds of soundtrack that behave differently,
// in at one end and a Matroska file out of the other.
func TestRemuxWithRealFFmpeg(t *testing.T) {
	tools, err := Find()
	if err != nil {
		t.Skip("no ffmpeg on this machine")
	}
	dir := t.TempDir()
	clip := filepath.Join(dir, "clip.m2ts")
	make := exec.Command(tools.FFmpeg, "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=d=4:s=320x180:r=24",
		"-f", "lavfi", "-i", "sine=d=4:f=440",
		"-map", "0:v", "-map", "1:a", "-map", "1:a", "-map", "1:a",
		"-c:v", "libx264", "-g", "24",
		"-c:a:0", "ac3", "-c:a:1", "dca", "-strict", "-2", "-ac:a:1", "2", "-c:a:2", "pcm_bluray", "-ac:a:2", "2",
		"-ar", "48000", "-output_ts_offset", "600",
		"-f", "mpegts", "-mpegts_m2ts_mode", "1", clip)
	if out, err := make.CombinedOutput(); err != nil {
		t.Skipf("this ffmpeg cannot make the fixture: %v: %s", err, out)
	}
	m2ts, err := os.ReadFile(clip)
	if err != nil {
		t.Fatal(err)
	}
	// A disc's 192-byte packets, less the four bytes in front of each.
	var ts []byte
	for i := 0; i+192 <= len(m2ts); i += 192 {
		ts = append(ts, m2ts[i+4:i+192]...)
	}

	ctx := context.Background()
	streams, err := Probe(ctx, tools, bytes.NewReader(ts))
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 4 {
		t.Fatalf("probed %d streams, want 4: %+v", len(streams), streams)
	}
	languages := map[int]string{}
	for i, s := range streams {
		if s.Kind == "audio" {
			languages[s.PID] = []string{"", "eng", "fra", "jpn"}[i]
		}
	}

	out := filepath.Join(dir, "out.tmp")
	plan, err := Args(streams, languages, "A Film (1982)", out)
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, tools, plan, bytes.NewReader(ts)); err != nil {
		t.Fatal(err)
	}

	// What came out, asked of ffprobe.
	raw, err := exec.Command(tools.FFprobe, "-v", "error", "-show_entries",
		"format=format_name,duration,start_time:format_tags=title:stream=codec_name,codec_type:stream_tags=language",
		"-of", "json", out).Output()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Format struct {
			Name     string            `json:"format_name"`
			Duration string            `json:"duration"`
			Start    string            `json:"start_time"`
			Tags     map[string]string `json:"tags"`
		} `json:"format"`
		Streams []struct {
			Codec string            `json:"codec_name"`
			Kind  string            `json:"codec_type"`
			Tags  map[string]string `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Format.Name, "matroska") {
		t.Errorf("format = %q, want Matroska", doc.Format.Name)
	}
	// Four seconds, starting at nought rather than at the disc's ten minutes.
	if !strings.HasPrefix(doc.Format.Duration, "4.") && !strings.HasPrefix(doc.Format.Duration, "3.9") {
		t.Errorf("duration = %s, want about 4 seconds", doc.Format.Duration)
	}
	if !strings.HasPrefix(doc.Format.Start, "0.") {
		t.Errorf("start = %s, want the film to begin at nought", doc.Format.Start)
	}
	if doc.Format.Tags["title"] != "A Film (1982)" {
		t.Errorf("title = %q", doc.Format.Tags["title"])
	}
	var got []string
	for _, s := range doc.Streams {
		got = append(got, s.Kind+":"+s.Codec+":"+s.Tags["language"])
	}
	want := "video:h264: audio:ac3:eng audio:dts:fra audio:pcm_s16le:jpn"
	if strings.Join(got, " ") != want {
		t.Errorf("tracks = %q, want %q", strings.Join(got, " "), want)
	}

	// Nothing re-encoded: the two compressed soundtracks are the same packets
	// that went in. The picture's packets are framed differently by the two
	// containers, so it is compared decoded, as the PCM is: every frame and
	// every sample the same.
	for _, sel := range []string{"a:0", "a:1"} {
		in := streamHash(t, tools, clip, sel, true)
		if copied := streamHash(t, tools, out, sel, true); in != copied {
			t.Errorf("stream %s was not copied bit for bit", sel)
		}
	}
	for _, sel := range []string{"v:0", "a:2"} {
		in := streamHash(t, tools, clip, sel, false)
		if copied := streamHash(t, tools, out, sel, false); in != copied {
			t.Errorf("stream %s does not decode to what went in", sel)
		}
	}
}

// streamHash is a digest of one stream, as ffmpeg's own hash muxer takes it:
// of its packets as they are stored, or of what they decode to.
func streamHash(t *testing.T, tools Tools, file, stream string, packets bool) string {
	t.Helper()
	args := []string{"-v", "error", "-i", file, "-map", "0:" + stream}
	if packets {
		args = append(args, "-c", "copy")
	}
	out, err := exec.Command(tools.FFmpeg, append(args, "-f", "hash", "-")...).Output()
	if err != nil {
		t.Fatalf("hashing %s of %s: %v", stream, file, err)
	}
	return strings.TrimSpace(string(out))
}

// A stopped conversion stops ffmpeg, promptly.
func TestRunStopsWithItsContext(t *testing.T) {
	tools, err := Find()
	if err != nil {
		t.Skip("no ffmpeg on this machine")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plan := Plan{Args: []string{"-nostdin", "-f", "mpegts", "-i", "pipe:0", "-f", "null", "-"}}
	if err := Run(ctx, tools, plan, neverEnding{}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

type neverEnding struct{}

func (neverEnding) Read(p []byte) (int, error) { return len(p), nil }
