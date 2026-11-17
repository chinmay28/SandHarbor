package sftp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// patterned returns n bytes no two nearby offsets of which agree, so a block
// written in the wrong place cannot pass for the right one.
func patterned(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func readersOver(data []byte, n int) []io.ReaderAt {
	out := make([]io.ReaderAt, n)
	for i := range out {
		out[i] = bytes.NewReader(data)
	}
	return out
}

func TestCopyStripedCopiesEveryShape(t *testing.T) {
	const block = 16
	data := patterned(10*block + 3)

	for _, readers := range []int{1, 2, 3, 8} {
		for _, start := range []int64{0, 1, block - 1, block, 5*block + 2} {
			for _, end := range []int64{start, start + 1, start + block, start + block + 1, int64(len(data))} {
				if end > int64(len(data)) || end < start {
					continue
				}
				name := fmt.Sprintf("readers=%d/start=%d/end=%d", readers, start, end)
				t.Run(name, func(t *testing.T) {
					var out bytes.Buffer
					n, err := copyStriped(&out, readersOver(data, readers), start, end, block, 2)
					if err != nil {
						t.Fatalf("copy: %v", err)
					}
					if n != end-start {
						t.Errorf("copied %d, want %d", n, end-start)
					}
					if !bytes.Equal(out.Bytes(), data[start:end]) {
						t.Errorf("wrong bytes")
					}
				})
			}
		}
	}
}

// jittery answers each read after a random pause, so blocks finish out of
// order and the writer has to put them back.
type jittery struct {
	r   io.ReaderAt
	mu  sync.Mutex
	rng *rand.Rand
}

func (j *jittery) ReadAt(b []byte, off int64) (int, error) {
	j.mu.Lock()
	d := time.Duration(j.rng.Intn(3000)) * time.Microsecond
	j.mu.Unlock()
	time.Sleep(d)
	return j.r.ReadAt(b, off)
}

func TestCopyStripedKeepsOrderWhenBlocksFinishOutOfOrder(t *testing.T) {
	data := patterned(200*64 + 17)
	var readers []io.ReaderAt
	for i := 0; i < 5; i++ {
		readers = append(readers, &jittery{r: bytes.NewReader(data), rng: rand.New(rand.NewSource(int64(i)))})
	}

	var out bytes.Buffer
	n, err := copyStriped(&out, readers, 0, int64(len(data)), 64, 2)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if n != int64(len(data)) || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("copied %d bytes, want %d, equal=%v", n, len(data), bytes.Equal(out.Bytes(), data))
	}
}

func TestCopyStripedStopsCleanlyAtAFileThatShrank(t *testing.T) {
	data := patterned(100)
	// The file was measured at 160 bytes and is 100 by the time it is read.
	var out bytes.Buffer
	n, err := copyStriped(&out, readersOver(data, 3), 0, 160, 16, 2)
	if err != nil {
		t.Fatalf("a shrunk file should read as a shorter file, got %v", err)
	}
	if n != 100 || !bytes.Equal(out.Bytes(), data) {
		t.Errorf("copied %d bytes, want the 100 that are there", n)
	}
}

// failingAt is a reader whose reads at or past one offset fail.
type failingAt struct {
	r   io.ReaderAt
	at  int64
	err error
}

func (f failingAt) ReadAt(b []byte, off int64) (int, error) {
	if off+int64(len(b)) > f.at {
		if off >= f.at {
			return 0, f.err
		}
		n, _ := f.r.ReadAt(b[:f.at-off], off)
		return n, f.err
	}
	return f.r.ReadAt(b, off)
}

func TestCopyStripedReportsAReadErrorWithAnHonestCount(t *testing.T) {
	data := patterned(160)
	boom := errors.New("connection lost")
	var readers []io.ReaderAt
	for i := 0; i < 4; i++ {
		readers = append(readers, failingAt{r: bytes.NewReader(data), at: 70, err: boom})
	}

	var out bytes.Buffer
	n, err := copyStriped(&out, readers, 0, int64(len(data)), 16, 2)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if n != 70 {
		t.Errorf("count %d, want 70: everything before the failure and nothing after", n)
	}
	if !bytes.Equal(out.Bytes(), data[:n]) {
		t.Errorf("what was written is not the file's leading bytes")
	}
}

type failingWriter struct{ after int }

func (f *failingWriter) Write(b []byte) (int, error) {
	if f.after <= 0 {
		return 0, errors.New("disk full")
	}
	f.after--
	return len(b), nil
}

func TestCopyStripedStopsAtAWriteError(t *testing.T) {
	data := patterned(64 * 100)
	n, err := copyStriped(&failingWriter{after: 3}, readersOver(data, 4), 0, int64(len(data)), 64, 2)
	if err == nil || err.Error() != "disk full" {
		t.Fatalf("err = %v, want the writer's", err)
	}
	if n != 3*64 {
		t.Errorf("count %d, want %d", n, 3*64)
	}
}

// countingReader counts reads begun.
type countingReader struct {
	r     io.ReaderAt
	reads *atomic.Int64
}

func (c countingReader) ReadAt(b []byte, off int64) (int, error) {
	c.reads.Add(1)
	return c.r.ReadAt(b, off)
}

// gatedWriter holds its first write until released.
type gatedWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedWriter) Write(b []byte) (int, error) {
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	return len(b), nil
}

func TestCopyStripedReadsOnlyABoundedDistanceAhead(t *testing.T) {
	const readers, block = 3, 16
	data := patterned(block * 1000)

	var reads atomic.Int64
	var rs []io.ReaderAt
	for i := 0; i < readers; i++ {
		rs = append(rs, countingReader{r: bytes.NewReader(data), reads: &reads})
	}
	w := &gatedWriter{entered: make(chan struct{}), release: make(chan struct{})}

	errc := make(chan error, 1)
	go func() {
		_, err := copyStriped(w, rs, 0, int64(len(data)), block, 2)
		errc <- err
	}()

	<-w.entered
	// Long enough for an unbounded reader to run far ahead of a writer that
	// is not taking anything.
	time.Sleep(50 * time.Millisecond)
	if got, limit := reads.Load(), int64(3*readers); got > limit {
		t.Errorf("%d blocks read while the writer was stuck on the first; the limit is %d", got, limit)
	}
	close(w.release)
	if err := <-errc; err != nil {
		t.Fatalf("copy: %v", err)
	}
}

func TestCopyStripedRefusesNonsense(t *testing.T) {
	if _, err := copyStriped(io.Discard, nil, 0, 10, 4, 2); err == nil {
		t.Error("no readers should be an error")
	}
	if _, err := copyStriped(io.Discard, readersOver(nil, 1), 0, 10, 0, 2); err == nil {
		t.Error("a zero block size should be an error")
	}
	if _, err := copyStriped(io.Discard, readersOver(nil, 1), 0, 10, 4, 0); err == nil {
		t.Error("no reads in flight should be an error")
	}
}
