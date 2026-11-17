package shardcache

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

const archA = "0123456789abcdef0123456789abcdef"
const archB = "fedcba9876543210fedcba9876543210"

// fakeDisk is a disk of a fixed size whose free space shrinks by what the
// store writes to it, so the reserve can be tested without filling a real one.
type fakeDisk struct {
	mu    sync.Mutex
	dir   string
	total int64
	other int64 // bytes used by everything else on the disk
	err   error
}

func (d *fakeDisk) measure(string) (int64, int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return 0, 0, d.err
	}
	return d.total - d.other - dirSize(d.dir), d.total, nil
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			n += info.Size()
		}
		return nil
	})
	return n
}

func openTest(t *testing.T, opts Options) (*Store, *fakeDisk) {
	t.Helper()
	dir := t.TempDir()
	disk := &fakeDisk{dir: dir, total: 1 << 40}
	if opts.Disk == nil {
		opts.Disk = disk.measure
	}
	s, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, disk
}

func TestPutGetRoundTrip(t *testing.T) {
	s, _ := openTest(t, Options{})
	k := Key{Archive: archA, Chunk: 3, Part: 2}
	blob := []byte("sealed bytes")

	if _, err := s.Get(k); !errors.Is(err, ErrMiss) {
		t.Fatalf("Get before Put: %v, want ErrMiss", err)
	}
	if err := s.Put(k, blob); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(k)
	if err != nil || !bytes.Equal(got, blob) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if !s.Has(k) {
		t.Fatal("Has = false after Put")
	}
	if u := s.Usage(); u.Used != int64(len(blob)) || u.Parts != 1 {
		t.Fatalf("Usage = %+v", u)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), archA, "c0000003-p2.sand")); err != nil {
		t.Fatalf("part not where the layout says: %v", err)
	}
}

func TestPutReplacesWithoutDoubleCounting(t *testing.T) {
	s, _ := openTest(t, Options{})
	k := Key{Archive: archA, Chunk: 0, Part: 1}
	_ = s.Put(k, []byte("12345"))
	_ = s.Put(k, []byte("12"))
	if u := s.Usage(); u.Used != 2 || u.Parts != 1 {
		t.Fatalf("Usage = %+v, want 2 bytes in 1 part", u)
	}
}

func TestBadKeysRefused(t *testing.T) {
	s, _ := openTest(t, Options{})
	for _, k := range []Key{
		{Archive: "../../etc", Chunk: 0, Part: 1},
		{Archive: "ABCDEF0123456789ABCDEF0123456789", Chunk: 0, Part: 1},
		{Archive: archA, Chunk: -1, Part: 1},
		{Archive: archA, Chunk: 0, Part: 0},
		{Archive: archA, Chunk: 0, Part: 256},
	} {
		if err := s.Put(k, []byte("x")); !errors.Is(err, ErrBadKey) {
			t.Errorf("Put(%+v) = %v, want ErrBadKey", k, err)
		}
		if _, err := s.Get(k); !errors.Is(err, ErrBadKey) {
			t.Errorf("Get(%+v) = %v, want ErrBadKey", k, err)
		}
	}
	if _, err := s.RemoveArchive(".."); !errors.Is(err, ErrBadKey) {
		t.Errorf("RemoveArchive(..) = %v", err)
	}
}

func TestLimitRefusesWhatDoesNotFit(t *testing.T) {
	s, _ := openTest(t, Options{Limit: 10, Reserve: -1})
	if err := s.Put(Key{archA, 0, 1}, make([]byte, 6)); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if err := s.Put(Key{archA, 0, 2}, make([]byte, 6)); !errors.Is(err, ErrFull) {
		t.Fatalf("second Put = %v, want ErrFull", err)
	}
	if s.Has(Key{archA, 0, 2}) {
		t.Fatal("a refused part is held")
	}
	if err := s.Put(Key{archA, 0, 2}, make([]byte, 4)); err != nil {
		t.Fatalf("a part that fits exactly: %v", err)
	}
	if u := s.Usage(); u.Room != 0 {
		t.Fatalf("Room = %d, want 0", u.Room)
	}
}

func TestReserveKeepsDiskFree(t *testing.T) {
	s, disk := openTest(t, Options{Reserve: 100})
	disk.total = 1000
	disk.other = 850 // 150 free, 100 of it reserved: 50 to spend

	if err := s.Put(Key{archA, 0, 1}, make([]byte, 40)); err != nil {
		t.Fatalf("Put within the reserve: %v", err)
	}
	if err := s.Put(Key{archA, 0, 2}, make([]byte, 20)); !errors.Is(err, ErrFull) {
		t.Fatalf("Put into the reserve = %v, want ErrFull", err)
	}
	// Something else on the machine frees space: the cache may grow again.
	disk.other = 700
	if err := s.Put(Key{archA, 0, 2}, make([]byte, 20)); err != nil {
		t.Fatalf("Put after space was freed: %v", err)
	}
}

func TestDefaultReserveIsATenthClamped(t *testing.T) {
	for _, c := range []struct{ total, want int64 }{
		{4 << 30, MinReserve},
		{64 << 30, 64 << 30 / 10},
		{1 << 44, MaxReserve},
	} {
		if got := DefaultReserve(c.total); got != c.want {
			t.Errorf("DefaultReserve(%d) = %d, want %d", c.total, got, c.want)
		}
	}
	s, disk := openTest(t, Options{})
	disk.total = 64 << 30
	if u := s.Usage(); u.Reserve != DefaultReserve(64<<30) {
		t.Fatalf("Usage.Reserve = %d", u.Reserve)
	}
}

func TestUnmeasurableDiskNeedsALimit(t *testing.T) {
	s, disk := openTest(t, Options{})
	disk.err = errors.New("no statfs here")
	if err := s.Put(Key{archA, 0, 1}, []byte("x")); !errors.Is(err, ErrFull) {
		t.Fatalf("Put with no limit on an unmeasurable disk = %v, want ErrFull", err)
	}
	s.SetLimits(10, 0)
	if err := s.Put(Key{archA, 0, 1}, []byte("x")); err != nil {
		t.Fatalf("Put with a limit on an unmeasurable disk: %v", err)
	}
}

func TestReopenTakesStockAndClearsHalfWrites(t *testing.T) {
	s, disk := openTest(t, Options{})
	_ = s.Put(Key{archA, 0, 1}, []byte("aaa"))
	_ = s.Put(Key{archA, 1, 2}, []byte("bb"))
	_ = s.Put(Key{archB, 0, 3}, []byte("c"))

	half := filepath.Join(s.Dir(), archA, tempPrefix+"123")
	stranger := filepath.Join(s.Dir(), "notes.txt")
	_ = os.WriteFile(half, []byte("half"), 0o600)
	_ = os.WriteFile(stranger, []byte("mine"), 0o600)

	again, err := Open(s.Dir(), Options{Disk: disk.measure})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if u := again.Usage(); u.Used != 6 || u.Parts != 3 {
		t.Fatalf("Usage after reopen = %+v", u)
	}
	if got := again.Parts(archA, 1); len(got) != 1 || got[0] != 2 {
		t.Fatalf("Parts(archA, 1) = %v", got)
	}
	if _, err := os.Stat(half); !os.IsNotExist(err) {
		t.Fatalf("half-written part survived reopen: %v", err)
	}
	if _, err := os.Stat(stranger); err != nil {
		t.Fatalf("a file the store does not own was touched: %v", err)
	}
}

func TestRemoveArchiveFreesItsBytes(t *testing.T) {
	s, _ := openTest(t, Options{})
	_ = s.Put(Key{archA, 0, 1}, []byte("aaa"))
	_ = s.Put(Key{archA, 1, 1}, []byte("aa"))
	_ = s.Put(Key{archB, 0, 1}, []byte("b"))

	freed, err := s.RemoveArchive(archA)
	if err != nil || freed != 5 {
		t.Fatalf("RemoveArchive = %d, %v", freed, err)
	}
	if got := s.Archives(); len(got) != 1 || got[archB] != 1 {
		t.Fatalf("Archives = %v", got)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), archA)); !os.IsNotExist(err) {
		t.Fatalf("archive folder survived: %v", err)
	}
	if freed, err := s.RemoveArchive(archA); err != nil || freed != 0 {
		t.Fatalf("second RemoveArchive = %d, %v", freed, err)
	}
}

func TestRemoveAndVanishedFiles(t *testing.T) {
	s, _ := openTest(t, Options{})
	k := Key{archA, 0, 1}
	_ = s.Put(k, []byte("aaa"))
	if err := s.Remove(k); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := s.Remove(k); err != nil {
		t.Fatalf("Remove twice: %v", err)
	}

	// A part deleted behind the store's back is a miss, and forgotten.
	_ = s.Put(k, []byte("aaa"))
	_ = os.Remove(filepath.Join(s.Dir(), archA, "c0000000-p1.sand"))
	if _, err := s.Get(k); !errors.Is(err, ErrMiss) {
		t.Fatalf("Get of a vanished part = %v", err)
	}
	if s.Has(k) || s.Usage().Used != 0 {
		t.Fatal("vanished part still counted")
	}
}

func TestConcurrentPutsStayWithinLimit(t *testing.T) {
	s, _ := openTest(t, Options{Limit: 100, Reserve: -1})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.Put(Key{archA, i, 1}, make([]byte, 10))
		}(i)
	}
	wg.Wait()
	if u := s.Usage(); u.Used > 100 || u.Parts != 10 {
		t.Fatalf("Usage = %+v, want exactly 10 parts within 100 bytes", u)
	}
}

func TestCountsPerChunk(t *testing.T) {
	s, _ := openTest(t, Options{})
	_ = s.Put(Key{archA, 0, 1}, []byte("a"))
	_ = s.Put(Key{archA, 0, 3}, []byte("a"))
	_ = s.Put(Key{archA, 2, 2}, []byte("a"))
	_ = s.Put(Key{archB, 0, 1}, []byte("b"))
	got := s.Counts(archA)
	if len(got) != 2 || got[0] != 2 || got[2] != 1 {
		t.Fatalf("Counts = %v", got)
	}
	if got := s.Counts("nothing"); len(got) != 0 {
		t.Fatalf("Counts of an unknown archive = %v", got)
	}
}
