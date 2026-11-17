package sftp

import (
	"errors"
	"io"
	"sync"
)

// copyStriped copies [start, end) of a file to w, reading it a block at a time
// through several readers at once and writing the blocks out in order.
//
// It exists because one SFTP session over one SSH connection is held to less
// than the link, two ways. golang.org/x/crypto/ssh fixes a channel's window at
// 2 MiB with no way to change it, so a session has at most 2 MiB on the wire
// however many requests pkg/sftp queues — about 30 MB/s to a machine 60 ms
// away. And one connection is one TCP flow, which on a path that loses
// packets runs far slower again: see Client.extraSessions. Each reader is a
// session on a connection of its own, so each brings its own window and its
// own flow.
//
// The readers are interchangeable: each is the same file, opened on a
// different connection. Which block a reader is handed is decided by whichever
// of its reads is free first, so a slow connection holds up only the blocks
// it is reading.
//
// Each reader has inFlight blocks being read at once. Blocks are meant to be
// small — one SFTP packet — and inFlight large enough to fill the window, the
// way pkg/sftp's own WriteTo does it: then w is handed the file a packet at a
// time, each as soon as it is next in order, and whoever is counting what
// has arrived sees it arrive. Large blocks would carry the same bytes as fast
// but would hand nothing on until a whole block was in, which on a slow link
// is minutes of a transfer that looks stopped.
//
// Memory is bounded. At most inFlight+1 blocks per reader are out at once,
// counting the ones being read and the ones waiting behind a slower block for
// their turn to be written; no block is handed out until the writer has given
// one back. The one over inFlight is what lets a reader start its next block
// while the writer is still busy with the last.
//
// The copy ends at end, or earlier at the first block that comes back short
// with io.EOF — a file that shrank after it was measured — in which case what
// is returned is everything up to where it stopped, and no error: the same
// answer a sequential read of the file would have given. Any other error ends
// the copy and is returned with the count of bytes w accepted before it, all
// of which are the file's leading bytes in order.
func copyStriped(w io.Writer, readers []io.ReaderAt, start, end int64, block, inFlight int) (int64, error) {
	if len(readers) == 0 {
		return 0, errors.New("copyStriped: no readers")
	}
	if block <= 0 {
		return 0, errors.New("copyStriped: block size must be positive")
	}
	if inFlight <= 0 {
		return 0, errors.New("copyStriped: reads in flight must be positive")
	}
	if end <= start {
		return 0, nil
	}

	size := int64(block)
	blocks := (end - start + size - 1) / size
	ahead := (inFlight + 1) * len(readers)

	type job struct {
		idx int64
		buf []byte
	}
	type result struct {
		idx int64
		buf []byte
		n   int
		err error
	}

	// free holds one token per block allowed out; a nil token is a buffer not
	// yet allocated, so a short file never allocates the whole allowance.
	free := make(chan []byte, ahead)
	for i := 0; i < ahead; i++ {
		free <- nil
	}
	jobs := make(chan job)
	results := make(chan result, ahead)
	done := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(jobs)
		for idx := int64(0); idx < blocks; idx++ {
			var buf []byte
			select {
			case buf = <-free:
			case <-done:
				return
			}
			if buf == nil {
				buf = make([]byte, block)
			}
			select {
			case jobs <- job{idx: idx, buf: buf}:
			case <-done:
				return
			}
		}
	}()

	for _, r := range readers {
		for range inFlight {
			wg.Add(1)
			go func(r io.ReaderAt) {
				defer wg.Done()
				for j := range jobs {
					off := start + j.idx*size
					want := min(size, end-off)
					n, err := r.ReadAt(j.buf[:want], off)
					select {
					case results <- result{idx: j.idx, buf: j.buf, n: n, err: err}:
					case <-done:
						return
					}
				}
			}(r)
		}
	}

	// Whatever way the copy ends, nothing it started is left running.
	defer func() {
		close(done)
		wg.Wait()
	}()

	var written int64
	pending := make(map[int64]result, ahead)
	for next := int64(0); next < blocks; {
		r := <-results
		pending[r.idx] = r

		for {
			r, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)

			want := int(min(size, end-(start+next*size)))
			if r.n < want && !errors.Is(r.err, io.EOF) {
				// What did arrive is still the file's next bytes, in order;
				// handing it on keeps the count returned honest about where
				// the failure was.
				n, werr := w.Write(r.buf[:r.n])
				written += int64(n)
				if werr != nil {
					return written, werr
				}
				if r.err == nil {
					r.err = io.ErrUnexpectedEOF
				}
				return written, r.err
			}

			n, err := w.Write(r.buf[:r.n])
			written += int64(n)
			if err != nil {
				return written, err
			}
			if n < r.n {
				return written, io.ErrShortWrite
			}
			if r.n < want {
				// Short with io.EOF: the file ends here.
				return written, nil
			}

			free <- r.buf
			next++
		}
	}
	return written, nil
}
