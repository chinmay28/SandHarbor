// Package grind makes a file's encrypted parts on the machine the file is on,
// rather than on the machine that runs the vault.
//
// An import from a machine normally brings the file across whole: the bytes
// come down over SFTP, are spooled beside the vault, and are compressed,
// erasure-coded and sealed there. Grinding moves that work to the far end. The
// vault runs `sand grind` over the same SSH login, hands it what it needs to
// cut one file, and gets the sealed parts back, which it checks and ships to
// the clouds as it would its own.
//
// # What the far end is trusted with
//
// The vault's data key never leaves it. For each file the grinder is sent the
// keys for that file's chunks and nothing else — crypto.DeriveChunkKey of the
// master, the file's archive ID and each chunk index — which open exactly the
// parts of a file the machine already holds in the clear. They travel inside
// the SSH channel, never on a command line where `ps` would show them, and are
// never written to the machine's disk.
//
// What comes back is not taken on trust either: the vault opens every part's
// seal before it ships it, and checks the part says it is the file, chunk and
// shard that was asked for. See Session.
//
// # What the far end is spared
//
// The grinder writes nothing to the machine's disk. Parts go down the SSH
// channel as they are made, and the channel's own flow control is what slows
// the grinder when the clouds are slower than it is. Memory is sized to what
// the machine has free when the file starts and checked again before every
// chunk — see Plan and Gate — and the grinder stops the moment its input
// ends, so a vault that goes away does not leave it running.
//
// # The protocol
//
// Frames on the command's standard streams: a type byte, a four-byte length
// and a payload. The grinder speaks first, with a preamble and a Hello, and
// then answers Jobs one file at a time:
//
//	grinder → Hello
//	vault   → Job          (one file: where it is, its archive ID, the code)
//	grinder → Read*        (how far the checksum pass has got)
//	grinder → Planned      (its size, its SHA-256, its first bytes)
//	vault   → Keys         (one per chunk)
//	grinder → Chunk*       (in order)
//	grinder → Done
//
// A grinder that cannot do a file answers Refused in place of any of its
// frames, and is ready for the next Job. The vault that cannot go on with a
// file closes the command; there is no cancel in the protocol, because ending
// the command is a cancel the far end cannot miss.
package grind
