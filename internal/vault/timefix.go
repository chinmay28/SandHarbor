package vault

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chinmay28/SandHarbor/internal/filedate"
)

// Putting back the date a file was made.
//
// A file carries one date of its own, its modified time, and it is only as
// good as whatever brought the file here. A photograph that came through a
// chat app, a zip, a cloud's web page or an upload from before the vault kept
// times is dated the day it arrived, and a folder of ten years of pictures
// reads as one afternoon. Filing that folder by date (datesort.go) then files
// it into one month.
//
// The date the picture was taken is usually still there, in two places: in a
// name like IMG_20230514_101502.jpg, and inside the file, where the camera
// wrote it. internal/filedate reads both. This is the vault's half, in the
// three pieces the organizer's other tools come in:
//
//   - TimeFixes plans, from the index alone. It names the files whose name
//     states a date their stored time disagrees with, and the pictures and
//     videos whose name says nothing and which would have to be opened.
//   - SetTimes applies the rows of that plan: one index write for the batch.
//   - FixTimesFromContent opens a batch of files, reads the date inside each,
//     and corrects the ones that are out.
//
// Only the modified time is ever changed. CreatedAt is when the file landed in
// this vault, which is a fact about the vault and what an import's resume check
// leans on. Nothing else about an entry moves either: same ID, same parts, same
// accounts.
//
// "Disagrees" is more than a day apart (filedate.Off). That is deliberate: the
// fault this finds is out by months, and anything finer would be correcting
// time zones it can only guess at.

// TimeFixOptions is which files to look at, and in whose time.
type TimeFixOptions struct {
	// Deep looks at every file under the folder rather than only the files
	// directly in it.
	Deep bool

	// Offset is the viewer's distance from UTC in minutes, east positive, as
	// DateSortOptions.Offset is. A date with no zone beside it — every name,
	// and most cameras — is a wall clock, and this is whose wall.
	Offset int
}

// location is the zone a zoneless date is read in.
func (o TimeFixOptions) location() (*time.Location, error) {
	if o.Offset < -1440 || o.Offset > 1440 {
		return nil, fmt.Errorf("offset from UTC out of range: %d minutes", o.Offset)
	}
	return time.FixedZone("", o.Offset*60), nil
}

// TimeFix is one file whose name states a date its stored time disagrees with.
type TimeFix struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Dir  string `json:"dir"`
	Size int64  `json:"size"`

	// Was is the modified time stored now, and To the one the name states,
	// both in UTC. Undated says there was no stored time at all, in which case
	// Was is the zero time and means nothing.
	Was     time.Time `json:"was"`
	To      time.Time `json:"to"`
	Undated bool      `json:"undated,omitempty"`

	// DayOnly says the name gives a day and no time, so To is noon of it.
	DayOnly bool `json:"day_only,omitempty"`
}

// TimeCandidate is a picture or video whose name states no date, and which
// would have to be opened to find one.
type TimeCandidate struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Dir  string `json:"dir"`
	Size int64  `json:"size"`
}

// TimePlan is what can be said about a folder's dates without opening a file.
type TimePlan struct {
	Path   string `json:"path"`
	Deep   bool   `json:"deep"`
	Offset int    `json:"offset"`

	// Fixes is the files whose name states a date the stored time is more
	// than a day from, by folder and then by name.
	Fixes []TimeFix `json:"fixes"`

	// Readable is the pictures and videos with no date in their name: the
	// ones FixTimesFromContent can be asked about. In the same order.
	Readable []TimeCandidate `json:"readable"`

	// Settled counts the files whose name states a date and whose stored time
	// agrees with it. Unknown counts the rest: no date in the name, and not a
	// kind of file with one inside. Neither is touched.
	Settled int `json:"settled"`
	Unknown int `json:"unknown"`
}

// TimeFixes plans the correction of a folder's dates from the index alone. It
// changes nothing and contacts no account.
func (v *Vault) TimeFixes(scope Scope, dir string, opts TimeFixOptions) (*TimePlan, error) {
	loc, err := opts.location()
	if err != nil {
		return nil, err
	}

	v.mu.RLock()
	defer v.mu.RUnlock()

	m, err := v.manifestForLocked(scope)
	if err != nil {
		return nil, err
	}
	dir = CleanDir(dir)
	if !m.FolderExists(dir) {
		return nil, fmt.Errorf("no such folder: %s", dir)
	}

	var files []*Entry
	for _, e := range m.Descendants(dir) {
		if opts.Deep || e.Dir == dir {
			files = append(files, e)
		}
	}

	plan := planTimeFixes(files, loc, time.Now())
	plan.Path, plan.Deep, plan.Offset = dir, opts.Deep, opts.Offset
	return plan, nil
}

// planTimeFixes sorts files into the ones a name corrects, the ones worth
// opening, and the ones left alone. A pure function of what it is given: the
// files, the zone their names are read in, and the present, which is what
// makes a date in the future not a date.
func planTimeFixes(files []*Entry, loc *time.Location, now time.Time) *TimePlan {
	plan := &TimePlan{Fixes: []TimeFix{}, Readable: []TimeCandidate{}}

	files = append([]*Entry(nil), files...)
	sort.SliceStable(files, func(i, j int) bool {
		if files[i].Dir != files[j].Dir {
			return files[i].Dir < files[j].Dir
		}
		return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name)
	})

	for _, e := range files {
		stamp, named := filedate.FromName(e.Name, loc)
		if named && !filedate.Plausible(stamp.At, now) {
			named = false
		}
		switch {
		case named && filedate.Off(e.ModifiedAt, stamp):
			plan.Fixes = append(plan.Fixes, TimeFix{
				ID: e.ID, Name: e.Name, Dir: e.Dir, Size: e.Size,
				Was: e.ModifiedAt.UTC(), To: stamp.At,
				Undated: e.ModifiedAt.IsZero(), DayOnly: stamp.DayOnly,
			})
		case named:
			plan.Settled++
		case filedate.Readable(e.Name):
			plan.Readable = append(plan.Readable, TimeCandidate{
				ID: e.ID, Name: e.Name, Dir: e.Dir, Size: e.Size,
			})
		default:
			plan.Unknown++
		}
	}
	return plan
}

// TimeOrder is one file and the modified time it should carry.
type TimeOrder struct {
	ID       string    `json:"id"`
	Modified time.Time `json:"modified"`
}

// TimeReport is what SetTimes came to.
type TimeReport struct {
	// Fixed counts the entries whose time changed. A file already carrying the
	// time asked for, to the second, is not counted and not written, so the
	// same orders sent twice change nothing the second time.
	Fixed int `json:"fixed"`

	// Missing lists the IDs that named no file.
	Missing []string `json:"missing"`
}

// SetTimes records modified times on files named by ID, in one index write.
//
// It is Retime for a caller holding IDs rather than paths, and makes the same
// promises: an order naming nothing is reported and does not fail the batch,
// an order with no time is ignored, and if the index cannot be written no
// entry is left changed. No scope is asked for; an ID is unique across every
// open vault in the file. No account is contacted.
func (v *Vault) SetTimes(orders []TimeOrder) (*TimeReport, error) {
	report := &TimeReport{Missing: []string{}}
	if len(orders) == 0 {
		return report, nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()

	if v.dataKey == nil {
		return nil, ErrLocked
	}

	type change struct {
		entry *Entry
		was   time.Time
	}
	var changed []change
	index := v.entryIndexLocked()
	for _, order := range orders {
		if order.Modified.IsZero() {
			continue
		}
		found, ok := index.byID[order.ID]
		if !ok {
			report.Missing = append(report.Missing, order.ID)
			continue
		}
		if SameModTime(found.entry.ModifiedAt, order.Modified) {
			continue
		}
		changed = append(changed, change{entry: found.entry, was: found.entry.ModifiedAt})
		found.entry.ModifiedAt = order.Modified.UTC()
	}
	if len(changed) == 0 {
		return report, nil
	}

	if err := v.persistLocked(); err != nil {
		for _, c := range changed {
			c.entry.ModifiedAt = c.was
		}
		return nil, fmt.Errorf("could not record the corrected times: %w", err)
	}
	report.Fixed = len(changed)
	return report, nil
}

// TimeReading is what opening a batch of files to read their dates came to.
// Every file asked about is in exactly one of its counts or lists.
type TimeReading struct {
	// Fixed is the files whose time was corrected to the date inside them.
	// Settled is the files carrying a date their stored time already agrees
	// with, and Undated the ones carrying none.
	Fixed   int `json:"fixed"`
	Settled int `json:"settled"`
	Undated int `json:"undated"`

	// Missing lists the IDs that named no file, and Failed one line per file
	// that could not be read back from its accounts.
	Missing []string `json:"missing"`
	Failed  []string `json:"failed"`
}

// timeReaders is how many files of a batch are open at once. Each is a fetch
// from the accounts holding its parts, so a few at a time is most of the gain
// and more is a way to be rate-limited.
const timeReaders = 4

// FixTimesFromContent reads the date inside each of a batch of pictures and
// videos, and corrects the stored time of every one that is more than a day
// from it.
//
// A read costs the part of the file the date is in: the first chunk of a
// photograph, the chunk holding a video's header. A file stored whole, in the
// format from before chunking, is fetched whole. The files are read a few at a
// time and the index written once, after the last of them.
//
// A file that cannot be read is listed and the rest carry on; so is an ID that
// names nothing. The error is the vault's own: locked, or an index that could
// not be written, in which case nothing changed. A batch cut short by ctx
// corrects what it had read and lists the rest as failed.
func (v *Vault) FixTimesFromContent(ctx context.Context, ids []string, opts TimeFixOptions) (*TimeReading, error) {
	loc, err := opts.location()
	if err != nil {
		return nil, err
	}
	if !v.Unlocked() {
		return nil, ErrLocked
	}

	type answer struct {
		entry   *Entry
		stamp   filedate.Stamp
		found   bool
		missing bool
		err     error
	}
	answers := make([]answer, len(ids))

	var wg sync.WaitGroup
	slots := make(chan struct{}, timeReaders)
	for i, id := range ids {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()

			entry, err := v.Entry(id)
			if err != nil {
				answers[i] = answer{missing: !errors.Is(err, ErrLocked), err: err}
				return
			}
			if err := ctx.Err(); err != nil {
				answers[i] = answer{entry: entry, err: err}
				return
			}
			stamp, found, err := v.dateInside(ctx, entry, loc)
			answers[i] = answer{entry: entry, stamp: stamp, found: found, err: err}
		}()
	}
	wg.Wait()

	reading := &TimeReading{Missing: []string{}, Failed: []string{}}
	now := time.Now()
	var orders []TimeOrder
	for i, a := range answers {
		switch {
		case a.missing:
			reading.Missing = append(reading.Missing, ids[i])
		case errors.Is(a.err, ErrLocked):
			return nil, ErrLocked
		case a.err != nil:
			reading.Failed = append(reading.Failed, fmt.Sprintf("%s: %s", a.entry.Path(), a.err))
		case !a.found || !filedate.Plausible(a.stamp.At, now):
			reading.Undated++
		case !filedate.Off(a.entry.ModifiedAt, a.stamp):
			reading.Settled++
		default:
			orders = append(orders, TimeOrder{ID: ids[i], Modified: a.stamp.At})
		}
	}

	report, err := v.SetTimes(orders)
	if err != nil {
		return nil, err
	}
	// A file deleted between being read and being written, or one named twice
	// in the batch, is corrected once or not at all; either way it is settled.
	reading.Fixed = report.Fixed
	reading.Settled += len(orders) - report.Fixed - len(report.Missing)
	reading.Missing = append(reading.Missing, report.Missing...)
	return reading, nil
}

// dateInside opens one stored file and reads the date it carries.
func (v *Vault) dateInside(ctx context.Context, entry *Entry, loc *time.Location) (filedate.Stamp, bool, error) {
	if !filedate.Readable(entry.Name) {
		return filedate.Stamp{}, false, nil
	}

	var source io.ReaderAt
	source, _, err := v.OpenReaderAt(ctx, entry.ID)
	if errors.Is(err, ErrNeedsConversion) {
		// Stored whole: there is no first chunk to ask for.
		var data []byte
		if data, _, err = v.Fetch(ctx, entry.ID); err == nil {
			source = bytes.NewReader(data)
		}
	}
	if err != nil {
		return filedate.Stamp{}, false, err
	}
	return filedate.FromContent(source, entry.Size, entry.Name, loc)
}
