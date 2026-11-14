package grind

import (
	"os/exec"
	"strings"
	"testing"
)

func TestParseFacts(t *testing.T) {
	f := ParseFacts(strings.Join([]string{
		"os=Linux",
		"arch=x86_64",
		"home=/home/me",
		"df=/dev/vda1 41152736 9000000 30000000 24% /",
		"cpus=8",
		"mem=MemAvailable:    2048 kB",
		"cgmax=max",
		"cgcur=12345",
		"installed=/usr/local/bin/sand",
		"noise without an equals sign",
	}, "\n"))
	want := Facts{System: "Linux", Machine: "x86_64", Home: "/home/me", FreeDisk: 30000000 << 10,
		CPUs: 8, Memory: Memory{Available: 2048 << 10}, Installed: "/usr/local/bin/sand"}
	if f != want {
		t.Fatalf("got %+v\nwant %+v", f, want)
	}
}

// A machine that answers little — macOS, a busybox box — reads as unknowns,
// not as zeros that would be mistaken for an empty disk.
func TestParseFactsOfASparseMachine(t *testing.T) {
	f := ParseFacts("os=Darwin\narch=arm64\nhome=/Users/me\ndf=\ncpus=\nmem=\n")
	if f.FreeDisk != -1 || f.Memory.Known() || f.CPUs != 0 || f.Installed != "" {
		t.Fatalf("got %+v", f)
	}
}

// The script is run for real here, by this machine's sh, so a quoting slip
// in it fails a test rather than every machine.
func TestProbeScriptRunsUnderSh(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	out, err := exec.Command("sh", "-c", ProbeScript).Output()
	if err != nil {
		t.Fatalf("running the probe: %v", err)
	}
	f := ParseFacts(string(out))
	if f.System == "" || f.Machine == "" || f.Home == "" {
		t.Fatalf("the probe said %q", out)
	}
}

func TestCommandQuotesThePath(t *testing.T) {
	for _, path := range []string{"/home/me/.cache/sand/grind/sand-v1-linux-amd64", "/home/it's me/sand"} {
		got, ok := CommandPath(Command(path))
		if !ok || got != path {
			t.Errorf("%q came back as %q, %v", path, got, ok)
		}
	}
	if _, ok := CommandPath(ProbeScript); ok {
		t.Error("the probe was read as a grind command")
	}
	out, err := exec.Command("sh", "-c", strings.Replace(Command("/x/it's here"), "exec ", "printf %s ", 1)).Output()
	if err == nil && string(out) != "/x/it's heregrind" {
		t.Errorf("sh read the quoted path as %q", out)
	}
}
