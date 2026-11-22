package bluray

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/chinmay28/SandHarbor/internal/bluray/bluraytest"
)

// A timestamp is 33 bits and wraps: a clip late on its clock, moved later
// still, comes round to the start rather than spilling into the bits beside it.
func TestShiftClocksWrapsAt33Bits(t *testing.T) {
	const top = 1<<33 - 1
	_, before := bluraytest.TimedClip(top-10, 1)
	_, want := bluraytest.TimedClip(top-10+500, 1) // TimedClip wraps the same way
	shiftClocks(before, 500)
	if !bytes.Equal(before, want) {
		t.Errorf("a shift past the top of the clock:\n got % x\nwant % x", before[:32], want[:32])
	}
}

// Moving a clock forward and then back by the same amount must leave every
// byte where it was: nothing but the timestamps may change.
func TestShiftClocksChangesOnlyTheTimestamps(t *testing.T) {
	_, packets := bluraytest.TimedClip(123456789, 50)
	original := bytes.Clone(packets)
	for i := 0; i < len(packets); i += tsPacket {
		shiftClocks(packets[i:i+tsPacket], 987654321)
	}
	if bytes.Equal(packets, original) {
		t.Fatal("the shift changed nothing")
	}
	for i := 0; i < len(packets); i += tsPacket {
		shiftClocks(packets[i:i+tsPacket], 1<<33-987654321)
	}
	if !bytes.Equal(packets, original) {
		t.Error("shifting forward and back did not give back the same bytes")
	}
}

// Packets that carry no clock are left alone, and no packet, whatever is in
// it, makes the shift read outside it: a disc's bytes are whatever was
// uploaded.
func TestShiftClocksSurvivesAnyPacket(t *testing.T) {
	// The program table and padding look, to a careless reader, like packets
	// with a header to rewrite.
	for _, pid := range []int{0, 0x1fff} {
		_, p := bluraytest.TimedClip(1000, 1)
		p[1], p[2] = p[1]&0xe0|byte(pid>>8), byte(pid)
		before := bytes.Clone(p)
		shiftClocks(p, 5000)
		if !bytes.Equal(p, before) {
			t.Errorf("a packet on PID %#x was changed", pid)
		}
	}

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200000; i++ {
		p := make([]byte, tsPacket)
		rng.Read(p)
		p[0] = syncByte
		if i%2 == 0 {
			// Often enough, something that claims to be a header.
			at := 4
			if p[3]&0x20 != 0 {
				at = 5 + int(p[4])
			}
			if at+3 < tsPacket {
				p[at], p[at+1], p[at+2] = 0, 0, 1
			}
		}
		shiftClocks(p, rng.Uint64())
	}
}
