// Package grindtest stands in for the far end of grinding in tests: a machine
// that answers the vault's probe and runs a grinder, in this process, for the
// sand binary the vault put on it.
package grindtest

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/chinmay28/SandHarbor/internal/grind"
)

// Machine is an sftptest.Server's Exec. Its home is a real directory, so what
// the vault copies there over SFTP lands where Machine looks for it.
type Machine struct {
	Home string

	// System and Arch are what `uname -s` and `uname -m` say. Empty is Linux
	// on the platform these tests run on.
	System, Arch string

	// FreeDisk is the bytes free in Home; zero reports plenty.
	FreeDisk int64

	// Installed is a sand on PATH, reported by the probe and runnable.
	Installed string

	// Memory and CPUs are the machine's; nil and zero are roomy.
	Memory func() grind.Memory
	CPUs   int

	// Runnable decides whether the file at a path runs as a grinder; nil
	// accepts any file that exists. A rejected one prints what a binary for
	// another machine would: nothing useful.
	Runnable func(path string, content []byte) bool

	// Grinder, when set, adjusts each grinder before it serves.
	Grinder func(*grind.Grinder)

	Probes atomic.Int32
	Runs   atomic.Int32

	mu  sync.Mutex
	ran []string
}

// Ran is the paths the grinders run so far were started from.
func (m *Machine) Ran() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.ran...)
}

// Exec answers one command, as sshd would.
func (m *Machine) Exec(command string, stdin io.Reader, stdout, stderr io.Writer) int {
	if command == grind.ProbeScript {
		m.Probes.Add(1)
		return m.probe(stdout)
	}
	path, ok := grind.CommandPath(command)
	if !ok {
		fmt.Fprintf(stderr, "sh: unexpected command %q\n", command)
		return 127
	}
	content, err := os.ReadFile(path)
	if err != nil && path != m.Installed {
		fmt.Fprintf(stderr, "sh: %s: not found\n", path)
		return 127
	}
	if m.Runnable != nil && !m.Runnable(path, content) {
		fmt.Fprintln(stdout, "\x7fELF: cannot execute binary file")
		return 126
	}

	m.Runs.Add(1)
	m.mu.Lock()
	m.ran = append(m.ran, path)
	m.mu.Unlock()

	g := &grind.Grinder{In: stdin, Out: stdout, Version: "test", CPUs: m.cpus(), Memory: m.memory}
	if m.Grinder != nil {
		m.Grinder(g)
	}
	if err := g.Serve(context.Background()); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func (m *Machine) cpus() int {
	if m.CPUs > 0 {
		return m.CPUs
	}
	return 4
}

func (m *Machine) memory() grind.Memory {
	if m.Memory != nil {
		return m.Memory()
	}
	return grind.Memory{Available: 8 << 30}
}

func (m *Machine) probe(stdout io.Writer) int {
	system, arch := m.System, m.Arch
	if system == "" {
		system = "Linux"
	}
	if arch == "" {
		arch = unameArch()
	}
	free := m.FreeDisk
	if free == 0 {
		free = 100 << 30
	}
	lines := []string{
		"os=" + system,
		"arch=" + arch,
		"home=" + m.Home,
		fmt.Sprintf("df=/dev/test 1 1 %d 1%% /", free>>10),
		fmt.Sprintf("cpus=%d", m.cpus()),
		fmt.Sprintf("mem=MemAvailable: %d kB", m.memory().Available>>10),
	}
	if m.Installed != "" {
		lines = append(lines, "installed="+m.Installed)
	}
	fmt.Fprintln(stdout, strings.Join(lines, "\n"))
	return 0
}
