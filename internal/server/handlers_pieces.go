package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/chinmay28/SandHarbor/internal/archive"
	"github.com/chinmay28/SandHarbor/internal/vault"
)

// Uploading one large file a piece at a time. See piece_uploads.go for why and
// for the putting back together; this is the three requests around it:
//
//	POST   /api/uploads               says what is coming, and is given an ID
//	PUT    /api/uploads/{id}?offset=  one piece, as the request's own body
//	POST   /api/uploads/{id}/finish   the file is whole: store it, or unpack it
//	DELETE /api/uploads/{id}          give it up
//
// Finishing does to the file exactly what POST /api/files would have done had
// it fitted in one request, and answers in the same shape, so everything the
// browser knows about a result it knows about this one.

// DefaultMaxPieceUploadSize is the largest file that may arrive in pieces.
// Not a limit on what the vault can hold: the file sits whole on this
// machine's disk before any of it is stored, and this is the bound on that.
const DefaultMaxPieceUploadSize = 128 << 30 // 128 GiB

// pieceSize is how much the browser is asked to send per piece, and
// maxPieceSize the most one piece may be. Small enough that a piece lost to a
// bad connection is seconds of sending to repeat, and well under what any
// proxy in front of this refuses.
const (
	pieceSize    = 16 << 20
	maxPieceSize = 64 << 20
)

// piecePlan is what was settled about a file when its upload was opened:
// where it goes, how it is stored, and whether it is stored at all or
// unpacked instead.
type piecePlan struct {
	scope   vault.Scope
	place   uploadPlace
	opts    vault.UploadOptions
	extract *extractor
}

// pieceUploadRequest opens an upload. It describes the one file the way a
// multipart upload's fields describe each of its files.
type pieceUploadRequest struct {
	Path string `json:"path"`
	Name string `json:"name"`
	// Rel is the path the file had inside an uploaded folder, if it had one.
	Rel  string `json:"rel,omitempty"`
	Size int64  `json:"size"`
	// Mod is the file's modification time, in milliseconds since the epoch.
	Mod       int64    `json:"mod,omitempty"`
	Overwrite bool     `json:"overwrite,omitempty"`
	Accounts  []string `json:"accounts,omitempty"`
	Scheme    string   `json:"scheme,omitempty"`
	// Extract asks for the file, an archive, to be unpacked into a folder
	// rather than stored.
	Extract bool `json:"extract,omitempty"`
}

func (s *Server) maxPieceUpload() int64 {
	if s.MaxPieceUploadSize > 0 {
		return s.MaxPieceUploadSize
	}
	return DefaultMaxPieceUploadSize
}

// handlePieceUploadBegin opens an upload. Everything that can be refused about
// the file is refused here, before a byte of it is sent: a path it may not
// have, a name that is not an archive when it was to be extracted, a size
// this machine will not hold.
func (s *Server) handlePieceUploadBegin(w http.ResponseWriter, r *http.Request) {
	var req pieceUploadRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body", "BAD_REQUEST")
		return
	}
	if req.Size <= 0 {
		writeError(w, http.StatusBadRequest, "the file's size is needed to upload it in pieces", "BAD_REQUEST")
		return
	}
	if limit := s.maxPieceUpload(); req.Size > limit {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"that file is over the %s one upload may carry — import it from the machine running SAND instead",
			formatBytes(limit)), "TOO_LARGE")
		return
	}

	dir := req.Path
	if dir == "" {
		dir = "/"
	}
	plan := piecePlan{scope: requestScope(r), place: placeUpload(dir, req.Rel, req.Name)}
	if plan.place.Err != nil {
		writeError(w, http.StatusBadRequest, plan.place.Err.Error(), "BAD_PATH")
		return
	}
	var scheme archive.Scheme
	if req.Scheme != "" {
		var err error
		if scheme, err = archive.ParseScheme(req.Scheme); err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "BAD_SCHEME")
			return
		}
	}
	plan.opts = vault.UploadOptions{Overwrite: req.Overwrite, Accounts: req.Accounts, Scheme: scheme}
	if req.Mod > 0 {
		plan.opts.ModifiedAt = time.UnixMilli(req.Mod).UTC()
	}

	v, _ := s.Vault()
	if req.Extract {
		x := extractor{v: v, scope: plan.scope, limits: s.extractLimits(), opts: plan.opts}
		// The archive's own time is not its contents': each entry has one.
		x.opts.ModifiedAt = time.Time{}
		var err error
		if x.format, x.root, x.stem, err = planExtract(plan.place); err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "NOT_AN_ARCHIVE")
			return
		}
		plan.extract = &x
	}

	spool, discard, err := v.NewSpool()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "SPOOL_ERROR")
		return
	}
	up, err := s.pieces.begin(req.Size, plan, spool, discard)
	if err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error(), "TOO_MANY_UPLOADS")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": up.ID, "piece_size": pieceSize, "received": 0})
}

// pieceErrorResponse answers for a piece upload that could not be found.
func pieceErrorResponse(w http.ResponseWriter, err error) {
	writeError(w, http.StatusNotFound, err.Error(), "NO_UPLOAD")
}

// handlePieceUploadWrite takes one piece. The answer always says how much of
// the file has arrived — on a piece that did not start there, with 409, which
// is how a sender that lost an answer finds its place again.
func (s *Server) handlePieceUploadWrite(w http.ResponseWriter, r *http.Request) {
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "offset must say where in the file this piece starts", "BAD_REQUEST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPieceSize)

	received, err := s.pieces.write(r.PathValue("id"), offset, r.Body)
	var misplaced *errPieceOffset
	var tooBig *http.MaxBytesError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"received": received})
	case errors.Is(err, errNoPieceUpload):
		pieceErrorResponse(w, err)
	case errors.As(err, &misplaced):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": err.Error(), "code": "OFFSET", "received": received,
		})
	case errors.As(err, &tooBig):
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("one piece may be at most %s", formatBytes(maxPieceSize)), "TOO_LARGE")
	case errors.Is(err, errPieceTooLong):
		writeError(w, http.StatusBadRequest, err.Error(), "TOO_LONG")
	default:
		// Cut short, most likely by the connection: what arrived is kept,
		// and the sender — if it is still there to hear — is told how much.
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "the piece did not arrive whole", "code": "READ_ERROR", "received": received,
		})
	}
}

// handlePieceUploadAbort gives an upload up.
func (s *Server) handlePieceUploadAbort(w http.ResponseWriter, r *http.Request) {
	if !s.pieces.abort(r.PathValue("id")) {
		pieceErrorResponse(w, errNoPieceUpload)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// handlePieceUploadFinish does with the whole file what was planned for it,
// and answers as POST /api/files does: one result, 201 if it was stored or
// taken to be unpacked and 502 if it was not.
func (s *Server) handlePieceUploadFinish(w http.ResponseWriter, r *http.Request) {
	up, err := s.pieces.take(r.PathValue("id"))
	var missing *errPiecesMissing
	switch {
	case errors.As(err, &missing):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": err.Error(), "code": "INCOMPLETE", "received": missing.Received,
		})
		return
	case err != nil:
		pieceErrorResponse(w, err)
		return
	}
	plan := up.Plan
	result := uploadResult{Name: plan.place.Label}
	answer := func(stored int) {
		status := http.StatusCreated
		if !result.OK {
			status = http.StatusBadGateway
		}
		writeJSON(w, status, map[string]any{"results": []uploadResult{result}, "stored": stored})
	}

	v, _ := s.Vault()
	if !v.FolderExists(plan.scope, plan.place.Dir) {
		if err := v.Mkdir(plan.scope, plan.place.Dir); err != nil {
			up.Discard()
			result.fail(err, vault.UploadPlacing)
			answer(0)
			return
		}
	}

	// An archive to be unpacked is handed to the run that does it, file and
	// all, and the request is done. See handlers_extract.go.
	if plan.extract != nil {
		run, err := s.queueExtractOf(*plan.extract, plan.place.Label, up.File, up.Size, up.Discard)
		if err != nil {
			result.fail(err, vault.UploadReading)
			answer(0)
			return
		}
		result.OK, result.Run = true, run.ID
		answer(0)
		return
	}

	defer up.Discard()
	ctx, cancel := contextWithTimeout(r, uploadTimeout)
	defer cancel()
	entry, warnings, err := v.UploadStreamAt(ctx, plan.scope, plan.place.Dir, plan.place.Name, up.File, up.Size, plan.opts)
	result.Warnings = warnings
	if err != nil {
		result.fail(err, vault.UploadScattering)
		answer(0)
		return
	}
	result.OK, result.File = true, entry
	answer(1)
}
