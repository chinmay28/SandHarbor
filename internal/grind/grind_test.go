package grind

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chinmay28/SandHarbor/internal/archive"
	"github.com/chinmay28/SandHarbor/internal/crypto"
	"github.com/google/uuid"
)

func testMaster() []byte { return bytes.Repeat([]byte{0x42}, crypto.MasterKeySize) }

// plenty is a machine with room to spare.
func plenty() Memory { return Memory{Available: 8 << 30} }

// pair starts a grinder and opens a session onto it, the two joined by pipes
// the way an SSH command's streams join them. The returned channel is what
// Serve came to.
func pair(t *testing.T, g *Grinder) (*Session, func(), <-chan error) {
	t.Helper()
	toGrinder, fromVault := io.Pipe()
	fromGrinder, toVault := io.Pipe()
	g.In, g.Out = toGrinder, toVault
	if g.Memory == nil {
		g.Memory = plenty
	}
	if g.CPUs == 0 {
		g.CPUs = 4
	}

	done := make(chan error, 1)
	go func() {
		err := g.Serve(context.Background())
		toVault.CloseWithError(io.EOF)
		done <- err
	}()

	hangUp := func() {
		fromVault.Close()
		fromGrinder.Close()
	}
	t.Cleanup(hangUp)

	s, err := Open(fromGrinder, fromVault)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, hangUp, done
}

func newJob(root, rel string, chunk uint32) Job {
	id := uuid.New()
	return Job{
		Root: root, Path: rel, ArchiveID: hex.EncodeToString(id[:]),
		Filename: filepath.Base(rel), ChunkSize: chunk, Data: 2, Total: 3, Level: 1,
	}
}

func writeRandom(t *testing.T, path string, n int) []byte {
	t.Helper()
	data := make([]byte, n)
	rand.Read(data)
	// Half of it compressible, so both kinds of chunk are cut.
	copy(data[n/2:], bytes.Repeat([]byte("sand"), n/8))
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return data
}

// grindAll runs one file through and hands back its chunks.
func grindAll(t *testing.T, s *Session, job Job) (*File, []*archive.EncodedChunk) {
	t.Helper()
	file, err := s.Begin(job, nil)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := file.SendKeys(testMaster()); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	var chunks []*archive.EncodedChunk
	for file.Remaining() {
		chunk, err := file.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		chunks = append(chunks, chunk)
	}
	if err := file.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return file, chunks
}

// What a grinder makes, the vault can read with nothing but its own key —
// from any two of three parts, as it could a file it cut itself.
func TestGroundPartsRebuildTheFile(t *testing.T) {
	root := t.TempDir()
	data := writeRandom(t, filepath.Join(root, "films", "a.mkv"), 10_000)

	s, hangUp, done := pair(t, &Grinder{Version: "test"})
	if h := s.Hello(); h.Protocol != Protocol || h.Version != "test" || !h.Memory.Known() {
		t.Fatalf("hello = %+v", h)
	}

	file, chunks := grindAll(t, s, newJob(root, "films/a.mkv", 1024))
	defer file.Close()
	if file.Size != int64(len(data)) || file.Hash != sha256.Sum256(data) {
		t.Fatalf("the grinder read %d bytes hashing %x", file.Size, file.Hash)
	}
	if len(chunks) != 10 {
		t.Fatalf("%d chunks, want 10", len(chunks))
	}

	var rebuilt []byte
	for _, chunk := range chunks {
		decoded, err := archive.DecodeChunk([][]byte{chunk.Parts[2], chunk.Parts[0]}, testMaster())
		if err != nil {
			t.Fatalf("DecodeChunk(%d): %v", chunk.Index, err)
		}
		rebuilt = append(rebuilt, decoded.Data...)
	}
	if !bytes.Equal(rebuilt, data) {
		t.Fatal("the parts do not rebuild the file")
	}
	if !bytes.Equal(file.Head, data[:headSize]) {
		t.Fatal("the head is not the file's first bytes")
	}

	// A second file over the same session.
	writeRandom(t, filepath.Join(root, "b.bin"), 3000)
	grindAll(t, s, newJob(root, "b.bin", 1024))

	hangUp()
	if err := <-done; err != nil && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve after the vault hung up: %v", err)
	}
}

func TestAnEmptyFileIsOneChunk(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "empty"), nil, 0o644)
	s, _, _ := pair(t, &Grinder{})
	_, chunks := grindAll(t, s, newJob(root, "empty", 1024))
	if len(chunks) != 1 {
		t.Fatalf("%d chunks for an empty file", len(chunks))
	}
}

// A file the grinder cannot do is refused, with the reason, and the session
// goes on to the next.
func TestRefusalsLeaveTheSessionUsable(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(filepath.Dir(root), "outside.txt"), []byte("x"), 0o644)
	writeRandom(t, filepath.Join(root, "ok.bin"), 2000)

	s, _, _ := pair(t, &Grinder{})
	for _, rel := range []string{"missing.bin", "../outside.txt"} {
		_, err := s.Begin(newJob(root, rel, 1024), nil)
		var refused *Refused
		if !errors.As(err, &refused) {
			t.Fatalf("%s: err = %v, want a refusal", rel, err)
		}
	}
	grindAll(t, s, newJob(root, "ok.bin", 1024))
}

func TestAMachineShortOfMemoryIsRefusedUpFront(t *testing.T) {
	root := t.TempDir()
	writeRandom(t, filepath.Join(root, "a.bin"), 2000)
	s, _, _ := pair(t, &Grinder{Memory: func() Memory { return Memory{Available: 64 << 20} }})

	_, err := s.Begin(newJob(root, "a.bin", 16<<20), nil)
	var refused *Refused
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "free") {
		t.Fatalf("err = %v, want a refusal about memory", err)
	}
}

// The machine running short partway through holds the next chunk back, and a
// shortage that does not pass refuses the file rather than pushing on.
func TestAShortageMidFileRefusesTheFile(t *testing.T) {
	root := t.TempDir()
	writeRandom(t, filepath.Join(root, "a.bin"), 8000)

	var reads atomic.Int32
	g := &Grinder{
		Memory: func() Memory {
			// Room when the file is planned, none after the second chunk.
			if reads.Add(1) > 4 {
				return Memory{Available: 1}
			}
			return plenty()
		},
		Patience: 30 * time.Millisecond, Poll: time.Millisecond,
	}
	s, _, _ := pair(t, g)
	file, err := s.Begin(newJob(root, "a.bin", 1024), nil)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	file.SendKeys(testMaster())
	for {
		_, err := file.Next()
		var refused *Refused
		if errors.As(err, &refused) {
			if !strings.Contains(err.Error(), "short of memory") {
				t.Fatalf("refused for %q", err)
			}
			return
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !file.Remaining() {
			t.Fatal("every chunk arrived on a machine with no memory")
		}
	}
}

// A file rewritten between the checksum pass and the cut is refused at the
// end, so the vault throws the parts away rather than storing a file whose
// checksum is not its own.
func TestAFileThatChangesWhileReadIsRefused(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.bin")
	data := writeRandom(t, path, 4000)

	var reads atomic.Int32
	g := &Grinder{Memory: func() Memory {
		// The third read is the gate before the second chunk: by then the
		// checksum has been taken.
		if reads.Add(1) == 3 {
			changed := append([]byte{}, data...)
			changed[3500] ^= 0xFF
			os.WriteFile(path, changed, 0o644)
		}
		return plenty()
	}}
	s, _, _ := pair(t, g)
	file, err := s.Begin(newJob(root, "a.bin", 1024), nil)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	file.SendKeys(testMaster())
	for file.Remaining() {
		if _, err := file.Next(); err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	err = file.Finish()
	var refused *Refused
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("Finish = %v, want a refusal about the change", err)
	}
}

func TestBeginReportsTheChecksumPass(t *testing.T) {
	root := t.TempDir()
	writeRandom(t, filepath.Join(root, "big.bin"), 3*progressEvery)
	s, _, _ := pair(t, &Grinder{})

	var last int64
	file, err := s.Begin(newJob(root, "big.bin", 4<<20), func(done int64) { last = done })
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer file.Close()
	if last < 2*progressEvery {
		t.Fatalf("last report %d, want the pass reported as it went", last)
	}
}

// A vault that hangs up mid-file stops the grinder: it does not carry on
// cutting a file for nobody.
func TestTheGrinderStopsWhenTheVaultHangsUp(t *testing.T) {
	root := t.TempDir()
	writeRandom(t, filepath.Join(root, "a.bin"), 50_000)
	s, hangUp, done := pair(t, &Grinder{})

	file, err := s.Begin(newJob(root, "a.bin", 1024), nil)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	file.SendKeys(testMaster())
	if _, err := file.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	hangUp()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the grinder was still running after the vault hung up")
	}
}

// Something that is not a grinder, and a grinder of another protocol, are
// both turned away before any file is handed over.
func TestOpenTurnsAwayWhatIsNotThisGrinder(t *testing.T) {
	if _, err := Open(strings.NewReader("Last login: Tue\n$ "), io.Discard); err == nil {
		t.Fatal("a shell prompt was taken for a grinder")
	}

	var buf bytes.Buffer
	buf.Write(preamble)
	writeJSON(&buf, frameHello, Hello{Protocol: Protocol + 1, Format: 4, Version: "2099.1.1"})
	_, err := Open(&buf, io.Discard)
	if !errors.Is(err, ErrIncompatible) || !strings.Contains(err.Error(), "2099.1.1") {
		t.Fatalf("err = %v, want an incompatibility naming the build", err)
	}
}

// fileFor builds the vault's side of a file without a grinder, for the checks
// on what comes back.
func fileFor(t *testing.T, data []byte) (*File, archive.ChunkPlan) {
	t.Helper()
	job := newJob("/", "a.bin", 1024)
	scheme, _ := job.scheme()
	id, _ := job.archiveID()
	sum := sha256.Sum256(data)
	f, err := newFile(&Session{w: io.Discard}, job, scheme, id, Planned{Size: int64(len(data)), Hash: hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.SendKeys(testMaster()); err != nil {
		t.Fatal(err)
	}
	return f, f.Plan()
}

// The checks that stand between a grinder and the clouds: a part relabelled,
// a part from another chunk, a part from another file, and a flipped bit are
// each caught.
func TestPartsThatAreNotWhatWasAskedForAreCaught(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 2048)
	f, plan := fileFor(t, data)

	good, err := archive.EncodeChunk(plan, 0, data[:1024], testMaster())
	if err != nil {
		t.Fatal(err)
	}
	for i, part := range good.Parts {
		if err := f.check(part, 0, i+1, good.CompressedSize); err != nil {
			t.Fatalf("a good part %d: %v", i+1, err)
		}
	}

	if err := f.check(good.Parts[1], 0, 1, good.CompressedSize); err == nil {
		t.Error("part 2 passed as part 1")
	}
	if err := f.check(good.Parts[0], 1, 1, good.CompressedSize); err == nil {
		t.Error("chunk 0 passed as chunk 1")
	}
	if err := f.check(good.Parts[0], 0, 1, good.CompressedSize+1); err == nil {
		t.Error("a part disagreeing with its frame's compressed size passed")
	}

	flipped := append([]byte{}, good.Parts[0]...)
	flipped[len(flipped)-1] ^= 1
	if err := f.check(flipped, 0, 1, good.CompressedSize); err == nil {
		t.Error("a flipped bit passed")
	}

	other, _ := fileFor(t, data)
	if err := other.check(good.Parts[0], 0, 1, good.CompressedSize); err == nil {
		t.Error("another file's part passed")
	}
}

func TestCloseForgetsTheKeys(t *testing.T) {
	f, _ := fileFor(t, []byte("abc"))
	key := f.keys[0]
	f.Close()
	if !bytes.Equal(key, make([]byte, 32)) {
		t.Fatal("a key survived Close")
	}
	f.Close()
}
