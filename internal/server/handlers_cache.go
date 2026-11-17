package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/chinmay28/SandHarbor/internal/vault"
)

// Keeping files on this machine over HTTP: the pins, the room the cache has,
// and the loop that fills it while the vault is open. What a pin is and what
// is kept lives in vault/localcache.go; this file is only the door to it.

const (
	// cacheTick is how often the loop looks for something to fetch when
	// nothing has woken it.
	cacheTick = time.Minute
	// cacheStepBudget is how much one step fetches before the loop checks
	// that the vault is still open and nobody has changed the pins.
	cacheStepBudget = 256 << 20
	// cacheStepTimeout bounds one step against a cloud that never answers.
	cacheStepTimeout = 30 * time.Minute
)

func (s *Server) cacheWakeCh() chan struct{} {
	s.cacheWakeOnce.Do(func() { s.cacheWake = make(chan struct{}, 1) })
	return s.cacheWake
}

// wakeCache has the loop fill now. It never blocks: one wake pending is as
// good as many.
func (s *Server) wakeCache() {
	select {
	case s.cacheWakeCh() <- struct{}{}:
	default:
	}
}

// localCacheLoop fills the cache while the vault is unlocked, step after step
// until there is nothing left to fetch or no room to put it, and then waits for
// a tick or a wake. It runs on every machine, the primary or not and a phone
// included: what it fills is this machine's own disk, for this machine's own
// reads, so it is nobody else's work to do.
func (s *Server) localCacheLoop() {
	ticker := time.NewTicker(cacheTick)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-s.cacheWakeCh():
		}
		s.fillCache()
	}
}

// fillCache runs steps until the cache is done, full, or the vault locks.
func (s *Server) fillCache() {
	v, err := s.Vault()
	if err != nil || !v.Unlocked() {
		return
	}
	s.cacheFilling.Store(true)
	defer s.cacheFilling.Store(false)
	for v.Unlocked() {
		step, err := s.cacheStep(v)
		switch {
		case errors.Is(err, vault.ErrLocked), errors.Is(err, vault.ErrCacheBusy):
			return
		case err != nil:
			log.Printf("local cache: %v", err)
			return
		}
		if step.Swept > 0 {
			log.Printf("local cache: let go of %s no longer kept", formatBytes(step.Swept))
		}
		if step.Idle || step.Full || step.Fetched == 0 {
			if step.Full {
				log.Print("local cache: out of room; what does not fit stays on the clouds")
			}
			return
		}
	}
}

// cacheStep runs one step with the idle timer held off while it fetches: a
// folder asked to be kept here should finish arriving even if nobody is
// watching the page, the way a replica does.
func (s *Server) cacheStep(v *vault.Vault) (vault.LocalCacheStep, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cacheStepTimeout)
	defer cancel()

	done := make(chan struct{})
	defer close(done)
	go func() {
		keep := time.NewTicker(time.Minute)
		defer keep.Stop()
		for {
			select {
			case <-done:
				return
			case <-keep.C:
				s.noteExternalActivity()
			}
		}
	}()
	return v.FillLocalCache(ctx, cacheStepBudget)
}

// cacheStatusResponse is the status plus whether the loop is mid-fill.
type cacheStatusResponse struct {
	vault.LocalCacheStatus
	Filling bool `json:"filling"`
}

func (s *Server) handleCacheStatus(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	status, err := v.LocalCache()
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cacheStatusResponse{status, s.cacheFilling.Load()})
}

// cacheKeepRequest names what to keep and until when: Until as a moment, or
// For as a length of time from now ("12h", "7d", "2w"). Exactly one. From is
// which placement is kept — "parts" (the default), "replica" or "either" —
// as a stream link's is chosen.
type cacheKeepRequest struct {
	Path    string    `json:"path"`
	VaultID string    `json:"vault_id"`
	Until   time.Time `json:"until"`
	For     string    `json:"for"`
	From    string    `json:"from"`
}

func (s *Server) handleCacheKeep(w http.ResponseWriter, r *http.Request) {
	var req cacheKeepRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeError(w, http.StatusBadRequest, "say which file or folder to keep", "BAD_REQUEST")
		return
	}
	until := req.Until
	switch {
	case req.For != "" && !until.IsZero():
		writeError(w, http.StatusBadRequest, "give either until or for, not both", "BAD_REQUEST")
		return
	case req.For != "":
		d, err := vault.ParseKeepFor(req.For)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
			return
		}
		until = time.Now().Add(d)
	case until.IsZero():
		writeError(w, http.StatusBadRequest, "say how long to keep it, with until or for", "BAD_REQUEST")
		return
	}

	from, err := vault.ParseReadSource(req.From)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}

	v, _ := s.Vault()
	pin, err := v.KeepLocally(vault.Scope(req.VaultID), req.Path, until, from)
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	s.wakeCache()
	writeJSON(w, http.StatusCreated, pin)
}

func (s *Server) handleCacheStop(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Vault()
	if err := v.StopKeeping(r.PathValue("id")); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// cacheLimitsRequest sets the most the cache may hold (0 for no limit of its
// own) and the free space it leaves on the disk (0 for a tenth of the disk,
// negative for none).
type cacheLimitsRequest struct {
	Limit   int64 `json:"limit"`
	Reserve int64 `json:"reserve"`
}

func (s *Server) handleCacheLimits(w http.ResponseWriter, r *http.Request) {
	var req cacheLimitsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return
	}
	if req.Limit < 0 {
		writeError(w, http.StatusBadRequest, "the limit cannot be negative; 0 is no limit", "BAD_REQUEST")
		return
	}
	v, _ := s.Vault()
	if err := v.SetLocalCacheLimits(req.Limit, req.Reserve); err != nil {
		vaultErrorResponse(w, err)
		return
	}
	// More room may be what a full fill was waiting for.
	s.wakeCache()
	status, err := v.LocalCache()
	if err != nil {
		vaultErrorResponse(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cacheStatusResponse{status, s.cacheFilling.Load()})
}
