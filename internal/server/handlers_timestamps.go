package server

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/chinmay28/SandHarbor/internal/vault"
)

// Fixing the dates of files that arrived with the wrong one: the organizer's
// "Fix timestamps". See vault/timefix.go for what is read and why.
//
// Two endpoints, shaped as the other tidying tools are. The GET plans from the
// index alone and changes nothing. The POST carries out a batch of that plan,
// and the browser sends the plan a batch at a time, so a run that stops has
// corrected exactly the batches that answered and running it again finds only
// what is left.

// maxTimeReads is how many files one request may ask to have opened. Each is a
// fetch from the accounts, so this is what bounds how long a request is held
// and how much a browser that went away leaves running.
const maxTimeReads = 32

// maxTimeSets is how many times one request may set outright.
const maxTimeSets = 5000

// handleFolderTimestamps answers which files under a folder carry a date in
// their name that the stored modified time disagrees with, and which pictures
// and videos would have to be opened to find theirs. `deep=1` looks below the
// folder as well; `offset` is the viewer's minutes east of UTC, which is the
// zone a name's wall clock is read in. Read-only: no account is contacted.
func (s *Server) handleFolderTimestamps(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dir := q.Get("path")
	if dir == "" {
		dir = "/"
	}

	opts := vault.TimeFixOptions{Deep: q.Get("deep") == "1"}
	if raw := q.Get("offset"); raw != "" {
		minutes, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "offset from UTC must be a whole number of minutes", "BAD_REQUEST")
			return
		}
		opts.Offset = minutes
	}

	v, _ := s.Vault()
	plan, err := v.TimeFixes(requestScope(r), dir, opts)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// timestampsFixRequest is one batch of a plan.
type timestampsFixRequest struct {
	// Set is files and the modified time each should carry: rows of the
	// plan's `fixes`, each with its `to`.
	Set []vault.TimeOrder `json:"set"`

	// Read is files to open and correct from the date inside them: IDs from
	// the plan's `readable`.
	Read []string `json:"read"`

	// Offset is the viewer's minutes east of UTC, for a date inside a file
	// that records no zone of its own.
	Offset int `json:"offset"`
}

// handleFolderTimestampsFix carries out one batch: it sets the times it is
// given, and opens the files it is asked to read and corrects each one whose
// stored time is more than a day from the date inside it.
//
// Only the modified time of an entry changes, in one index write for the sets
// and one for the reads. A file that could not be read back is listed in
// `failed` and an ID naming nothing in `missing`; neither fails the batch.
// Reading is the one part of the organizer's tidying that contacts an
// account, and it only reads.
func (s *Server) handleFolderTimestampsFix(w http.ResponseWriter, r *http.Request) {
	var req timestampsFixRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	switch {
	case len(req.Set) == 0 && len(req.Read) == 0:
		writeError(w, http.StatusBadRequest, "nothing to set and nothing to read", "BAD_REQUEST")
		return
	case len(req.Set) > maxTimeSets:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d times can be set in one request", maxTimeSets), "BAD_REQUEST")
		return
	case len(req.Read) > maxTimeReads:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d files can be read in one request", maxTimeReads), "BAD_REQUEST")
		return
	}
	for _, order := range req.Set {
		if order.ID == "" || order.Modified.IsZero() {
			writeError(w, http.StatusBadRequest, "every time to set needs a file id and a modified time", "BAD_REQUEST")
			return
		}
	}

	ctx, cancel := contextWithTimeout(r, 30*time.Minute)
	defer cancel()

	v, _ := s.Vault()
	set, err := v.SetTimes(req.Set)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	reading := &vault.TimeReading{Missing: []string{}, Failed: []string{}}
	if len(req.Read) > 0 {
		reading, err = v.FixTimesFromContent(ctx, req.Read, vault.TimeFixOptions{Offset: req.Offset})
		if err != nil {
			vaultErrorResponse(w, err)
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"fixed":   set.Fixed + reading.Fixed,
		"settled": reading.Settled,
		"undated": reading.Undated,
		"missing": append(set.Missing, reading.Missing...),
		"failed":  reading.Failed,
	})
}
