package grind

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const mib = 1 << 20

func TestHeadroomIsTheTighterOfMachineAndCgroup(t *testing.T) {
	cases := []struct {
		m    Memory
		want int64
	}{
		{Memory{Available: 4000}, 4000},
		{Memory{Available: 4000, Limit: 3000, Used: 2500}, 500},
		{Memory{Available: 400, Limit: 3000, Used: 1000}, 400},
		{Memory{Limit: 3000, Used: 1000}, 2000},
		{Memory{Available: 100, Limit: 3000, Used: 5000}, 0},
		{Memory{}, 0},
	}
	for _, c := range cases {
		if got := c.m.Headroom(); got != c.want {
			t.Errorf("%+v: Headroom = %d, want %d", c.m, got, c.want)
		}
	}
}

func TestReadMemoryFromProcAndCgroup(t *testing.T) {
	dir := t.TempDir()
	proc, cg := filepath.Join(dir, "proc"), filepath.Join(dir, "cg")
	os.MkdirAll(proc, 0o755)
	os.MkdirAll(cg, 0o755)
	os.WriteFile(filepath.Join(proc, "meminfo"), []byte(
		"MemTotal:        8000000 kB\nMemFree:          100000 kB\nMemAvailable:    2048 kB\n"), 0o644)
	os.WriteFile(filepath.Join(cg, "memory.max"), []byte("max\n"), 0o644)

	defer func(p, c string) { procRoot, cgroupRoot = p, c }(procRoot, cgroupRoot)
	procRoot, cgroupRoot = proc, cg

	m := ReadMemory()
	if m.Available != 2048<<10 || m.Limit != 0 {
		t.Fatalf("got %+v, want MemAvailable and no ceiling", m)
	}

	os.WriteFile(filepath.Join(cg, "memory.max"), []byte("1048576\n"), 0o644)
	os.WriteFile(filepath.Join(cg, "memory.current"), []byte("524288\n"), 0o644)
	m = ReadMemory()
	if m.Limit != 1048576 || m.Used != 524288 {
		t.Fatalf("got %+v, want the cgroup read", m)
	}

	procRoot, cgroupRoot = filepath.Join(dir, "nowhere"), filepath.Join(dir, "nowhere")
	if m := ReadMemory(); m.Known() {
		t.Fatalf("a machine with neither read as %+v", m)
	}
}

// A roomy machine gets a window, bounded by its cores and by the cap; a
// machine whose half will not hold one chunk is refused, and the refusal says
// what it had and what was wanted.
func TestPlanSizesTheWindowToTheMachine(t *testing.T) {
	const chunk = 16 * mib
	per := perChunkCost(chunk, 2, 3)

	b, err := Plan(Memory{Available: 64 << 30}, 16, chunk, 2, 3)
	if err != nil || b.Window != maxWindow {
		t.Fatalf("a big machine: %+v, %v; want the cap", b, err)
	}

	b, err = Plan(Memory{Available: 64 << 30}, 3, chunk, 2, 3)
	if err != nil || b.Window != 2 {
		t.Fatalf("three cores: %+v, %v; want a core left for the machine", b, err)
	}

	b, err = Plan(Memory{Available: 64 << 30}, 2, chunk, 2, 3)
	if err != nil || b.Window != 1 {
		t.Fatalf("two cores: %+v, %v; want a core left for the machine", b, err)
	}

	// One core has none to spare: it is used, one chunk at a time, rather
	// than the machine being refused.
	b, err = Plan(Memory{Available: 64 << 30}, 1, chunk, 2, 3)
	if err != nil || b.Window != 1 {
		t.Fatalf("one core: %+v, %v; want a window of one", b, err)
	}

	// Room for exactly two chunks in half the headroom.
	free := 2 * (2*per + fixedCost)
	b, err = Plan(Memory{Available: free}, 8, chunk, 2, 3)
	if err != nil || b.Window != 2 {
		t.Fatalf("room for two: %+v, %v", b, err)
	}
	if b.Total > free/headroomShare {
		t.Fatalf("budget %d is over half the headroom %d", b.Total, free)
	}

	_, err = Plan(Memory{Available: 2 * per}, 8, chunk, 2, 3)
	if err == nil || !strings.Contains(err.Error(), "free") {
		t.Fatalf("a small machine: err = %v, want a refusal", err)
	}

	// Known through a cgroup only: the cgroup decides.
	if _, err := Plan(Memory{Limit: 256 * mib, Used: 200 * mib}, 8, chunk, 2, 3); err == nil {
		t.Fatal("a cgroup with 56 MiB left was planned for")
	}
}

func TestPlanForAnUnreadableMachineIsTheSmallestWindow(t *testing.T) {
	b, err := Plan(Memory{}, 32, 16*mib, 2, 3)
	if err != nil || b.Window != 1 {
		t.Fatalf("got %+v, %v; want a window of one", b, err)
	}
}

// A wider code costs more per chunk, because the parts are n/k of the
// compressed chunk whatever n is.
func TestPerChunkCostFollowsTheCode(t *testing.T) {
	if perChunkCost(mib, 2, 3) >= perChunkCost(mib, 2, 6) {
		t.Fatal("2-of-6 was reckoned no dearer than 2-of-3")
	}
}

func TestGateLetsAChunkThroughWhenThereIsRoom(t *testing.T) {
	g := Gate{Read: func() Memory { return Memory{Available: 100} }, Floor: 50, Patience: time.Hour}
	if err := g.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestGateWaitsForMemoryToComeBack(t *testing.T) {
	var reads atomic.Int32
	g := Gate{
		Read: func() Memory {
			if reads.Add(1) < 3 {
				return Memory{Available: 10}
			}
			return Memory{Available: 100}
		},
		Floor: 50, Patience: time.Minute, Poll: time.Millisecond,
	}
	if err := g.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reads.Load() < 3 {
		t.Fatalf("let through after %d reads, before the memory came back", reads.Load())
	}
}

func TestGateGivesUpAfterItsPatience(t *testing.T) {
	g := Gate{
		Read:  func() Memory { return Memory{Available: 10} },
		Floor: 50, Patience: 20 * time.Millisecond, Poll: time.Millisecond,
	}
	err := g.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "short of memory") {
		t.Fatalf("err = %v", err)
	}
}

func TestGateStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := Gate{Read: func() Memory { return Memory{Available: 10} }, Floor: 50, Patience: time.Hour, Poll: time.Hour}
	if err := g.Wait(ctx); err != context.Canceled {
		t.Fatalf("err = %v, want cancelled", err)
	}
}
