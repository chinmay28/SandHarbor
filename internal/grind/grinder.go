package grind

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/chinmay28/SandHarbor/internal/archive"
	"github.com/chinmay28/SandHarbor/internal/crypto"
	"github.com/chinmay28/SandHarbor/internal/sandfile"
)

// Grinder is the far end of the protocol: it reads Jobs on In and answers on
// Out until In ends. `sand grind` is a Grinder on the process's standard
// streams.
type Grinder struct {
	In  io.Reader
	Out io.Writer

	// Version is this build of sand, for the Hello.
	Version string

	// CPUs and Memory say what the machine has. Zero and nil read the
	// machine itself.
	CPUs   int
	Memory func() Memory

	// Patience is how long a chunk waits for memory before the file is
	// refused, and Poll how often it looks. Zero is two minutes and a second.
	Patience time.Duration
	Poll     time.Duration

	// OnBudget, when set, is told each file's budget before its work starts.
	// `sand grind` sets the runtime's memory limit from it.
	OnBudget func(Budget)

	// OnJob, when set, is told each Job before its work starts. `sand grind`
	// sets the compressor's level from it — a process-wide setting, which is
	// why it is the command's to make and not this type's: a Grinder running
	// inside a vault's own tests must not retune the vault's compressor.
	OnJob func(Job)
}

// keysFrame bounds a Keys frame: a million chunks, which at the smallest
// chunk the vault cuts is a file of several terabytes.
const keysFrame = 32 << 20

// progressEvery is how much of the checksum pass is read between reports.
const progressEvery = 8 << 20

// defaultPatience is how long a chunk waits for the machine's memory.
const defaultPatience = 2 * time.Minute

// Serve runs the protocol until the vault closes its end, which is a clean
// finish. It returns early, with the reason, when the conversation goes wrong
// or the output can no longer be written — which is what a vault that has
// gone away looks like from here.
func (g *Grinder) Serve(ctx context.Context) error {
	if g.CPUs <= 0 {
		g.CPUs = runtime.NumCPU()
	}
	if g.Memory == nil {
		g.Memory = ReadMemory
	}
	if g.Patience <= 0 {
		g.Patience = defaultPatience
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if _, err := g.Out.Write(preamble); err != nil {
		return err
	}
	hello := Hello{
		Protocol: Protocol, Format: sandfile.ErasureFormatVersion, Version: g.Version,
		OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: g.CPUs, Memory: g.Memory(),
	}
	if err := writeJSON(g.Out, frameHello, hello); err != nil {
		return err
	}

	// Input is read on a goroutine of its own, so that the end of it is
	// noticed while a file is being worked on rather than only between files:
	// the vault closing the command is how it says stop, and a grinder that
	// carried on cutting a 40 GB film for nobody would be the one thing here
	// that could hurt the machine.
	in := newInbox(ctx, cancel, g.In)

	for {
		f, err := in.next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var job Job
		if err := f.decode(frameJob, &job); err != nil {
			return err
		}

		err = g.grind(ctx, job, in)
		var refused *Refused
		if errors.As(err, &refused) {
			if err := writeJSON(g.Out, frameRefused, refused); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
	}
}

// refuse is a file the grinder will not finish, and the session carries on.
func refuse(format string, args ...any) error {
	return &Refused{Reason: fmt.Sprintf(format, args...)}
}

// grind does one Job. A *Refused error abandons the file; any other ends the
// session.
func (g *Grinder) grind(ctx context.Context, job Job, in *inbox) error {
	scheme, err := job.scheme()
	if err != nil {
		return refuse("%v", err)
	}
	archiveID, err := job.archiveID()
	if err != nil {
		return refuse("%v", err)
	}
	if job.ChunkSize == 0 || job.Filename == "" {
		return refuse("the job names no chunk size or no file name")
	}

	file, _, err := openUnder(job.Root, job.Path)
	if err != nil {
		return refuse("%v", err)
	}
	defer file.Close()

	budget, err := Plan(g.Memory(), g.CPUs, job.ChunkSize, scheme.Data, scheme.Total)
	if err != nil {
		return refuse("%v", err)
	}
	if g.OnBudget != nil {
		g.OnBudget(budget)
	}
	if g.OnJob != nil {
		g.OnJob(job)
	}

	// The first pass: the checksum every part carries, and the file's size as
	// it was read rather than as it was listed.
	size, sum, head, err := g.checksum(ctx, file)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return refuse("reading %s: %v", job.Path, err)
	}
	plan, err := archive.PlanChunks(archiveID, job.Filename, sum, uint64(size), job.ChunkSize, scheme)
	if err != nil {
		return refuse("%v", err)
	}
	planned := Planned{Size: size, Hash: hex.EncodeToString(sum[:]), Head: head, Budget: budget}
	if err := writeJSON(g.Out, framePlanned, planned); err != nil {
		return err
	}

	keys, err := in.keys(ctx, int(plan.ChunkCount))
	if err != nil {
		return err
	}
	defer func() {
		for _, k := range keys {
			crypto.ZeroBytes(k)
		}
	}()

	// The second pass: cut, seal and send, with the file hashed again on the
	// way so a file that changed between the two passes is caught rather
	// than stored with a checksum that is not its own.
	again := sha256.New()
	gate := Gate{Read: g.Memory, Floor: budget.Floor, Patience: g.Patience, Poll: g.Poll}
	if err := g.encode(ctx, file, plan, keys, budget.Window, gate, again); err != nil {
		return err
	}
	var second [32]byte
	copy(second[:], again.Sum(nil))
	if second != sum {
		return refuse("%s changed while it was being read; run the import again once it is still", job.Path)
	}
	return writeFrame(g.Out, frameDone)
}

// checksum reads the file through once, reporting as it goes, and answers its
// length, SHA-256 and first headSize bytes.
func (g *Grinder) checksum(ctx context.Context, file *os.File) (int64, [32]byte, []byte, error) {
	var sum [32]byte
	digest := sha256.New()
	head := make([]byte, 0, headSize)
	buf := make([]byte, 1<<20)

	var done, since int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, sum, nil, err
		}
		n, err := file.Read(buf)
		if n > 0 {
			digest.Write(buf[:n])
			if room := headSize - len(head); room > 0 {
				head = append(head, buf[:min(n, room)]...)
			}
			done += int64(n)
			since += int64(n)
			if since >= progressEvery {
				since = 0
				if err := g.sayRead(done); err != nil {
					return 0, sum, nil, err
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, sum, nil, err
		}
	}
	copy(sum[:], digest.Sum(nil))
	return done, sum, head, nil
}

func (g *Grinder) sayRead(done int64) error {
	return writeFrame(g.Out, frameRead, binary.BigEndian.AppendUint64(nil, uint64(done)))
}

// sealed is one chunk's work, finished or failed.
type sealed struct {
	chunk *archive.EncodedChunk
	err   error
}

// encode cuts the file into the plan's chunks and sends them in order, with
// at most window of them read and being sealed at once.
//
// The order is the protocol's, not a convenience: the vault reads chunk i
// after chunk i−1, and the window is what lets chunk i+1 be sealing while
// chunk i is on the wire.
func (g *Grinder) encode(ctx context.Context, file *os.File, plan archive.ChunkPlan,
	keys [][]byte, window int, gate Gate, again hash.Hash) error {

	var pending []chan sealed

	// Every chunk started is waited for, however this ends, so nothing is
	// still sealing — and holding a chunk's worth of memory — once it has.
	defer func() {
		for _, ch := range pending {
			<-ch
		}
	}()

	send := func() error {
		result := <-pending[0]
		pending = pending[1:]
		if result.err != nil {
			return refuse("%v", result.err)
		}
		return writeChunk(g.Out, result.chunk.Index, result.chunk.CompressedSize, result.chunk.Parts)
	}

	for index := uint32(0); index < plan.ChunkCount; index++ {
		if len(pending) >= window {
			if err := send(); err != nil {
				return err
			}
		}
		if err := gate.Wait(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return refuse("%v", err)
		}

		want, err := plan.PlaintextSize(index)
		if err != nil {
			return refuse("%v", err)
		}
		plaintext := make([]byte, want)
		offset := int64(index) * int64(plan.ChunkSize)
		if _, err := file.ReadAt(plaintext, offset); err != nil {
			if errors.Is(err, io.EOF) {
				return refuse("%s got shorter while it was being read; run the import again once it is still", file.Name())
			}
			return refuse("reading chunk %d: %v", index, err)
		}
		again.Write(plaintext)

		ch := make(chan sealed, 1)
		pending = append(pending, ch)
		go func(index uint32, plaintext, key []byte) {
			chunk, err := archive.EncodeChunkKeyed(plan, index, plaintext, key)
			ch <- sealed{chunk: chunk, err: err}
		}(index, plaintext, keys[index])
	}
	for len(pending) > 0 {
		if err := send(); err != nil {
			return err
		}
	}

	// Anything past the planned length is the file having grown: the
	// checksum pass saw it shorter, so these parts would not be all of it.
	var probe [1]byte
	if n, _ := file.ReadAt(probe[:], int64(plan.OriginalSize)); n > 0 {
		return refuse("%s grew while it was being read; run the import again once it is still", file.Name())
	}
	return nil
}

// inbox is the grinder's input, read ahead on a goroutine of its own.
type inbox struct {
	frames chan frame
	err    error // set before frames is closed
}

func newInbox(ctx context.Context, cancel context.CancelFunc, r io.Reader) *inbox {
	in := &inbox{frames: make(chan frame, 1)}
	go func() {
		defer close(in.frames)
		for {
			f, err := readFrame(r, keysFrame)
			if err != nil {
				in.err = err
				// The end of the input stops whatever is in hand.
				cancel()
				return
			}
			select {
			case in.frames <- f:
			case <-ctx.Done():
				in.err = ctx.Err()
				return
			}
		}
	}()
	return in
}

// next answers the next frame, io.EOF once the vault has closed its end
// cleanly, or whatever else ended the input.
func (in *inbox) next(ctx context.Context) (frame, error) {
	select {
	case f, ok := <-in.frames:
		if !ok {
			return frame{}, in.err
		}
		return f, nil
	case <-ctx.Done():
		// The context was cancelled by the reader itself when the input
		// ended; say why rather than just that it was.
		select {
		case f, ok := <-in.frames:
			if ok {
				return f, nil
			}
			return frame{}, in.err
		default:
		}
		return frame{}, ctx.Err()
	}
}

// keys reads the Keys frame for a file of count chunks.
func (in *inbox) keys(ctx context.Context, count int) ([][]byte, error) {
	f, err := in.next(ctx)
	if err != nil {
		return nil, unexpected(err)
	}
	if f.t != frameKeys {
		return nil, fmt.Errorf("expected a %s frame, got %s", frameKeys, f.t)
	}
	if len(f.payload) != count*32 {
		return nil, fmt.Errorf("%d bytes of keys for a file of %d chunks", len(f.payload), count)
	}
	keys := make([][]byte, count)
	for i := range keys {
		keys[i] = f.payload[i*32 : (i+1)*32 : (i+1)*32]
	}
	return keys, nil
}
