package vault

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chinmay28/SandHarbor/internal/grind"
	"github.com/chinmay28/SandHarbor/internal/grind/grindtest"
	"github.com/chinmay28/SandHarbor/internal/grindbin"
	"github.com/chinmay28/SandHarbor/internal/sftp/sftptest"
)

// testSandVersion is the version the test vaults say they are.
const testSandVersion = "v2026.11.999"

// fakeSand is what the test vaults' own binary holds: small, so copying it is
// cheap, and recognisable, so a test can tell it was what landed.
const fakeSand = "#!fake sand\n"

// grindFixture is a vault with three accounts and one source machine that can
// run commands, set up as the machine says. The machine's home and its source
// folder are real directories; what the vault copies over SFTP lands there.
func grindFixture(t *testing.T, m *grindtest.Machine) (*Vault, string, string, []string) {
	t.Helper()
	v, roots := newTestVault(t, 3)

	exe := filepath.Join(t.TempDir(), "sand")
	os.WriteFile(exe, []byte(fakeSand), 0o755)
	v.grindBins = &grindbin.Store{
		Dir: t.TempDir(), Version: testSandVersion,
		Executable: func() (string, error) { return exe, nil },
	}

	disk := t.TempDir()
	if m.Home == "" {
		m.Home = filepath.Join(disk, "home")
	}
	os.MkdirAll(m.Home, 0o755)
	root := filepath.Join(disk, "share")
	os.MkdirAll(root, 0o755)

	server := sftptest.NewServer(t, disk)
	key, pub := sftptest.NewClientKey(t)
	server.Authorized = pub
	server.Exec = m.Exec
	host, port := server.HostPort(t)
	added, err := v.AddSource(context.Background(), Source{
		Name: "workstation", Host: host, Port: port, User: "sand", PrivateKey: key,
		Root: filepath.ToSlash(root),
	})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	return v, added.ID, root, roots
}

// copiedTo is where the vault puts its sand on a machine of this platform.
func copiedTo(m *grindtest.Machine) string {
	return filepath.Join(m.Home, ".cache", "sand", "grind",
		grindbin.AssetName(testSandVersion, grindbin.Self()))
}

func TestGroundImportStoresFilesThatReadBack(t *testing.T) {
	m := &grindtest.Machine{}
	v, id, root, _ := grindFixture(t, m)
	film := strings.Repeat("a frame of film ", 4000)
	seed(t, filepath.Join(root, "films", "one.mp4"), film)
	seed(t, filepath.Join(root, "notes.txt"), "made on the workstation")
	taken := time.Date(2019, 6, 1, 12, 0, 0, 0, time.UTC)
	touch(t, filepath.Join(root, "notes.txt"), taken)

	v.chunkSize = 4096
	var stages sync.Map
	summary, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
		Paths: []string{"films", "notes.txt"}, Dest: "/in", Grind: true,
		OnProgress: func(p TransferProgress) { stages.Store(p.Stage, true) },
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.Imported != 2 || summary.Failed != 0 {
		t.Fatalf("summary: %+v", summary)
	}
	if got := read(t, v, "/in/films/one.mp4"); got != film {
		t.Fatalf("the film read back as %d bytes, want %d", len(got), len(film))
	}
	if got := read(t, v, "/in/notes.txt"); got != "made on the workstation" {
		t.Fatalf("notes read back as %q", got)
	}
	if entry := v.manifest.ByPath("/in/films/one.mp4"); entry.ChunkCount < 2 {
		t.Fatalf("the film was cut into %d chunks; the test wants several", entry.ChunkCount)
	}
	notes := v.manifest.ByPath("/in/notes.txt")
	if !notes.ModifiedAt.Equal(taken) {
		t.Fatalf("notes are dated %v, want the machine's %v", notes.ModifiedAt, taken)
	}
	if !strings.HasPrefix(notes.MIME, "text/plain") {
		t.Fatalf("notes typed as %q: the head the machine sent was not used", notes.MIME)
	}
	for _, stage := range []TransferStage{StageGrinding, StageScattering} {
		if _, ok := stages.Load(stage); !ok {
			t.Errorf("never reported %s", stage)
		}
	}
	if _, ok := stages.Load(StageFetching); ok {
		t.Error("reported fetching: the file came across whole")
	}

	// The vault's own sand was copied to the machine, private to its owner,
	// and that is what ran — once, for the whole import.
	at := copiedTo(m)
	got, err := os.ReadFile(at)
	if err != nil || string(got) != fakeSand {
		t.Fatalf("sand at %s: %q, %v", at, got, err)
	}
	if info, _ := os.Stat(at); info.Mode().Perm() != 0o700 {
		t.Fatalf("copied sand is %v, want 0700", info.Mode().Perm())
	}
	if ran := m.Ran(); len(ran) != 1 || ran[0] != at {
		t.Fatalf("ran %v, want the copy once", ran)
	}

	// A second import uses the copy that is there rather than copying again.
	os.WriteFile(at, []byte(fakeSand+"kept"), 0o700)
	seed(t, filepath.Join(root, "later.txt"), "later")
	if _, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
		Paths: []string{"later.txt"}, Dest: "/in", Grind: true,
	}); err != nil {
		t.Fatalf("second import: %v", err)
	}
	if got, _ := os.ReadFile(at); string(got) != fakeSand+"kept" {
		t.Fatal("sand was copied again although it was there")
	}
}

// An upgraded vault replaces what an older one put on the machine rather than
// adding a binary beside it.
func TestCopyingSandRemovesOlderCopies(t *testing.T) {
	m := &grindtest.Machine{}
	v, id, root, _ := grindFixture(t, m)
	dir := filepath.Dir(copiedTo(m))
	os.MkdirAll(dir, 0o700)
	old := filepath.Join(dir, "sand-v2026.10.1-linux-amd64")
	os.WriteFile(old, []byte("an older sand"), 0o700)
	mine := filepath.Join(dir, "not-ours.txt")
	os.WriteFile(mine, []byte("somebody's"), 0o600)
	seed(t, filepath.Join(root, "a.txt"), "a")

	if _, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
		Paths: []string{"a.txt"}, Dest: "/", Grind: true,
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("the older copy is still there")
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal("a file that is not a sand was removed")
	}
}

// A copy that is there and does not run as a grinder — cut off, or wrong — is
// replaced, once, and the import goes ahead.
func TestABrokenCopyIsReplaced(t *testing.T) {
	m := &grindtest.Machine{Runnable: func(_ string, content []byte) bool { return string(content) == fakeSand }}
	v, id, root, _ := grindFixture(t, m)
	os.MkdirAll(filepath.Dir(copiedTo(m)), 0o700)
	os.WriteFile(copiedTo(m), []byte("half a sa"), 0o700)
	seed(t, filepath.Join(root, "a.txt"), "a")

	summary, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
		Paths: []string{"a.txt"}, Dest: "/", Grind: true,
	})
	if err != nil || summary.Imported != 1 {
		t.Fatalf("import: %+v, %v", summary, err)
	}
	if got, _ := os.ReadFile(copiedTo(m)); string(got) != fakeSand {
		t.Fatalf("the copy holds %q", got)
	}
}

// A machine with too little disk is not filled: the import is refused before
// anything is copied, and says what it had and what was wanted.
func TestAMachineWithTooLittleDiskIsNotCopiedTo(t *testing.T) {
	m := &grindtest.Machine{FreeDisk: 100 << 20}
	v, id, root, _ := grindFixture(t, m)
	seed(t, filepath.Join(root, "a.txt"), "a")

	_, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
		Paths: []string{"a.txt"}, Dest: "/", Grind: true,
	})
	if !errors.Is(err, ErrCannotGrind) || !strings.Contains(err.Error(), "free") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Dir(copiedTo(m))); !os.IsNotExist(err) {
		t.Fatal("something was written to the machine")
	}
}

// A machine of another platform cannot be given a development build's own
// binary, and there is no release to fetch one from: with nothing installed
// there, the import says exactly that, and the probe says it first.
func TestAnotherPlatformFromADevelopmentBuild(t *testing.T) {
	m := &grindtest.Machine{Arch: grindtest.OtherArch()}
	v, id, root, _ := grindFixture(t, m)
	seed(t, filepath.Join(root, "a.txt"), "a")

	_, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
		Paths: []string{"a.txt"}, Dest: "/", Grind: true,
	})
	if !errors.Is(err, ErrCannotGrind) || !strings.Contains(err.Error(), "development build") {
		t.Fatalf("err = %v", err)
	}
	info, err := v.ProbeGrinder(context.Background(), id)
	if err != nil || info.Available || !strings.Contains(info.Reason, "development build") {
		t.Fatalf("probe: %+v, %v", info, err)
	}
}

// ...but a sand somebody installed there is used.
func TestAnotherPlatformUsesAnInstalledSand(t *testing.T) {
	m := &grindtest.Machine{Arch: grindtest.OtherArch(), Installed: "/usr/local/bin/sand"}
	v, id, root, _ := grindFixture(t, m)
	seed(t, filepath.Join(root, "a.txt"), "a")

	summary, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
		Paths: []string{"a.txt"}, Dest: "/", Grind: true,
	})
	if err != nil || summary.Imported != 1 {
		t.Fatalf("import: %+v, %v", summary, err)
	}
	if ran := m.Ran(); len(ran) != 1 || ran[0] != "/usr/local/bin/sand" {
		t.Fatalf("ran %v", ran)
	}
}

// A release build fetches the other platform's binary from its own release,
// checks it, and copies that.
func TestAnotherPlatformFromARelease(t *testing.T) {
	other, err := grindbin.FromUname("Linux", grindtest.OtherArch())
	if err != nil {
		t.Fatal(err)
	}
	name := grindbin.AssetName(testSandVersion, other)
	const binary = "#!the other platform's sand\n"
	sum := sha256.Sum256([]byte(binary))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + testSandVersion + "/SHA256SUMS":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), name)
		case "/" + testSandVersion + "/" + name:
			io.WriteString(w, binary)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m := &grindtest.Machine{Arch: grindtest.OtherArch()}
	v, id, root, _ := grindFixture(t, m)
	v.grindBins.Released, v.grindBins.Releases = true, srv.URL
	seed(t, filepath.Join(root, "a.txt"), "a")

	info, err := v.ProbeGrinder(context.Background(), id)
	if err != nil || !info.Available || !info.Copies {
		t.Fatalf("probe: %+v, %v", info, err)
	}
	summary, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
		Paths: []string{"a.txt"}, Dest: "/", Grind: true,
	})
	if err != nil || summary.Imported != 1 {
		t.Fatalf("import: %+v, %v", summary, err)
	}
	at := filepath.Join(m.Home, ".cache", "sand", "grind", name)
	if got, _ := os.ReadFile(at); string(got) != binary {
		t.Fatalf("copied %q, want the release's binary", got)
	}
}

// The probe says what an import would do and does none of it.
func TestProbeReportsTheMachineAndChangesNothing(t *testing.T) {
	m := &grindtest.Machine{CPUs: 6}
	v, id, root, _ := grindFixture(t, m)

	info, err := v.ProbeGrinder(context.Background(), id)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !info.Available || !info.Copies || info.CopyBytes != int64(len(fakeSand)) || info.Version != testSandVersion ||
		info.CPUs != 6 || info.Budget == nil || info.Path != filepath.ToSlash(copiedTo(m)) {
		t.Fatalf("probe: %+v", info)
	}
	if _, err := os.Stat(filepath.Dir(copiedTo(m))); !os.IsNotExist(err) {
		t.Fatal("the probe wrote to the machine")
	}
	if m.Runs.Load() != 0 {
		t.Fatal("the probe ran a grinder")
	}

	seed(t, filepath.Join(root, "a.txt"), "a")
	v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{Paths: []string{"a.txt"}, Dest: "/", Grind: true})
	if info, _ := v.ProbeGrinder(context.Background(), id); info.Copies {
		t.Fatal("the probe would copy sand that is already there")
	}
}

// A login that may only speak SFTP cannot be asked anything, and says so.
func TestGroundImportOnAnSFTPOnlyLogin(t *testing.T) {
	v, _ := newTestVault(t, 3)
	src, root := testSource(t, "nas")
	added, err := v.AddSource(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	seed(t, filepath.Join(root, "a.txt"), "a")

	_, err = v.ImportFromSource(context.Background(), MainScope, added.ID, ImportRequest{
		Paths: []string{"a.txt"}, Dest: "/", Grind: true,
	})
	if !errors.Is(err, ErrCannotGrind) || !strings.Contains(err.Error(), "cannot run commands") {
		t.Fatalf("err = %v", err)
	}
	if len(stored(v, "/a.txt")) != 0 {
		t.Fatal("the file was brought in another way")
	}
	info, err := v.ProbeGrinder(context.Background(), added.ID)
	if err != nil || info.Available || !strings.Contains(info.Reason, "cannot run commands") {
		t.Fatalf("probe: %+v, %v", info, err)
	}
}

// A file the grinder refuses fails on its own line, and the conversation goes
// on: the next file is asked of the same grinder.
func TestGroundImportRefusalFailsOnlyThatFile(t *testing.T) {
	var plans atomic.Int32
	m := &grindtest.Machine{Grinder: func(g *grind.Grinder) {
		g.Memory = func() grind.Memory {
			// The Hello reads once; then the first file's plan finds the
			// machine short, and every read after that finds it roomy.
			if plans.Add(1) == 2 {
				return grind.Memory{Available: 1 << 20}
			}
			return grind.Memory{Available: 8 << 30}
		}
	}}
	v, id, root, roots := grindFixture(t, m)
	seed(t, filepath.Join(root, "a.txt"), "first")
	seed(t, filepath.Join(root, "b.txt"), "second")

	summary, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
		Paths: []string{"a.txt", "b.txt"}, Dest: "/", Grind: true,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.Imported != 1 || summary.Failed != 1 || !strings.Contains(summary.Results[0].Error, "free") {
		t.Fatalf("summary: %+v", summary)
	}
	if m.Runs.Load() != 1 {
		t.Fatalf("%d grinders started: a refusal should not end the conversation", m.Runs.Load())
	}
	if got := read(t, v, "/b.txt"); got != "second" {
		t.Fatalf("b.txt = %q", got)
	}
	if objects := storedObjects(t, roots); len(objects) != 3 {
		t.Fatalf("%d objects on the accounts, want the three parts of b.txt", len(objects))
	}
}

// cutOff is a grinder's output that goes dead after a number of bytes, the
// way a machine that crashes or loses its network looks from the vault.
type cutOff struct {
	w    io.Writer
	left int
}

func (c *cutOff) Write(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errors.New("the machine went away")
	}
	if len(p) > c.left {
		n, _ := c.w.Write(p[:c.left])
		c.left = 0
		return n, errors.New("the machine went away")
	}
	c.left -= len(p)
	return c.w.Write(p)
}

// A grinder that dies partway through a file leaves nothing of it behind on
// the accounts, and the next file is asked of a fresh one.
func TestGroundImportCutOffMidFileLeavesNoParts(t *testing.T) {
	var runs atomic.Int32
	m := &grindtest.Machine{Grinder: func(g *grind.Grinder) {
		if runs.Add(1) == 1 {
			// Enough for the introduction, the plan and a couple of chunks.
			g.Out = &cutOff{w: g.Out, left: 12 << 10}
		}
	}}
	v, id, root, roots := grindFixture(t, m)
	v.chunkSize = 2048
	big := make([]byte, 64<<10)
	rand.Read(big)
	os.WriteFile(filepath.Join(root, "a.bin"), big, 0o644)
	seed(t, filepath.Join(root, "b.txt"), "after")

	summary, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
		Paths: []string{"a.bin", "b.txt"}, Dest: "/", Grind: true,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.Failed != 1 || summary.Imported != 1 {
		t.Fatalf("summary: %+v", summary)
	}
	if len(stored(v, "/a.bin")) != 0 {
		t.Fatal("a file cut off partway was recorded")
	}
	if m.Runs.Load() != 2 {
		t.Fatalf("%d grinders started, want a fresh one after the first died", m.Runs.Load())
	}
	if objects := storedObjects(t, roots); len(objects) != 3 {
		t.Fatalf("%d objects on the accounts, want only b.txt's three", len(objects))
	}
}
