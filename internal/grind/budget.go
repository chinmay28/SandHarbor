package grind

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chinmay28/SandHarbor/internal/tuning"
)

// Memory is what the machine has free right now. A zero field is one that
// could not be read.
type Memory struct {
	// Available is the kernel's MemAvailable: what can be had without
	// swapping, page cache it would drop included.
	Available int64 `json:"available"`

	// Limit and Used are the cgroup's ceiling and its current use, when the
	// grinder runs inside one with a ceiling — a systemd user slice with
	// MemoryMax, a container. Whichever of the two is tighter is the one that
	// counts.
	Limit int64 `json:"limit,omitempty"`
	Used  int64 `json:"used,omitempty"`
}

// Headroom is how much more this process could use before something gives:
// the smaller of what the machine has free and what its cgroup has left.
// Zero means it could not be read at all.
func (m Memory) Headroom() int64 {
	free := m.Available
	if m.Limit > 0 {
		left := max(m.Limit-m.Used, 0)
		if free == 0 || left < free {
			free = left
		}
	}
	return free
}

// Known reports whether anything could be read.
func (m Memory) Known() bool { return m.Available > 0 || m.Limit > 0 }

// procRoot and cgroupRoot are variables so a test can point them elsewhere.
var (
	procRoot   = "/proc"
	cgroupRoot = "/sys/fs/cgroup"
)

// ReadMemory reads what this process has free, from /proc and cgroup v2.
// Where neither exists — macOS, BSD — it answers the zero Memory, which Plan
// treats as "be as small as possible" rather than as "unlimited".
func ReadMemory() Memory {
	var m Memory
	if kb, ok := meminfoField(procRoot+"/meminfo", "MemAvailable:"); ok {
		m.Available = kb << 10
	}
	if limit, ok := readInt(cgroupRoot + "/memory.max"); ok && limit > 0 {
		m.Limit = limit
		if used, ok := readInt(cgroupRoot + "/memory.current"); ok {
			m.Used = used
		}
	}
	return m
}

func meminfoField(path, field string) (int64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, field) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		return kb, err == nil && kb > 0
	}
	return 0, false
}

// readInt reads a file holding one integer. "max" and anything else that is
// not a number read as absent.
func readInt(path string) (int64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return n, err == nil
}

// Budget is how hard a grinder may work on one file: how many chunks it holds
// at once, and what that costs.
type Budget struct {
	// Window is how many chunks are read, compressed, coded and sealed at
	// once. Never less than one.
	Window int `json:"window"`

	// PerChunk is what one chunk in flight is reckoned to cost.
	PerChunk int64 `json:"per_chunk"`

	// Total is Window × PerChunk: the most the file's work should hold.
	Total int64 `json:"total"`

	// Floor is the headroom below which the next chunk waits rather than
	// starts. See Gate.
	Floor int64 `json:"floor"`
}

const (
	// headroomShare is the fraction of the machine's free memory one file's
	// work may take. Half: the machine is somebody's, it is doing other
	// things, and what was free when the file started is not a promise about
	// the next minute.
	headroomShare = 2

	// maxWindow is the most chunks a grinder holds whatever the machine has.
	// Past a handful, a wider window buys nothing the network and the
	// clouds on the vault's side can take.
	maxWindow = 4

	// fixedCost is the grinder's own floor: the runtime, the buffers that are
	// not a chunk's, the checksum pass.
	fixedCost = 32 << 20
)

// perChunkCost is what one chunk in flight holds: its plaintext, the
// compressed copy, the coded shards and the sealed parts made of them — the
// last two n/k times the compressed size each — and a compressor's working
// state, which for a chunk of this size is about twice the chunk again.
func perChunkCost(chunkSize uint32, data, total int) int64 {
	c := int64(chunkSize)
	coded := c * int64(total) / int64(max(data, 1))
	return c + c + 2*coded + 2*c
}

// Plan sizes one file's work to what the machine has free, or says why the
// machine has too little to do it at all.
//
// The rule is that one file's work takes at most half of the headroom, and
// that the next chunk will not start once the headroom has fallen below what
// a chunk costs plus the fixed cost (see Gate). A machine whose half will not
// hold one chunk is refused rather than tried: a refusal is a sentence in the
// import's summary, and the alternative is the machine's own processes being
// killed for memory.
//
// A machine whose memory cannot be read at all gets a window of one, which is
// the least a grinder can do and is the same cost the vault itself pays on a
// Raspberry Pi for every chunk it uploads.
func Plan(m Memory, cpus int, chunkSize uint32, data, total int) (Budget, error) {
	per := perChunkCost(chunkSize, data, total)
	b := Budget{Window: 1, PerChunk: per, Floor: per + fixedCost}

	if m.Known() {
		share := m.Headroom() / headroomShare
		if share < per+fixedCost {
			return Budget{}, fmt.Errorf(
				"the machine has %s free, and making the parts there needs %s for one chunk "+
					"without taking more than half of it — bring the files in without "+
					"\"make the parts on the machine\"",
				tuning.FormatBytes(m.Headroom()), tuning.FormatBytes(2*(per+fixedCost)))
		}
		b.Window = int((share - fixedCost) / per)
	}

	// A core is left for the machine's own work whenever there is one to
	// leave, and the window never asks for more cores than that.
	b.Window = min(b.Window, max(cpus-1, 1), maxWindow)
	b.Window = max(b.Window, 1)
	b.Total = int64(b.Window)*per + fixedCost
	return b, nil
}

// Gate holds the next chunk back while the machine is short of memory.
//
// Plan decides the window once, from the headroom when a file starts. A
// machine's memory is not the grinder's to plan around, though: somebody can
// open a browser halfway through a film. So before each chunk the headroom is
// read again, and a chunk that would push it below the floor waits for it to
// recover. A machine that stays short for Patience has the file refused
// rather than pushed into swap — the next run picks it up.
type Gate struct {
	Read     func() Memory
	Floor    int64
	Patience time.Duration
	Poll     time.Duration
}

// Wait returns once there is room for a chunk, or an error saying how long it
// waited and for what. A Gate with no Read, or one that cannot read anything,
// lets every chunk through: Plan has already kept such a machine to a window
// of one.
func (g Gate) Wait(ctx context.Context) error {
	if g.Read == nil {
		return nil
	}
	m := g.Read()
	if !m.Known() || m.Headroom() >= g.Floor {
		return nil
	}

	poll := g.Poll
	if poll <= 0 {
		poll = time.Second
	}
	deadline := time.NewTimer(g.Patience)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf(
				"the machine stayed short of memory for %s (%s free, %s wanted for the next part of the file), "+
					"so this file was left for another run",
				g.Patience, tuning.FormatBytes(m.Headroom()), tuning.FormatBytes(g.Floor))
		case <-tick.C:
			m = g.Read()
			if !m.Known() || m.Headroom() >= g.Floor {
				return nil
			}
		}
	}
}
