package vault

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// delegatingReader is a source with a faster way to move bytes than Read —
// the shape of an *sftp.File — that records which of the two was used.
type delegatingReader struct {
	*bytes.Reader
	delegated bool
}

func (d *delegatingReader) WriteTo(w io.Writer) (int64, error) {
	d.delegated = true
	return d.Reader.WriteTo(w)
}

// The wrapper must not cost the source its fast path. io.Copy prefers a
// source's WriteTo over a Read loop, and for an SFTP file the two differ by
// an order of magnitude on a distant server — so watching a transfer has to
// delegate, and still count every byte while it does.
func TestProgressReaderDelegatesWriteTo(t *testing.T) {
	body := bytes.Repeat([]byte("x"), progressEvery+123)
	src := &delegatingReader{Reader: bytes.NewReader(body)}

	var reports []int64
	p := &progressReader{r: src, say: func(done int64) { reports = append(reports, done) }}

	var dst bytes.Buffer
	n, err := io.Copy(&dst, p)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if !src.delegated {
		t.Error("io.Copy went through Read: the source's own WriteTo was never used")
	}
	if n != int64(len(body)) || !bytes.Equal(dst.Bytes(), body) {
		t.Fatalf("copied %d bytes, want %d intact", n, len(body))
	}

	// The tail past the last throttle window is still said — the same "no bar
	// stuck at 99%" promise Read makes — and the counts only ever grow.
	if len(reports) == 0 || reports[len(reports)-1] != int64(len(body)) {
		t.Fatalf("reports were %v, want the last to be %d", reports, len(body))
	}
	for i := 1; i < len(reports); i++ {
		if reports[i] < reports[i-1] {
			t.Fatalf("reports went backwards: %v", reports)
		}
	}
}

// A source with nothing faster underneath is copied through Read, without the
// delegation looping back into itself.
func TestProgressReaderWriteToWithoutADelegate(t *testing.T) {
	const body = "nothing faster here"
	src := struct{ io.Reader }{strings.NewReader(body)}

	var last int64
	p := &progressReader{r: src, say: func(done int64) { last = done }}

	var dst bytes.Buffer
	n, err := p.WriteTo(&dst)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if n != int64(len(body)) || dst.String() != body {
		t.Fatalf("copied %d bytes as %q, want %q", n, dst.String(), body)
	}
	if last != int64(len(body)) {
		t.Errorf("the final report said %d bytes, want %d", last, len(body))
	}
}

// A slow transfer is still heard from. Four megabytes is six minutes at
// 90 kbit/s, and a reader that waited for them would leave the dialog with
// nothing to show a speed from — so bytes moving at all are said at least
// once a progressInterval.
func TestProgressReaderSpeaksUpOnASlowTransfer(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	var reports []int64
	p := &progressReader{
		r:   strings.NewReader(strings.Repeat("x", 100)),
		say: func(done int64) { reports = append(reports, done) },
		now: func() time.Time { return clock },
	}

	buf := make([]byte, 10)
	read := func() {
		if _, err := p.Read(buf); err != nil {
			t.Fatalf("read: %v", err)
		}
	}

	read() // counting starts
	clock = clock.Add(progressInterval / 2)
	read()
	if len(reports) != 0 {
		t.Fatalf("reported %v within one interval of a few bytes", reports)
	}
	clock = clock.Add(progressInterval / 2)
	read()
	if len(reports) != 1 || reports[0] != 30 {
		t.Fatalf("reports %v, want one saying 30 once the interval passed", reports)
	}

	// And the interval runs from that report, not from the start.
	clock = clock.Add(progressInterval - time.Millisecond)
	read()
	if len(reports) != 1 {
		t.Fatalf("reported again %v before an interval had passed since the last", reports)
	}
	clock = clock.Add(time.Millisecond)
	read()
	if len(reports) != 2 || reports[1] != 50 {
		t.Fatalf("reports %v, want a second saying 50", reports)
	}
}

// Time alone says nothing: a stalled transfer is not reported as moving.
func TestProgressReaderSaysNothingWhenNothingMoves(t *testing.T) {
	clock := time.Unix(1_000_000, 0)
	var reports []int64
	p := &progressReader{
		r:   strings.NewReader("abc"),
		say: func(done int64) { reports = append(reports, done) },
		now: func() time.Time { return clock },
	}
	p.count(0)
	clock = clock.Add(10 * progressInterval)
	p.count(0)
	if len(reports) != 0 {
		t.Errorf("reported %v with no bytes moved", reports)
	}
}
