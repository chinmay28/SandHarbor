package vault

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/chinmay28/SandHarbor/internal/grind"
	"github.com/chinmay28/SandHarbor/internal/grindbin"
	sandsftp "github.com/chinmay28/SandHarbor/internal/sftp"
	"github.com/chinmay28/SandHarbor/internal/tuning"
	"github.com/chinmay28/SandHarbor/internal/version"
)

// Putting sand on the machine.
//
// Grinding needs sand on the far end, and sand is one static file — no
// libraries, no configuration, no service — so rather than asking anybody to
// install it, the vault copies one there the first time it is needed: this
// process's own executable when the machine is the same platform, and the
// release this build was cut as when it is not (see internal/grindbin). It
// lands in one place, ~/.cache/sand/grind, under a name that says its version
// and platform, is replaced rather than added to when the vault is upgraded,
// and runs from there.
//
// That is the one thing grinding writes outside the folder a machine is
// scoped to, and only when the box is ticked. A sand already installed on the
// machine is used instead when nothing can be copied: a development build has
// no release to fetch another platform's sand from.

// remoteGrindDir is where a copied sand lives, under the login's home.
const remoteGrindDir = ".cache/sand/grind"

// pushMargin is the disk a copy leaves free on the machine beyond the binary
// itself. The machine is somebody's, and filling its home is not ours to do.
const pushMargin = 256 << 20

// probeTimeout bounds the probe: one shell script that reads a few files.
const probeTimeout = 30 * time.Second

// grindBinaries is the store binaries for other machines come from: beside the
// vault, for this build. A test sets v.grindBins to one of its own.
func (v *Vault) grindBinaries() *grindbin.Store {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.grindBins == nil {
		v.grindBins = &grindbin.Store{
			Dir:      filepath.Join(filepath.Dir(v.path), "grind-binaries"),
			Version:  version.String(),
			Released: version.Patch != "0",
		}
	}
	return v.grindBins
}

// grindPlan is what will run on a machine, and what it takes to get it there.
type grindPlan struct {
	facts    grind.Facts
	platform grindbin.Platform

	// path is the sand that will run there.
	path string

	// copies says path is not there yet and will be copied; copyBytes is its
	// size when known before fetching it, zero when not.
	copies    bool
	copyBytes int64

	// installed says path is a sand that was already on the machine.
	installed bool

	// unable is why nothing can run there, when nothing can.
	unable error
}

// planGrind asks the machine what it is and decides what to run there. The
// error is for a machine that could not be asked at all — one whose login may
// not run commands; a machine that answered but cannot grind has unable set.
func (v *Vault) planGrind(ctx context.Context, client *sandsftp.Client) (grindPlan, error) {
	out, err := runProbe(ctx, client)
	if err != nil {
		return grindPlan{}, err
	}
	plan := grindPlan{facts: grind.ParseFacts(out)}
	if plan.facts.Home == "" {
		return grindPlan{}, errors.New("the machine did not say where its home is")
	}

	bins := v.grindBinaries()
	platform, platformErr := grindbin.FromUname(plan.facts.System, plan.facts.Machine)
	plan.platform = platform

	var copyErr error
	switch {
	case platformErr != nil:
		copyErr = platformErr
	default:
		copyErr = bins.CanProvide(platform)
	}

	if copyErr == nil {
		plan.path = path.Join(plan.facts.Home, remoteGrindDir, grindbin.AssetName(bins.Version, platform))
		if _, err := client.SFTP().Stat(plan.path); err == nil {
			return plan, nil
		}
		plan.copies = true
		plan.copyBytes = knownSize(bins, platform)
		if need := max(plan.copyBytes, 40<<20) + pushMargin; plan.facts.FreeDisk >= 0 && plan.facts.FreeDisk < need {
			copyErr = fmt.Errorf("the machine has %s free in %s, and copying sand there needs %s with room to spare",
				tuning.FormatBytes(plan.facts.FreeDisk), plan.facts.Home, tuning.FormatBytes(need))
		} else {
			return plan, nil
		}
	}

	if plan.facts.Installed != "" {
		plan.path, plan.installed, plan.copies, plan.copyBytes = plan.facts.Installed, true, false, 0
		return plan, nil
	}

	plan.path, plan.copies, plan.copyBytes = "", false, 0
	if errors.Is(copyErr, grindbin.ErrNoRelease) {
		copyErr = fmt.Errorf("this vault is a development build for %s and the machine is %s, "+
			"so there is no release to copy a matching sand from", grindbin.Self(), platform)
	}
	plan.unable = fmt.Errorf("%w, and none is installed there", copyErr)
	return plan, nil
}

// knownSize is the size of the binary that would be copied, when it is at hand
// without a download.
func knownSize(bins *grindbin.Store, p grindbin.Platform) int64 {
	if p == bins.Native() {
		if b, err := bins.Get(context.Background(), p); err == nil {
			return b.Size
		}
		return 0
	}
	if info, err := os.Stat(filepath.Join(bins.Dir, grindbin.AssetName(bins.Version, p))); err == nil {
		return info.Size()
	}
	return 0
}

// runProbe runs grind.ProbeScript and answers what it printed.
func runProbe(ctx context.Context, client *sandsftp.Client) (string, error) {
	cmd, err := client.Start(grind.ProbeScript)
	if err != nil {
		return "", err
	}
	defer cmd.Close()
	cmd.Stdin.Close()

	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := io.ReadAll(io.LimitReader(cmd.Stdout, 64<<10))
		if err == nil {
			err = cmd.Wait()
		}
		ch <- result{out, err}
	}()

	timer := time.NewTimer(probeTimeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		if r.err != nil {
			return "", fmt.Errorf("asking the machine what it is: %w", r.err)
		}
		return string(r.out), nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
		return "", fmt.Errorf("the machine did not say what it is within %s", probeTimeout)
	}
}

// copySand puts the binary for the plan's platform at the plan's path: under
// a temporary name first, made executable by its owner only, then renamed
// into place, so a copy cut off partway is never the file that runs. Other
// versions beside it are removed, so an upgraded vault replaces what it put
// there rather than adding to it.
func (v *Vault) copySand(ctx context.Context, client *sandsftp.Client, plan grindPlan) error {
	bin, err := v.grindBinaries().Get(ctx, plan.platform)
	if err != nil {
		return fmt.Errorf("finding a sand for %s: %w", plan.platform, err)
	}
	if free := plan.facts.FreeDisk; free >= 0 && free < bin.Size+pushMargin {
		return fmt.Errorf("the machine has %s free in %s, and copying sand there needs %s with room to spare",
			tuning.FormatBytes(free), plan.facts.Home, tuning.FormatBytes(bin.Size+pushMargin))
	}

	fs := client.SFTP()
	dir := path.Dir(plan.path)
	if err := fs.MkdirAll(dir); err != nil {
		return fmt.Errorf("making %s on the machine: %w", dir, err)
	}
	_ = fs.Chmod(dir, 0o700)

	local, err := os.Open(bin.Path)
	if err != nil {
		return err
	}
	defer local.Close()

	tmp := path.Join(dir, fmt.Sprintf(".copy-%d", time.Now().UnixNano()))
	remote, err := fs.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return fmt.Errorf("copying sand to the machine: %w", err)
	}
	_, err = io.Copy(remote, local)
	if closeErr := remote.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = fs.Chmod(tmp, 0o700)
	}
	if err == nil {
		err = sandsftp.RenameOver(client, tmp, plan.path)
	}
	if err != nil {
		_ = fs.Remove(tmp)
		return fmt.Errorf("copying sand to the machine: %w", err)
	}

	pruneSand(client, dir, path.Base(plan.path))
	return nil
}

// pruneSand removes the copies of sand an older vault left, and copies cut off
// partway long enough ago that nothing is still writing them.
func pruneSand(client *sandsftp.Client, dir, keep string) {
	fs := client.SFTP()
	entries, err := fs.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		stale := strings.HasPrefix(name, "sand-") && name != keep
		abandoned := strings.HasPrefix(name, ".copy-") && time.Since(e.ModTime()) > time.Hour
		if stale || abandoned {
			_ = fs.Remove(path.Join(dir, name))
		}
	}
}

// startGrinder gets a grinder talking on the machine: copying sand there
// first if it needs to be, and copying it again, once, if what was there
// already does not answer as a grinder of this vault's protocol.
func (l *grindLink) startGrinder(ctx context.Context) (*sandsftp.Command, *grind.Session, error) {
	plan, err := l.v.planGrind(ctx, l.client)
	if err != nil {
		return nil, nil, err
	}
	if plan.unable != nil {
		return nil, nil, plan.unable
	}
	if plan.copies {
		if err := l.v.copySand(ctx, l.client, plan); err != nil {
			return nil, nil, err
		}
	}

	cmd, session, err := openGrinder(l.client, plan.path)
	if err == nil || plan.installed || plan.copies {
		return cmd, session, err
	}

	// A copy that was there already and does not answer: cut off by a disk
	// that filled, or put there by hand and wrong. Replaced once.
	_ = l.client.SFTP().Remove(plan.path)
	if copyErr := l.v.copySand(ctx, l.client, plan); copyErr != nil {
		return nil, nil, fmt.Errorf("%w (and copying it again: %v)", err, copyErr)
	}
	return openGrinder(l.client, plan.path)
}

// openGrinder runs the sand at path as a grinder and reads its introduction.
func openGrinder(client *sandsftp.Client, at string) (*sandsftp.Command, *grind.Session, error) {
	cmd, err := client.Start(grind.Command(at))
	if err != nil {
		return nil, nil, err
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
		return nil, nil, grinderFailure(cmd, got.err)
	}
	return cmd, got.session, nil
}

// grinderFailure is the best account of a grinder that did not start: what
// the command said on its way out, when it said anything, and otherwise what
// went wrong in reading it.
func grinderFailure(cmd *sandsftp.Command, err error) error {
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
	}
	cmd.Close()
	if said := strings.TrimSpace(cmd.Stderr()); said != "" && !bytes.Contains([]byte(err.Error()), []byte(said)) {
		return fmt.Errorf("%w: %s", err, said)
	}
	return err
}
