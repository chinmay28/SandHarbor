package server

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// The archives being unpacked right now, so the browser can draw one — and so
// that one outlives the page that uploaded it.
//
// An archive sent to be extracted is two pieces of work. Getting its bytes
// here is the browser's, and nothing but the browser can do it. Unpacking it
// is not: every file in it is split, encrypted and scattered by this machine,
// which for an archive of a few thousand photographs is an hour of work the
// page contributed nothing to but a held connection — and a page that is
// closed, or walked away from, drops that connection and with it the rest of
// the archive. So the upload ends when the archive has arrived, and the
// unpacking is a run here, on a context of its own, reporting as it goes and
// remembered for a while after it ends so the outcome is still there when
// somebody comes back to look.
//
// One at a time, oldest first. Unpacking is an upload of everything in the
// archive, and two of them abreast are two uploads competing for the same
// clouds; the rest wait their turn and are listed as waiting, and a stop
// while waiting is a run that never stored anything.
//
// The lifetime is the process, and not a restart: what a run had stored when
// it was cut short is stored, the archive it was reading is a spool the
// housekeeping scan will offer to remove, and uploading it again is how it is
// finished. There is nothing worth writing down.

// maxWaitingExtracts is how many archives may be waiting for a turn. Each is
// a file on this machine's disk until its turn comes, so the ceiling is on
// that as much as on patience.
const maxWaitingExtracts = 16

// extractRunTimeout is the most one archive may take to unpack. A run stores
// one file at a time, so a timeout costs the file in flight and nothing else.
const extractRunTimeout = 24 * time.Hour

// maxExtractNotes is how many lines of each kind a report keeps before it
// only counts: an archive of twenty thousand entries that all fail the same
// way is one reason said twenty thousand times.
const maxExtractNotes = 10

// extractProgress is where a run has got to.
type extractProgress struct {
	// Files is how many files have been stored so far.
	Files int `json:"files"`
	// Done and Total are bytes of the archive itself, read and in all: the
	// one measure both a zip and a tar have before they have been walked.
	Done  int64 `json:"done"`
	Total int64 `json:"total"`
	// Current is the entry being stored, as the archive names it.
	Current string `json:"current,omitempty"`
}

// extractReport is what unpacking an archive came to.
type extractReport struct {
	// Extracted counts the files stored.
	Extracted int `json:"extracted"`

	// Failed counts the entries that could not be stored, and Failures says
	// why of the first few, a line each: the entry's path and its reason.
	Failed   int      `json:"failed"`
	Failures []string `json:"failures,omitempty"`

	// Skipped counts what was left out for not being a file — a link, a
	// device, a password-protected entry — and Skips names the first few.
	Skipped int      `json:"skipped"`
	Skips   []string `json:"skips,omitempty"`

	// Warnings is what storing the files had to say, the first few of them.
	Warnings []string `json:"warnings,omitempty"`
}

// note appends a line to one of a report's capped lists.
func note(list *[]string, line string) {
	if len(*list) < maxExtractNotes {
		*list = append(*list, line)
	}
}

// extractRun is one archive, waiting, being unpacked or lately finished.
type extractRun struct {
	ID string `json:"id"`

	// Name is the archive as it was uploaded, and Dir the folder it is
	// unpacking into.
	Name string `json:"name"`
	Dir  string `json:"dir"`
	// Vault is the sub vault Dir is in, or empty for the vault itself.
	Vault string `json:"vault,omitempty"`

	// Waiting says it has not begun: another archive has the turn.
	Waiting bool `json:"waiting,omitempty"`

	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	At extractProgress `json:"at"`

	// Done, and what it came to. Report is set whenever anything was
	// attempted, including beside an Error: an archive that turned out to be
	// cut short has still given up the files before the cut.
	Done      bool           `json:"done,omitempty"`
	Report    *extractReport `json:"report,omitempty"`
	Error     string         `json:"error,omitempty"`
	Cancelled bool           `json:"cancelled,omitempty"`
}

// extractWork unpacks one archive under ctx, saying where it has got to as it
// goes. It returns what it managed alongside any error that stopped it.
type extractWork func(ctx context.Context, onProgress func(extractProgress)) (*extractReport, error)

// errTooManyExtracts is what add answers past the ceiling.
var errTooManyExtracts = errors.New(
	"too many archives are already waiting to be extracted; wait for one to finish or stop it")

// extractRuns holds them, and runs them one after another.
type extractRuns struct {
	mu   sync.Mutex
	runs map[string]*queuedExtract
	// queue is the runs waiting, oldest first.
	queue []*queuedExtract
	// draining says a goroutine is working through the queue.
	draining bool

	timeout time.Duration
	// now is time.Now everywhere but in tests.
	now func() time.Time
}

// queuedExtract is a run plus what it takes to carry it out and to stop it.
type queuedExtract struct {
	run    extractRun
	ctx    context.Context
	cancel context.CancelFunc
	work   extractWork
	// release lets go of whatever the work reads from. Called exactly once,
	// whether the work ran or not.
	release func()
}

func newExtractRuns() *extractRuns {
	return &extractRuns{
		runs:    map[string]*queuedExtract{},
		timeout: extractRunTimeout,
		now:     time.Now,
	}
}

// add queues an archive to be unpacked and answers with its run. The work
// starts when every run queued before it has ended. release is called once
// the run is over — or straight away, if it is refused.
func (q *extractRuns) add(run extractRun, work extractWork, release func()) (extractRun, error) {
	q.mu.Lock()
	q.sweepLocked()
	if len(q.queue) >= maxWaitingExtracts {
		q.mu.Unlock()
		release()
		return extractRun{}, errTooManyExtracts
	}

	ctx, cancel := context.WithCancel(context.Background())
	run.ID = uuid.NewString()
	run.Waiting = true
	run.StartedAt = q.now()
	run.Done, run.Report, run.Error, run.Cancelled, run.FinishedAt = false, nil, "", false, nil
	entry := &queuedExtract{run: run, ctx: ctx, cancel: cancel, work: work, release: release}
	q.runs[run.ID] = entry
	q.queue = append(q.queue, entry)
	if !q.draining {
		q.draining = true
		go q.drain()
	}
	q.mu.Unlock()
	return run, nil
}

// drain carries out the queue until it is empty.
func (q *extractRuns) drain() {
	for {
		q.mu.Lock()
		if len(q.queue) == 0 {
			q.draining = false
			q.mu.Unlock()
			return
		}
		entry := q.queue[0]
		q.queue = q.queue[1:]
		id := entry.run.ID
		entry.run.Waiting = false
		q.mu.Unlock()

		var (
			report *extractReport
			err    error
		)
		// Dropped by a lock while it waited: nothing was touched, and nothing is.
		if entry.ctx.Err() == nil {
			ctx, cancel := context.WithTimeout(entry.ctx, q.timeout)
			report, err = entry.work(ctx, func(at extractProgress) {
				q.mu.Lock()
				if q.runs[id] == entry {
					entry.run.At = at
				}
				q.mu.Unlock()
			})
			cancel()
		}
		entry.release()
		cancelled := errors.Is(entry.ctx.Err(), context.Canceled)
		entry.cancel()

		q.mu.Lock()
		// A run dropped by stopAll is nobody's to report on.
		if q.runs[id] == entry {
			at := q.now()
			entry.run.Done, entry.run.FinishedAt, entry.run.Cancelled = true, &at, cancelled
			entry.run.Report = report
			entry.run.At.Current = ""
			if err != nil && !cancelled {
				entry.run.Error = err.Error()
			}
			q.trimLocked()
		}
		q.mu.Unlock()
	}
}

// all lists every run, oldest first.
func (q *extractRuns) all() []extractRun {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sweepLocked()

	out := make([]extractRun, 0, len(q.runs))
	for _, entry := range q.runs {
		out = append(out, entry.run)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// running counts the runs not yet over, waiting ones included: an archive
// waiting its turn is as much a reason to stay unlocked as one being read.
func (q *extractRuns) running() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	n := 0
	for _, entry := range q.runs {
		if !entry.run.Done {
			n++
		}
	}
	return n
}

// stop cancels a waiting or running run, or forgets a finished one's result.
// Both are the same gesture: stop showing me this. A run stopped while it
// waited is over there and then, having stored nothing; one stopped while it
// ran ends in its own time, and is then listed with what it had stored.
func (q *extractRuns) stop(id string) bool {
	q.mu.Lock()
	entry, ok := q.runs[id]
	if !ok {
		q.mu.Unlock()
		return false
	}
	release := func() {}
	switch {
	case entry.run.Done:
		delete(q.runs, id)
	case entry.run.Waiting:
		for i, waiting := range q.queue {
			if waiting == entry {
				q.queue = append(q.queue[:i:i], q.queue[i+1:]...)
				release = entry.release
				break
			}
		}
		at := q.now()
		entry.run.Waiting, entry.run.Done, entry.run.Cancelled, entry.run.FinishedAt = false, true, true, &at
	}
	q.mu.Unlock()

	entry.cancel()
	release()
	return true
}

// stopAll cancels every run and forgets all of them, finished ones too.
// Locking the vault calls it: a run that carried on would find no key to
// store anything with, and what the last one unpacked names files.
func (q *extractRuns) stopAll() {
	q.mu.Lock()
	dropped := q.runs
	q.runs = map[string]*queuedExtract{}
	q.mu.Unlock()

	// The queue is left as it is: drain meets each waiting run already
	// cancelled, skips its work and releases what it held.
	for _, entry := range dropped {
		entry.cancel()
	}
}

// stopVault does for one sub vault what stopAll does for everything: its runs
// are cancelled and forgotten. Locking or deleting a sub vault calls it.
func (q *extractRuns) stopVault(vault string) {
	q.mu.Lock()
	var dropped []*queuedExtract
	for id, entry := range q.runs {
		if entry.run.Vault == vault {
			dropped = append(dropped, entry)
			delete(q.runs, id)
		}
	}
	q.mu.Unlock()

	for _, entry := range dropped {
		entry.cancel()
	}
}

// sweepLocked forgets results that have been kept long enough.
func (q *extractRuns) sweepLocked() {
	for id, entry := range q.runs {
		if entry.run.Done && entry.run.FinishedAt != nil && q.now().Sub(*entry.run.FinishedAt) > finishedTransferTTL {
			delete(q.runs, id)
		}
	}
}

// trimLocked keeps the newest few results and drops the rest.
func (q *extractRuns) trimLocked() {
	var finished []extractRun
	for _, entry := range q.runs {
		if entry.run.Done {
			finished = append(finished, entry.run)
		}
	}
	if len(finished) <= maxFinishedTransfers {
		return
	}
	sort.Slice(finished, func(i, j int) bool { return finished[i].StartedAt.Before(finished[j].StartedAt) })
	for _, run := range finished[:len(finished)-maxFinishedTransfers] {
		delete(q.runs, run.ID)
	}
}
