package grind

import (
	"encoding/hex"
	"fmt"

	"github.com/chinmay28/SandHarbor/internal/archive"
)

// Hello is the grinder introducing itself: what it speaks, what it runs on,
// and what the machine has to spare. The vault shows the last of these to the
// person deciding whether to tick the box.
type Hello struct {
	Protocol int `json:"protocol"`

	// Format is the part format the grinder writes, sandfile's
	// ErasureFormatVersion. A grinder that writes another is not used.
	Format int `json:"format"`

	// Version is the far end's sand build, said in messages and nowhere
	// relied on.
	Version string `json:"version"`

	OS     string `json:"os"`
	Arch   string `json:"arch"`
	CPUs   int    `json:"cpus"`
	Memory Memory `json:"memory"`
}

// Job is one file for the grinder to cut.
type Job struct {
	// Root is the folder the machine is scoped to and Path the file under it.
	// The grinder opens nothing that is not under Root; see openUnder.
	Root string `json:"root"`
	Path string `json:"path"`

	// ArchiveID is the file's archive ID in hex, minted by the vault. The
	// chunk keys are derived from it, so it is the vault's to choose.
	ArchiveID string `json:"archive_id"`

	// Filename is the name the parts carry inside their seal.
	Filename string `json:"filename"`

	ChunkSize uint32 `json:"chunk_size"`
	Data      int    `json:"data"`
	Total     int    `json:"total"`

	// Level is the compression level the vault would have used itself.
	Level int `json:"level"`
}

// scheme is the code the job asks for.
func (j Job) scheme() (archive.Scheme, error) {
	s := archive.Scheme{Data: j.Data, Total: j.Total}
	if err := s.Storable(); err != nil {
		return archive.Scheme{}, err
	}
	return s, nil
}

// archiveID decodes the job's archive ID.
func (j Job) archiveID() ([16]byte, error) {
	var id [16]byte
	raw, err := hex.DecodeString(j.ArchiveID)
	if err != nil || len(raw) != len(id) {
		return id, fmt.Errorf("%q is not an archive ID", j.ArchiveID)
	}
	copy(id[:], raw)
	return id, nil
}

// Planned is the grinder's answer to a Job once it has read the file through:
// what the vault needs to know to hand over the keys and, later, to record the
// file.
type Planned struct {
	Size int64  `json:"size"`
	Hash string `json:"hash"` // SHA-256 of the whole file, hex

	// Head is the file's first bytes, for the vault to tell its type by, as
	// it would from a file it read itself.
	Head []byte `json:"head"`

	// Budget is what the grinder settled on for this file.
	Budget Budget `json:"budget"`
}

// headSize is how much of a file Planned carries: what DetectMIME looks at.
const headSize = 512
