// Package grindbin finds a sand binary to run on another machine: the one
// making an import's parts where the files are (see internal/grind).
//
// sand is one static executable with nothing beside it — no CGO, no shared
// libraries, no data files — so "sand on the machine" means one file there and
// not an installation. The question this package answers is only where that
// file comes from for a machine of a given OS and architecture:
//
//   - this process's own executable, when the machine is the same platform;
//   - otherwise the release this build was cut as, downloaded once, checked
//     against the release's SHA256SUMS, and kept beside the vault.
//
// A development build has no release, so for another platform it has nothing
// to offer; the caller falls back to a sand already on the machine.
package grindbin

import (
	"fmt"
	"runtime"
	"strings"
)

// Platform is an OS and architecture in Go's own names.
type Platform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

func (p Platform) String() string { return p.OS + "/" + p.Arch }

// Self is the platform this process was built for.
func Self() Platform { return Platform{OS: runtime.GOOS, Arch: runtime.GOARCH} }

// FromUname reads `uname -s` and `uname -m` as a Platform, for the platforms
// sand is released for. Anything else is refused by name rather than guessed
// at: a binary for the wrong machine fails with "exec format error", which
// explains nothing to whoever reads it.
func FromUname(system, machine string) (Platform, error) {
	var p Platform
	switch strings.ToLower(strings.TrimSpace(system)) {
	case "linux":
		p.OS = "linux"
	case "darwin":
		p.OS = "darwin"
	default:
		return p, fmt.Errorf("sand is not built for %q machines", strings.TrimSpace(system))
	}
	switch strings.ToLower(strings.TrimSpace(machine)) {
	case "x86_64", "amd64":
		p.Arch = "amd64"
	case "aarch64", "arm64", "aarch64_be", "armv8l":
		p.Arch = "arm64"
	default:
		return p, fmt.Errorf("sand is not built for %s on %q processors", p.OS, strings.TrimSpace(machine))
	}
	return p, nil
}

// AssetName is what a release calls the binary for a platform:
// sand-v2026.11.240-linux-amd64. It is build-release.sh's naming, and the
// release workflow's.
func AssetName(version string, p Platform) string {
	return "sand-" + version + "-" + p.OS + "-" + p.Arch
}
