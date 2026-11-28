package server

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// A file arriving in pieces, and being put back together.
//
// An upload is one request per batch of files, and a request has a ceiling
// (MaxUploadSize) for good reasons: it is parsed whole before any of it is
// looked at, and everything between the browser and here — a proxy, a phone's
// radio — gets to fail it whole as well. That leaves a single file bigger than
// the ceiling with no way in through the browser at all, and those are exactly
// the files people most want in: a film, or the archive a photo service
// exports, which comes in parts of two gigabytes and up.
//
// So a big file is sent a piece at a time. Opening an upload says how long the
// file is and makes somewhere to put it; each piece says where in the file it
// starts and is appended there; and when the last has landed the whole file is
// handed over, to be stored or unpacked exactly as it would have been had it
// arrived in one request. A piece that fails is sent again, alone, and a
// sender that has lost track of what arrived is told.
//
// This type is only the putting back together. It knows nothing of vaults or
// archives: P is whatever the caller decided, when the upload was opened,
// should happen to the file once it is whole.

// maxOpenPieceUploads is how many files may be part-way here at once. Each is
// a file on this machine's disk, so the ceiling is on that. A browser sends
// one at a time.
const maxOpenPieceUploads = 8

// pieceUploadIdle is how long an upload may go without a piece before it is
// given up on and what had arrived removed. Long enough for a phone to come
// back from a tunnel; short enough that an abandoned film is not on the disk
// tomorrow.
const pieceUploadIdle = 30 * time.Minute

var (
	errNoPieceUpload       = errors.New("no such upload — it was finished, cancelled, or left idle too long")
	errTooManyPieceUploads = errors.New("too many large uploads are already under way; wait for one to finish")
	errPieceTooLong        = errors.New("more was sent than the file was said to hold")
)

// errPieceOffset is a piece that does not start where the file has got to.
// Received is where it has got to, which is where the next piece must start.
type errPieceOffset struct {
	Received int64
}

func (e *errPieceOffset) Error() string {
	return fmt.Sprintf("that piece does not start where the upload has got to (%d bytes)", e.Received)
}

// errPiecesMissing is an upload asked for before all of it arrived.
type errPiecesMissing struct {
	Received, Size int64
}

func (e *errPiecesMissing) Error() string {
	return fmt.Sprintf("only %d of the file's %d bytes have arrived", e.Received, e.Size)
}

// pieceUpload is one file part-way here.
type pieceUpload[P any] struct {
	ID   string
	Size int64
	// Plan is what the opener said should become of the file.
	Plan P

	// File holds what has arrived, and Discard removes it. Whoever takes the
	// upload owns both.
	File    *os.File
	Discard func()

	mu       sync.Mutex
	received int64
	closed   bool
	// touched is when a piece last landed, in nanoseconds: read by the sweep
	// without the lock above, which a piece in flight is holding.
	touched atomic.Int64
}

// pieceUploads holds the files part-way here.
type pieceUploads[P any] struct {
	mu   sync.Mutex
	open map[string]*pieceUpload[P]

	idle time.Duration
	// now is time.Now everywhere but in tests.
	now func() time.Time
}

func newPieceUploads[P any]() *pieceUploads[P] {
	return &pieceUploads[P]{open: map[string]*pieceUpload[P]{}, idle: pieceUploadIdle, now: time.Now}
}

// begin opens an upload of a file size bytes long, to be written into file.
// discard is called when the upload is given up on — or straight away, if it
// is refused.
func (u *pieceUploads[P]) begin(size int64, plan P, file *os.File, discard func()) (*pieceUpload[P], error) {
	u.sweep()

	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.open) >= maxOpenPieceUploads {
		discard()
		return nil, errTooManyPieceUploads
	}
	up := &pieceUpload[P]{ID: uuid.NewString(), Size: size, Plan: plan, File: file, Discard: discard}
	up.touched.Store(u.now().UnixNano())
	u.open[up.ID] = up
	return up, nil
}

func (u *pieceUploads[P]) find(id string) (*pieceUpload[P], error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	up, ok := u.open[id]
	if !ok {
		return nil, errNoPieceUpload
	}
	return up, nil
}

// write appends a piece that starts at offset, and answers with how much of
// the file has now arrived. A piece that starts anywhere but where the file
// has got to is refused with *errPieceOffset and nothing is written — which
// is also how a sender whose last answer was lost finds out it landed. A
// piece cut short part-way is kept as far as it got.
func (u *pieceUploads[P]) write(id string, offset int64, piece io.Reader) (int64, error) {
	up, err := u.find(id)
	if err != nil {
		return 0, err
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	if up.closed {
		return 0, errNoPieceUpload
	}
	if offset != up.received {
		return up.received, &errPieceOffset{Received: up.received}
	}

	// One byte past the end is read for, so that too much is caught as too
	// much rather than quietly cut to fit.
	room := up.Size - up.received
	n, err := io.Copy(io.NewOffsetWriter(up.File, up.received), io.LimitReader(piece, room+1))
	if n > room {
		return up.received, errPieceTooLong
	}
	up.received += n
	up.touched.Store(u.now().UnixNano())
	return up.received, err
}

// take hands a whole upload over and forgets it: the caller now owns its file
// and must call Discard. An upload that is not all here is left open and
// answered with *errPiecesMissing.
func (u *pieceUploads[P]) take(id string) (*pieceUpload[P], error) {
	up, err := u.find(id)
	if err != nil {
		return nil, err
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	if up.closed {
		return nil, errNoPieceUpload
	}
	if up.received != up.Size {
		return nil, &errPiecesMissing{Received: up.received, Size: up.Size}
	}
	up.closed = true

	u.mu.Lock()
	delete(u.open, id)
	u.mu.Unlock()
	return up, nil
}

// abort gives an upload up and removes what had arrived of it.
func (u *pieceUploads[P]) abort(id string) bool {
	u.mu.Lock()
	up, ok := u.open[id]
	delete(u.open, id)
	u.mu.Unlock()
	if !ok {
		return false
	}
	up.drop()
	return true
}

// drop closes an upload nobody took, once, waiting out a piece in flight.
func (up *pieceUpload[P]) drop() {
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.closed {
		return
	}
	up.closed = true
	up.Discard()
}

// stopAll gives every upload up. Locking the vault calls it: what has arrived
// is plaintext on the disk, and nothing can be stored with the keys gone.
func (u *pieceUploads[P]) stopAll() {
	u.mu.Lock()
	dropped := u.open
	u.open = map[string]*pieceUpload[P]{}
	u.mu.Unlock()

	for _, up := range dropped {
		up.drop()
	}
}

// abortWhere gives up every upload whose plan the caller picks out — the ones
// bound for a sub vault that is being locked.
func (u *pieceUploads[P]) abortWhere(match func(P) bool) {
	u.mu.Lock()
	var dropped []*pieceUpload[P]
	for id, up := range u.open {
		if match(up.Plan) {
			dropped = append(dropped, up)
			delete(u.open, id)
		}
	}
	u.mu.Unlock()

	for _, up := range dropped {
		up.drop()
	}
}

// count is how many uploads are part-way here.
func (u *pieceUploads[P]) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.open)
}

// sweep gives up on the uploads that have gone too long without a piece.
func (u *pieceUploads[P]) sweep() {
	u.mu.Lock()
	var stale []*pieceUpload[P]
	for id, up := range u.open {
		if u.now().Sub(time.Unix(0, up.touched.Load())) > u.idle {
			stale = append(stale, up)
			delete(u.open, id)
		}
	}
	u.mu.Unlock()

	for _, up := range stale {
		up.drop()
	}
}
