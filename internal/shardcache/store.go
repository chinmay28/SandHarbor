// Package shardcache keeps copies of a vault's stored parts on this machine's
// own disk, so that reading a file it holds costs a disk read rather than a
// round trip to several clouds.
//
// What it holds is exactly what the clouds hold: the sealed part files,
// byte for byte, as they were fetched. Nothing in this package can open one,
// and nothing here ever sees a key or a byte of plaintext. A file is put back
// together only when it is read, by the vault, in memory, from these parts —
// the same way it is from parts that came off the network. A stolen cache
// folder is therefore worth what a stolen cloud account is: noise.
//
// The store is a bounded blob store and nothing more. How many parts of a
// chunk to keep (the vault keeps k of n, never more), which files deserve a
// place in it, and for how long, are the vault's policy (see the vault's
// localcache.go); the store answers only "is this part here", "keep this
// part" and "how much room is left". Room is bounded two ways, whichever
// bites first:
//
//   - Limit: the most bytes the cache may hold, if the owner set one.
//   - Reserve: the free space the cache leaves on the disk it lives on, so a
//     cache can never be what fills the machine. Measured on every write,
//     because the rest of the disk is shared with everything else on it.
//
// A part that does not fit is refused with ErrFull, not squeezed in by
// evicting another: what to give up is a policy question, and the policy lives
// with the caller.
//
// Layout on disk, one folder per archive so a file is dropped in one call:
//
//	<dir>/<archive id>/c0000012-p2.sand
//
// The names say which archive, chunk and part a file is — no more than the
// object key on the cloud it came from already says.
package shardcache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
)

// Errors the store answers with.
var (
	// ErrMiss is a part the store is not holding.
	ErrMiss = errors.New("not in the local cache")
	// ErrFull is a part that would take the cache past its limit, or the disk
	// below the space the cache must leave free.
	ErrFull = errors.New("the local cache has no room")
	// ErrBadKey is a key that cannot name a stored part.
	ErrBadKey = errors.New("not a valid cache key")

	errNoDisk = errors.New("the free space on this disk cannot be measured here")
)

const (
	// MinReserve and MaxReserve bound the default reserve, which is a tenth of
	// the disk: a phone keeps a tenth of itself free, and a 20 TB array is not
	// asked to keep two of them idle.
	MinReserve int64 = 1 << 30
	MaxReserve int64 = 20 << 30
)

// DefaultReserve is the free space left on a disk of the given size when the
// owner has not said otherwise: a tenth of it, between MinReserve and
// MaxReserve.
func DefaultReserve(total int64) int64 {
	return min(max(total/10, MinReserve), MaxReserve)
}

// Key names one stored part.
type Key struct {
	// Archive is the archive ID, 32 lowercase hex digits.
	Archive string
	// Chunk is the chunk index, from zero.
	Chunk int
	// Part is the part number, from one.
	Part int
}

func (k Key) valid() bool {
	return validArchive(k.Archive) && k.Chunk >= 0 && k.Chunk < 1e7 && k.Part >= 1 && k.Part <= 255
}

func (k Key) fileName() string { return fmt.Sprintf("c%07d-p%d.sand", k.Chunk, k.Part) }

// String is the key as the vault names the same object on a cloud.
func (k Key) String() string { return fmt.Sprintf("%s-%s", k.Archive, k.fileName()) }

var (
	archivePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	partPattern    = regexp.MustCompile(`^c([0-9]{7})-p([0-9]{1,3})\.sand$`)
)

func validArchive(id string) bool { return archivePattern.MatchString(id) }

// Disk measures the filesystem holding dir: the bytes still free to spend, and
// its size. Tests stand in their own.
type Disk func(dir string) (free, total int64, err error)

// Options sets the bounds a store keeps to.
type Options struct {
	// Limit is the most bytes the cache may hold. Zero or less sets no limit
	// of the cache's own, leaving Reserve as the only bound.
	Limit int64
	// Reserve is the free space to leave on the disk. Zero picks
	// DefaultReserve for the disk's size; negative leaves nothing in reserve.
	Reserve int64
	// Disk measures the disk. Nil measures the real one.
	Disk Disk
}

// Usage is what the cache holds and the room it has.
type Usage struct {
	// Used is the bytes of parts held.
	Used int64 `json:"used"`
	// Parts is how many parts are held.
	Parts int `json:"parts"`
	// Limit is the cap of the cache's own, zero for none.
	Limit int64 `json:"limit"`
	// Reserve is the free space the cache leaves on the disk, as it applies
	// to this disk.
	Reserve int64 `json:"reserve"`
	// DiskFree and DiskTotal describe the disk the cache is on, zero when it
	// cannot be measured.
	DiskFree  int64 `json:"disk_free"`
	DiskTotal int64 `json:"disk_total"`
	// Room is how many more bytes the cache could take right now.
	Room int64 `json:"room"`
}

// Store is the cache on disk. It is safe for concurrent use.
type Store struct {
	dir  string
	disk Disk

	mu      sync.Mutex
	limit   int64
	reserve int64                            // as set: zero is the default, negative none
	held    map[string]map[int]map[int]int64 // archive → chunk → part → bytes
	used    int64
	parts   int
	pending int64 // bytes promised to writes not yet finished
}

// Open opens (creating if need be) the cache in dir and takes stock of what it
// holds. Files under names the store does not write are left alone; the
// half-written ones a crash leaves behind are removed.
func Open(dir string, opts Options) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating the local cache at %s: %w", dir, err)
	}
	s := &Store{
		dir:   dir,
		disk:  opts.Disk,
		held:  map[string]map[int]map[int]int64{},
		limit: max(opts.Limit, 0),
	}
	s.reserve = opts.Reserve
	if s.disk == nil {
		s.disk = realDisk
	}
	if err := s.scan(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir is the folder the cache lives in.
func (s *Store) Dir() string { return s.dir }

func (s *Store) scan() error {
	archives, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("reading the local cache at %s: %w", s.dir, err)
	}
	for _, a := range archives {
		if !a.IsDir() || !validArchive(a.Name()) {
			continue
		}
		files, err := os.ReadDir(filepath.Join(s.dir, a.Name()))
		if err != nil {
			return fmt.Errorf("reading the local cache at %s: %w", s.dir, err)
		}
		for _, f := range files {
			name := f.Name()
			if isTemp(name) {
				_ = os.Remove(filepath.Join(s.dir, a.Name(), name))
				continue
			}
			key, ok := parseName(a.Name(), name)
			if !ok || !f.Type().IsRegular() {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			s.recordLocked(key, info.Size())
		}
	}
	return nil
}

func parseName(archive, name string) (Key, bool) {
	m := partPattern.FindStringSubmatch(name)
	if m == nil {
		return Key{}, false
	}
	chunk, _ := strconv.Atoi(m[1])
	part, _ := strconv.Atoi(m[2])
	k := Key{Archive: archive, Chunk: chunk, Part: part}
	return k, k.valid()
}

const tempPrefix = ".partial-"

func isTemp(name string) bool {
	return len(name) > len(tempPrefix) && name[:len(tempPrefix)] == tempPrefix
}

func (s *Store) path(k Key) string { return filepath.Join(s.dir, k.Archive, k.fileName()) }

func (s *Store) recordLocked(k Key, size int64) {
	chunks := s.held[k.Archive]
	if chunks == nil {
		chunks = map[int]map[int]int64{}
		s.held[k.Archive] = chunks
	}
	parts := chunks[k.Chunk]
	if parts == nil {
		parts = map[int]int64{}
		chunks[k.Chunk] = parts
	}
	if old, ok := parts[k.Part]; ok {
		s.used -= old
		s.parts--
	}
	parts[k.Part] = size
	s.used += size
	s.parts++
}

func (s *Store) forgetLocked(k Key) {
	chunks := s.held[k.Archive]
	size, ok := chunks[k.Chunk][k.Part]
	if !ok {
		return
	}
	delete(chunks[k.Chunk], k.Part)
	if len(chunks[k.Chunk]) == 0 {
		delete(chunks, k.Chunk)
	}
	if len(chunks) == 0 {
		delete(s.held, k.Archive)
	}
	s.used -= size
	s.parts--
}

func (s *Store) sizeLocked(k Key) (int64, bool) {
	size, ok := s.held[k.Archive][k.Chunk][k.Part]
	return size, ok
}

// Has reports whether the store holds a part.
func (s *Store) Has(k Key) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sizeLocked(k)
	return ok
}

// Parts lists the part numbers held for one chunk of an archive, in order.
func (s *Store) Parts(archive string, chunk int) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int
	for part := range s.held[archive][chunk] {
		out = append(out, part)
	}
	sort.Ints(out)
	return out
}

// Counts returns how many parts are held for each chunk of an archive that
// has any, which is what "could this chunk be rebuilt from here" asks.
func (s *Store) Counts(archive string) map[int]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int]int{}
	for chunk, parts := range s.held[archive] {
		out[chunk] = len(parts)
	}
	return out
}

// Count is how many parts are held for one chunk of an archive.
func (s *Store) Count(archive string, chunk int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.held[archive][chunk])
}

// Get returns a held part's bytes, or ErrMiss. A part whose file has gone
// from under the store is forgotten and reported as a miss.
func (s *Store) Get(k Key) ([]byte, error) {
	if !k.valid() {
		return nil, ErrBadKey
	}
	s.mu.Lock()
	_, ok := s.sizeLocked(k)
	s.mu.Unlock()
	if !ok {
		return nil, ErrMiss
	}
	blob, err := os.ReadFile(s.path(k))
	if err != nil {
		s.mu.Lock()
		s.forgetLocked(k)
		s.mu.Unlock()
		if os.IsNotExist(err) {
			return nil, ErrMiss
		}
		return nil, fmt.Errorf("reading %s from the local cache: %w", k, err)
	}
	return blob, nil
}

// Put keeps a part, replacing any copy already held. It is refused with
// ErrFull, and nothing is written, when the part does not fit.
func (s *Store) Put(k Key, blob []byte) error {
	if !k.valid() {
		return ErrBadKey
	}
	n := int64(len(blob))

	s.mu.Lock()
	old, _ := s.sizeLocked(k)
	if err := s.fitsLocked(n - old); err != nil {
		s.mu.Unlock()
		return err
	}
	s.pending += n
	s.mu.Unlock()

	err := s.write(k, blob)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending -= n
	if err != nil {
		return err
	}
	s.recordLocked(k, n)
	return nil
}

func (s *Store) write(k Key, blob []byte) error {
	dir := filepath.Join(s.dir, k.Archive)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("writing %s to the local cache: %w", k, err)
	}
	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return fmt.Errorf("writing %s to the local cache: %w", k, err)
	}
	_, err = tmp.Write(blob)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), s.path(k))
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing %s to the local cache: %w", k, err)
	}
	return nil
}

// fitsLocked says whether n more bytes fit, against the limit and the disk.
func (s *Store) fitsLocked(n int64) error {
	if n <= 0 {
		return nil
	}
	room, err := s.roomLocked()
	if err != nil {
		return err
	}
	if n > room {
		return ErrFull
	}
	return nil
}

// roomLocked is how many more bytes the cache may take. A disk that cannot be
// measured is trusted only as far as a limit the owner set: with no limit
// either, nothing is written, because "as much as there is" is not a bound
// when how much there is cannot be known.
func (s *Store) roomLocked() (int64, error) {
	room := int64(-1) // unbounded until something bounds it
	if s.limit > 0 {
		room = max(s.limit-s.used-s.pending, 0)
	}
	free, total, err := s.disk(s.dir)
	if err != nil {
		if room < 0 {
			return 0, fmt.Errorf("%w: %v, and no size limit is set", ErrFull, err)
		}
		return room, nil
	}
	onDisk := max(free-s.pending-s.reserveFor(total), 0)
	if room < 0 || onDisk < room {
		room = onDisk
	}
	return room, nil
}

func (s *Store) reserveFor(total int64) int64 {
	switch {
	case s.reserve < 0:
		return 0
	case s.reserve == 0:
		return DefaultReserve(total)
	}
	return s.reserve
}

// Remove drops one part. Removing a part that is not held is not an error.
func (s *Store) Remove(k Key) error {
	if !k.valid() {
		return ErrBadKey
	}
	s.mu.Lock()
	s.forgetLocked(k)
	s.mu.Unlock()
	if err := os.Remove(s.path(k)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s from the local cache: %w", k, err)
	}
	return nil
}

// RemoveArchive drops every part held for an archive and returns the bytes
// that frees.
func (s *Store) RemoveArchive(archive string) (int64, error) {
	if !validArchive(archive) {
		return 0, ErrBadKey
	}
	s.mu.Lock()
	var freed int64
	for _, parts := range s.held[archive] {
		for _, size := range parts {
			freed += size
			s.used -= size
			s.parts--
		}
	}
	delete(s.held, archive)
	s.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(s.dir, archive)); err != nil {
		return freed, fmt.Errorf("removing %s from the local cache: %w", archive, err)
	}
	return freed, nil
}

// Archives returns the bytes held for each archive.
func (s *Store) Archives() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int64, len(s.held))
	for archive, chunks := range s.held {
		for _, parts := range chunks {
			for _, size := range parts {
				out[archive] += size
			}
		}
	}
	return out
}

// SetLimits changes the bounds, with the meaning Options gives them. What is
// already held stays even if it is now over: the caller decides what to drop.
func (s *Store) SetLimits(limit, reserve int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limit = max(limit, 0)
	s.reserve = reserve
}

// Usage reports what is held and the room left.
func (s *Store) Usage() Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := Usage{Used: s.used, Parts: s.parts, Limit: s.limit}
	if free, total, err := s.disk(s.dir); err == nil {
		u.DiskFree, u.DiskTotal = free, total
		u.Reserve = s.reserveFor(total)
	} else if s.reserve > 0 {
		u.Reserve = s.reserve
	}
	if room, err := s.roomLocked(); err == nil {
		u.Room = room
	}
	return u
}
