package vault

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chinmay28/SandHarbor/internal/grind"
	"github.com/chinmay28/SandHarbor/internal/sftp/sftptest"
)

// grindMachine is a source machine with sand on it: an sshd whose exec runs
// a real grinder on the command's streams, in this process.
type grindMachine struct {
	root string

	// grinder, when set, makes the grinder for each run; the default is one
	// with room to spare.
	grinder func(stdin io.Reader, stdout io.Writer) *grind.Grinder

	// runs counts the commands started, which is how many conversations an
	// import opened.
	runs atomic.Int32

	mu       sync.Mutex
	commands []string
}

// grindFixture is importFixture with a machine that can make parts.
func grindFixture(t *testing.T, m *grindMachine) (*Vault, string, []string) {
	t.Helper()
	v, roots := newTestVault(t, 3)

	disk := t.TempDir()
	server := sftptest.NewServer(t, disk)
	key, pub := sftptest.NewClientKey(t)
	server.Authorized = pub
	m.root = filepath.Join(disk, "share")
	os.MkdirAll(m.root, 0o755)

	server.Exec = func(command string, stdin io.Reader, stdout, stderr io.Writer) int {
		m.runs.Add(1)
		m.mu.Lock()
		m.commands = append(m.commands, command)
		m.mu.Unlock()

		g := &grind.Grinder{In: stdin, Out: stdout, Version: "test", CPUs: 4,
			Memory: func() grind.Memory { return grind.Memory{Available: 8 << 30} }}
		if m.grinder != nil {
			g = m.grinder(stdin, stdout)
		}
		if err := g.Serve(context.Background()); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}

	host, port := server.HostPort(t)
	added, err := v.AddSource(context.Background(), Source{
		Name: "workstation", Host: host, Port: port, User: "sand", PrivateKey: key,
		Root: filepath.ToSlash(m.root),
	})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	return v, added.ID, roots
}

func TestGroundImportStoresFilesThatReadBack(t *testing.T) {
	m := &grindMachine{}
	v, id, _ := grindFixture(t, m)
	film := strings.Repeat("a frame of film ", 4000)
	seed(t, filepath.Join(m.root, "films", "one.mp4"), film)
	seed(t, filepath.Join(m.root, "notes.txt"), "made on the workstation")
	taken := time.Date(2019, 6, 1, 12, 0, 0, 0, time.UTC)
	touch(t, filepath.Join(m.root, "notes.txt"), taken)

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

	entry := v.manifest.ByPath("/in/films/one.mp4")
	if entry.ChunkCount < 2 {
		t.Fatalf("the film was cut into %d chunks; the test wants several", entry.ChunkCount)
	}
	if notes := v.manifest.ByPath("/in/notes.txt"); !notes.ModifiedAt.Equal(taken) {
		t.Fatalf("notes are dated %v, want the machine's %v", notes.ModifiedAt, taken)
	}
	if !strings.HasPrefix(v.manifest.ByPath("/in/notes.txt").MIME, "text/plain") {
		t.Fatalf("notes typed as %q: the head the machine sent was not used", v.manifest.ByPath("/in/notes.txt").MIME)
	}
	for _, stage := range []TransferStage{StageGrinding, StageScattering} {
		if _, ok := stages.Load(stage); !ok {
			t.Errorf("never reported %s", stage)
		}
	}
	if _, ok := stages.Load(StageFetching); ok {
		t.Error("reported fetching: the file came across whole")
	}

	// One conversation for the whole import, and the command found sand by
	// asking for it.
	if m.runs.Load() != 1 {
		t.Fatalf("%d grinders started for one import", m.runs.Load())
	}
	if !strings.Contains(m.commands[0], " grind") {
		t.Fatalf("ran %q", m.commands[0])
	}

	// A re-run skips what is there, exactly as a plain import does.
	again, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
		Paths: []string{"films", "notes.txt"}, Dest: "/in", Grind: true,
	})
	if err != nil || again.Skipped != 2 || again.Imported != 0 {
		t.Fatalf("re-run: %+v, %v", again, err)
	}
}

// A machine without sand is told apart from a broken one: the import is
// refused up front with the machine's own words, and the probe says the same
// without it being an error.
func TestGroundImportOnAMachineWithoutSand(t *testing.T) {
	v, _ := newTestVault(t, 3)
	disk := t.TempDir()
	server := sftptest.NewServer(t, disk)
	key, pub := sftptest.NewClientKey(t)
	server.Authorized = pub
	server.Exec = func(_ string, _ io.Reader, _, stderr io.Writer) int {
		fmt.Fprintln(stderr, "sand is not installed on this machine")
		return 127
	}
	host, port := server.HostPort(t)
	os.MkdirAll(filepath.Join(disk, "share"), 0o755)
	seed(t, filepath.Join(disk, "share", "a.txt"), "a")
	added, err := v.AddSource(context.Background(), Source{
		Name: "nas", Host: host, Port: port, User: "sand", PrivateKey: key,
		Root: filepath.ToSlash(filepath.Join(disk, "share")),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = v.ImportFromSource(context.Background(), MainScope, added.ID, ImportRequest{
		Paths: []string{"a.txt"}, Dest: "/", Grind: true,
	})
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("err = %v, want the machine's own reason", err)
	}
	if len(stored(v, "/a.txt")) != 0 {
		t.Fatal("the file was brought in another way")
	}

	info, err := v.ProbeGrinder(context.Background(), added.ID)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if info.Available || !strings.Contains(info.Reason, "not installed") {
		t.Fatalf("probe: %+v", info)
	}
}

func TestProbeReportsTheMachineAndItsBudget(t *testing.T) {
	m := &grindMachine{}
	v, id, _ := grindFixture(t, m)

	info, err := v.ProbeGrinder(context.Background(), id)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !info.Available || info.Version != "test" || info.CPUs != 4 || info.Budget == nil || info.Budget.Window < 1 {
		t.Fatalf("probe: %+v", info)
	}
}

// A file the grinder refuses fails on its own line, and the conversation goes
// on: the next file is asked of the same grinder.
func TestGroundImportRefusalFailsOnlyThatFile(t *testing.T) {
	m := &grindMachine{}
	var plans atomic.Int32
	m.grinder = func(stdin io.Reader, stdout io.Writer) *grind.Grinder {
		return &grind.Grinder{In: stdin, Out: stdout, CPUs: 4, Memory: func() grind.Memory {
			// The Hello reads once; then the first file's plan finds the
			// machine short, and every read after that finds it roomy.
			if plans.Add(1) == 2 {
				return grind.Memory{Available: 1 << 20}
			}
			return grind.Memory{Available: 8 << 30}
		}}
	}
	v, id, roots := grindFixture(t, m)
	seed(t, filepath.Join(m.root, "a.txt"), "first")
	seed(t, filepath.Join(m.root, "b.txt"), "second")

	summary, err := v.ImportFromSource(context.Background(), MainScope, id, ImportRequest{
		Paths: []string{"a.txt", "b.txt"}, Dest: "/", Grind: true,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.Imported != 1 || summary.Failed != 1 {
		t.Fatalf("summary: %+v", summary)
	}
	if !strings.Contains(summary.Results[0].Error, "free") {
		t.Fatalf("the failure says %q", summary.Results[0].Error)
	}
	if m.runs.Load() != 1 {
		t.Fatalf("%d grinders started: a refusal should not end the conversation", m.runs.Load())
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
	m := &grindMachine{}
	var runs atomic.Int32
	m.grinder = func(stdin io.Reader, stdout io.Writer) *grind.Grinder {
		out := stdout
		if runs.Add(1) == 1 {
			// Enough for the introduction, the plan and a couple of chunks.
			out = &cutOff{w: stdout, left: 12 << 10}
		}
		return &grind.Grinder{In: stdin, Out: out, CPUs: 4,
			Memory: func() grind.Memory { return grind.Memory{Available: 8 << 30} }}
	}
	v, id, roots := grindFixture(t, m)
	v.chunkSize = 2048
	// Incompressible, so its parts are well past where the output dies.
	big := make([]byte, 64<<10)
	rand.Read(big)
	os.WriteFile(filepath.Join(m.root, "a.bin"), big, 0o644)
	seed(t, filepath.Join(m.root, "b.txt"), "after")

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
	if m.runs.Load() != 2 {
		t.Fatalf("%d grinders started, want a fresh one after the first died", m.runs.Load())
	}
	if objects := storedObjects(t, roots); len(objects) != 3 {
		t.Fatalf("%d objects on the accounts, want only b.txt's three", len(objects))
	}
}
