package archive

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/chinmay28/SandHarbor/internal/crypto"
)

// A chunk encoded with only its own derived key opens under the master, which
// is the property grinding on another machine rests on: the vault can read
// what that machine made without ever having handed it the master.
func TestEncodeChunkKeyedOpensUnderTheMaster(t *testing.T) {
	data := bytes.Repeat([]byte("ground elsewhere "), 300)
	plan, err := PlanChunks(testArchiveID(t), "a.txt", sha256.Sum256(data), uint64(len(data)), 1024, SchemeDefault)
	if err != nil {
		t.Fatalf("PlanChunks: %v", err)
	}

	var rebuilt []byte
	for index := uint32(0); index < plan.ChunkCount; index++ {
		size, _ := plan.PlaintextSize(index)
		offset := uint64(index) * uint64(plan.ChunkSize)

		key, err := crypto.DeriveChunkKey(testMaster(), plan.ArchiveID, index)
		if err != nil {
			t.Fatalf("DeriveChunkKey: %v", err)
		}
		encoded, err := EncodeChunkKeyed(plan, index, data[offset:offset+size], key)
		if err != nil {
			t.Fatalf("EncodeChunkKeyed(%d): %v", index, err)
		}
		for _, part := range encoded.Parts {
			if _, err := OpenShard(part, testMaster()); err != nil {
				t.Fatalf("chunk %d: a keyed part does not open under the master: %v", index, err)
			}
		}
		decoded, err := DecodeChunk(encoded.Parts[1:], testMaster())
		if err != nil {
			t.Fatalf("DecodeChunk(%d): %v", index, err)
		}
		rebuilt = append(rebuilt, decoded.Data...)
	}
	if !bytes.Equal(rebuilt, data) {
		t.Fatalf("rebuilt %d bytes, want the original %d", len(rebuilt), len(data))
	}
}

// The key for one chunk does not seal another: a machine handed keys in the
// wrong order makes parts that refuse to open, rather than parts that open as
// the wrong piece of the file.
func TestEncodeChunkKeyedWithAnotherChunksKeyDoesNotOpen(t *testing.T) {
	data := make([]byte, 2048)
	plan, err := PlanChunks(testArchiveID(t), "a.bin", sha256.Sum256(data), uint64(len(data)), 1024, SchemeDefault)
	if err != nil {
		t.Fatalf("PlanChunks: %v", err)
	}
	wrong, _ := crypto.DeriveChunkKey(testMaster(), plan.ArchiveID, 1)
	encoded, err := EncodeChunkKeyed(plan, 0, data[:1024], wrong)
	if err != nil {
		t.Fatalf("EncodeChunkKeyed: %v", err)
	}
	if _, err := OpenShard(encoded.Parts[0], testMaster()); err == nil {
		t.Fatal("a part sealed under chunk 1's key opened as chunk 0")
	}
}

func TestEncodeChunkKeyedRefusesAShortKey(t *testing.T) {
	plan, _ := PlanChunks(testArchiveID(t), "a.bin", [32]byte{}, 0, 1024, SchemeDefault)
	_, err := EncodeChunkKeyed(plan, 0, nil, make([]byte, 16))
	if err == nil || !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("err = %v, want a refusal naming the key length", err)
	}
}
