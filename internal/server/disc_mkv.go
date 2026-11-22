package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chinmay28/SandHarbor/internal/remux"
	"github.com/chinmay28/SandHarbor/internal/vault"
)

// Converting a disc image's film to a Matroska file, as a job the server runs.
//
// Playing a film out of a disc image (disc.go) gives a player a transport
// stream: every track is in it, but a player has to be able to read each of a
// disc's kinds of sound and subtitle out of that container, and several
// cannot. A Matroska file of the same film is one everything plays, with its
// tracks named by language. So: read the title out of the image exactly as a
// player would be sent it, hand it to ffmpeg to copy into Matroska
// (internal/remux), and store what comes out as a new file beside the image.
//
// Nothing about the image changes, and nothing is re-encoded. The cost is
// time, and room: the new file is written whole to this machine's disk before
// it is stored, beside the vault where an upload is spooled, and removed
// when it has been.
//
// One at a time: it is tens of gigabytes read from the clouds and as many
// written back. Asking again for the same image joins the run going. The run
// holds off the idle lock, and ends, keeping nothing, if the vault is locked
// by hand or the run is stopped.

// mkvTimeout bounds a run. A UHD disc over a slow link is a day's work.
const mkvTimeout = 72 * time.Hour

// mkvProgress is where a run has got to.
type mkvProgress struct {
	// File is the image being converted, and Name the file being made.
	File string `json:"file"`
	Name string `json:"name"`
	// Stage is "reading" while the film is read out of the image and written
	// as Matroska, then "storing" while the new file goes to the accounts.
	Stage string `json:"stage"`
	// Done and Total are bytes of the stage: of the film read, or of the new
	// file stored.
	Done  int64 `json:"done"`
	Total int64 `json:"total"`
}

// mkvResult is what a finished run made.
type mkvResult struct {
	// ID, Name and Dir are the new file.
	ID   string `json:"id"`
	Name string `json:"name"`
	Dir  string `json:"dir"`
	Size int64  `json:"size"`
	// From is the image it was made from.
	From string `json:"from"`

	// How many tracks of each kind the file has.
	Video     int `json:"video"`
	Audio     int `json:"audio"`
	Subtitles int `json:"subtitles"`
	// Rewritten names tracks stored another way rather than copied, and
	// Dropped the streams left out. Both are usually empty.
	Rewritten []string `json:"rewritten,omitempty"`
	Dropped   []string `json:"dropped,omitempty"`
	// Warnings are what storing the file had to say.
	Warnings []string `json:"warnings,omitempty"`
}

type mkvWatch = jobWatch[mkvProgress, mkvResult]

// findRemuxTools is remux.Find, a variable so a test can stand in for a
// machine with or without ffmpeg.
var findRemuxTools = remux.Find

// mkvStatusResponse is a poll's answer: whether this machine can convert at
// all, and the run going or the last one that ran.
type mkvStatusResponse struct {
	// Available is whether ffmpeg was found, and Reason why not.
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	jobStatus[mkvProgress, mkvResult]
}

func (s *Server) mkvStatus() mkvStatusResponse {
	resp := mkvStatusResponse{jobStatus: s.mkvs.status()}
	if _, err := findRemuxTools(); err != nil {
		resp.Reason = "ffmpeg is not installed on the machine running SAND, which converting a disc image needs."
	} else {
		resp.Available = true
	}
	return resp
}

// mkvName is the name of the file made from a disc image: the image's own,
// as a Matroska file.
func mkvName(image string) string {
	return strings.TrimSuffix(image, path.Ext(image)) + ".mkv"
}

// handleMkvStatus answers with whether this machine can convert, and the run
// going or the last one that ran.
func (s *Server) handleMkvStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.mkvStatus())
}

// handleMkvStop ends the run. Nothing it made is kept.
func (s *Server) handleMkvStop(w http.ResponseWriter, r *http.Request) {
	s.mkvs.cancel()
	writeJSON(w, http.StatusOK, s.mkvStatus())
}

// handleMkvStart starts converting the disc image {id}, or joins the run
// already converting it. 202 when it started one, 200 when it joined one, 409
// when another image's is running or the file it would make is already there.
//
// Everything that can be refused is refused here, before anything long
// begins: a machine with no ffmpeg, a file that is not a disc image, an image
// with no film on it, a name already taken.
func (s *Server) handleMkvStart(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	entry, err := v.Entry(r.PathValue("id"))
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	if !isDiscImage(entry) {
		writeError(w, http.StatusUnprocessableEntity, "Only a disc image can be converted to MKV.", "NOT_A_DISC")
		return
	}
	tools, err := findRemuxTools()
	if err != nil {
		writeError(w, http.StatusNotImplemented, s.mkvStatus().Reason, "NO_FFMPEG")
		return
	}

	// The same run asked for again is joined before anything is looked at:
	// the name it is making is, by now, a name that is taken.
	if st := s.mkvs.status(); st.Running && st.Scope == entry.ID {
		writeJSON(w, http.StatusOK, st)
		return
	}

	ctx, cancel := contextWithTimeout(r, discProbeTimeout)
	film, err := s.discFilmOf(ctx, v, entry, vault.ReadParts)
	cancel()
	if err != nil {
		if unplayableDisc(err) {
			discErrorResponse(w, err)
		} else {
			vaultErrorResponse(w, err)
		}
		return
	}

	scope, _ := v.ScopeOf(entry.ID)
	name := mkvName(entry.Name)
	if listing, err := v.List(scope, entry.Dir); err == nil {
		for _, f := range listing.Files {
			if strings.EqualFold(f.Name, name) {
				writeError(w, http.StatusConflict,
					fmt.Sprintf("%s is already in this folder. Rename or remove it to convert the image again.", f.Name), "EXISTS")
				return
			}
		}
	}

	// The film's own title where it has been matched: what a player shows
	// while the file plays.
	title := strings.TrimSuffix(entry.Name, path.Ext(entry.Name))
	if info := v.Movie(entry.ID); info != nil && info.Title != "" {
		title = info.Title
		if info.Year != 0 {
			title = fmt.Sprintf("%s (%d)", info.Title, info.Year)
		}
	}

	job := mkvJob{s: s, v: v, tools: tools, entry: entry, scope: scope, film: film, name: name, title: title}
	status, outcome := s.mkvs.start(entry.ID, job.run, mkvTimeout)
	switch outcome {
	case jobStarted:
		writeJSON(w, http.StatusAccepted, status)
	case jobJoined:
		writeJSON(w, http.StatusOK, status)
	default:
		writeError(w, http.StatusConflict,
			"Another disc image is being converted. Wait for it to finish, or stop it.", "MKV_BUSY")
	}
}

// mkvJob is one conversion: everything settled when it was asked for.
type mkvJob struct {
	s     *Server
	v     *vault.Vault
	tools remux.Tools
	entry *vault.Entry
	scope vault.Scope
	film  *discFilm
	name  string
	title string
}

// mkvProgressEvery is how often a run says where it has got to.
const mkvProgressEvery = 500 * time.Millisecond

func (j mkvJob) run(ctx context.Context, onProgress func(mkvProgress)) (*mkvResult, error) {
	at := mkvProgress{File: j.entry.Path(), Name: j.name, Stage: "reading"}
	report := func() {
		onProgress(at)
		j.s.noteExternalActivity()
	}

	image, opened, err := j.v.OpenReaderAt(ctx, j.entry.ID)
	if err != nil {
		return nil, err
	}
	if opened.Hash != j.film.hash {
		return nil, errors.New("the disc image changed while the conversion was starting; try again")
	}
	ts := j.film.title.TS(image)
	at.Total = ts.Size()
	report()

	// What is in it, from its start.
	streams, err := remux.Probe(ctx, j.tools, io.NewSectionReader(ts, 0, min(ts.Size(), remux.ProbeBytes)))
	if err != nil {
		return nil, err
	}

	spool, discard, err := j.v.NewSpool()
	if err != nil {
		return nil, err
	}
	defer discard()

	plan, err := remux.Args(streams, j.film.title.Languages, j.title, spool.Name())
	if err != nil {
		return nil, err
	}

	// The film goes through ffmpeg; how far is how much of it has been read.
	var read atomic.Int64
	stop := every(mkvProgressEvery, func() {
		at.Done = read.Load()
		report()
	})
	err = remux.Run(ctx, j.tools, plan, &countingReader{r: io.NewSectionReader(ts, 0, ts.Size()), n: &read})
	stop()
	if err != nil {
		return nil, err
	}

	info, err := spool.Stat()
	if err != nil {
		return nil, fmt.Errorf("reading the converted file: %w", err)
	}
	if info.Size() == 0 {
		return nil, errors.New("ffmpeg wrote nothing")
	}

	// And the new file goes to the accounts, as an upload of it would.
	at = mkvProgress{File: at.File, Name: j.name, Stage: "storing", Total: info.Size()}
	report()
	var (
		mu   sync.Mutex // the parts of a file leave from several goroutines
		last time.Time
	)
	made, warnings, err := j.v.UploadStreamAt(ctx, j.scope, j.entry.Dir, j.name, spool, info.Size(), vault.UploadOptions{
		OnScattered: func(done, size int64) {
			mu.Lock()
			defer mu.Unlock()
			if now := time.Now(); now.Sub(last) >= mkvProgressEvery {
				last = now
				at.Done, at.Total = done, size
				report()
			}
		},
	})
	if err != nil {
		return nil, err
	}

	// The image's film details and poster are the new file's too. Without
	// them it is a nameless row beside the one it came from; failing to copy
	// them is not failing to convert.
	if movie := j.v.Movie(j.entry.ID); movie != nil {
		copied := *movie
		if err := j.v.SetMovie(made.ID, &copied); err != nil {
			log.Printf("converted %s: its film details were not copied: %v", made.Path(), err)
		}
	}
	if thumb, err := j.v.Thumb(ctx, j.entry.ID); err == nil {
		if err := j.v.SetThumb(ctx, made.ID, thumb); err != nil {
			log.Printf("converted %s: its poster was not copied: %v", made.Path(), err)
		}
	}

	log.Printf("converted %s to %s: %d video, %d audio, %d subtitle tracks",
		j.entry.Path(), made.Path(), plan.Video, plan.Audio, plan.Subtitles)
	return &mkvResult{
		ID: made.ID, Name: made.Name, Dir: made.Dir, Size: made.Size, From: j.entry.ID,
		Video: plan.Video, Audio: plan.Audio, Subtitles: plan.Subtitles,
		Rewritten: plan.Rewritten, Dropped: plan.Dropped, Warnings: warnings,
	}, nil
}

// countingReader counts what is read through it.
type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// every calls fn at an interval until the function it returns is called,
// which waits for a call in flight to finish.
func every(interval time.Duration, fn func()) (stop func()) {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				fn()
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}
