package vault

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chinmay28/SandHarbor/internal/archive"
	"github.com/chinmay28/SandHarbor/internal/sandfile"
)

// keptVault is a chunked test vault whose local cache leaves nothing in
// reserve, so the tests do not depend on how full the machine's disk is.
func keptVault(t *testing.T) (*Vault, []string) {
	t.Helper()
	v, roots := chunkedVault(t, 3, 1024)
	if err := v.SetLocalCacheLimits(0, -1); err != nil {
		t.Fatalf("SetLocalCacheLimits: %v", err)
	}
	return v, roots
}

func putFile(t *testing.T, v *Vault, dir, name string, data []byte) *Entry {
	t.Helper()
	if dir != "/" {
		_ = v.Mkdir(MainScope, dir)
	}
	entry, _, err := v.Upload(t.Context(), MainScope, dir, name, data, UploadOptions{})
	if err != nil {
		t.Fatalf("Upload %s: %v", name, err)
	}
	return entry
}

// fillAll runs fill steps until there is nothing left to do.
func fillAll(t *testing.T, v *Vault) LocalCacheStep {
	t.Helper()
	var last LocalCacheStep
	for i := 0; i < 100; i++ {
		step, err := v.FillLocalCache(t.Context(), 1<<20)
		if err != nil {
			t.Fatalf("FillLocalCache: %v", err)
		}
		last = step
		if step.Idle || step.Full {
			return last
		}
	}
	t.Fatal("the fill never finished")
	return last
}

func pinStatus(t *testing.T, v *Vault, path string) CachePinStatus {
	t.Helper()
	status, err := v.LocalCache()
	if err != nil {
		t.Fatalf("LocalCache: %v", err)
	}
	for _, p := range status.Pins {
		if p.Path == path {
			return p
		}
	}
	t.Fatalf("no pin for %s in %+v", path, status.Pins)
	return CachePinStatus{}
}

func readWhole(t *testing.T, v *Vault, id string) []byte {
	t.Helper()
	v.chunks.clear() // the decrypted chunks in memory are not what is tested
	rs, _, err := v.OpenReadSeeker(t.Context(), id)
	if err != nil {
		t.Fatalf("OpenReadSeeker: %v", err)
	}
	got, err := io.ReadAll(rs)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	return got
}

func TestKeptFolderReadsWithEveryCloudGone(t *testing.T) {
	v, roots := keptVault(t)
	film := readerPayload(5000)
	photo := readerPayload(3000)
	e1 := putFile(t, v, "/films", "film.mkv", film)
	e2 := putFile(t, v, "/films", "photo.jpg", photo)

	if _, err := v.KeepLocally(MainScope, "/films", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("KeepLocally: %v", err)
	}
	if pin := pinStatus(t, v, "/films"); pin.Complete || pin.Files != 2 || pin.Cached != 0 {
		t.Fatalf("before the fill: %+v", pin)
	}

	fillAll(t, v)
	pin := pinStatus(t, v, "/films")
	if !pin.Complete || pin.Cached != pin.Bytes || pin.Bytes == 0 {
		t.Fatalf("after the fill: %+v", pin)
	}

	for _, root := range roots {
		darken(t, root)
	}
	if got := readWhole(t, v, e1.ID); !bytes.Equal(got, film) {
		t.Fatal("the film read back from the cache is not the film")
	}
	if got := readWhole(t, v, e2.ID); !bytes.Equal(got, photo) {
		t.Fatal("the photo read back from the cache is not the photo")
	}
	// A whole-file read checks the hash recorded at upload, end to end.
	if data, _, err := v.Fetch(t.Context(), e1.ID); err != nil || !bytes.Equal(data, film) {
		t.Fatalf("Fetch with every cloud gone: %v", err)
	}
}

func TestCacheHoldsOnlySealedParts(t *testing.T) {
	v, _ := keptVault(t)
	secret := bytes.Repeat([]byte("the plaintext must never touch this disk. "), 100)
	putFile(t, v, "/docs", "secret.txt", secret)
	if _, err := v.KeepLocally(MainScope, "/docs/secret.txt", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("KeepLocally: %v", err)
	}
	fillAll(t, v)

	parts := 0
	err := filepath.Walk(localCacheDir(v.Path()), func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(blob, []byte("plaintext must never")) {
			t.Errorf("%s holds plaintext", path)
		}
		if _, err := sandfile.ReadPart(blob); err != nil {
			t.Errorf("%s is not a sealed SAND part: %v", path, err)
		}
		parts++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if parts == 0 {
		t.Fatal("nothing was cached")
	}

	sidecar, err := os.ReadFile(localPinsPath(v.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sidecar, []byte("secret.txt")) || bytes.Contains(sidecar, []byte("/docs")) {
		t.Fatal("the list of kept files is readable on disk")
	}
}

func TestOnlyKOfNPartsAreKept(t *testing.T) {
	v, _ := keptVault(t)
	e := putFile(t, v, "/", "a.bin", readerPayload(4000))
	_, _ = v.KeepLocally(MainScope, "/a.bin", time.Now().Add(time.Hour))
	fillAll(t, v)

	store, _ := v.localStore(false)
	counts := store.Counts(e.ArchiveID)
	if len(counts) != e.ChunkCount {
		t.Fatalf("%d chunks cached, want %d", len(counts), e.ChunkCount)
	}
	for chunk, n := range counts {
		if n != e.Scheme().Data {
			t.Errorf("chunk %d: %d parts kept, want %d", chunk, n, e.Scheme().Data)
		}
	}
}

func TestExpiredPinIsForgottenAndSwept(t *testing.T) {
	v, _ := keptVault(t)
	putFile(t, v, "/", "short.bin", readerPayload(3000))
	if _, err := v.KeepLocally(MainScope, "/short.bin", time.Now().Add(300*time.Millisecond)); err != nil {
		t.Fatalf("KeepLocally: %v", err)
	}
	fillAll(t, v)
	store, _ := v.localStore(false)
	if store.Usage().Used == 0 {
		t.Fatal("nothing cached before expiry")
	}

	time.Sleep(400 * time.Millisecond)
	step, err := v.FillLocalCache(t.Context(), 1<<20)
	if err != nil {
		t.Fatalf("FillLocalCache: %v", err)
	}
	if step.Swept == 0 || store.Usage().Used != 0 {
		t.Fatalf("after expiry: step %+v, %d bytes still cached", step, store.Usage().Used)
	}
	status, _ := v.LocalCache()
	if len(status.Pins) != 0 {
		t.Fatalf("expired pin still listed: %+v", status.Pins)
	}
}

func TestPastDateRefused(t *testing.T) {
	v, _ := keptVault(t)
	putFile(t, v, "/", "a.bin", []byte("a"))
	if _, err := v.KeepLocally(MainScope, "/a.bin", time.Now().Add(-time.Minute)); err == nil {
		t.Fatal("a date in the past was accepted")
	}
	if _, err := v.KeepLocally(MainScope, "/nowhere", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("a path that is not there was accepted")
	}
}

func TestStopKeepingDropsOnlyWhatNothingElseWants(t *testing.T) {
	v, _ := keptVault(t)
	a := putFile(t, v, "/films", "a.mkv", readerPayload(3000))
	b := putFile(t, v, "/films", "b.mkv", readerPayload(2000))
	folder, _ := v.KeepLocally(MainScope, "/films", time.Now().Add(time.Hour))
	if _, err := v.KeepLocally(MainScope, "/films/a.mkv", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	fillAll(t, v)

	if err := v.StopKeeping(folder.ID); err != nil {
		t.Fatalf("StopKeeping: %v", err)
	}
	store, _ := v.localStore(false)
	held := store.Archives()
	if held[a.ArchiveID] == 0 {
		t.Fatal("a.mkv, still pinned on its own, was dropped")
	}
	if held[b.ArchiveID] != 0 {
		t.Fatal("b.mkv, pinned by nothing now, is still cached")
	}
	if err := v.StopKeeping(folder.ID); !errors.Is(err, ErrNoSuchPin) {
		t.Fatalf("StopKeeping twice = %v, want ErrNoSuchPin", err)
	}
	// By path as well as by ID.
	if err := v.StopKeeping("/films/a.mkv"); err != nil {
		t.Fatalf("StopKeeping by path: %v", err)
	}
	if store.Usage().Used != 0 {
		t.Fatal("parts left over with nothing pinned")
	}
}

func TestPinningAgainMovesTheDate(t *testing.T) {
	v, _ := keptVault(t)
	putFile(t, v, "/", "a.bin", []byte("a"))
	first, _ := v.KeepLocally(MainScope, "/a.bin", time.Now().Add(time.Hour))
	later := time.Now().Add(48 * time.Hour)
	second, err := v.KeepLocally(MainScope, "/a.bin", later)
	if err != nil {
		t.Fatal(err)
	}
	status, _ := v.LocalCache()
	if len(status.Pins) != 1 || second.ID != first.ID || !status.Pins[0].Until.Equal(later) {
		t.Fatalf("pins after re-pinning: %+v", status.Pins)
	}
}

func TestFilePinFollowsARename(t *testing.T) {
	v, _ := keptVault(t)
	e := putFile(t, v, "/", "old.bin", readerPayload(2000))
	_, _ = v.KeepLocally(MainScope, "/old.bin", time.Now().Add(time.Hour))
	if _, err := v.Move(t.Context(), e.ID, "/", "new.bin"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	fillAll(t, v)
	if pin := pinStatus(t, v, "/old.bin"); !pin.Found || !pin.Complete {
		t.Fatalf("pin lost its file across a rename: %+v", pin)
	}
}

func TestLimitStopsTheFillWithoutEvicting(t *testing.T) {
	v, _ := keptVault(t)
	putFile(t, v, "/", "a.bin", readerPayload(4000))
	putFile(t, v, "/", "b.bin", readerPayload(4000))
	_, _ = v.KeepLocally(MainScope, "/a.bin", time.Now().Add(time.Hour))
	fillAll(t, v)
	a := pinStatus(t, v, "/a.bin")

	// Room for what is held and very little more.
	if err := v.SetLocalCacheLimits(a.Cached+100, -1); err != nil {
		t.Fatal(err)
	}
	_, _ = v.KeepLocally(MainScope, "/b.bin", time.Now().Add(time.Hour))
	last := fillAll(t, v)
	if !last.Full {
		t.Fatalf("the fill did not say it ran out of room: %+v", last)
	}
	if got := pinStatus(t, v, "/a.bin"); !got.Complete {
		t.Fatal("making room for b.bin evicted a.bin")
	}
	if got := pinStatus(t, v, "/b.bin"); got.Complete {
		t.Fatal("b.bin is complete in a cache too small for it")
	}
	status, _ := v.LocalCache()
	if !status.Full || status.Usage.Used > a.Cached+100 {
		t.Fatalf("status: %+v", status)
	}
}

func TestReadingAPinnedFileKeepsWhatItFetched(t *testing.T) {
	v, _ := keptVault(t)
	data := readerPayload(5000)
	e := putFile(t, v, "/", "film.mkv", data)
	_, _ = v.KeepLocally(MainScope, "/film.mkv", time.Now().Add(time.Hour))

	if got := readWhole(t, v, e.ID); !bytes.Equal(got, data) {
		t.Fatal("read back wrong")
	}
	if pin := pinStatus(t, v, "/film.mkv"); !pin.Complete {
		t.Fatalf("watching a pinned file did not keep it: %+v", pin)
	}
}

func TestReadingAnUnpinnedFileKeepsNothing(t *testing.T) {
	v, _ := keptVault(t)
	data := readerPayload(3000)
	e := putFile(t, v, "/", "a.bin", data)
	readWhole(t, v, e.ID)
	if store, _ := v.localStore(false); store != nil && store.Usage().Used != 0 {
		t.Fatal("an unpinned read was cached")
	}
}

func TestDamagedCachedPartIsReplacedFromTheClouds(t *testing.T) {
	v, _ := keptVault(t)
	data := readerPayload(3000)
	e := putFile(t, v, "/", "a.bin", data)
	_, _ = v.KeepLocally(MainScope, "/a.bin", time.Now().Add(time.Hour))
	fillAll(t, v)

	store, _ := v.localStore(false)
	part := store.Parts(e.ArchiveID, 0)[0]
	path := filepath.Join(localCacheDir(v.Path()), e.ArchiveID, cacheKeyOf(e.ArchiveID, 0, part).String()[len(e.ArchiveID)+1:])
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blob[len(blob)-1] ^= 0xff
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	if got := readWhole(t, v, e.ID); !bytes.Equal(got, data) {
		t.Fatal("a damaged cached part corrupted the read")
	}
	if pin := pinStatus(t, v, "/a.bin"); !pin.Complete {
		t.Fatal("the damaged part was not fetched again")
	}
	if store.Has(cacheKeyOf(e.ArchiveID, 0, part)) {
		if again, _ := os.ReadFile(path); bytes.Equal(again, blob) {
			t.Fatal("the damaged part is still the one held")
		}
	}
}

func TestPinsSurviveALockAndAPasswordChange(t *testing.T) {
	v, roots := keptVault(t)
	data := readerPayload(3000)
	e := putFile(t, v, "/", "a.bin", data)
	_, _ = v.KeepLocally(MainScope, "/a.bin", time.Now().Add(time.Hour))
	fillAll(t, v)

	v.Lock()
	if _, err := v.LocalCache(); !errors.Is(err, ErrLocked) {
		t.Fatalf("LocalCache while locked = %v", err)
	}
	if _, err := v.FillLocalCache(t.Context(), 1<<20); !errors.Is(err, ErrLocked) {
		t.Fatalf("FillLocalCache while locked = %v", err)
	}
	if err := v.Unlock(testPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := v.ChangePassword(t.Context(), testPassword, "a different password entirely", false); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if pin := pinStatus(t, v, "/a.bin"); !pin.Complete {
		t.Fatalf("pin after a lock and a password change: %+v", pin)
	}
	if step := fillAll(t, v); step.Swept != 0 {
		t.Fatalf("the cache was swept after a password change: %+v", step)
	}
	for _, root := range roots {
		darken(t, root)
	}
	if got := readWhole(t, v, e.ID); !bytes.Equal(got, data) {
		t.Fatal("read back wrong after a password change")
	}
}

func TestSubVaultFilesCannotBeKept(t *testing.T) {
	v, _ := keptVault(t)
	id := createSubVault(t, v, "Taxes", subPassword)
	if _, err := v.KeepLocally(Scope(id), "/", time.Now().Add(time.Hour)); !errors.Is(err, ErrCacheSubVault) {
		t.Fatalf("KeepLocally in a sub vault = %v", err)
	}
}

func TestDeletedFileIsSwept(t *testing.T) {
	v, _ := keptVault(t)
	e := putFile(t, v, "/films", "a.mkv", readerPayload(3000))
	_, _ = v.KeepLocally(MainScope, "/films", time.Now().Add(time.Hour))
	fillAll(t, v)
	if _, err := v.Delete(t.Context(), e.ID); err != nil {
		t.Fatal(err)
	}
	fillAll(t, v)
	if store, _ := v.localStore(false); store.Usage().Used != 0 {
		t.Fatal("a deleted file's parts are still cached")
	}
}

func TestStatusKnowsTheRoomBeforeTheFirstFill(t *testing.T) {
	v, _ := keptVault(t)
	putFile(t, v, "/", "a.bin", []byte("a"))
	_, _ = v.KeepLocally(MainScope, "/a.bin", time.Now().Add(time.Hour))
	status, err := v.LocalCache()
	if err != nil {
		t.Fatal(err)
	}
	if status.Usage.Room == 0 || status.Usage.DiskTotal == 0 {
		t.Fatalf("usage before the first fill: %+v", status.Usage)
	}
}

func TestParseKeepFor(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"12h": 12 * time.Hour, "7d": 7 * 24 * time.Hour, "2w": 14 * 24 * time.Hour,
		"1.5d": 36 * time.Hour, " 90m ": 90 * time.Minute,
	} {
		if got, err := ParseKeepFor(in); err != nil || got != want {
			t.Errorf("ParseKeepFor(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "soon", "-1d", "0h", "d"} {
		if _, err := ParseKeepFor(in); err == nil {
			t.Errorf("ParseKeepFor(%q) accepted", in)
		}
	}
}

// keptReplicaFile is one file in a vault whose replica is cut 2-of-3 across
// replica folders of its own, kept on this machine, with nothing fetched yet.
func keptReplicaFile(t *testing.T) (v *Vault, e *Entry, payload []byte, roots, replicaRoots []string) {
	t.Helper()
	v, roots, replicaRoots = replicaVault(t, 3, 3, archive.Scheme{Data: 2, Total: 3})
	if err := v.SetLocalCacheLimits(0, -1); err != nil {
		t.Fatal(err)
	}
	if err := v.SetReplicate("/", on(true)); err != nil {
		t.Fatal(err)
	}
	payload = readerPayload(5000)
	e = putFile(t, v, "/", "film.mkv", payload)
	replicate(t, v)
	if _, err := v.KeepLocally(MainScope, "/film.mkv", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return v, e, payload, roots, replicaRoots
}

func replicaArchive(t *testing.T, v *Vault, id string) string {
	t.Helper()
	r := replicaOf(t, v, id)
	if r == nil {
		t.Fatal("no replica")
	}
	return r.ArchiveID
}

func TestKeptFileFillsFromItsReplicaWhenThePartsCannot(t *testing.T) {
	v, e, payload, roots, replicaRoots := keptReplicaFile(t)
	emptyCloud(t, roots[0])
	emptyCloud(t, roots[1])

	fillAll(t, v)
	if pin := pinStatus(t, v, "/film.mkv"); !pin.Complete {
		t.Fatalf("not kept with the parts gone and the replica whole: %+v", pin)
	}
	store, _ := v.localStore(false)
	ra := replicaArchive(t, v, e.ID)
	for chunk := 0; chunk < e.ChunkCount; chunk++ {
		if n := store.Count(ra, chunk); n != 2 {
			t.Errorf("chunk %d: %d replica shards kept, want exactly 2", chunk, n)
		}
		if n := store.Count(e.ArchiveID, chunk); n != 0 {
			t.Errorf("chunk %d: %d parts kept beside a whole replica chunk", chunk, n)
		}
	}

	// Every cloud gone now, parts and replica alike: an ordinary read of the
	// file — from its parts, as every read is unless it says otherwise —
	// comes off this machine.
	for _, root := range append(append([]string{}, roots...), replicaRoots...) {
		darken(t, root)
	}
	if got := readWhole(t, v, e.ID); !bytes.Equal(got, payload) {
		t.Fatal("the file read back from its kept replica is not the file")
	}
	if data, _, err := v.Fetch(t.Context(), e.ID); err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("Fetch from the kept replica: %v", err)
	}
}

func TestPartsArePreferredAndOnlyKShardsKept(t *testing.T) {
	v, e, _, _, _ := keptReplicaFile(t)
	fillAll(t, v)
	store, _ := v.localStore(false)
	held := store.Archives()
	if held[replicaArchive(t, v, e.ID)] != 0 {
		t.Fatal("replica shards kept though the parts answered")
	}
	for chunk := 0; chunk < e.ChunkCount; chunk++ {
		if n := store.Count(e.ArchiveID, chunk); n != e.Scheme().Data {
			t.Errorf("chunk %d: %d parts kept, want exactly %d", chunk, n, e.Scheme().Data)
		}
	}
}

func TestAChunkIsNeverKeptFromBothPlacements(t *testing.T) {
	v, e, _, roots, _ := keptReplicaFile(t)
	fillAll(t, v)

	// Each chunk is left one part short, and the parts can no longer make it
	// up: the replica has to, and the stray part goes.
	store, _ := v.localStore(false)
	for chunk := 0; chunk < e.ChunkCount; chunk++ {
		_ = store.Remove(cacheKeyOf(e.ArchiveID, chunk, store.Parts(e.ArchiveID, chunk)[0]))
	}
	for _, root := range roots {
		emptyCloud(t, root)
	}
	fillAll(t, v)

	ra := replicaArchive(t, v, e.ID)
	for chunk := 0; chunk < e.ChunkCount; chunk++ {
		if p, r := store.Count(e.ArchiveID, chunk), store.Count(ra, chunk); p != 0 || r != 2 {
			t.Errorf("chunk %d: %d parts and %d replica shards kept, want 0 and 2", chunk, p, r)
		}
	}
}

func TestReadingFromTheReplicaKeepsWhatItFetched(t *testing.T) {
	v, e, payload, roots, replicaRoots := keptReplicaFile(t)
	emptyCloud(t, roots[0])
	emptyCloud(t, roots[1])

	v.chunks.clear()
	got, err := readFrom(t, v, e.ID, ReadEither)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("ReadEither with the parts gone: %v", err)
	}
	if pin := pinStatus(t, v, "/film.mkv"); !pin.Complete {
		t.Fatalf("the replica's shards the read fetched were not kept: %+v", pin)
	}
	for _, root := range replicaRoots {
		darken(t, root)
	}
	if got := readWhole(t, v, e.ID); !bytes.Equal(got, payload) {
		t.Fatal("read back wrong from what the replica read kept")
	}
}

func TestReplicaShardsAreSweptWithTheirFile(t *testing.T) {
	v, e, _, roots, _ := keptReplicaFile(t)
	emptyCloud(t, roots[0])
	emptyCloud(t, roots[1])
	fillAll(t, v)
	if err := v.StopKeeping("/film.mkv"); err != nil {
		t.Fatal(err)
	}
	store, _ := v.localStore(false)
	if store.Archives()[replicaArchive(t, v, e.ID)] != 0 || store.Usage().Used != 0 {
		t.Fatal("replica shards outlived their pin")
	}
}

func TestAWholeCopyReplicaKeepsOneShardAChunk(t *testing.T) {
	v, roots, replicaRoots := replicaVault(t, 3, 1, archive.Scheme{})
	_ = v.SetLocalCacheLimits(0, -1)
	_ = v.SetReplicate("/", on(true))
	payload := readerPayload(4000)
	e := putFile(t, v, "/", "a.bin", payload)
	replicate(t, v)
	_, _ = v.KeepLocally(MainScope, "/a.bin", time.Now().Add(time.Hour))
	for _, root := range roots {
		emptyCloud(t, root)
	}
	fillAll(t, v)

	store, _ := v.localStore(false)
	ra := replicaArchive(t, v, e.ID)
	for chunk := 0; chunk < e.ChunkCount; chunk++ {
		if n := store.Count(ra, chunk); n != 1 {
			t.Errorf("chunk %d: %d shards of a whole-copy replica kept, want 1", chunk, n)
		}
	}
	darken(t, replicaRoots[0])
	if got := readWhole(t, v, e.ID); !bytes.Equal(got, payload) {
		t.Fatal("read back wrong from a kept whole-copy replica")
	}
}
