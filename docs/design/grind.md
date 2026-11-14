# Making the parts on the machine the file is on

An import from a machine normally brings each file across whole: SFTP down to
the vault, a spool beside the vault file, then compress, erasure-code, seal and
scatter there. **Make the parts on the machine** moves the middle of that to
the far end. The vault runs `sand grind` over the same SSH login and gets back
sealed parts, which it checks and ships to the clouds.

```
  machine (sand grind)                         vault (sand serve)
  ───────────────────                          ──────────────────
  read file → SHA-256            ── Planned ─▶  derive this file's chunk keys
                                 ◀── Keys ────
  read again → compress → code →
  seal (keyed) → in order        ── Chunk* ──▶  open every seal, check header
                                                and metadata → scatter
  re-hash matches?               ── Done ────▶  commit the entry
```

What it is for: a vault on a Raspberry Pi importing from a workstation does
the cheap half, checking seals and talking to clouds. It leaves the
compressing and coding to the machine with the cores. The vault spools
nothing, and the plaintext never crosses the link.

What it costs: the link carries the parts, which are n/k times the compressed
file. On incompressible video at 2-of-3, that is half as much again as
bringing the file across whole.

Code: `internal/grind` (protocol, budget, grinder, the vault's session),
`internal/vault/grindsource.go` (the import path), `cmd/sand/grind.go`.

## Running code on the far end

The far end needs `sand` installed, the same release as the vault: the
protocol version and the part format must both match, or the vault refuses
with both versions named. The vault runs one POSIX `sh` script that looks for
it on `PATH`, then in `/opt/sand/bin`, `/usr/local/bin`, `~/.local/bin` and
`~/bin`. If it isn't in any of those, the script says so on stderr, and that
message is what the dialog shows.

The grinder starts its output with a fixed preamble before any frame. The
preamble is how the vault tells a grinder from a shell banner, a NAS menu or
an older `sand` that doesn't know `grind`. A login that may only speak SFTP
(`ForceCommand internal-sftp`, storage boxes, most NAS accounts) refuses the
exec. Such a machine can still be imported from the ordinary way.

Asking for grinding on a machine that can't do it fails the import up front,
with the reason (HTTP 409, `GRIND_UNAVAILABLE`). The import does not fall back
to bringing the files across whole, because that isn't what was asked for.

## Keys: what the far end is trusted with

The vault's data key never leaves the vault. For each file the vault mints the
archive ID, derives `DeriveChunkKey(master, archiveID, i)` for each chunk, and
sends only those keys. Those keys open exactly the parts of a file the machine
already holds in the clear. The keys:

- travel inside the SSH channel, on the grinder's stdin. They are never on a
  command line, where `ps` would show them, and never in the environment;
- are never written to the machine's disk;
- are zeroed by both ends when the file is done.

`archive.EncodeChunkKeyed` is the encoder both sides run. `EncodeChunk` derives
the key and calls it, so the far end makes byte-for-byte the format the vault
makes itself.

## What comes back is checked, not trusted

Before a part goes to any cloud, the vault opens its seal with the chunk key
and checks:

- the header says this archive, this chunk index, this part number and this
  code, at the current format;
- the sealed metadata names this file, this hash, size, chunk size and chunk
  count, and the compressed size the frame claimed;
- the shard is the length the code gives that compressed size.

A relabelled part, another file's part, a flipped bit or a part from the wrong
chunk all fail there. What the check does not prove is that the parity shards
agree with the data shards: that would mean decoding and re-encoding, the very
work being moved off the vault. A buggy grinder of the same release is the only
way to get there, and the format and protocol checks are what rule out a
mismatched one.

The checksum every part carries has to be known before the first chunk is
sealed. So the grinder reads the file twice: once to hash it, then again to
cut it, hashing a second time on the way. If the two hashes differ, the file
changed under it. The grinder then refuses at the end, and the vault erases
the parts it already shipped. A file that got shorter or longer between the
passes is caught the same way.

## Sparing the machine

The machine is somebody's, and doing other things, so:

- **Nothing is written to its disk.** Parts go down the channel as they are
  made. SSH flow control is what slows the grinder when the clouds are slower
  than it is.
- **Memory is planned per file** (`grind.Plan`) from live readings:
  `MemAvailable`, and the cgroup's `memory.max` less `memory.current` when it
  has one, whichever is tighter. One chunk in flight is reckoned at about 7×
  the chunk size: plaintext, compressed copy, coded shards, sealed parts and
  the compressor's state. One file's work may take at most half of the
  headroom, and the window is also held to one less than the cores, at most 4.
  A machine whose half won't hold one chunk is refused with what it had and
  what was wanted. A machine whose memory can't be read (no `/proc`) gets a
  window of one.
- **Memory is checked again before every chunk** (`grind.Gate`). If the
  headroom has fallen below one chunk's cost, the chunk waits for it to
  recover. After two minutes the file is refused, and a re-run picks it up.
- **The Go runtime is held to the budget**: `debug.SetMemoryLimit` is set to
  the file's budget plus 64 MiB, so the collector works to stay under it.
- **It runs at nice 10** and compresses with one thread per chunk. The window
  is the parallelism.
- **It stops when the vault does.** Input is read on its own goroutine, so the
  end of it is noticed mid-file and cancels the work. A vault that gives up on
  a file (a cloud failed, the import was stopped) closes the command, and that
  is the cancel. There is no cancel message in the protocol to be missed.

The probe (`GET /api/remote/{id}/grind`) reports the machine's version, cores,
free memory and the budget a file would get. The dialog puts those figures
beside the tick-box, so the cost is visible before it is chosen.

## The protocol

Frames on the command's stdin and stdout: a type byte, a big-endian `u32`
length, then the payload. The reader refuses an oversized length before
allocating anything for it. The limit is 1 MiB for JSON frames, 32 MiB for
keys, and for a chunk frame the most that chunk size and code can honestly
produce.

| Frame | From | Payload |
|---|---|---|
| `H` Hello | grinder | protocol, format, version, OS/arch, CPUs, memory (JSON) |
| `J` Job | vault | root, path, archive ID, file name, chunk size, k, n, level (JSON) |
| `R` Read | grinder | bytes read by the checksum pass (`u64`) |
| `P` Planned | grinder | size, SHA-256, first 512 bytes, budget (JSON) |
| `K` Keys | vault | 32 bytes per chunk |
| `C` Chunk | grinder | index `u32`, compressed size `u64`, count `u16`, then length-prefixed parts |
| `D` Done | grinder | empty |
| `X` Refused | grinder | reason (JSON); the file is abandoned, the session goes on |

Chunks arrive in index order; the grinder seals a window of them abreast and
sends them as they finish in order. One command serves a whole import. A
refusal keeps it, and any other failure closes it, so the next file opens a
fresh one.

## Paths

The grinder runs as the login and can read whatever the login can read, so the
source's folder scope is kept on the far end too, the same way the SFTP client
keeps it. A path that climbs is refused as written (`sftp.Under`), and a
symlink is resolved and its target checked against the resolved root. Only
regular files are opened.
