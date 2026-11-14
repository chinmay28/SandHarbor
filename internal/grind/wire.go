package grind

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode"
)

// Protocol is the version of the conversation in this file. A vault and a
// grinder that disagree about it do not talk: there is one of each per
// release, and an old sand on the far end is put right by upgrading it.
const Protocol = 1

// preamble opens the grinder's output, before any frame. It is what tells a
// grinder from whatever else answered the command: a login shell that prints
// a banner, a NAS that runs its own menu, a `sand` too old to know `grind`.
var preamble = []byte("\x00SAND-GRIND\n")

// frameType says what a frame's payload is.
type frameType byte

const (
	frameHello   frameType = 'H' // grinder: Hello, as JSON
	frameJob     frameType = 'J' // vault: Job, as JSON
	frameRead    frameType = 'R' // grinder: bytes read so far by the checksum pass
	framePlanned frameType = 'P' // grinder: Planned, as JSON
	frameKeys    frameType = 'K' // vault: one 32-byte key per chunk, back to back
	frameChunk   frameType = 'C' // grinder: one chunk's parts; see writeChunk
	frameDone    frameType = 'D' // grinder: the file is finished
	frameRefused frameType = 'X' // grinder: this file cannot be done; see Refused
)

func (t frameType) String() string {
	if unicode.IsPrint(rune(t)) {
		return fmt.Sprintf("%q", rune(t))
	}
	return fmt.Sprintf("0x%02x", byte(t))
}

// smallFrame bounds every frame that is not a chunk or a run of keys. They
// are JSON of a few hundred bytes, so a megabyte is a frame gone wrong.
const smallFrame = 1 << 20

// frameHeaderSize is the type byte and the length.
const frameHeaderSize = 5

// writeFrame writes one frame whose payload is the concatenation of pieces,
// without copying them together: a chunk's parts are tens of megabytes, and
// the grinder is the side being careful with memory.
func writeFrame(w io.Writer, t frameType, pieces ...[]byte) error {
	var n int
	for _, p := range pieces {
		n += len(p)
	}
	if uint64(n) > 0xFFFFFFFF {
		return fmt.Errorf("a %s frame of %d bytes is too large to send", t, n)
	}
	var header [frameHeaderSize]byte
	header[0] = byte(t)
	binary.BigEndian.PutUint32(header[1:], uint32(n))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	for _, p := range pieces {
		if _, err := w.Write(p); err != nil {
			return err
		}
	}
	return nil
}

// writeJSON writes one frame holding v.
func writeJSON(w io.Writer, t frameType, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFrame(w, t, raw)
}

// frame is one frame read off the wire.
type frame struct {
	t       frameType
	payload []byte
}

// readFrame reads one frame, refusing a payload longer than limit before
// reading any of it: a length is the one thing on the wire that decides an
// allocation, and the far end does not get to decide this side's.
func readFrame(r io.Reader, limit int) (frame, error) {
	var header [frameHeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return frame{}, err
	}
	t := frameType(header[0])
	n := binary.BigEndian.Uint32(header[1:])
	if uint64(n) > uint64(limit) {
		return frame{}, fmt.Errorf("a %s frame of %d bytes is over the %d this side accepts", t, n, limit)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return frame{}, fmt.Errorf("reading a %s frame: %w", t, unexpected(err))
	}
	return frame{t: t, payload: payload}, nil
}

// unexpected turns a clean EOF partway through something into the error it
// is: the other side went away in the middle.
func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// decode reads a JSON frame of the type wanted into out.
func (f frame) decode(want frameType, out any) error {
	if f.t != want {
		return fmt.Errorf("expected a %s frame, got %s", want, f.t)
	}
	if err := json.Unmarshal(f.payload, out); err != nil {
		return fmt.Errorf("a %s frame that does not parse: %w", want, err)
	}
	return nil
}

// readPreamble reads the grinder's opening bytes and says, in a way a person
// can act on, what answered instead when it was not a grinder.
func readPreamble(r io.Reader) error {
	got := make([]byte, len(preamble))
	n, err := io.ReadFull(r, got)
	if err == nil && bytes.Equal(got, preamble) {
		return nil
	}
	if n == 0 {
		return errors.New("the machine ran the command and it said nothing")
	}
	return fmt.Errorf("something other than sand grind answered on the machine: %q", printable(got[:n]))
}

// printable keeps what is worth showing of bytes that were not what was
// expected: the text, without control characters.
func printable(b []byte) string {
	var out []rune
	for _, r := range string(b) {
		if unicode.IsPrint(r) {
			out = append(out, r)
		}
	}
	return string(out)
}

// Refused is a grinder declining one file, with its reason. The session is
// still good: the grinder has abandoned the file and waits for the next Job.
type Refused struct {
	Reason string `json:"reason"`
}

func (r *Refused) Error() string { return r.Reason }

// chunkHeaderSize is the fixed part of a chunk frame: the index, the
// compressed size and how many parts follow.
const chunkHeaderSize = 4 + 8 + 2

// writeChunk sends one chunk's parts as a single frame:
//
//	index u32 | compressed size u64 | count u16 | count × (length u32 | part)
func writeChunk(w io.Writer, index uint32, compressed uint64, parts [][]byte) error {
	if len(parts) > 0xFFFF {
		return fmt.Errorf("%d parts is more than a chunk frame holds", len(parts))
	}
	head := make([]byte, chunkHeaderSize, chunkHeaderSize+4*len(parts))
	binary.BigEndian.PutUint32(head[0:], index)
	binary.BigEndian.PutUint64(head[4:], compressed)
	binary.BigEndian.PutUint16(head[12:], uint16(len(parts)))

	pieces := make([][]byte, 0, 1+2*len(parts))
	pieces = append(pieces, head)
	for _, part := range parts {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(part)))
		pieces = append(pieces, length[:], part)
	}
	return writeFrame(w, frameChunk, pieces...)
}

// parsedChunk is a chunk frame taken apart. The parts are slices of the
// frame's payload, not copies.
type parsedChunk struct {
	index      uint32
	compressed uint64
	parts      [][]byte
}

// parseChunk takes a chunk frame apart, refusing one whose lengths do not add
// up to exactly the payload it came in.
func parseChunk(payload []byte) (parsedChunk, error) {
	if len(payload) < chunkHeaderSize {
		return parsedChunk{}, errors.New("a chunk frame too short to hold its own header")
	}
	out := parsedChunk{
		index:      binary.BigEndian.Uint32(payload[0:]),
		compressed: binary.BigEndian.Uint64(payload[4:]),
	}
	count := int(binary.BigEndian.Uint16(payload[12:]))
	rest := payload[chunkHeaderSize:]
	out.parts = make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		if len(rest) < 4 {
			return parsedChunk{}, fmt.Errorf("chunk %d: part %d is cut off", out.index, i+1)
		}
		n := binary.BigEndian.Uint32(rest)
		rest = rest[4:]
		if uint64(n) > uint64(len(rest)) {
			return parsedChunk{}, fmt.Errorf("chunk %d: part %d claims %d bytes and %d are left", out.index, i+1, n, len(rest))
		}
		out.parts = append(out.parts, rest[:n])
		rest = rest[n:]
	}
	if len(rest) != 0 {
		return parsedChunk{}, fmt.Errorf("chunk %d: %d bytes after the last part", out.index, len(rest))
	}
	return out, nil
}
