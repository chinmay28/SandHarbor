package sftp

import (
	"io"
	"slices"
	"sync"

	pkgsftp "github.com/pkg/sftp"
)

// How a file read through several sessions is cut up.
//
// A block is one SFTP packet, the most pkg/sftp asks a server for in one
// request, and a session has 64 of them outstanding — the 2 MiB that fills
// its SSH window, and what pkg/sftp's own WriteTo keeps in flight. Packets
// rather than larger blocks so that the file reaches the writer, and anything
// counting it on the way, as it arrives: see copyStriped. What is out at once
// is 65 packets a session, about 16 MiB at the default eight streams.
const (
	defaultStripeBlock    = 32 << 10
	defaultStripeInFlight = 64

	// defaultStripeMin is the smallest remainder worth spreading out: two
	// windows. Anything less fits in what one session carries in a round trip
	// or two, and opening handles on the other sessions would cost as much as
	// it saved.
	defaultStripeMin = 4 << 20
)

// RemoteFile is a file on the server opened for reading.
//
// It reads like any other file. What it adds is WriteTo, which is what io.Copy
// uses when it can: a large file is read through several sessions at once, so
// a copy to anywhere is not held to what one session can carry. See
// copyStriped for why that matters.
type RemoteFile struct {
	c    *Client
	f    *pkgsftp.File
	path string

	// size is what the file measured when it was opened. It decides whether a
	// copy is worth spreading out and where the spread stops; a file that has
	// changed size since is still copied correctly — see WriteTo.
	size int64
}

// Read reads from the file's current offset, through the primary session.
func (r *RemoteFile) Read(b []byte) (int, error) { return r.f.Read(b) }

// Close closes the file.
func (r *RemoteFile) Close() error { return r.f.Close() }

// WriteTo copies the file from its current offset to its end into w.
//
// A file with at least defaultStripeMin left to go is read through every session the
// connection has, or can open, up to Config.Streams. Anything smaller, a
// connection limited to one stream, or a server that will not open a second
// session goes through the primary session's own WriteTo, which is what this
// replaced and is still the right thing when one window is enough.
//
// The copy runs to the size measured at open, then reads on from there until
// the file ends: a file that grew in between is copied whole, and one that
// shrank is copied as far as it now goes, exactly as a plain read to the end
// would have done.
func (r *RemoteFile) WriteTo(w io.Writer) (int64, error) {
	start, err := r.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if r.c.streams <= 1 || r.size-start < r.c.stripeMin {
		return r.f.WriteTo(w)
	}

	handles := r.openOn(r.c.extraSessions())
	defer func() {
		for _, h := range handles {
			h.Close()
		}
	}()
	if len(handles) == 0 {
		return r.f.WriteTo(w)
	}

	readers := make([]io.ReaderAt, 0, 1+len(handles))
	readers = append(readers, r.f)
	for _, h := range handles {
		readers = append(readers, h)
	}

	n, err := copyStriped(w, readers, start, r.size, r.c.block, r.c.inFlight)
	if _, serr := r.f.Seek(start+n, io.SeekStart); serr != nil && err == nil {
		err = serr
	}
	if err != nil {
		// The failure may have been one of the extra sessions dying; a dead
		// one must not be handed to the next file.
		r.c.pruneExtraSessions()
		return n, err
	}
	if start+n < r.size {
		return n, nil // shrank: what is there has been copied
	}

	// Ordinarily one round trip that finds the end where it was measured.
	// The bare Reader keeps io.Copy from calling back into this method.
	m, err := io.Copy(w, struct{ io.Reader }{r.f})
	return n + m, err
}

// openOn opens this file on each of sessions at once, and returns the handles
// that opened. One that does not is simply not used: fewer streams is slower,
// not wrong.
func (r *RemoteFile) openOn(sessions []*pkgsftp.Client) []*pkgsftp.File {
	opened := make([]*pkgsftp.File, len(sessions))
	var wg sync.WaitGroup
	for i, s := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if f, err := s.Open(r.path); err == nil {
				opened[i] = f
			}
		}()
	}
	wg.Wait()
	return slices.DeleteFunc(opened, func(f *pkgsftp.File) bool { return f == nil })
}

// extraSessions returns the sessions beyond the primary one that a large read
// may use, opening any that are missing.
//
// They are opened all at once, and a refusal is remembered: a server whose
// MaxSessions is lower than Config.Streams turns some of them down, which is
// the server saying how many it allows rather than something going wrong, so
// the connection carries on with what it was given and does not ask again.
func (c *Client) extraSessions() []*pkgsftp.Client {
	want := c.streams - 1
	c.extraMu.Lock()
	defer c.extraMu.Unlock()
	if want <= 0 || c.closed.Load() {
		return nil
	}

	if missing := want - len(c.extra); missing > 0 && !c.extraFull {
		opened := make([]*pkgsftp.Client, missing)
		var wg sync.WaitGroup
		for i := range opened {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if s, err := newSession(c.ssh); err == nil {
					opened[i] = s
				}
			}()
		}
		wg.Wait()
		for _, s := range opened {
			if s == nil {
				c.extraFull = true
				continue
			}
			c.extra = append(c.extra, s)
		}
	}
	return slices.Clone(c.extra)
}

// pruneExtraSessions drops the extra sessions that no longer answer, and lets
// the next large read open replacements for them.
func (c *Client) pruneExtraSessions() {
	c.extraMu.Lock()
	defer c.extraMu.Unlock()
	before := len(c.extra)
	c.extra = slices.DeleteFunc(c.extra, func(s *pkgsftp.Client) bool {
		if _, err := s.Getwd(); err != nil {
			s.Close()
			return true
		}
		return false
	})
	if len(c.extra) < before {
		c.extraFull = false
	}
}

// dropExtraSessions closes every extra session.
func (c *Client) dropExtraSessions() {
	c.extraMu.Lock()
	defer c.extraMu.Unlock()
	for _, s := range c.extra {
		s.Close()
	}
	c.extra = nil
}
