package grind

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/chinmay28/SandHarbor/internal/archive"
	"github.com/chinmay28/SandHarbor/internal/crypto"
	"github.com/chinmay28/SandHarbor/internal/sandfile"
)

// ErrIncompatible is a grinder that speaks another protocol or writes another
// part format than this vault: it answered, and it is the wrong sand.
var ErrIncompatible = errors.New("the sand on the machine does not match this one")

// Session is the vault's end of a conversation with one grinder. It is used
// from one goroutine, one file at a time.
type Session struct {
	r     io.Reader
	w     io.Writer
	hello Hello
}

// Open reads a grinder's introduction off r and checks it is one this vault
// can work with. w is the grinder's input.
func Open(r io.Reader, w io.Writer) (*Session, error) {
	if err := readPreamble(r); err != nil {
		return nil, err
	}
	f, err := readFrame(r, smallFrame)
	if err != nil {
		return nil, fmt.Errorf("the grinder's introduction: %w", unexpected(err))
	}
	var hello Hello
	if err := f.decode(frameHello, &hello); err != nil {
		return nil, err
	}
	if hello.Protocol != Protocol || hello.Format != sandfile.ErasureFormatVersion {
		return nil, fmt.Errorf("%w: it is %s, speaking protocol %d and writing format %d, "+
			"and this one speaks %d and writes %d — install the same release on both",
			ErrIncompatible, orUnknown(hello.Version), hello.Protocol, hello.Format,
			Protocol, sandfile.ErasureFormatVersion)
	}
	return &Session{r: r, w: w, hello: hello}, nil
}

func orUnknown(v string) string {
	if v == "" {
		return "an unknown build"
	}
	return v
}

// Hello is what the grinder said about itself and the machine.
func (s *Session) Hello() Hello { return s.hello }

// Begin hands the grinder one file and waits for it to have read the file
// through. onRead, when set, is told how far that reading has got.
//
// A *Refused error is the grinder declining the file; the session can be
// given the next one. Any other error means the session is finished with.
func (s *Session) Begin(job Job, onRead func(done int64)) (*File, error) {
	scheme, err := job.scheme()
	if err != nil {
		return nil, err
	}
	archiveID, err := job.archiveID()
	if err != nil {
		return nil, err
	}
	if err := writeJSON(s.w, frameJob, job); err != nil {
		return nil, err
	}

	for {
		f, err := readFrame(s.r, smallFrame)
		if err != nil {
			return nil, unexpected(err)
		}
		switch f.t {
		case frameRead:
			if len(f.payload) != 8 {
				return nil, errors.New("a progress frame that is not a count")
			}
			if onRead != nil {
				onRead(int64(binary.BigEndian.Uint64(f.payload)))
			}
		case frameRefused:
			return nil, decodeRefused(f)
		case framePlanned:
			var planned Planned
			if err := f.decode(framePlanned, &planned); err != nil {
				return nil, err
			}
			return newFile(s, job, scheme, archiveID, planned)
		default:
			return nil, fmt.Errorf("expected the file's plan, got a %s frame", f.t)
		}
	}
}

func decodeRefused(f frame) error {
	var refused Refused
	if err := f.decode(frameRefused, &refused); err != nil {
		return err
	}
	if refused.Reason == "" {
		refused.Reason = "the machine declined the file without saying why"
	}
	return &refused
}

// File is one file being ground: what the grinder found when it read it, and
// the chunks as they arrive.
type File struct {
	// Size, Hash and Head are the file as the grinder read it.
	Size   int64
	Hash   [32]byte
	Head   []byte
	Budget Budget

	s      *Session
	plan   archive.ChunkPlan
	keys   [][]byte
	next   uint32
	limit  int
	closed bool
}

func newFile(s *Session, job Job, scheme archive.Scheme, archiveID [16]byte, planned Planned) (*File, error) {
	raw, err := hex.DecodeString(planned.Hash)
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("the grinder named %q as the file's checksum", planned.Hash)
	}
	if planned.Size < 0 || len(planned.Head) > headSize {
		return nil, errors.New("the grinder's plan does not hold together")
	}
	f := &File{Size: planned.Size, Head: planned.Head, Budget: planned.Budget, s: s}
	copy(f.Hash[:], raw)

	f.plan, err = archive.PlanChunks(archiveID, job.Filename, f.Hash, uint64(f.Size), job.ChunkSize, scheme)
	if err != nil {
		return nil, err
	}
	f.limit = chunkFrameLimit(job.ChunkSize, scheme)
	return f, nil
}

// chunkFrameLimit is the most a chunk frame of this code can honestly hold:
// every part of a chunk that compressed to slightly more than it started as,
// which incompressible data does, with room for each part's header and seal.
func chunkFrameLimit(chunkSize uint32, scheme archive.Scheme) int {
	worst := int(chunkSize) + int(chunkSize)/64 + 1<<16
	part := scheme.ShardSize(worst) + 4<<10
	return chunkHeaderSize + scheme.Total*(4+part)
}

// Plan is how the file is cut: what every part of it must say it belongs to.
func (f *File) Plan() archive.ChunkPlan { return f.plan }

// SendKeys derives this file's chunk keys from the vault's data key and hands
// them to the grinder. The data key itself goes nowhere; the derived keys are
// kept, to check the parts that come back, until Close.
func (f *File) SendKeys(master []byte) error {
	if f.keys != nil {
		return errors.New("the keys for this file have been sent already")
	}
	payload := make([]byte, 0, int(f.plan.ChunkCount)*32)
	defer crypto.ZeroBytes(payload[:cap(payload)])

	f.keys = make([][]byte, f.plan.ChunkCount)
	for i := range f.keys {
		key, err := crypto.DeriveChunkKey(master, f.plan.ArchiveID, uint32(i))
		if err != nil {
			return err
		}
		f.keys[i] = key
		payload = append(payload, key...)
	}
	return writeFrame(f.s.w, frameKeys, payload)
}

// Remaining reports whether chunks are still to come.
func (f *File) Remaining() bool { return f.next < f.plan.ChunkCount }

// Next reads the next chunk and checks every part of it before handing it
// over: it must open under the key for its chunk, and say it is this file's
// chunk, this shard of it, cut with this code. A grinder cannot get a part
// past this that the vault would not have made itself.
//
// A *Refused error is the grinder abandoning the file partway; anything else
// is the session gone wrong.
func (f *File) Next() (*archive.EncodedChunk, error) {
	if f.keys == nil {
		return nil, errors.New("no keys sent for this file")
	}
	if !f.Remaining() {
		return nil, errors.New("every chunk of this file has arrived")
	}
	fr, err := readFrame(f.s.r, f.limit)
	if err != nil {
		return nil, unexpected(err)
	}
	switch fr.t {
	case frameRefused:
		return nil, decodeRefused(fr)
	case frameChunk:
	default:
		return nil, fmt.Errorf("expected chunk %d, got a %s frame", f.next, fr.t)
	}

	parsed, err := parseChunk(fr.payload)
	if err != nil {
		return nil, err
	}
	if parsed.index != f.next {
		return nil, fmt.Errorf("expected chunk %d, got chunk %d", f.next, parsed.index)
	}
	scheme := f.plan.Scheme
	if len(parsed.parts) != scheme.Total {
		return nil, fmt.Errorf("chunk %d came with %d parts, and %s has %d", parsed.index, len(parsed.parts), scheme, scheme.Total)
	}
	for i, part := range parsed.parts {
		if err := f.check(part, parsed.index, i+1, parsed.compressed); err != nil {
			return nil, fmt.Errorf("chunk %d, part %d, as made on the machine: %w", parsed.index, i+1, err)
		}
	}

	f.next++
	return &archive.EncodedChunk{
		Index: parsed.index, CompressedSize: parsed.compressed,
		Parts: parsed.parts, Scheme: scheme,
	}, nil
}

// check opens one part and holds what it says against what was asked for.
func (f *File) check(blob []byte, index uint32, number int, compressed uint64) error {
	part, err := sandfile.ReadPart(blob)
	if err != nil {
		return err
	}
	h, plan := part.Header, f.plan
	switch {
	case h.Version != sandfile.ErasureFormatVersion:
		return fmt.Errorf("format version %d", h.Version)
	case h.ArchiveID != plan.ArchiveID:
		return errors.New("it belongs to another file")
	case h.ChunkIndex != index:
		return fmt.Errorf("it says it is chunk %d", h.ChunkIndex)
	case int(h.PartNumber) != number:
		return fmt.Errorf("it says it is part %d", h.PartNumber)
	case int(h.DataShards) != plan.Scheme.Data || int(h.TotalShards) != plan.Scheme.Total:
		return fmt.Errorf("it is cut %d-of-%d", h.DataShards, h.TotalShards)
	}

	meta, data, err := part.Open(f.keys[index])
	if err != nil {
		return fmt.Errorf("its seal does not open: %w", err)
	}
	switch {
	case meta.Filename != plan.Filename,
		meta.OriginalHash != plan.OriginalHash,
		meta.OriginalSize != plan.OriginalSize,
		meta.ChunkSize != plan.ChunkSize,
		meta.ChunkCount != plan.ChunkCount:
		return errors.New("it describes a different file than the one asked for")
	case meta.CompressedSize != compressed:
		return fmt.Errorf("it says the chunk compressed to %d bytes, and the frame says %d", meta.CompressedSize, compressed)
	case len(data) != shardLen(plan.Scheme, compressed):
		return fmt.Errorf("it holds %d bytes of shard where %d were due", len(data), shardLen(plan.Scheme, compressed))
	}
	return nil
}

// shardLen is the length splitter.Encode gives every shard of a chunk: the
// compressed bytes over k, rounded up, and never less than one — an empty
// chunk still has shards.
func shardLen(scheme archive.Scheme, compressed uint64) int {
	return max(scheme.ShardSize(int(compressed)), 1)
}

// Finish reads the grinder's word that the file is done, after the last
// chunk. A *Refused here is the file having changed under the grinder — the
// parts that came are not a true copy of anything, and must be thrown away.
func (f *File) Finish() error {
	if f.Remaining() {
		return fmt.Errorf("chunks %d to %d have not arrived", f.next, f.plan.ChunkCount-1)
	}
	fr, err := readFrame(f.s.r, smallFrame)
	if err != nil {
		return unexpected(err)
	}
	switch fr.t {
	case frameDone:
		return nil
	case frameRefused:
		return decodeRefused(fr)
	default:
		return fmt.Errorf("expected the end of the file, got a %s frame", fr.t)
	}
}

// Close forgets the file's keys. Safe to call more than once.
func (f *File) Close() {
	if f.closed {
		return
	}
	f.closed = true
	for _, k := range f.keys {
		crypto.ZeroBytes(k)
	}
	f.keys = nil
}
