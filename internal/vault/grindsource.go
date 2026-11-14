package vault

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
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

// grindCommand runs the grinder on the far end, wherever sand is installed
// there. It is a POSIX sh script run through sh, so a login whose shell is
// something else — fish, a NAS's own — still runs it, and it looks in the
// places an install puts sand before giving up and saying so.
const grindCommand = `sh -c 'for s in sand /opt/sand/bin/sand /usr/local/bin/sand "$HOME/.local/bin/sand" "$HOME/bin/sand"; do ` +
	`if command -v "$s" >/dev/null 2>&1; then exec "$s" grind; fi; done; ` +
	`echo "sand is not installed on this machine (looked on PATH, in /opt/sand/bin, /usr/local/bin, ~/.local/bin and ~/bin)" >&2; exit 127'`

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

	Version string       `json:"version,omitempty"`
	OS      string       `json:"os,omitempty"`
	Arch    string       `json:"arch,omitempty"`
	CPUs    int          `json:"cpus,omitempty"`
	Memory  grind.Memory `json:"memory"`

	// Budget is what one file would be given there now, at this vault's
	// chunk size and default code: the window and the memory it holds.
	Budget *grind.Budget `json:"budget,omitempty"`
}

// ProbeGrinder asks a source's machine whether it can make parts, and how
// much it has to make them with. A machine that cannot is an answer, not an
// error; the error is for a source that does not exist or cannot be reached.
func (v *Vault) ProbeGrinder(ctx context.Context, id string) (GrinderInfo, error) {
	client, _, err := v.connectSource(ctx, id)
	if err != nil {
		return GrinderInfo{}, err
	}
	defer client.Close()

	link := &grindLink{client: client}
	defer link.close()
	session, err := link.get()
	if err != nil {
		return GrinderInfo{Reason: strings.TrimPrefix(err.Error(), ErrCannotGrind.Error()+": ")}, nil
	}

	hello := session.Hello()
	info := GrinderInfo{
		Available: true,
		Version:   hello.Version, OS: hello.OS, Arch: hello.Arch, CPUs: hello.CPUs, Memory: hello.Memory,
	}
	scheme := v.defaultSchemeForProbe()
	budget, err := grind.Plan(hello.Memory, hello.CPUs, v.uploadChunkSize(), scheme.Data, scheme.Total)
	if err != nil {
		info.Available, info.Reason = false, err.Error()
		return info, nil
	}
	info.Budget = &budget
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
	cmd, err := l.client.Start(grindCommand)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCannotGrind, err)
	}

	type opened struct {
		session *grind.Session
		err     error
	}
	ch := make(chan opened, 1)
	go func() {
		s, err := grind.Open(cmd.Stdout, cmd.Stdin)
		ch <- opened{s, err}
	}()

	var got opened
	select {
	case got = <-ch:
	case <-time.After(openTimeout):
		cmd.Close()
		got = <-ch
		got.err = fmt.Errorf("sand grind did not answer within %s", openTimeout)
	}
	if got.err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCannotGrind, l.failure(cmd, got.err))
	}
	l.cmd, l.session = cmd, got.session
	return l.session, nil
}

// failure is the best account of a grinder that did not start: what the
// command said on its way out, when it said anything, and otherwise what
// went wrong in reading it.
func (l *grindLink) failure(cmd *sandsftp.Command, err error) error {
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case waitErr := <-exited:
		if said := cmd.Stderr(); said != "" && waitErr != nil {
			cmd.Close()
			return errors.New(said)
		}
	case <-time.After(2 * time.Second):
	}
	cmd.Close()
	if said := cmd.Stderr(); said != "" {
		return errors.New(said)
	}
	return err
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
