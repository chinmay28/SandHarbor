package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/chinmay28/SandHarbor/internal/unpack"
	"github.com/chinmay28/SandHarbor/internal/vault"
)

// Unpacking an uploaded archive into a folder instead of storing it as it is.
//
// An upload names the files it wants treated this way ("extract-N"), and each
// one becomes a folder beside where the archive itself would have landed,
// named after it: photos.zip posted to /inbox is /inbox/photos, holding what
// the archive held. The archive is never stored. Everything in it goes through
// the same door a file dropped in a folder goes through — the same path check,
// the same scatter — because an archive's paths are its author's word about a
// tree, exactly as a form's "rel-N" is the browser's, and neither is believed.
//
// The request only brings the archive here. Unpacking it is a run of its own
// (extract_runs.go) that carries on whether or not the page that uploaded it
// is still open, so the request is answered as soon as the archive is safely
// on this machine's disk and the run has a place in the queue.

// DefaultMaxExtractSize is the ceiling on what one uploaded archive may unpack
// to. A request is capped at MaxUploadSize, but what is inside an archive is
// not what it weighs: this is the cap on the other side of the compression, so
// a few megabytes of zeros cannot ask for a terabyte of somebody's clouds.
const DefaultMaxExtractSize = 256 << 30 // 256 GiB

// maxExtractEntries caps how many entries one archive may hold — the same
// ceiling the browser puts on a dropped folder, and for the same reason.
const maxExtractEntries = 200000

// maxNamedSkips is how many passed-over entries an answer names before it
// starts counting them instead.
const maxNamedSkips = 10

// formExtract reports whether the nth file of an upload was sent to be
// unpacked rather than stored. Named for the file's position, like every other
// per-file field.
func formExtract(r *http.Request, index int) bool {
	values := r.MultipartForm.Value[fmt.Sprintf("extract-%d", index)]
	return len(values) > 0 && (values[0] == "1" || values[0] == "true")
}

// extractLimits is how much one archive may unpack to on this server.
func (s *Server) extractLimits() unpack.Limits {
	limit := s.MaxExtractSize
	if limit <= 0 {
		limit = DefaultMaxExtractSize
	}
	return unpack.Limits{MaxEntries: maxExtractEntries, MaxBytes: limit}
}

// extractor unpacks one archive into the vault.
type extractor struct {
	v     *vault.Vault
	scope vault.Scope
	opts  vault.UploadOptions
	// root is the folder the archive unpacks into, and stem that folder's
	// own name.
	root, stem string
	format     unpack.Format
	limits     unpack.Limits
}

// planExtract settles, before anything is kept, what unpacking the archive
// uploaded as place would mean: which format it is and which folder it makes.
// An archive that cannot be unpacked at all is refused here, while the
// request that brought it is still there to be told.
func planExtract(place uploadPlace) (format unpack.Format, root, stem string, err error) {
	format, ok := unpack.FormatOf(place.Name)
	if !ok {
		return 0, "", "", errors.New("only a .zip, .tar.gz or .tgz archive can be extracted")
	}
	stem, err = vault.SanitizeName(unpack.FolderName(place.Name))
	if err != nil {
		return 0, "", "", err
	}
	return format, vault.JoinPath(place.Dir, stem), stem, nil
}

// queueExtract takes an uploaded archive off the request that brought it and
// queues it to be unpacked. The archive is copied beside the vault first: the
// multipart parser's own copy is gone the moment the request ends, and the
// run will only be starting then.
func (s *Server) queueExtract(v *vault.Vault, x extractor, name string, src io.Reader, size int64) (extractRun, error) {
	spool, discard, err := v.NewSpool()
	if err != nil {
		return extractRun{}, err
	}
	if _, err := io.Copy(spool, src); err != nil {
		discard()
		return extractRun{}, fmt.Errorf("could not keep the archive to extract it: %w", err)
	}
	return s.queueExtractOf(x, name, spool, size, discard)
}

// queueExtractOf queues an archive that is already in a file of its own, and
// takes that file over: discard is called when the run is done with it, or at
// once if the run is refused. An archive that arrived in pieces is already
// beside the vault, so it comes straight here.
func (s *Server) queueExtractOf(x extractor, name string, spool io.ReaderAt, size int64, discard func()) (extractRun, error) {
	run := extractRun{Name: name, Dir: x.root, Vault: string(x.scope), At: extractProgress{Total: size}}
	return s.extracts.add(run, func(ctx context.Context, onProgress func(extractProgress)) (*extractReport, error) {
		report, err := x.extract(ctx, spool, size, onProgress)
		log.Printf("extracted %s into %s: %d stored, %d failed, %d left out",
			name, x.root, report.Extracted, report.Failed, report.Skipped)
		return report, err
	}, discard)
}

// extractProgressEvery is how often a run says how much of its archive it has
// read, between the entries it also reports at.
const extractProgressEvery = 500 * time.Millisecond

// extract unpacks the archive in src into x.root and reports what happened.
// The report is never nil, and stands beside an error as what was managed
// before it: an archive cut short has still given up the files before the cut.
//
// Folders are made as entries need them rather than up front, so an archive
// that is refused whole — over a limit, or not an archive — leaves nothing.
func (x extractor) extract(ctx context.Context, src io.ReaderAt, size int64, onProgress func(extractProgress)) (*extractReport, error) {
	report := &extractReport{}
	counted := &countingReaderAt{r: src}

	// Said on a clock rather than as each entry ends: a film inside an
	// archive is minutes with no entry ending, and the bar should still move
	// and say which file it is on. The walk and the clock are two goroutines,
	// so what the clock reads is kept apart from the report the walk writes.
	var (
		files   atomic.Int64
		current atomic.Value
	)
	current.Store("")
	tell := func() {
		onProgress(extractProgress{
			Files: int(files.Load()), Done: min(counted.n.Load(), size), Total: size,
			Current: current.Load().(string),
		})
	}
	tell()
	stop := every(extractProgressEvery, tell)

	made := map[string]bool{}
	ensure := func(folder string) error {
		if made[folder] {
			return nil
		}
		if !x.v.FolderExists(x.scope, folder) {
			if err := x.v.Mkdir(x.scope, folder); err != nil {
				return err
			}
		}
		made[folder] = true
		return nil
	}
	refuse := func(path string, err error) {
		report.Failed++
		note(&report.Failures, path+": "+err.Error())
	}

	skipped, err := unpack.Walk(x.format, counted, size, x.limits, func(entry unpack.Entry, body io.Reader) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		segments, err := pathSegments(entry.Path)
		if err != nil {
			refuse(entry.Path, err)
			return nil
		}
		// An archive that is one folder named for itself — which is how most
		// are made, project-1.0.tar.gz holding project-1.0/ — is that folder,
		// not a folder holding it.
		if len(segments) > 0 && segments[0] == x.stem && (entry.Dir || len(segments) > 1) {
			segments = segments[1:]
		}

		folders := segments
		if !entry.Dir && len(segments) > 0 {
			folders = segments[:len(segments)-1]
		}
		dir := x.root
		for _, segment := range folders {
			dir = vault.JoinPath(dir, segment)
		}
		if err := ensure(dir); err != nil {
			refuse(entry.Path, err)
			return nil
		}
		if entry.Dir || len(segments) == 0 {
			return nil
		}

		current.Store(entry.Path)
		opts := x.opts
		opts.ModifiedAt = entry.ModTime
		_, warnings, err := x.v.UploadStream(ctx, x.scope, dir, segments[len(segments)-1], body, opts)
		for _, w := range warnings {
			note(&report.Warnings, entry.Path+": "+w)
		}
		if err != nil {
			// Stopped, not failed: the file in hand is not a casualty to
			// list, and nothing after it can store either.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			refuse(entry.Path, err)
			return nil
		}
		report.Extracted++
		files.Add(1)
		return nil
	})
	stop()
	current.Store("")
	tell()

	for _, skip := range skipped {
		report.Skipped++
		note(&report.Skips, fmt.Sprintf("%s (%s)", skip.Path, skip.Reason))
	}

	switch {
	case errors.Is(err, unpack.ErrTooLarge):
		err = fmt.Errorf("it unpacks to more than the %s one archive may — extract it yourself and upload the folder",
			formatBytes(x.limits.MaxBytes))
	case errors.Is(err, unpack.ErrTooManyEntries):
		err = fmt.Errorf("it holds more than the %d entries one archive may — extract it yourself and upload the folder a part at a time",
			x.limits.MaxEntries)
	}
	return report, err
}

// countingReaderAt counts what is read through it, for a bar that measures an
// archive by how much of it has been read.
type countingReaderAt struct {
	r io.ReaderAt
	n atomic.Int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n.Add(int64(n))
	return n, err
}

// handleExtractRuns lists the archives waiting, being unpacked and lately
// finished.
func (s *Server) handleExtractRuns(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"runs": s.extracts.all()})
}

// handleExtractStop cancels a waiting or running extraction, or forgets a
// finished one's result. What it had stored stays stored.
func (s *Server) handleExtractStop(w http.ResponseWriter, r *http.Request) {
	if !s.extracts.stop(r.PathValue("run")) {
		writeError(w, http.StatusNotFound, "no extraction by that name is running", "NOT_FOUND")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": s.extracts.all()})
}
