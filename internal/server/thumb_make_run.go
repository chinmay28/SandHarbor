package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/chinmay28/SandHarbor/internal/vault"
)

// Making a folder's missing thumbnails, as a job the server runs — the shape
// of every other long job here (job_watch.go). The page that asked can be
// closed, the phone it was on can sleep: the reading, the drawing and the
// storing are the machine's (vault.MakeThumbs).
//
// Two things set it apart from the other jobs. It holds off the idle lock
// while it runs, as they do, but locking the vault by hand does not end it:
// the run waits, and carries on from the same file when the vault is next
// unlocked. And it never stops for a file: whatever cannot be read, drawn or
// stored is counted, and the run goes on to the end. Only Stop ends it early.
//
// One at a time. A second ask for the same folder joins the run going; any
// other is refused with 409 until it is done.

// thumbMakeTimeout bounds a run. Generous: a folder of twenty thousand
// photographs over a home connection is days, not hours, and a run that sat
// waiting for an unlock over a weekend should still be there on Monday.
const thumbMakeTimeout = 14 * 24 * time.Hour

// thumbMakeWatch holds the run that is going, or the last one that ran.
type thumbMakeWatch = jobWatch[vault.ThumbRun, vault.ThumbRun]

func newThumbMakeWatch() *thumbMakeWatch {
	return newJobWatch[vault.ThumbRun, vault.ThumbRun]()
}

func thumbMakeScope(scope vault.Scope, dir string) string {
	return fmt.Sprintf("%s:%s", scope, dir)
}

// handleThumbPlan counts what a run under ?path= would do, before it starts.
func (s *Server) handleThumbPlan(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}
	v, _ := s.Vault()
	plan, err := v.PlanThumbs(requestScope(r), path)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// handleThumbMakeStart starts a run for {path} (and ?vault=), or joins the one
// already going for it. 202 when it started one, 200 when it joined one, 409
// when another folder's is running (GET says which).
func (s *Server) handleThumbMakeStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	v, _ := s.Vault()
	scope := requestScope(r)
	dir := vault.CleanDir(req.Path)
	// Refused now rather than as a run that fails a moment later.
	if _, err := v.PlanThumbs(scope, dir); err != nil {
		vaultErrorResponse(w, err)
		return
	}

	status, outcome := s.thumbMakes.start(thumbMakeScope(scope, dir),
		func(ctx context.Context, onProgress func(vault.ThumbRun)) (*vault.ThumbRun, error) {
			run, err := v.MakeThumbs(ctx, scope, dir, func(at vault.ThumbRun) {
				onProgress(at)
				s.noteExternalActivity()
			})
			if err != nil {
				return nil, err
			}
			return &run, nil
		}, thumbMakeTimeout)

	switch outcome {
	case jobStarted:
		writeJSON(w, http.StatusAccepted, status)
	case jobJoined:
		writeJSON(w, http.StatusOK, status)
	default:
		writeError(w, http.StatusConflict,
			"thumbnails are already being made for another folder — wait for that to finish", "THUMBS_BUSY")
	}
}

// handleThumbMakeStatus answers with the run going, or the last one that ran.
func (s *Server) handleThumbMakeStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.thumbMakes.status())
}

// handleThumbMakeStop ends the run. What it made is kept, and its counts stand
// as its result, marked as stopped.
func (s *Server) handleThumbMakeStop(w http.ResponseWriter, r *http.Request) {
	s.thumbMakes.cancel()
	writeJSON(w, http.StatusOK, s.thumbMakes.status())
}
