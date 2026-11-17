package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chinmay28/SandHarbor/internal/archive"
	"github.com/chinmay28/SandHarbor/internal/crypto"
	"github.com/chinmay28/SandHarbor/internal/provider"
	"github.com/chinmay28/SandHarbor/internal/shardcache"
	"github.com/google/uuid"
)

// Keeping files on this machine, so they open without the clouds.
//
// A film on three cloud accounts streams as fast as the slowest of the two it
// needs, and a folder of RAW photos is a round trip per picture. Asking for a
// file or folder to be *kept here* puts a copy of its parts on this machine's
// own disk until a date the owner chooses, and from then on every read of it
// takes its parts from the disk first and the clouds only for what is missing.
//
// **What is kept is the parts, sealed.** The cache holds the very objects the
// clouds hold — compressed, erasure-coded, AES-256-GCM sealed — and only the
// fewest that rebuild each chunk: k of n, never more. A 2-of-3 film costs its
// own size here, not one and a half times it.
//
// **Which placement is kept is the owner's choice,** per pin, the same three a
// stream link offers (ReadSource): the file's parts, its replica alone — which
// is often the copy on a cloud of its own, nearer or faster than the rest — or
// either, the parts with the replica standing in for any chunk they cannot
// give. Whichever it is, only that placement's shards are kept, k of them a
// chunk. Under "either" a chunk is kept from one placement or the other, never
// both: once it is whole from one, what is here of the other is dropped. Where
// pins overlap, the most specific decides: a file's own pin over its folder's,
// a deeper folder's over a shallower one's.
//
// A read of a kept file takes a chunk kept from its replica before asking any
// cloud for its parts when the replica is cut at the same chunk seams; one cut
// differently serves reads that ask for the replica, as a stream link can. A file is put back together only
// when it is opened, in memory, exactly as it is from parts off the network;
// nothing readable is ever written to the disk. Each part read from the cache
// is opened and checked like one from a cloud, so a cached part that has
// rotted is dropped and fetched again rather than trusted. See
// internal/shardcache for the store itself.
//
// **What is kept, and for how long, is a pin.** A pin names a file (followed
// by its ID, so a rename does not lose it) or a folder (everything under it,
// including what is added later) and the moment it expires. Pins live in a
// sidecar beside the vault, `<vault>.pins`, sealed under a key derived from the
// data key the way the read history is, because a list of the folders somebody
// wants close at hand is a thing about them. They are this machine's and only
// this machine's: the cache is on this disk, so another machine opening the
// same vault has its own pins or none.
//
// **Room.** The cache never takes the disk below a reserve (a tenth of it by
// default) and never past a limit if one is set. A pin that does not fit is
// filled as far as it goes and says so; nothing already kept is evicted to
// make room for it, since which file matters more is the owner's call.
//
// **Filling** is a background step the server takes while the vault is open
// (FillLocalCache): it resolves the pins against the index, drops from the
// disk whatever no unexpired pin wants any more — an expired pin, a deleted
// file, a file re-encrypted under a new archive — and fetches what is missing,
// a bounded amount per step. A pinned file that is read before the fill reaches
// it keeps the parts that read fetched, so watching a pinned film fills it too.
//
// Main vault only. A sub vault is opened by a password of its own, and a pin
// naming one of its folders would have to be sealed under a key the main
// vault's password opens, which is exactly what a sub vault exists to avoid.

const (
	localPinsVersion = 1
	localPinsPurpose = "sand-local-cache-pins-v1"

	// LocalCacheAccountID stands in for an account on read events whose part
	// came from this machine's cache rather than a cloud.
	LocalCacheAccountID = "local-cache"
)

// Errors the local cache answers with.
var (
	ErrCacheBusy     = errors.New("the local cache is already being filled")
	ErrCacheSubVault = errors.New("only the main vault's files can be kept on this machine")
	ErrNoSuchPin     = errors.New("nothing is being kept under that name")
)

// CachePin is one file or folder asked to be kept on this machine.
type CachePin struct {
	ID string `json:"id"`
	// Path is the file or folder, as it was when pinned for a file (which is
	// followed by EntryID) and as it is for a folder.
	Path    string    `json:"path"`
	Folder  bool      `json:"folder"`
	EntryID string    `json:"entry_id,omitempty"`
	Added   time.Time `json:"added"`
	Until   time.Time `json:"until"`
	// From is which placement is kept: ReadParts (also the empty value),
	// ReadReplica or ReadEither.
	From ReadSource `json:"from,omitempty"`
}

// source is the pin's From, with the empty value read as the parts.
func (p CachePin) source() ReadSource {
	if p.From == "" {
		return ReadParts
	}
	return p.From
}

// localPinsFile is the sidecar's contents.
type localPinsFile struct {
	Version int        `json:"version"`
	Limit   int64      `json:"limit,omitempty"`   // the store's Limit
	Reserve int64      `json:"reserve,omitempty"` // the store's Reserve
	Pins    []CachePin `json:"pins"`
}

// localCacheState is what the vault remembers about the cache in memory.
type localCacheState struct {
	mu     sync.Mutex // a leaf lock; nothing is taken while it is held
	store  *shardcache.Store
	wanted map[string]bool // archives some unexpired pin wants; nil while locked
	// partner pairs a kept file's parts archive with its replica's, both ways:
	// the other placement of the same chunks, whose shards for a chunk are
	// surplus once this one holds k of it.
	partner map[string]string
	full    bool   // the last fill stopped for want of room
	last    string // what the last fill could not do, if anything

	fill sync.Mutex // the one slot a fill runs in, taken with TryLock
}

func localPinsPath(vaultPath string) string { return vaultPath + ".pins" }
func localCacheDir(vaultPath string) string { return vaultPath + ".cache" }
func cacheKeyOf(archiveID string, chunk, part int) shardcache.Key {
	return shardcache.Key{Archive: archiveID, Chunk: chunk, Part: part}
}

// localStore returns the store, opening it the first time. With create false
// it opens only a cache that already exists, so that reading a file on a
// machine that has never kept one does not make a folder for it.
func (v *Vault) localStore(create bool) (*shardcache.Store, error) {
	v.local.mu.Lock()
	defer v.local.mu.Unlock()
	if v.local.store != nil {
		return v.local.store, nil
	}
	dir := localCacheDir(v.path)
	if !create {
		if _, err := os.Stat(dir); err != nil {
			return nil, nil
		}
	}
	store, err := shardcache.Open(dir, shardcache.Options{})
	if err != nil {
		return nil, err
	}
	v.local.store = store
	return store, nil
}

// --- the sealed list of pins ------------------------------------------------

func readLocalPins(vaultPath string, dataKey []byte) (*localPinsFile, error) {
	file := &localPinsFile{Version: localPinsVersion}
	sealed, err := os.ReadFile(localPinsPath(vaultPath))
	if os.IsNotExist(err) {
		return file, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the list of files kept on this machine: %w", err)
	}
	plain, err := openPurpose(dataKey, localPinsPurpose, sealed)
	if err != nil {
		// A password changed while nothing could re-seal this, most likely.
		// The pins are lost; what they kept is dropped by the next fill.
		log.Printf("the list of files kept on this machine does not open under this vault's key; starting again")
		return file, nil
	}
	defer crypto.ZeroBytes(plain)
	if err := json.Unmarshal(plain, file); err != nil {
		log.Printf("the list of files kept on this machine could not be read; starting again: %v", err)
		return &localPinsFile{Version: localPinsVersion}, nil
	}
	if file.Version > localPinsVersion {
		return nil, fmt.Errorf("the list of files kept on this machine was written by a later version of SAND")
	}
	return file, nil
}

func writeLocalPins(vaultPath string, dataKey []byte, file *localPinsFile) error {
	file.Version = localPinsVersion
	plain, err := json.Marshal(file)
	if err != nil {
		return err
	}
	defer crypto.ZeroBytes(plain)
	sealed, err := sealPurpose(dataKey, localPinsPurpose, plain)
	if err != nil {
		return err
	}
	return writeFileAtomically(localPinsPath(vaultPath), sealed)
}

func sealPurpose(dataKey []byte, purpose string, plain []byte) ([]byte, error) {
	key, err := crypto.DerivePurposeKey(dataKey, purpose)
	if err != nil {
		return nil, err
	}
	defer crypto.ZeroBytes(key)
	nonce, err := crypto.GenerateNonce()
	if err != nil {
		return nil, err
	}
	sealed, err := crypto.Encrypt(key, nonce, plain, []byte(purpose))
	if err != nil {
		return nil, err
	}
	return append(nonce, sealed...), nil
}

func openPurpose(dataKey []byte, purpose string, sealed []byte) ([]byte, error) {
	if len(sealed) <= crypto.NonceSize {
		return nil, errors.New("too short")
	}
	key, err := crypto.DerivePurposeKey(dataKey, purpose)
	if err != nil {
		return nil, err
	}
	defer crypto.ZeroBytes(key)
	return crypto.Decrypt(key, sealed[:crypto.NonceSize], sealed[crypto.NonceSize:], []byte(purpose))
}

// resealLocalPinsLocked moves the sidecar from one data key to another, for a
// password change. The pins are never held in memory, so the file is always
// the only copy and is always re-sealed rather than rewritten.
func (v *Vault) resealLocalPinsLocked(oldKey, newKey []byte) {
	if _, err := os.Stat(localPinsPath(v.path)); err != nil {
		return
	}
	file, err := readLocalPins(v.path, oldKey)
	if err != nil {
		log.Printf("could not re-seal the list of files kept on this machine: %v", err)
		return
	}
	if err := writeLocalPins(v.path, newKey, file); err != nil {
		log.Printf("could not re-seal the list of files kept on this machine: %v", err)
	}
}

// editLocalPins reads the pins, lets edit change them, and writes them back,
// all under the vault's write lock so two edits cannot interleave.
func (v *Vault) editLocalPins(edit func(file *localPinsFile, m *Manifest) error) (*localPinsFile, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return nil, ErrLocked
	}
	file, err := readLocalPins(v.path, v.dataKey)
	if err != nil {
		return nil, err
	}
	if err := edit(file, v.manifest); err != nil {
		return nil, err
	}
	if err := writeLocalPins(v.path, v.dataKey, file); err != nil {
		return nil, fmt.Errorf("saving the list of files kept on this machine: %w", err)
	}
	v.setLocalWanted(resolvePins(file.Pins, v.manifest, time.Now()))
	return file, nil
}

// --- what the pins want -----------------------------------------------------

// keepPlan is how one file is kept: which placement is filled, from which
// root of the accounts, and — under ReadEither only — what stands in for it.
type keepPlan struct {
	file    *Entry     // the file, as the index has it
	from    ReadSource // the choice of the pin that decides it
	primary *Entry     // the placement kept: the file, or its replica's view
	role    provider.Role
	standby *Entry // the replica standing in chunk for chunk, or nil
}

// planFor makes a file's plan, or nil when the choice cannot be met: the
// replica alone, asked of a file with none.
func planFor(e *Entry, from ReadSource) *keepPlan {
	p := &keepPlan{file: e, from: from, primary: e, role: provider.Parts}
	switch from {
	case ReadReplica:
		view := e.replicaView()
		if view == nil || !view.Chunked() {
			return nil
		}
		p.primary, p.role = view, provider.Replicas
	case ReadEither:
		p.standby = keptReplica(e)
	}
	return p
}

// need is what keeping the file takes on disk.
func (p *keepPlan) need() int64 { return cachedBytesNeeded(p.primary) }

// here is how much of that is on disk, by archive.
func (p *keepPlan) here(held map[string]int64) int64 {
	n := held[p.primary.ArchiveID]
	if p.standby != nil {
		n += held[p.standby.ArchiveID]
	}
	return min(n, p.need())
}

// kept is every chunk of the file being rebuildable from this machine alone.
func (p *keepPlan) kept(store *shardcache.Store) bool {
	primary := store.Counts(p.primary.ArchiveID)
	var standby map[int]int
	if p.standby != nil {
		standby = store.Counts(p.standby.ArchiveID)
	}
	for i := 0; i < p.primary.ChunkCount; i++ {
		if !p.chunkKept(primary, standby, i) {
			return false
		}
	}
	return true
}

// chunkKept is chunk i being whole here: k of the kept placement's shards, or
// of the stand-in's.
func (p *keepPlan) chunkKept(primary, standby map[int]int, index int) bool {
	if primary[index] >= p.primary.Scheme().Data {
		return true
	}
	return p.standby != nil && standby[index] >= p.standby.Scheme().Data
}

// pinnedFiles is the pins resolved against the index at one moment.
type pinnedFiles struct {
	// plans is how every chunked file some unexpired pin wants is kept, by
	// the file's archive.
	plans map[string]*keepPlan
	// byPin is each pin's files, by archive, and whether what it names was
	// found.
	byPin map[string][]string
	found map[string]bool
	// unsupported counts each pin's files in the old whole-file format, and
	// noReplica the ones it would keep from a replica they do not have.
	unsupported map[string]int
	noReplica   map[string]int
}

func resolvePins(pins []CachePin, m *Manifest, now time.Time) pinnedFiles {
	out := pinnedFiles{
		plans:       map[string]*keepPlan{},
		byPin:       map[string][]string{},
		found:       map[string]bool{},
		unsupported: map[string]int{},
		noReplica:   map[string]int{},
	}
	if m == nil {
		return out
	}

	// Least specific first, so that a later pin's choice overrides: folders
	// by depth, then single files.
	ordered := make([]CachePin, 0, len(pins))
	for _, pin := range pins {
		if now.Before(pin.Until) {
			ordered = append(ordered, pin)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.Folder != b.Folder {
			return a.Folder
		}
		return len(a.Path) < len(b.Path)
	})

	files := map[string]*Entry{}
	choice := map[string]ReadSource{}
	for _, pin := range ordered {
		var matched []*Entry
		switch {
		case pin.Folder:
			if m.FolderExists(pin.Path) {
				out.found[pin.ID] = true
				matched = m.Descendants(pin.Path)
			}
		default:
			e := m.ByID(pin.EntryID)
			if e == nil {
				e = m.ByPath(pin.Path)
			}
			if e != nil {
				out.found[pin.ID] = true
				matched = []*Entry{e}
			}
		}
		for _, e := range matched {
			if !e.Chunked() {
				out.unsupported[pin.ID]++
				continue
			}
			if files[e.ArchiveID] == nil {
				snapshot := *e
				snapshot.Shards = append([]Shard(nil), e.Shards...)
				files[e.ArchiveID] = &snapshot
			}
			choice[e.ArchiveID] = pin.source()
			out.byPin[pin.ID] = append(out.byPin[pin.ID], e.ArchiveID)
		}
	}
	for archive, e := range files {
		if plan := planFor(e, choice[archive]); plan != nil {
			out.plans[archive] = plan
		}
	}
	for id, archives := range out.byPin {
		for _, archive := range archives {
			if out.plans[archive] == nil {
				out.noReplica[id]++
			}
		}
	}
	return out
}

// keptReplica is the replica that can stand in for a file's parts chunk for
// chunk, or nil: one cut at the same seams, so that its chunk i is the file's
// chunk i. The rule the reader's own fallback keeps to (see OpenReader).
func keptReplica(e *Entry) *Entry {
	view := e.replicaView()
	if view == nil || !view.Chunked() || view.ChunkSize != e.ChunkSize || view.ChunkCount != e.ChunkCount {
		return nil
	}
	return view
}

func (v *Vault) setLocalWanted(p pinnedFiles) {
	wanted := make(map[string]bool, len(p.plans))
	partner := map[string]string{}
	for _, plan := range p.plans {
		wanted[plan.primary.ArchiveID] = true
		if plan.standby != nil {
			a, b := plan.primary.ArchiveID, plan.standby.ArchiveID
			wanted[b] = true
			partner[a], partner[b] = b, a
		}
	}
	v.local.mu.Lock()
	v.local.wanted, v.local.partner = wanted, partner
	v.local.mu.Unlock()
}

// settleChunk drops the other placement's shards of a chunk once this
// archive holds k of it, so a chunk is never kept twice over.
func (v *Vault) settleChunk(store *shardcache.Store, archive string, index, need int) {
	if store.Count(archive, index) < need {
		return
	}
	v.local.mu.Lock()
	other := v.local.partner[archive]
	v.local.mu.Unlock()
	if other == "" {
		return
	}
	for _, part := range store.Parts(other, index) {
		_ = store.Remove(cacheKeyOf(other, index, part))
	}
}

// loadLocalWantedLocked works out what the pins want as the vault opens. The
// caller holds mu.
func (v *Vault) loadLocalWantedLocked() {
	if v.dataKey == nil {
		return
	}
	if _, err := os.Stat(localPinsPath(v.path)); err != nil {
		v.setLocalWanted(pinnedFiles{})
		return
	}
	file, err := readLocalPins(v.path, v.dataKey)
	if err != nil {
		log.Printf("local cache: %v", err)
		return
	}
	v.setLocalWanted(resolvePins(file.Pins, v.manifest, time.Now()))
}

// forgetLocalWanted is called on lock: with no key, nothing can say what the
// pins want, so reads stop keeping what they fetch. The parts on disk stay —
// they are sealed, and keeping them across a lock is the point.
func (v *Vault) forgetLocalWanted() {
	v.local.mu.Lock()
	v.local.wanted, v.local.partner = nil, nil
	v.local.mu.Unlock()
}

func (v *Vault) localWants(archiveID string) bool {
	v.local.mu.Lock()
	defer v.local.mu.Unlock()
	return v.local.wanted[archiveID]
}

// --- the read path ----------------------------------------------------------

// cachedChunkShards opens whatever parts of one chunk this machine holds. A
// part that will not open as the part it is filed as is dropped from the
// cache, and the read goes to the clouds for it as if it had never been here.
func (v *Vault) cachedChunkShards(entry *Entry, index int, dataKey []byte, obs ReadObserver) map[int]*archive.OpenedShard {
	held := map[int]*archive.OpenedShard{}
	store, err := v.localStore(false)
	if err != nil || store == nil {
		return held
	}
	need := entry.Scheme().Data
	for _, part := range store.Parts(entry.ArchiveID, index) {
		if len(held) >= need {
			break
		}
		key := cacheKeyOf(entry.ArchiveID, index, part)
		blob, err := store.Get(key)
		if err != nil {
			continue
		}
		shard := Shard{Part: part, ProviderID: LocalCacheAccountID, ProviderName: "this machine"}
		opened, err := openChunkShard(entry, index, shard, blob, dataKey)
		if err != nil {
			log.Printf("local cache: %s chunk %d part %d is damaged; fetching it again: %v",
				entry.ArchiveID, index, part, err)
			_ = store.Remove(key)
			continue
		}
		held[part] = opened
		notifyRead(obs, entry, ReadEvent{Kind: ReadShardArrived, Chunk: index, Shard: shard})
	}
	return held
}

// keepFetchedShards files the parts a read fetched from the clouds, when the
// file is one a pin wants. Best effort: a full disk costs the copy, never the
// read.
func (v *Vault) keepFetchedShards(entry *Entry, index int, fetched map[int][]byte) {
	if len(fetched) == 0 || !v.localWants(entry.ArchiveID) {
		return
	}
	store, err := v.localStore(true)
	if err != nil {
		return
	}
	for part, blob := range fetched {
		if err := store.Put(cacheKeyOf(entry.ArchiveID, index, part), blob); err != nil {
			return
		}
	}
	v.settleChunk(store, entry.ArchiveID, index, entry.Scheme().Data)
}

// chunkFromKeptReplica rebuilds one chunk of a file from its replica's shards
// on this machine, when that is how the chunk is kept here: the file's own
// parts are not (k of them would have answered first), and its replica's are.
// No cloud is asked. False sends the read on its ordinary way.
func (v *Vault) chunkFromKeptReplica(entry *Entry, index int, obs ReadObserver) ([]byte, bool) {
	store, err := v.localStore(false)
	if err != nil || store == nil || store.Count(entry.ArchiveID, index) >= entry.Scheme().Data {
		return nil, false
	}
	r := keptReplica(entry)
	if r == nil || store.Count(r.ArchiveID, index) < r.Scheme().Data {
		return nil, false
	}
	snap, err := v.snapshotRead(r, provider.Replicas)
	if err != nil {
		return nil, false
	}
	defer crypto.ZeroBytes(snap.dataKey)
	held := v.cachedChunkShards(r, index, snap.dataKey, obs)
	if len(held) < r.Scheme().Data {
		return nil, false
	}
	notifyRead(obs, entry, ReadEvent{Kind: ReadChunkDecrypting, Chunk: index})
	decoded, err := archive.AssembleChunk(heldShards(held))
	if err != nil {
		return nil, false
	}
	return decoded.Data, true
}

// --- what the outside world calls -------------------------------------------

// KeepLocally asks for a file or folder of the main vault to be kept on this
// machine until until, from the placement from names (empty is the parts).
// Asking again for the same path moves its date and its placement.
//
// Keeping a file from its replica when it has none is refused; a folder takes
// whichever of its files have one, and says how many do not.
func (v *Vault) KeepLocally(scope Scope, path string, until time.Time, from ReadSource) (CachePin, error) {
	if !scope.Main() {
		return CachePin{}, ErrCacheSubVault
	}
	from, err := ParseReadSource(string(from))
	if err != nil {
		return CachePin{}, err
	}
	now := time.Now()
	if !until.After(now) {
		return CachePin{}, fmt.Errorf("the date to keep it until has to be in the future")
	}
	var pin CachePin
	_, err = v.editLocalPins(func(file *localPinsFile, m *Manifest) error {
		pin = CachePin{Added: now, Until: until, From: from}
		if e := m.ByPath(path); e != nil {
			if from == ReadReplica && e.Replica == nil {
				return fmt.Errorf("%s: %w", e.Path(), ErrNoReplica)
			}
			pin.Path, pin.EntryID = e.Path(), e.ID
		} else if dir := CleanDir(path); m.FolderExists(dir) {
			pin.Path, pin.Folder = dir, true
		} else {
			return fmt.Errorf("no such file or folder: %s", path)
		}
		for i, old := range file.Pins {
			if old.Folder == pin.Folder && (old.Path == pin.Path || (!pin.Folder && old.EntryID == pin.EntryID)) {
				pin.ID, pin.Added = old.ID, old.Added
				file.Pins[i] = pin
				return nil
			}
		}
		pin.ID = uuid.NewString()[:8]
		file.Pins = append(file.Pins, pin)
		return nil
	})
	if err != nil {
		return CachePin{}, err
	}
	return pin, nil
}

// StopKeeping removes a pin, by its ID or its path, and drops at once the
// parts no other pin still wants.
func (v *Vault) StopKeeping(idOrPath string) error {
	_, err := v.editLocalPins(func(file *localPinsFile, _ *Manifest) error {
		for i, pin := range file.Pins {
			if pin.ID == idOrPath || pin.Path == idOrPath || pin.Path == CleanDir(idOrPath) {
				file.Pins = append(file.Pins[:i], file.Pins[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("%w: %s", ErrNoSuchPin, idOrPath)
	})
	if err != nil {
		return err
	}
	v.sweepLocalCache()
	return nil
}

// SetLocalCacheLimits sets the most the cache may hold (zero for no limit of
// its own) and the free space it leaves on the disk (zero for a tenth of the
// disk, negative for none).
func (v *Vault) SetLocalCacheLimits(limit, reserve int64) error {
	_, err := v.editLocalPins(func(file *localPinsFile, _ *Manifest) error {
		file.Limit, file.Reserve = max(limit, 0), reserve
		return nil
	})
	if err != nil {
		return err
	}
	if store, err := v.localStore(false); err == nil && store != nil {
		store.SetLimits(max(limit, 0), reserve)
	}
	return nil
}

// LocalCacheStatus is what is kept on this machine and how far along it is.
type LocalCacheStatus struct {
	Dir   string           `json:"dir"`
	Usage shardcache.Usage `json:"usage"`
	// Limit and Reserve are as set: zero Reserve is the default.
	Limit   int64            `json:"limit"`
	Reserve int64            `json:"reserve"`
	Pins    []CachePinStatus `json:"pins"`
	// Full is the last fill having stopped for want of room, and Problem what
	// it last could not do.
	Full    bool   `json:"full"`
	Problem string `json:"problem,omitempty"`
}

// CachePinStatus is one pin and how much of it is here.
type CachePinStatus struct {
	CachePin
	// Found is false for a file or folder that is no longer in the vault.
	Found bool `json:"found"`
	// Files is how many files the pin keeps. Unsupported is how many more it
	// cannot, being stored in the format SAND used before chunking, and
	// NoReplica how many it would keep from a replica they do not have.
	Files       int `json:"files"`
	Unsupported int `json:"unsupported"`
	NoReplica   int `json:"no_replica"`
	// Bytes is what keeping all of it takes on disk, and Cached how much of
	// that is here.
	Bytes    int64 `json:"bytes"`
	Cached   int64 `json:"cached"`
	Complete bool  `json:"complete"`
}

// LocalCache reports the pins and the store.
func (v *Vault) LocalCache() (LocalCacheStatus, error) {
	now := time.Now()
	v.mu.RLock()
	if v.dataKey == nil {
		v.mu.RUnlock()
		return LocalCacheStatus{}, ErrLocked
	}
	file, err := readLocalPins(v.path, v.dataKey)
	if err != nil {
		v.mu.RUnlock()
		return LocalCacheStatus{}, err
	}
	resolved := resolvePins(file.Pins, v.manifest, now)
	v.mu.RUnlock()

	status := LocalCacheStatus{
		Dir: localCacheDir(v.path), Limit: file.Limit, Reserve: file.Reserve,
		Pins: []CachePinStatus{},
	}
	// Opened (and made) once anything is kept, so the room it has is known
	// before the first part arrives.
	store, err := v.localStore(len(file.Pins) > 0)
	if err != nil {
		return status, err
	}
	var held map[string]int64
	if store != nil {
		store.SetLimits(file.Limit, file.Reserve)
		status.Usage = store.Usage()
		held = store.Archives()
	}
	v.local.mu.Lock()
	status.Full, status.Problem = v.local.full, v.local.last
	v.local.mu.Unlock()

	for _, pin := range file.Pins {
		if !now.Before(pin.Until) {
			continue
		}
		pin.From = pin.source()
		row := CachePinStatus{CachePin: pin, Found: resolved.found[pin.ID],
			Unsupported: resolved.unsupported[pin.ID], NoReplica: resolved.noReplica[pin.ID],
			Complete: true}
		for _, archive := range resolved.byPin[pin.ID] {
			plan := resolved.plans[archive]
			if plan == nil {
				continue
			}
			row.Files++
			row.Bytes += plan.need()
			row.Cached += plan.here(held)
			if store == nil || !plan.kept(store) {
				row.Complete = false
			}
		}
		status.Pins = append(status.Pins, row)
	}
	sort.Slice(status.Pins, func(i, j int) bool { return status.Pins[i].Path < status.Pins[j].Path })
	return status, nil
}

// cachedBytesNeeded is what keeping a file takes: k of its parts, every part
// of a chunk being the same size.
func cachedBytesNeeded(e *Entry) int64 {
	var largest int64
	for _, s := range e.Shards {
		largest = max(largest, s.Size)
	}
	return largest * int64(e.Scheme().Data)
}

// LocalCacheStep is what one fill step did.
type LocalCacheStep struct {
	// Idle is every pin being fully kept: nothing to fetch.
	Idle bool `json:"idle"`
	// Full is the step having stopped because the cache has no room.
	Full bool `json:"full"`
	// Fetched is the bytes of parts kept by this step, Chunks how many
	// chunks they completed, and Swept the bytes dropped because nothing
	// wants them any more.
	Fetched int64 `json:"fetched"`
	Chunks  int   `json:"chunks"`
	Swept   int64 `json:"swept"`
	// Path is the file the step was filling, if any.
	Path string `json:"path,omitempty"`
}

// FillLocalCache takes one step: it forgets expired pins, drops what nothing
// wants, and fetches up to budget bytes of what the pins still need. Run it
// until it answers Idle or Full.
func (v *Vault) FillLocalCache(ctx context.Context, budget int64) (LocalCacheStep, error) {
	if !v.local.fill.TryLock() {
		return LocalCacheStep{}, ErrCacheBusy
	}
	defer v.local.fill.Unlock()

	var step LocalCacheStep
	resolved, file, err := v.expireLocalPins()
	if err != nil {
		return step, err
	}
	if len(file.Pins) == 0 {
		// Nothing has ever been kept, or everything has expired. Only open
		// the store if one exists, to sweep it.
		step.Swept = v.sweepLocalCache()
		step.Idle = true
		return step, nil
	}

	store, err := v.localStore(true)
	if err != nil {
		return step, err
	}
	store.SetLimits(file.Limit, file.Reserve)
	step.Swept = v.sweepLocalCache()

	// Files in a fixed order, so a fill that keeps running out of room keeps
	// the same files rather than a different half of each every time.
	archives := make([]string, 0, len(resolved.plans))
	for archive := range resolved.plans {
		archives = append(archives, archive)
	}
	sort.Slice(archives, func(i, j int) bool {
		a, b := resolved.plans[archives[i]].file, resolved.plans[archives[j]].file
		if a.Path() != b.Path() {
			return a.Path() < b.Path()
		}
		return a.ArchiveID < b.ArchiveID
	})

	for _, archive := range archives {
		plan := resolved.plans[archive]
		if plan.kept(store) {
			continue
		}
		step.Path = plan.file.Path()
		err := v.fillEntry(ctx, store, plan, budget, &step)
		v.noteFill(step.Full, err)
		if err != nil || step.Full || step.Fetched >= budget {
			return step, err
		}
	}
	step.Idle = step.Fetched == 0
	v.noteFill(false, nil)
	return step, nil
}

func (v *Vault) noteFill(full bool, err error) {
	v.local.mu.Lock()
	defer v.local.mu.Unlock()
	v.local.full = full
	v.local.last = ""
	if full {
		v.local.last = "the local cache is out of room; raise its limit or free some disk"
	}
	if err != nil {
		v.local.last = err.Error()
	}
}

// fillEntry fetches the chunks of one file that are not yet kept here, a
// window of them at a time, until the budget is spent.
//
// Each chunk is asked of the plan's placement — the parts, or the replica
// alone — and k of its shards are kept. Under ReadEither a chunk the parts
// cannot give, too many of their clouds down or emptied, is asked of the
// replica standing in, and k of its shards are kept instead. Never both.
func (v *Vault) fillEntry(ctx context.Context, store *shardcache.Store, plan *keepPlan, budget int64, step *LocalCacheStep) error {
	primary, standby := plan.primary, plan.standby
	snap, err := v.snapshotRead(primary, plan.role)
	if err != nil {
		return err
	}
	defer crypto.ZeroBytes(snap.dataKey)

	// The replica is sealed under its own key generation and lives on the
	// accounts' replica roots, so it has a snapshot of its own. One that
	// cannot be taken leaves the parts to answer alone.
	var standbySnap *readSnapshot
	if standby != nil {
		if rs, err := v.snapshotRead(standby, provider.Replicas); err == nil {
			standbySnap = rs
			defer crypto.ZeroBytes(rs.dataKey)
		}
	}

	primaryHeld := store.Counts(primary.ArchiveID)
	var standbyHeld map[int]int
	if standby != nil {
		standbyHeld = store.Counts(standby.ArchiveID)
	}

	// What one chunk takes here, roughly: k of its shards. Asked of the room
	// before each fetch, so a full cache does not cost a window of downloads
	// that are then thrown away.
	perChunk := plan.need() / int64(max(primary.ChunkCount, 1))
	window := make(chan struct{}, max(v.Tuning().ReadAhead, 1))
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		first error
	)
	stop := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return first != nil || step.Full || step.Fetched >= budget || ctx.Err() != nil
	}

	for index := 0; index < primary.ChunkCount && !stop(); index++ {
		if plan.chunkKept(primaryHeld, standbyHeld, index) {
			continue
		}
		// The key in hand is a copy; a fill must stop with the lock rather
		// than carry on from it, as a reader does.
		if !v.Unlocked() {
			mu.Lock()
			if first == nil {
				first = ErrLocked
			}
			mu.Unlock()
			break
		}
		if store.Usage().Room < perChunk {
			mu.Lock()
			step.Full = true
			mu.Unlock()
			break
		}
		window <- struct{}{}
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			defer func() { <-window }()
			if stop() {
				return
			}
			from := primary
			_, fetched, err := v.raceChunkShards(ctx, primary, index, snap.configs, snap.dataKey, nil)
			if err != nil && standbySnap != nil && ctx.Err() == nil {
				var serr error
				_, fetched, serr = v.raceChunkShards(ctx, standby, index, standbySnap.configs, standbySnap.dataKey, nil)
				if serr == nil {
					from, err = standby, nil
				} else {
					err = fmt.Errorf("%w; nor from its replica: %v", err, serr)
				}
			}
			kept, full, perr := v.keepChunk(store, from, index, fetched, err)
			mu.Lock()
			defer mu.Unlock()
			step.Fetched += kept
			switch {
			case full:
				step.Full = true
			case err == nil && perr == nil:
				step.Chunks++
			}
			if err == nil {
				err = perr
			}
			if err != nil && first == nil && ctx.Err() == nil {
				first = fmt.Errorf("keeping %s: %w", plan.file.Path(), err)
			}
		}(index)
	}
	wg.Wait()
	if first == nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return first
}

// keepChunk files the shards a fill fetched for one chunk of from (the file
// or its replica), then drops whatever the other placement had of the chunk.
func (v *Vault) keepChunk(store *shardcache.Store, from *Entry, index int, fetched map[int][]byte, fetchErr error) (kept int64, full bool, err error) {
	if fetchErr != nil {
		return 0, false, nil
	}
	for part, blob := range fetched {
		perr := store.Put(cacheKeyOf(from.ArchiveID, index, part), blob)
		if errors.Is(perr, shardcache.ErrFull) {
			return kept, true, nil
		}
		if perr != nil {
			return kept, false, perr
		}
		kept += int64(len(blob))
	}
	v.settleChunk(store, from.ArchiveID, index, from.Scheme().Data)
	return kept, false, nil
}

// expireLocalPins drops the pins whose date has passed, writing the list back
// if any were, and resolves what is left. It also refreshes the set reads
// consult.
func (v *Vault) expireLocalPins() (pinnedFiles, *localPinsFile, error) {
	now := time.Now()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dataKey == nil {
		return pinnedFiles{}, nil, ErrLocked
	}
	file, err := readLocalPins(v.path, v.dataKey)
	if err != nil {
		return pinnedFiles{}, nil, err
	}
	kept := file.Pins[:0]
	for _, pin := range file.Pins {
		if now.Before(pin.Until) {
			kept = append(kept, pin)
		}
	}
	if len(kept) != len(file.Pins) {
		file.Pins = kept
		if err := writeLocalPins(v.path, v.dataKey, file); err != nil {
			return pinnedFiles{}, nil, fmt.Errorf("saving the list of files kept on this machine: %w", err)
		}
	}
	resolved := resolvePins(file.Pins, v.manifest, now)
	v.setLocalWanted(resolved)
	return resolved, file, nil
}

// sweepLocalCache drops every archive no unexpired pin wants, and returns the
// bytes that freed. It does nothing while the vault is locked: with no key the
// pins cannot be read, and not knowing what is wanted is not the same as
// wanting nothing.
func (v *Vault) sweepLocalCache() int64 {
	store, err := v.localStore(false)
	if err != nil || store == nil {
		return 0
	}
	v.local.mu.Lock()
	wanted := v.local.wanted
	v.local.mu.Unlock()
	if wanted == nil {
		return 0
	}
	var freed int64
	for archive := range store.Archives() {
		if wanted[archive] {
			continue
		}
		n, err := store.RemoveArchive(archive)
		if err != nil {
			log.Printf("local cache: %v", err)
		}
		freed += n
	}
	return freed
}

// ParseKeepFor reads how long to keep something the way people say it: a
// number of hours, days or weeks ("36h", "7d", "2w"), or anything
// time.ParseDuration takes.
func ParseKeepFor(text string) (time.Duration, error) {
	t := strings.TrimSpace(strings.ToLower(text))
	bad := fmt.Errorf("%q is not a length of time — write one such as 12h, 7d or 2w", text)
	if t == "" {
		return 0, bad
	}
	unit := time.Duration(0)
	switch t[len(t)-1] {
	case 'd':
		unit = 24 * time.Hour
	case 'w':
		unit = 7 * 24 * time.Hour
	}
	var d time.Duration
	if unit > 0 {
		n, err := strconv.ParseFloat(t[:len(t)-1], 64)
		if err != nil {
			return 0, bad
		}
		d = time.Duration(n * float64(unit))
	} else {
		var err error
		if d, err = time.ParseDuration(t); err != nil {
			return 0, bad
		}
	}
	if d <= 0 {
		return 0, bad
	}
	return d, nil
}
