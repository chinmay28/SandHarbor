package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

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

// DefaultMaxExtractSize is the ceiling on what one uploaded archive may unpack
// to. A request is capped at MaxUploadSize, but what is inside an archive is
// not what it weighs: this is the cap on the other side of the compression, so
// a few megabytes of zeros cannot ask for a terabyte of somebody's clouds.
const DefaultMaxExtractSize = 32 << 30 // 32 GiB

// maxExtractEntries caps how many entries one archive may hold — the same
// ceiling the browser puts on a dropped folder, and for the same reason.
const maxExtractEntries = 20000

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

// extractor unpacks one uploaded archive into the vault.
type extractor struct {
	v     *vault.Vault
	scope vault.Scope
	opts  vault.UploadOptions
	// ensure makes a folder, and every folder above it, if it is not there.
	ensure func(folder string) error
	limits unpack.Limits
}

// extract unpacks the archive in src, uploaded as place, into a folder named
// after it, and reports what happened.
//
// The first result is always the archive's own: OK if it was read to its end,
// with Extracted saying how many files it gave up, and a warning for anything
// in it that is not a file. After it comes one result for each entry that
// could not be stored, named by its path under the new folder. Files that did
// store are counted rather than listed, since an archive of ten thousand of
// them would otherwise answer with ten thousand lines saying nothing went
// wrong. The second return is that count.
func (x *extractor) extract(ctx context.Context, place uploadPlace, src io.ReaderAt, size int64) ([]uploadResult, int) {
	archive := uploadResult{Name: place.Label}
	stored := 0
	archive.Extracted = &stored

	format, ok := unpack.FormatOf(place.Name)
	if !ok {
		archive.fail(errors.New("only a .zip, .tar.gz or .tgz archive can be extracted"), vault.UploadPlacing)
		return []uploadResult{archive}, 0
	}
	stem, err := vault.SanitizeName(unpack.FolderName(place.Name))
	if err != nil {
		archive.fail(err, vault.UploadPlacing)
		return []uploadResult{archive}, 0
	}
	root := vault.JoinPath(place.Dir, stem)

	var failed []uploadResult
	refuse := func(label string, err error, stage vault.UploadStage) {
		result := uploadResult{Name: label}
		result.fail(err, stage)
		failed = append(failed, result)
	}

	skipped, err := unpack.Walk(format, src, size, x.limits, func(entry unpack.Entry, body io.Reader) error {
		label := stem + "/" + entry.Path
		segments, err := pathSegments(entry.Path)
		if err != nil {
			refuse(label, err, vault.UploadPlacing)
			return nil
		}
		// An archive that is one folder named for itself — which is how most
		// are made, project-1.0.tar.gz holding project-1.0/ — is that folder,
		// not a folder holding it.
		if len(segments) > 0 && segments[0] == stem && (entry.Dir || len(segments) > 1) {
			segments = segments[1:]
		}

		dir := root
		if !entry.Dir && len(segments) > 0 {
			for _, segment := range segments[:len(segments)-1] {
				dir = vault.JoinPath(dir, segment)
			}
		} else {
			for _, segment := range segments {
				dir = vault.JoinPath(dir, segment)
			}
		}
		if err := x.ensure(dir); err != nil {
			refuse(label, err, vault.UploadPlacing)
			return nil
		}
		if entry.Dir || len(segments) == 0 {
			return nil
		}

		opts := x.opts
		opts.ModifiedAt = entry.ModTime
		_, warnings, err := x.v.UploadStream(ctx, x.scope, dir, segments[len(segments)-1], body, opts)
		for _, w := range warnings {
			archive.Warnings = append(archive.Warnings, entry.Path+": "+w)
		}
		if err != nil {
			refuse(label, err, vault.UploadScattering)
			// The request is over, not this file: nothing after it can
			// store either, and saying so twenty thousand times helps nobody.
			return ctx.Err()
		}
		stored++
		return nil
	})

	if n := len(skipped); n > 0 {
		named := make([]string, 0, maxNamedSkips)
		for _, skip := range skipped[:min(n, maxNamedSkips)] {
			named = append(named, fmt.Sprintf("%s (%s)", skip.Path, skip.Reason))
		}
		more := ""
		if n > maxNamedSkips {
			more = fmt.Sprintf(" and %d more", n-maxNamedSkips)
		}
		archive.Warnings = append(archive.Warnings,
			fmt.Sprintf("left out what is not a file: %s%s", strings.Join(named, ", "), more))
	}

	switch {
	case err == nil:
		archive.OK = true
	case errors.Is(err, unpack.ErrTooLarge):
		archive.fail(fmt.Errorf("it unpacks to more than the %s one archive may — extract it yourself and upload the folder",
			formatBytes(x.limits.MaxBytes)), vault.UploadReading)
	case errors.Is(err, unpack.ErrTooManyEntries):
		archive.fail(fmt.Errorf("it holds more than the %d entries one archive may — extract it yourself and upload the folder a part at a time",
			x.limits.MaxEntries), vault.UploadReading)
	default:
		archive.fail(err, vault.UploadReading)
	}
	if !archive.OK && stored > 0 {
		archive.Error += fmt.Sprintf(" (%d %s from before that %s in %s)",
			stored, plural(stored, "file", "files"), plural(stored, "is", "are"), root)
	}
	return append([]uploadResult{archive}, failed...), stored
}
