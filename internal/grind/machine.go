package grind

import (
	"bufio"
	"strconv"
	"strings"
)

// ProbeScript is what the vault runs on a machine before grinding there, to
// learn what to run and whether the machine can take it: its platform, its
// home, the disk free there, its cores and memory, and any sand already
// installed. One round trip, plain POSIX sh run through sh so a login whose
// shell is something else still runs it, and nothing written anywhere.
//
// Each line is key=value; ParseFacts reads them. A line whose command does not
// exist on the machine is simply empty.
const ProbeScript = `sh -c '` +
	`echo "os=$(uname -s 2>/dev/null)"; ` +
	`echo "arch=$(uname -m 2>/dev/null)"; ` +
	`echo "home=$HOME"; ` +
	`echo "df=$(df -Pk "$HOME" 2>/dev/null | tail -n 1)"; ` +
	`echo "cpus=$(getconf _NPROCESSORS_ONLN 2>/dev/null || nproc 2>/dev/null)"; ` +
	`echo "mem=$(grep MemAvailable: /proc/meminfo 2>/dev/null)"; ` +
	`echo "cgmax=$(cat /sys/fs/cgroup/memory.max 2>/dev/null)"; ` +
	`echo "cgcur=$(cat /sys/fs/cgroup/memory.current 2>/dev/null)"; ` +
	`for s in sand /opt/sand/bin/sand /usr/local/bin/sand "$HOME/.local/bin/sand" "$HOME/bin/sand"; do ` +
	`if command -v "$s" >/dev/null 2>&1; then echo "installed=$(command -v "$s")"; break; fi; done'`

// Facts is what ProbeScript found.
type Facts struct {
	// System and Machine are `uname -s` and `uname -m`, as the machine said them.
	System  string
	Machine string

	Home string

	// FreeDisk is the bytes free on the filesystem holding Home, or -1 when it
	// could not be read.
	FreeDisk int64

	CPUs   int
	Memory Memory

	// Installed is a sand found on the machine, or empty.
	Installed string
}

// ParseFacts reads ProbeScript's output.
func ParseFacts(out string) Facts {
	f := Facts{FreeDisk: -1}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "os":
			f.System = value
		case "arch":
			f.Machine = value
		case "home":
			f.Home = value
		case "df":
			// Filesystem 1024-blocks Used Available Capacity Mounted-on
			if fields := strings.Fields(value); len(fields) >= 4 {
				if kb, err := strconv.ParseInt(fields[3], 10, 64); err == nil && kb >= 0 {
					f.FreeDisk = kb << 10
				}
			}
		case "cpus":
			f.CPUs, _ = strconv.Atoi(value)
		case "mem":
			if fields := strings.Fields(value); len(fields) >= 2 {
				if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil && kb > 0 {
					f.Memory.Available = kb << 10
				}
			}
		case "cgmax":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
				f.Memory.Limit = n
			}
		case "cgcur":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
				f.Memory.Used = n
			}
		case "installed":
			f.Installed = value
		}
	}
	if f.Memory.Limit == 0 {
		f.Memory.Used = 0
	}
	return f
}

// Command is the command line that runs the grinder at path on the far end.
// The path is quoted for sh, so a home with a space in it is one argument.
func Command(path string) string {
	return "exec " + ShellQuote(path) + " grind"
}

// ShellQuote quotes s as one sh word.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// CommandPath reads back the path a Command line runs, for a test standing in
// for the far end. ok is false for anything that is not a Command line.
func CommandPath(command string) (string, bool) {
	rest, ok := strings.CutPrefix(command, "exec ")
	if !ok {
		return "", false
	}
	quoted, ok := strings.CutSuffix(rest, " grind")
	if !ok || len(quoted) < 2 || quoted[0] != '\'' || quoted[len(quoted)-1] != '\'' {
		return "", false
	}
	return strings.ReplaceAll(quoted[1:len(quoted)-1], `'\''`, "'"), true
}
