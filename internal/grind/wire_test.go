package grind

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := writeFrame(&buf, frameRead, []byte("ab"), []byte("cd")); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	f, err := readFrame(&buf, 16)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if f.t != frameRead || string(f.payload) != "abcd" {
		t.Fatalf("got %s %q", f.t, f.payload)
	}
}

// The length decides an allocation, so a frame over the limit is refused
// before a byte of its payload is read or room is made for it.
func TestReadFrameRefusesAnOversizeLengthUpFront(t *testing.T) {
	r := bytes.NewReader([]byte{byte(frameChunk), 0xFF, 0xFF, 0xFF, 0xFF})
	_, err := readFrame(r, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("err = %v, want a refusal of the length", err)
	}
}

func TestReadFrameCutOffMidPayloadIsUnexpected(t *testing.T) {
	var buf bytes.Buffer
	writeFrame(&buf, frameDone, []byte("whole payload"))
	cut := bytes.NewReader(buf.Bytes()[:buf.Len()-3])
	if _, err := readFrame(cut, 64); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want unexpected EOF", err)
	}
}

func TestDecodeRefusesTheWrongType(t *testing.T) {
	var out Hello
	err := frame{t: frameDone, payload: []byte("{}")}.decode(frameHello, &out)
	if err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("err = %v", err)
	}
}

func TestPreambleNamesWhatAnsweredInstead(t *testing.T) {
	if err := readPreamble(bytes.NewReader(preamble)); err != nil {
		t.Fatalf("the real preamble: %v", err)
	}
	err := readPreamble(strings.NewReader("Welcome to MyNAS\n"))
	if err == nil || !strings.Contains(err.Error(), "Welcome") {
		t.Fatalf("err = %v, want the banner quoted", err)
	}
	if err := readPreamble(strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "said nothing") {
		t.Fatalf("err = %v, want silence named", err)
	}
}

func TestChunkFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	parts := [][]byte{[]byte("one"), {}, []byte("three")}
	if err := writeChunk(&buf, 7, 42, parts); err != nil {
		t.Fatalf("writeChunk: %v", err)
	}
	f, err := readFrame(&buf, 1<<10)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	got, err := parseChunk(f.payload)
	if err != nil {
		t.Fatalf("parseChunk: %v", err)
	}
	if got.index != 7 || got.compressed != 42 || len(got.parts) != 3 {
		t.Fatalf("got %+v", got)
	}
	for i := range parts {
		if !bytes.Equal(got.parts[i], parts[i]) {
			t.Fatalf("part %d = %q, want %q", i, got.parts[i], parts[i])
		}
	}
}

// Lengths that do not add up to exactly the frame are refused, whichever way
// they are wrong.
func TestParseChunkRefusesLengthsThatDoNotAddUp(t *testing.T) {
	var buf bytes.Buffer
	writeChunk(&buf, 1, 1, [][]byte{[]byte("abc")})
	payload := buf.Bytes()[frameHeaderSize:]

	if _, err := parseChunk(payload[:len(payload)-1]); err == nil {
		t.Error("a part cut short was accepted")
	}
	if _, err := parseChunk(append(append([]byte{}, payload...), 'x')); err == nil {
		t.Error("bytes after the last part were accepted")
	}
	if _, err := parseChunk(payload[:5]); err == nil {
		t.Error("a frame shorter than its header was accepted")
	}
}
