package server

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pieceFile makes somewhere for an upload to land, and counts its discards.
func pieceFile(t *testing.T) (f *os.File, discard func(), discards *int) {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "spool"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	return f, func() { n++; f.Close() }, &n
}

func contents(t *testing.T, f *os.File) string {
	t.Helper()
	data, err := io.ReadAll(io.NewSectionReader(f, 0, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPiecesArePutBackTogetherInOrder(t *testing.T) {
	u := newPieceUploads[string]()
	f, discard, discards := pieceFile(t)
	up, err := u.begin(11, "the plan", f, discard)
	if err != nil {
		t.Fatal(err)
	}

	if got, err := u.write(up.ID, 0, strings.NewReader("hello ")); got != 6 || err != nil {
		t.Fatalf("first piece: %d, %v", got, err)
	}
	if got, err := u.write(up.ID, 6, strings.NewReader("world")); got != 11 || err != nil {
		t.Fatalf("second piece: %d, %v", got, err)
	}

	whole, err := u.take(up.ID)
	if err != nil {
		t.Fatal(err)
	}
	if whole.Plan != "the plan" || whole.Size != 11 || contents(t, whole.File) != "hello world" {
		t.Errorf("took %q with plan %q", contents(t, whole.File), whole.Plan)
	}
	// Taken is the caller's: forgotten here, and not discarded behind its back.
	if u.count() != 0 || *discards != 0 {
		t.Errorf("count = %d, discards = %d", u.count(), *discards)
	}
	if _, err := u.take(up.ID); !errors.Is(err, errNoPieceUpload) {
		t.Errorf("a second take: %v", err)
	}
	if _, err := u.write(up.ID, 11, strings.NewReader("x")); !errors.Is(err, errNoPieceUpload) {
		t.Errorf("a write after the take: %v", err)
	}
}

// How a sender whose answer was lost finds out its piece landed, and how one
// that skipped ahead is put right: told where the file has got to, with
// nothing written.
func TestAPieceInTheWrongPlaceIsRefusedAndToldWhere(t *testing.T) {
	u := newPieceUploads[int]()
	f, discard, _ := pieceFile(t)
	up, _ := u.begin(10, 0, f, discard)
	u.write(up.ID, 0, strings.NewReader("abcde"))

	for _, offset := range []int64{0, 3, 7} {
		got, err := u.write(up.ID, offset, strings.NewReader("ZZZZZ"))
		var misplaced *errPieceOffset
		if !errors.As(err, &misplaced) || misplaced.Received != 5 || got != 5 {
			t.Errorf("offset %d: %d, %v — want to be told 5", offset, got, err)
		}
	}
	if got := contents(t, f); got != "abcde" {
		t.Errorf("file = %q, a refused piece wrote to it", got)
	}
}

func TestMoreThanTheFileWasSaidToHoldIsRefused(t *testing.T) {
	u := newPieceUploads[int]()
	f, discard, _ := pieceFile(t)
	up, _ := u.begin(4, 0, f, discard)

	got, err := u.write(up.ID, 0, strings.NewReader("abcdefgh"))
	if !errors.Is(err, errPieceTooLong) || got != 0 {
		t.Errorf("%d, %v — want the whole piece refused", got, err)
	}
	// And it can still be sent properly.
	if got, err := u.write(up.ID, 0, strings.NewReader("abcd")); got != 4 || err != nil {
		t.Errorf("%d, %v", got, err)
	}
}

// brokenReader gives some bytes and then fails, as a dropped connection does.
type brokenReader struct{ data string }

func (b *brokenReader) Read(p []byte) (int, error) {
	if b.data == "" {
		return 0, errors.New("connection reset")
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

func TestAPieceCutShortIsKeptAsFarAsItGot(t *testing.T) {
	u := newPieceUploads[int]()
	f, discard, _ := pieceFile(t)
	up, _ := u.begin(10, 0, f, discard)

	got, err := u.write(up.ID, 0, &brokenReader{data: "abc"})
	if err == nil || got != 3 {
		t.Fatalf("%d, %v — want the three bytes kept and the failure said", got, err)
	}
	// The sender picks up from where it is told.
	if got, err := u.write(up.ID, 3, strings.NewReader("defghij")); got != 10 || err != nil {
		t.Fatalf("%d, %v", got, err)
	}
	if whole, _ := u.take(up.ID); contents(t, whole.File) != "abcdefghij" {
		t.Errorf("file = %q", contents(t, whole.File))
	}
}

func TestAnUploadIsNotHandedOverUntilAllOfItIsHere(t *testing.T) {
	u := newPieceUploads[int]()
	f, discard, _ := pieceFile(t)
	up, _ := u.begin(10, 0, f, discard)
	u.write(up.ID, 0, strings.NewReader("abc"))

	_, err := u.take(up.ID)
	var missing *errPiecesMissing
	if !errors.As(err, &missing) || missing.Received != 3 || missing.Size != 10 {
		t.Fatalf("err = %v", err)
	}
	// Still open, to be finished.
	if got, err := u.write(up.ID, 3, strings.NewReader("defghij")); got != 10 || err != nil {
		t.Errorf("%d, %v", got, err)
	}
}

func TestAbortAndStopAllDiscardExactlyOnce(t *testing.T) {
	u := newPieceUploads[int]()
	f1, d1, n1 := pieceFile(t)
	f2, d2, n2 := pieceFile(t)
	a, _ := u.begin(5, 0, f1, d1)
	b, _ := u.begin(5, 0, f2, d2)

	if !u.abort(a.ID) || u.abort(a.ID) {
		t.Error("abort should answer once")
	}
	u.stopAll()
	u.stopAll()
	if *n1 != 1 || *n2 != 1 || u.count() != 0 {
		t.Errorf("discards = %d, %d; count = %d", *n1, *n2, u.count())
	}
	if _, err := u.write(b.ID, 0, strings.NewReader("x")); !errors.Is(err, errNoPieceUpload) {
		t.Errorf("a write after stopAll: %v", err)
	}
}

func TestAnUploadLeftIdleIsGivenUpOn(t *testing.T) {
	u := newPieceUploads[int]()
	now := time.Now()
	u.now = func() time.Time { return now }

	f1, d1, stale := pieceFile(t)
	f2, d2, fresh := pieceFile(t)
	old, _ := u.begin(5, 0, f1, d1)
	now = now.Add(pieceUploadIdle - time.Minute)
	kept, _ := u.begin(5, 0, f2, d2)
	// A piece is what keeps one alive, so only the other has gone quiet.
	now = now.Add(2 * time.Minute)
	u.sweep()

	if *stale != 1 || *fresh != 0 {
		t.Errorf("discards = %d, %d — want only the idle one", *stale, *fresh)
	}
	if _, err := u.write(old.ID, 0, strings.NewReader("x")); !errors.Is(err, errNoPieceUpload) {
		t.Errorf("the idle upload: %v", err)
	}
	if _, err := u.write(kept.ID, 0, strings.NewReader("x")); err != nil {
		t.Errorf("the live upload: %v", err)
	}
}

func TestTooManyUploadsAtOnceAreRefusedAndDiscarded(t *testing.T) {
	u := newPieceUploads[int]()
	for i := 0; i < maxOpenPieceUploads; i++ {
		f, d, _ := pieceFile(t)
		if _, err := u.begin(1, 0, f, d); err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
	}
	f, d, n := pieceFile(t)
	if _, err := u.begin(1, 0, f, d); !errors.Is(err, errTooManyPieceUploads) || *n != 1 {
		t.Errorf("err = %v, discards = %d", err, *n)
	}
}

func TestAbortWhereGivesUpOnlyTheUploadsPickedOut(t *testing.T) {
	u := newPieceUploads[string]()
	f1, d1, n1 := pieceFile(t)
	f2, d2, n2 := pieceFile(t)
	u.begin(1, "sub", f1, d1)
	u.begin(1, "main", f2, d2)

	u.abortWhere(func(plan string) bool { return plan == "sub" })
	if *n1 != 1 || *n2 != 0 || u.count() != 1 {
		t.Errorf("discards = %d, %d; count = %d", *n1, *n2, u.count())
	}
}
