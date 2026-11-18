package vault

// Making the thumbnails a folder is missing, on the server.
//
// A file that arrived without a picture (from the command line, over WebDAV,
// or before thumbnails existed) keeps its icon until somebody makes it one.
// This is the whole of that, as a job the machine runs by itself: plan from
// the index, read each file back from its clouds, draw it (internal/thumb),
// and store the pictures a batch at a time.
//
// It runs to the end. A file that cannot be read or drawn is counted and left;
// a batch that will not store is counted and left. A vault that locks while it
// runs — by hand, since the idle timer does not lock under a running job —
// pauses it: it waits, and carries on from the same file once the vault is
// unlocked again. Only its context, which is Stop, ends it early.

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/chinmay28/SandHarbor/internal/thumb"
)

// ThumbBatch is how many pictures are stored at once: one rewrite of a
// folder's pack per batch rather than per picture (SetThumbsByID).
const ThumbBatch = 32

// thumbLockPoll is how often a run waiting for an unlock looks again; a
// variable so a test need not wait it out.
var thumbLockPoll = 5 * time.Second

// ThumbPlan is what a run would do, counted before it starts.
type ThumbPlan struct {
	// Files is how many files would get a thumbnail, in how many folders.
	Files   int   `json:"files"`
	Folders int   `json:"folders"`
	Bytes   int64 `json:"bytes"`
	// TooLarge files are over thumb.MaxSourceBytes; OverFull ones are in a
	// folder whose pack has no room left (MaxPackEntries). Both are left out.
	TooLarge int `json:"too_large"`
	OverFull int `json:"over_full"`

	files []*Entry
}

// PlanThumbs counts what MakeThumbs would do under dir: every file there and
// in every folder below that has no thumbnail and is a kind the server can
// draw (thumb.SourceKind), within each folder's room.
func (v *Vault) PlanThumbs(scope Scope, dir string) (ThumbPlan, error) {
	gaps, err := v.MissingThumbs(scope, dir)
	if err != nil {
		return ThumbPlan{}, err
	}
	var plan ThumbPlan
	for _, gap := range gaps {
		room, planned := gap.Room, 0
		for _, e := range gap.Files {
			if thumb.SourceKind(e.MIME, e.Name) == "" {
				continue
			}
			if e.Size > thumb.MaxSourceBytes {
				plan.TooLarge++
				continue
			}
			if room <= 0 {
				plan.OverFull++
				continue
			}
			room--
			planned++
			plan.files = append(plan.files, e)
			plan.Bytes += e.Size
		}
		if planned > 0 {
			plan.Folders++
		}
	}
	plan.Files = len(plan.files)
	return plan, nil
}

// ThumbRun is where a run has got to, and, once it is done, what it came to.
type ThumbRun struct {
	Path  string `json:"path"`
	Total int    `json:"total"`
	Done  int    `json:"done"`

	// Made is how many thumbnails were stored.
	Made int `json:"made"`
	// The files that did not get one, by why: not read back from their
	// clouds, not drawn (not a picture the server can read, a PDF on a
	// machine without pdftoppm), or drawn and not stored.
	Unread   int `json:"unread"`
	Undrawn  int `json:"undrawn"`
	Unstored int `json:"unstored"`
	// Failed is the three together.
	Failed int `json:"failed"`

	// Errors keeps the first few reasons, one per file, for the panel to show.
	Errors []ThumbError `json:"errors,omitempty"`

	// Locked is set while the run waits for the vault to be unlocked.
	Locked bool `json:"locked,omitempty"`
	// Current is the file in hand.
	Current string `json:"current,omitempty"`
	// Stopped says the run was ended before it got through every file.
	Stopped bool `json:"stopped,omitempty"`
}

// ThumbError is one file that did not get a thumbnail, and why.
type ThumbError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// maxThumbErrors is how many reasons a run keeps.
const maxThumbErrors = 20

// MakeThumbs makes the thumbnails dir and every folder below are missing, and
// says how it went. onProgress is called after each file and batch, and on
// going into and out of a wait for an unlock; it may be nil.
//
// It returns an error only when it could not start: the folder does not exist.
// A vault that is locked when it starts is waited for, like one that locks
// part way. Once started it always runs to the end, or until
// ctx ends, and the counts say what came of each file.
func (v *Vault) MakeThumbs(ctx context.Context, scope Scope, dir string, onProgress func(ThumbRun)) (ThumbRun, error) {
	run := ThumbRun{Path: CleanDir(dir)}
	report := func() {
		if onProgress != nil {
			out := run
			out.Errors = append([]ThumbError(nil), run.Errors...)
			onProgress(out)
		}
	}
	fail := func(count *int, e *Entry, err error) {
		*count++
		run.Failed++
		if len(run.Errors) < maxThumbErrors {
			run.Errors = append(run.Errors, ThumbError{Path: e.Path(), Error: err.Error()})
		}
	}

	// waitForUnlock blocks while the vault is locked, saying so, and reports
	// whether the run may go on (false once ctx has ended).
	waitForUnlock := func() bool {
		if v.Unlocked() {
			return ctx.Err() == nil
		}
		run.Locked = true
		report()
		defer func() { run.Locked = false; report() }()
		for !v.Unlocked() {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(thumbLockPoll):
			}
		}
		return ctx.Err() == nil
	}

	// Planned once the vault is open: a lock that lands between the start
	// and the plan is waited out like any other.
	var plan ThumbPlan
	for {
		var err error
		plan, err = v.PlanThumbs(scope, dir)
		if errors.Is(err, ErrLocked) {
			if !waitForUnlock() {
				run.Stopped = true
				report()
				return run, nil
			}
			continue
		}
		if err != nil {
			return ThumbRun{}, err
		}
		break
	}
	run.Total = plan.Files

	pending := map[string][]byte{}
	pendingEntries := map[string]*Entry{}
	flush := func() bool {
		if len(pending) == 0 {
			return true
		}
		for {
			stored, err := v.SetThumbsByID(context.WithoutCancel(ctx), pending)
			if errors.Is(err, ErrLocked) {
				if !waitForUnlock() {
					// Stopped while locked: what was drawn cannot be kept.
					for _, e := range sortedEntries(pendingEntries) {
						fail(&run.Unstored, e, ErrLocked)
					}
					return false
				}
				continue
			}
			if err != nil {
				for _, e := range sortedEntries(pendingEntries) {
					fail(&run.Unstored, e, err)
				}
			} else {
				run.Made += stored
				// SetThumbsByID skips a file deleted since it was planned.
				for i := stored; i < len(pending); i++ {
					run.Unstored++
					run.Failed++
				}
			}
			break
		}
		pending = map[string][]byte{}
		pendingEntries = map[string]*Entry{}
		report()
		return true
	}

	report()
	for _, e := range plan.files {
		if ctx.Err() != nil {
			break
		}
		run.Current = e.Path()
		var (
			data []byte
			err  error
		)
		for {
			data, _, err = v.Fetch(ctx, e.ID)
			if errors.Is(err, ErrLocked) {
				if !waitForUnlock() {
					break
				}
				continue
			}
			break
		}
		if ctx.Err() != nil {
			break
		}
		run.Done++
		if err != nil {
			fail(&run.Unread, e, err)
			report()
			continue
		}
		picture, err := thumb.FromSource(ctx, data, e.MIME, e.Name)
		data = nil
		if err != nil {
			fail(&run.Undrawn, e, err)
			report()
			continue
		}
		pending[e.ID] = picture
		pendingEntries[e.ID] = e
		if len(pending) >= ThumbBatch {
			flush()
		}
		report()
	}
	flush()
	run.Current = ""
	run.Stopped = ctx.Err() != nil && run.Done < run.Total
	report()
	return run, nil
}

func sortedEntries(m map[string]*Entry) []*Entry {
	out := make([]*Entry, 0, len(m))
	for _, e := range m {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path() < out[j].Path() })
	return out
}
