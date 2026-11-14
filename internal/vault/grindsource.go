package vault

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/chinmay28/SandHarbor/internal/archive"
	"github.com/chinmay28/SandHarbor/internal/compress"
	"github.com/chinmay28/SandHarbor/internal/crypto"
	"github.com/chinmay28/SandHarbor/internal/grind"
	sandsftp "github.com/chinmay28/SandHarbor/internal/sftp"
	"github.com/google/uuid"
)

// Grinding is the other way of bringing a file in from a machine: its parts
// are made on the machine, by `sand grind`, and only the sealed parts cross to
// this one, which checks them and ships them. See internal/grind for the
// protocol and for what the machine is and is not trusted with.
//
// What it is for: a vault on a Raspberry Pi importing from a workstation does
// the cheap half of the work — checking seals, talking to clouds — and leaves
// compressing and coding to the machine with the cores. Nothing of the file is
// spooled here, and its plaintext never crosses the link. What it costs: the
// link carries the parts, which for a code of n over k is n/k times the
// compressed file, so on incompressible media at 2-of-3 it moves half as much
// again as bringing the file across whole would.

// ErrCannotGrind is an import that asked for its parts to be made on the
// machine, from a machine that cannot make them: no sand there, a login that
// may only speak SFTP, a sand of another release. The wrapped error says which.
var ErrCannotGrind = errors.New("could not make parts on the machine")

// GrinderInfo is what a machine said when asked whether it can make parts.
type GrinderInfo struct {
	// Available is whether an import may ask for it. When it is false,
	// Reason says why in words for the person deciding.
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`

	// Version is the sand that would run there: this vault's own when it is
	// copied there or already was, empty for one that was installed by hand.
	Version string       `json:"version,omitempty"`
	OS      string       `json:"os,omitempty"`
	Arch    string       `json:"arch,omitempty"`
	CPUs    int          `json:"cpus,omitempty"`
	Memory  grind.Memory `json:"memory"`

	// Path is where that sand is, or will be, on the machine.
	Path string `json:"path,omitempty"`

	// Copies says sand is not on the machine yet and the first import will
	// copy it there; CopyBytes is how big it is, when known before fetching.
	Copies    bool  `json:"copies,omitempty"`
	CopyBytes int64 `json:"copy_bytes,omitempty"`

	// Installed says what will run is a sand somebody installed there.
	Installed bool `json:"installed,omitempty"`

	// FreeDisk is what the machine has free in its home, -1 when unknown.
	FreeDisk int64 `json:"free_disk"`

	// Budget is what one file would be given there now, at this vault's
	// chunk size and default code: the window and the memory it holds.
	Budget *grind.Budget `json:"budget,omitempty"`
}

// ProbeGrinder asks a source's machine whether it can make parts, and how
// much it has to make them with. It changes nothing there: what it reports is
// what an import would do, including copying sand there first. A machine
// that cannot is an answer, not an error; the error is for a source that does
// not exist or cannot be reached.
func (v *Vault) ProbeGrinder(ctx context.Context, id string) (GrinderInfo, error) {
	client, _, err := v.connectSource(ctx, id)
	if err != nil {
		return GrinderInfo{}, err
	}
	defer client.Close()

	plan, err := v.planGrind(ctx, client)
	if err != nil {
		return GrinderInfo{Reason: err.Error(), FreeDisk: -1}, nil
	}
	facts := plan.facts
	info := GrinderInfo{
		OS: plan.platform.OS, Arch: plan.platform.Arch, CPUs: facts.CPUs, Memory: facts.Memory,
		Path: plan.path, Copies: plan.copies, CopyBytes: plan.copyBytes, Installed: plan.installed,
		FreeDisk: facts.FreeDisk,
	}
	if plan.unable != nil {
		info.Reason = plan.unable.Error()
		return info, nil
	}
	if !plan.installed {
		info.Version = v.grindBinaries().Version
	}

	scheme := v.defaultSchemeForProbe()
	budget, err := grind.Plan(facts.Memory, max(facts.CPUs, 1), v.uploadChunkSize(), scheme.Data, scheme.Total)
	if err != nil {
		info.Reason = err.Error()
		return info, nil
	}
	info.Available, info.Budget = true, &budget
	return info, nil
}

// defaultSchemeForProbe is the code a file is most likely to be cut with, for
// sizing a budget before any file is named: the vault's own default when it
// has one, two of three when not. A wider code costs more per chunk, and the
// import itself sizes each file to the code it is actually given.
func (v *Vault) defaultSchemeForProbe() archive.Scheme {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if s := v.store.DefaultScheme; s != "" {
		if parsed, err := archive.ParseScheme(s); err == nil {
			return parsed
		}
	}
	return archive.SchemeDefault
}

// grindLink is one import's conversation with a machine's grinder, opened
// when the first file needs it and opened again after anything leaves it out
// of step. A file the grinder refuses leaves it in step; a file this side
// gives up on partway does not — the grinder is still sending — and closing
// the command is how that file is stopped.
type grindLink struct {
	v       *Vault
	ctx     context.Context
	client  *sandsftp.Client
	cmd     *sandsftp.Command
	session *grind.Session
}

// openTimeout bounds how long a grinder has to introduce itself. A login that
// runs something else in place of the command — a menu, a shell waiting for
// input — would otherwise hold the import for as long as its context allows.
const openTimeout = 30 * time.Second

func (l *grindLink) get() (*grind.Session, error) {
	if l.session != nil {
		return l.session, nil
	}
	ctx := l.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	cmd, session, err := l.startGrinder(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCannotGrind, err)
	}
	l.cmd, l.session = cmd, session
	return l.session, nil
}

// drop ends the conversation, so the next file opens a fresh one.
func (l *grindLink) drop() {
	if l.cmd != nil {
		l.cmd.Close()
	}
	l.cmd, l.session = nil, nil
}

func (l *grindLink) close() { l.drop() }

// importGround brings one file in by having the machine make its parts.
func (v *Vault) importGround(ctx context.Context, scope Scope, link *grindLink, source Source,
	f importFile, req ImportRequest, report func(TransferStage, int64)) ([]string, error) {

	name, err := SanitizeName(f.name)
	if err != nil {
		return nil, failedAt(UploadPlacing, err)
	}
	dir, err := v.destinationLocked(scope, f.dir)
	if err != nil {
		return nil, failedAt(UploadPlacing, err)
	}

	session, err := link.get()
	if err != nil {
		return nil, failedAt(UploadReading, err)
	}

	placed, ground, err := v.scatterGround(ctx, scope, name, session, source.Root, f.remote,
		spread{preferred: req.Accounts, exact: true, scheme: req.Scheme}, v.uploadChunkSize(), report)
	defer v.releaseDataKey(placed.keyID)
	if err != nil {
		// Out of step unless the grinder itself gave the file up: then it has
		// said so and is waiting for the next one.
		var refused *grind.Refused
		if !errors.As(err, &refused) {
			link.drop()
		}
		return placed.warnings, err
	}

	opts := UploadOptions{
		Overwrite: req.Overwrite, Accounts: req.Accounts, Scheme: req.Scheme, ModifiedAt: f.mod,
	}
	_, warnings, err := v.commitUpload(ctx, scope, dir, name, ground.size, DetectMIME(name, ground.head), placed, opts)
	return warnings, err
}

// groundFile is what the machine said about the file it cut.
type groundFile struct {
	size int64
	head []byte
}

// scatterGround is scatterStream with the chunks made on the machine the file
// is on. The placement, the archive ID and the code are decided here exactly
// as they are for a file cut here; what differs is only where each chunk's
// parts come from — see putChunkWork — and the contract is the same: what it
// returns in shards is really on the accounts, and on a failure it erases what
// it wrote.
func (v *Vault) scatterGround(ctx context.Context, scope Scope, name string, session *grind.Session,
	root, rel string, sp spread, chunkSize uint32, report func(TransferStage, int64)) (placement, groundFile, error) {

	started := time.Now()
	target, err := v.snapshotTarget(scope, sp)
	if err != nil {
		return placement{}, groundFile{}, failedAt(UploadPlacing, err)
	}
	defer crypto.ZeroBytes(target.dataKey)

	out := placement{keyID: target.keyID}

	var archiveID [16]byte
	u := uuid.New()
	copy(archiveID[:], u[:])
	out.archiveID = hex.EncodeToString(archiveID[:])
	defer v.flights.hold(out.archiveID)()

	seed := binary.BigEndian.Uint64(archiveID[:8])
	plan, scheme, err := target.planFor(seed)
	if err != nil {
		return out, groundFile{}, failedAt(UploadPlacing, err)
	}
	out.scheme = scheme

	say := func(stage TransferStage, done int64) {
		if report != nil {
			report(stage, done)
		}
	}
	say(StageGrinding, 0)

	file, err := session.Begin(grind.Job{
		Root: root, Path: rel, ArchiveID: out.archiveID, Filename: name,
		ChunkSize: chunkSize, Data: scheme.Data, Total: scheme.Total,
		Level: compress.Current().Level,
	}, func(done int64) { say(StageGrinding, done) })
	if err != nil {
		return out, groundFile{}, failedAt(UploadReading, err)
	}
	defer file.Close()

	ground := groundFile{size: file.Size, head: file.Head}
	out.originalHash = file.Hash
	chunks := file.Plan()
	out.chunkSize = int64(chunks.ChunkSize)
	out.chunkCount = int(chunks.ChunkCount)

	if err := file.SendKeys(target.dataKey); err != nil {
		return out, ground, failedAt(UploadReading, err)
	}

	say(StageScattering, 0)
	arrived := uint32(0)
	written, failures, err := v.putChunkWork(ctx, target, plan, chunks, func() (chunkWork, error) {
		chunk, err := file.Next()
		if err != nil {
			return nil, failedAt(UploadReading, err)
		}
		arrived++
		say(StageScattering, min(int64(arrived)*int64(chunks.ChunkSize), file.Size))
		return func() (*archive.EncodedChunk, error) { return chunk, nil }, nil
	})
	if err == nil {
		// Only now is it known that what arrived is the file: the grinder
		// read it a second time while cutting it, and says here whether the
		// two readings agreed.
		if finish := file.Finish(); finish != nil {
			err = failedAt(UploadReading, finish)
		}
	}
	if err != nil {
		v.eraseChunks(context.WithoutCancel(ctx), target, plan, out.archiveID, chunks.ChunkCount)
		return out, ground, failedAt(UploadScattering, err)
	}

	for shard, failure := range failures {
		out.warnings = append(out.warnings, failure.String())
		v.eraseShardChunks(context.WithoutCancel(ctx), target, plan, out.archiveID, shard, chunks.ChunkCount)
	}
	out.shards = written
	sortShards(out.shards)

	if distinctShards(out.shards) < scheme.Data {
		v.eraseChunks(context.WithoutCancel(ctx), target, plan, out.archiveID, chunks.ChunkCount)
		err := shortOfShards(distinctShards(out.shards), scheme.Data, scheme.Total, failures)
		out.shards = nil
		return out, ground, err
	}

	v.reads.loaded(kindOfName(name), loadStore, file.Size, time.Since(started))
	return out, ground, nil
}
