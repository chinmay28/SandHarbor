// Package remux turns the transport stream of a Blu-ray title into a Matroska
// file, by way of ffmpeg.
//
// Nothing is re-encoded. The picture, every soundtrack and every subtitle
// track are copied as they are into another container — one that says how
// long it is, seeks by an index, carries a language on each track, and that
// every player opens. The one exception is a disc's uncompressed PCM sound,
// which Matroska cannot hold in the form a disc stores it: that is rewritten
// as plain PCM of the same depth, which is the same samples in another order.
//
// ffmpeg is a program on the machine, not part of this one: demuxing and
// muxing every codec a disc can carry is what it is for, and doing it again
// here would be doing it worse. Find looks for it; a machine without it
// cannot convert, and says so.
//
// The package is three steps, each usable alone: Probe asks what streams the
// transport stream holds, Args turns that into ffmpeg's command line (a pure
// function, and where the decisions are), and Run feeds the stream through.
package remux

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ErrNoFFmpeg is returned by Find on a machine with no ffmpeg to run.
var ErrNoFFmpeg = errors.New("remux: ffmpeg is not installed on this machine")

// EnvFFmpeg names the environment variable that points at the ffmpeg to use,
// for a machine where it is not on the PATH. ffprobe is looked for beside it.
const EnvFFmpeg = "SAND_FFMPEG"

// Tools is where the two programs are.
type Tools struct {
	FFmpeg  string
	FFprobe string
}

// Find locates ffmpeg and ffprobe: where EnvFFmpeg says, or else on the PATH.
func Find() (Tools, error) {
	if path := os.Getenv(EnvFFmpeg); path != "" {
		probe := filepath.Join(filepath.Dir(path), "ffprobe"+filepath.Ext(path))
		if isFile(path) && isFile(probe) {
			return Tools{FFmpeg: path, FFprobe: probe}, nil
		}
		return Tools{}, fmt.Errorf("%w: %s names %s, and ffmpeg and ffprobe are not both there",
			ErrNoFFmpeg, EnvFFmpeg, path)
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return Tools{}, ErrNoFFmpeg
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		return Tools{}, fmt.Errorf("%w: ffmpeg is here but ffprobe, which comes with it, is not", ErrNoFFmpeg)
	}
	return Tools{FFmpeg: ffmpeg, FFprobe: ffprobe}, nil
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Stream is one stream of a transport stream, as ffprobe describes it.
type Stream struct {
	// Index is the stream's position in the input, which is how ffmpeg is
	// told which stream is meant.
	Index int
	// PID is the stream's packet ID in the transport stream, which is how a
	// disc's playlist names it.
	PID int
	// Kind is "video", "audio" or "subtitle"; anything else is not copied.
	Kind string
	// Codec is ffmpeg's name for what the stream holds, such as "hevc",
	// "dts", "truehd", "pcm_bluray" or "hdmv_pgs_subtitle".
	Codec string
	// Bits is the sample depth of a PCM stream, or zero.
	Bits int
}

// probeLimits tell ffmpeg how far into the stream to look for what is in it.
// Far: a disc's subtitle streams are declared at the start but may not carry
// a packet for minutes, and a stream ffmpeg has only heard of is one it knows
// too little about to copy. The same limits are given to the probe and to
// the run, so that both see the same streams in the same order.
var probeLimits = []string{"-probesize", "200M", "-analyzeduration", "200M"}

// ProbeBytes is how much of the start of a stream Probe wants to be given:
// enough to cover probeLimits.
const ProbeBytes = 200 << 20

// Probe asks ffprobe what streams a transport stream holds, given its start.
func Probe(ctx context.Context, tools Tools, head io.Reader) ([]Stream, error) {
	args := append([]string{"-v", "error"}, probeLimits...)
	args = append(args, "-f", "mpegts", "-i", "pipe:0",
		"-show_entries", "stream=index,id,codec_type,codec_name,sample_fmt,bits_per_raw_sample",
		"-of", "json")
	cmd := exec.CommandContext(ctx, tools.FFprobe, args...)
	cmd.Stdin = head
	var out, complaints bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &tail{max: 4096, buf: &complaints}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("remux: ffprobe could not read the film: %w: %s", err, strings.TrimSpace(complaints.String()))
	}
	return parseProbe(out.Bytes())
}

// parseProbe reads ffprobe's JSON description of a file's streams.
func parseProbe(data []byte) ([]Stream, error) {
	var doc struct {
		Streams []struct {
			Index     int    `json:"index"`
			ID        string `json:"id"`
			Kind      string `json:"codec_type"`
			Codec     string `json:"codec_name"`
			RawBits   string `json:"bits_per_raw_sample"`
			SampleFmt string `json:"sample_fmt"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("remux: ffprobe's answer could not be read: %w", err)
	}
	var out []Stream
	for _, s := range doc.Streams {
		// The ID is the PID, written as ffprobe writes it: "0x1011".
		pid, _ := strconv.ParseInt(s.ID, 0, 32)
		// The depth of sound: said outright for 20 and 24 bits, and for 16
		// only by the samples being decoded to 16.
		bits, _ := strconv.Atoi(s.RawBits)
		if bits == 0 && s.Kind == "audio" && strings.HasPrefix(s.SampleFmt, "s16") {
			bits = 16
		}
		out = append(out, Stream{Index: s.Index, PID: int(pid), Kind: s.Kind, Codec: s.Codec, Bits: bits})
	}
	return out, nil
}

// Plan is what a conversion will do, worked out before it is run.
type Plan struct {
	// Args is ffmpeg's command line, reading the stream from standard input.
	Args []string
	// Video, Audio and Subtitles count the tracks the file will have.
	Video, Audio, Subtitles int
	// Rewritten names the tracks that are not copied bit for bit: a disc's
	// PCM sound, which is the same samples stored another way.
	Rewritten []string
	// Dropped names the streams left out, because ffmpeg could not tell what
	// they hold.
	Dropped []string
}

// ErrNothingToCopy is returned by Args for a stream with no video in it that
// ffmpeg recognises: there is no film to write.
var ErrNothingToCopy = errors.New("remux: no video stream was found in the film")

// Args works out the ffmpeg command line that writes the given streams to a
// Matroska file at out.
//
// languages gives a stream's language by its PID, as a disc's playlist does;
// a stream it does not name is left unlabelled. title, when not empty, is
// written as the file's title.
//
// The tracks are written video first, then sound, then subtitles, each kind
// in the order the disc has it, which is the order its menus list them in.
func Args(streams []Stream, languages map[int]string, title, out string) (Plan, error) {
	order := map[string]int{"video": 0, "audio": 1, "subtitle": 2}
	var keep []Stream
	var plan Plan
	for _, s := range streams {
		_, known := order[s.Kind]
		if !known || s.Codec == "" || s.Codec == "none" || s.Codec == "unknown" {
			plan.Dropped = append(plan.Dropped, fmt.Sprintf("stream %d (PID %#x)", s.Index, s.PID))
			continue
		}
		keep = append(keep, s)
	}
	sort.SliceStable(keep, func(i, j int) bool { return order[keep[i].Kind] < order[keep[j].Kind] })

	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}
	args = append(args, probeLimits...)
	args = append(args, "-f", "mpegts", "-i", "pipe:0")
	for _, s := range keep {
		args = append(args, "-map", fmt.Sprintf("0:%d", s.Index))
	}
	args = append(args, "-c", "copy")

	// Options for one track name it by its kind and its place among that
	// kind in the output: a:0 is the first soundtrack written.
	letter := map[string]string{"video": "v", "audio": "a", "subtitle": "s"}
	place := map[string]int{}
	for _, s := range keep {
		spec := fmt.Sprintf("%s:%d", letter[s.Kind], place[s.Kind])
		place[s.Kind]++

		if s.Codec == "pcm_bluray" {
			// Matroska has no way to hold a disc's PCM as the disc packs it.
			// The same samples as ordinary PCM, at the depth they have: 20
			// bits are stored in 24 on a disc, and so here.
			codec := "pcm_s24le"
			if s.Bits > 0 && s.Bits <= 16 {
				codec = "pcm_s16le"
			}
			args = append(args, "-c:"+spec, codec)
			plan.Rewritten = append(plan.Rewritten, fmt.Sprintf("soundtrack %d (PCM, stored as %s)", place[s.Kind], codec))
		}
		if lang := languages[s.PID]; lang != "" && s.Kind != "video" {
			args = append(args, "-metadata:s:"+spec, "language="+lang)
		}
	}
	plan.Video, plan.Audio, plan.Subtitles = place["video"], place["audio"], place["subtitle"]
	if plan.Video == 0 {
		return Plan{}, ErrNothingToCopy
	}

	if title != "" {
		args = append(args, "-metadata", "title="+title)
	}
	// The container is named outright: the file being written is a temporary
	// one, whose name says nothing about what it is.
	plan.Args = append(args, "-f", "matroska", out)
	return plan, nil
}

// Run runs ffmpeg with a plan's arguments, feeding it input as the stream.
//
// It returns when ffmpeg has written the file and exited, or with ctx's error
// when ctx ends first, in which case ffmpeg is killed and the file it was
// writing is whatever it had got to. A failure of ffmpeg's own carries the
// last of what it said.
func Run(ctx context.Context, tools Tools, plan Plan, input io.Reader) error {
	cmd := exec.CommandContext(ctx, tools.FFmpeg, plan.Args...)
	cmd.Stdin = input
	var complaints bytes.Buffer
	cmd.Stderr = &tail{max: 4096, buf: &complaints}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("remux: ffmpeg failed: %w: %s", err, strings.TrimSpace(complaints.String()))
	}
	return nil
}

// tail keeps the last max bytes written to it: the end of what a program
// said, which is where it says what went wrong.
type tail struct {
	max int
	buf *bytes.Buffer
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if over := t.buf.Len() - t.max; over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}
