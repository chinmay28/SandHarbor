package vault

import (
	"io"
	"time"
)

// Where a transfer has got to, while it is still getting there.
//
// A transfer is a file moving between this vault and a machine somebody has a
// login on, in either direction: an import brings the machine's files in, an
// export puts the vault's files back out. Both are long, both are worked a
// file at a time, and both are watched through this one window.
//
// This is a window onto a request that is running, and nothing more. It is not
// job state: it lives in memory for as long as the transfer does, it is never
// written down, and losing it costs nothing, because the transfer it describes
// went with it. That is the same bargain ImportFromSource and ExportToSource
// make about resuming — there is no job, only a request — and adding a
// progress bar was not allowed to quietly turn one into the other.
//
// What it is for is the case the dialog used to have nothing to say about: one
// very large file. A folder of small files reports itself, a file at a time, in
// the summary at the end; an 18 GB film reports nothing for an hour and looks
// identical to a hang.

// TransferStage is which leg of the journey a file is on.
//
// An import has two legs worth naming because both are long and they are long
// for different reasons: fetching is the source's upstream, scattering is this
// machine's, and one being slow says something quite different from the other.
// An export has one — the parts are gathered from the clouds and written to
// the machine in the same pass — so it only ever reports sending.
type TransferStage string

const (
	// StageFetching is the source coming down to this machine's spool.
	StageFetching TransferStage = "fetching"

	// StageScattering is the spool going back up to the connected accounts,
	// compressed, split and encrypted on the way. See UploadStream.
	StageScattering TransferStage = "scattering"

	// StageGrinding is a file being read through on the machine it is on,
	// for the checksum every one of its parts has to carry, before any part
	// of it can be made. Done counts what the machine has read. It is the
	// first leg of an import that makes its parts on the machine; the second
	// is StageScattering, with the parts arriving made. See importGround.
	StageGrinding TransferStage = "grinding"

	// StageSending is a file leaving the vault for a machine: gathered from
	// the clouds, decrypted and written out as it arrives. See ExportToSource.
	StageSending TransferStage = "sending"

	// StageChecking is a file being looked at rather than moved: deciding
	// whether the vault already holds it, and putting its recorded
	// modification time back when that is the only thing wrong with it.
	//
	// No bytes move on this leg, so on one file it is over before it can be
	// drawn. It is named because of the run where it is the *whole* transfer:
	// re-importing a folder that is already here is thousands of these and
	// nothing else — one stat per file over SFTP, and an index write per run
	// of times put right — and without a stage of its own that run reported
	// nothing at all from beginning to end. See StagePlanning for the same
	// argument about the walk that comes before it.
	StageChecking TransferStage = "checking"

	// StagePlanning is before any file: the selection being walked to find out
	// what is in it.
	//
	// It is the one report that names no file, because there is no file yet —
	// what it carries is Files, the count found so far, which is the only
	// honest thing to say while a folder of ten thousand is being listed a
	// directory at a time over a link with a round trip in it.
	StagePlanning TransferStage = "planning"
)

// TransferProgress is one file of a transfer, mid-flight.
//
// The counts are what the summary would say if the transfer stopped here, which
// is what makes it readable while it runs: "4 of 12, one already there".
type TransferProgress struct {
	// File is the file being worked on, 1-based, out of Files. Files is what
	// the plan holds, so it is the whole selection rather than what is left.
	// While the selection is still being walked (StagePlanning) there is no
	// file yet: File is zero and Files is how many the walk has found so far.
	File  int `json:"file"`
	Files int `json:"files"`

	// Path is where the file is coming from and Dest where it is going. For an
	// import that is a path on the source and a path in the vault; for an
	// export it is the other way round. Both are relative to their own root.
	Path string `json:"path"`
	Dest string `json:"dest"`
	Name string `json:"name"`

	Stage TransferStage `json:"stage"`

	// Done is how much of this file has moved in this stage, out of Size.
	// It restarts at zero when the stage changes, because an import's two
	// stages are two passes over the same bytes rather than halves of one.
	Done int64 `json:"done"`
	Size int64 `json:"size"`

	// What has become of the files before this one: carried across, passed
	// over because they were already there, left where they were with their
	// modification time put right, or failed.
	Completed int `json:"completed"`
	Skipped   int `json:"skipped"`
	Retimed   int `json:"retimed"`
	Failed    int `json:"failed"`
}

// progressEvery is how much has to move before the reader says so again.
//
// A read off an SFTP handle comes back in tens of kilobytes, and an 18 GB file
// is half a million of them — a report per read would be half a million
// callbacks to draw a bar that moves in whole percent.
const progressEvery = 4 << 20

// progressInterval is how long the reader goes without saying anything while
// bytes are moving, however few.
//
// Without it the reports are paced by the link: four megabytes is a fraction
// of a second on a good one and six minutes at 90 kbit/s, and the dialog drops
// a speed that has had nothing behind it for twelve seconds (staleRateAfter in
// internal/server) — so below about 3 Mbit/s the transfer showed no speed at
// all, on exactly the connections where somebody most wants to know it. A
// second keeps a slow transfer visible, and is still well apart from the
// half-second the speed is measured over.
const progressInterval = time.Second

// progressReader counts what passes through it and says so, not too often.
type progressReader struct {
	r     io.Reader
	say   func(int64)
	done  int64
	since int64

	// last is when it last said anything, or when counting began. now is
	// time.Now unless a test is driving the clock.
	last time.Time
	now  func() time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.count(int64(n))
	// The end of the file is worth saying whatever the count is at: a bar that
	// stops at 99% because the last read was short is a bar that looks stuck.
	if err != nil {
		p.flush()
	}
	return n, err
}

// WriteTo hands the whole copy to the source when the source has a faster way
// to move bytes than Read, and counts them as they land.
//
// This method existing is what an import's speed rests on. io.Copy prefers a
// source's WriteTo over calling Read in a loop, and for an *sftp.File the two
// are not the same operation at different spellings: WriteTo keeps dozens of
// requests on the wire at once, where Read is one 32 KiB packet per network
// round trip — a hard ceiling of about 200 KB/s to a server 150 ms away,
// however fast the link. Without this method the wrapper hides the file's
// WriteTo from io.Copy, and watching the transfer silently costs the very
// speed that made it worth watching.
func (p *progressReader) WriteTo(w io.Writer) (int64, error) {
	wt, ok := p.r.(io.WriterTo)
	if !ok {
		// Nothing faster underneath. The bare-Reader wrapper keeps io.Copy
		// from landing straight back here.
		return io.Copy(w, struct{ io.Reader }{p})
	}
	n, err := wt.WriteTo(&progressWriter{w: w, count: p})
	p.flush()
	return n, err
}

// count tallies bytes that moved, reporting every progressEvery of them or
// every progressInterval, whichever comes first.
func (p *progressReader) count(n int64) {
	if n <= 0 {
		return
	}
	now := p.clock()
	if p.last.IsZero() {
		p.last = now
	}
	p.done += n
	p.since += n
	if p.since >= progressEvery || now.Sub(p.last) >= progressInterval {
		p.report(now)
	}
}

// flush reports whatever the throttle is still holding.
func (p *progressReader) flush() {
	if p.since > 0 {
		p.report(p.clock())
	}
}

func (p *progressReader) report(now time.Time) {
	p.since = 0
	p.last = now
	p.say(p.done)
}

func (p *progressReader) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// progressWriter is the receiving end of a delegated WriteTo: the bytes are
// counted as they are written rather than as they are read, which is the same
// moment — the source hands them over in order, however many requests it has
// in flight behind the scenes.
type progressWriter struct {
	w     io.Writer
	count *progressReader
}

func (pw *progressWriter) Write(b []byte) (int, error) {
	n, err := pw.w.Write(b)
	pw.count.count(int64(n))
	return n, err
}
