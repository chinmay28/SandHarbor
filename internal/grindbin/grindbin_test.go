package grindbin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFromUname(t *testing.T) {
	good := map[[2]string]Platform{
		{"Linux", "x86_64"}:     {"linux", "amd64"},
		{"Linux", "aarch64"}:    {"linux", "arm64"},
		{"Darwin", "arm64"}:     {"darwin", "arm64"},
		{"Darwin", "x86_64"}:    {"darwin", "amd64"},
		{"Linux\n", "x86_64\n"}: {"linux", "amd64"},
	}
	for in, want := range good {
		got, err := FromUname(in[0], in[1])
		if err != nil || got != want {
			t.Errorf("%q: %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range [][2]string{{"Linux", "armv7l"}, {"FreeBSD", "amd64"}, {"", ""}, {"Linux", "riscv64"}} {
		if _, err := FromUname(in[0], in[1]); err == nil {
			t.Errorf("%q was taken for a platform sand is built for", in)
		}
	}
}

// selfStore is a Store whose own binary is a small file on disk.
func selfStore(t *testing.T) (*Store, string) {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "sand")
	os.WriteFile(exe, []byte("this sand"), 0o755)
	return &Store{
		Dir: t.TempDir(), Version: "v2026.11.300",
		Here:       Platform{"linux", "arm64"},
		Executable: func() (string, error) { return exe, nil },
	}, exe
}

func TestTheSamePlatformIsThisSandsOwnBinary(t *testing.T) {
	s, exe := selfStore(t)
	b, err := s.Get(context.Background(), Platform{"linux", "arm64"})
	if err != nil || b.Path != exe || b.Size != 9 || b.Sum != sha256.Sum256([]byte("this sand")) {
		t.Fatalf("got %+v, %v", b, err)
	}
	if err := s.CanProvide(Platform{"linux", "arm64"}); err != nil {
		t.Fatal(err)
	}
}

func TestADevelopmentBuildHasNoOtherPlatform(t *testing.T) {
	s, _ := selfStore(t)
	_, err := s.Get(context.Background(), Platform{"linux", "amd64"})
	if !errors.Is(err, ErrNoRelease) || !errors.Is(s.CanProvide(Platform{"linux", "amd64"}), ErrNoRelease) {
		t.Fatalf("err = %v", err)
	}
}

// release serves a release's binaries and checksums, counting downloads.
func release(t *testing.T, files map[string]string, sums map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/v2026.11.300/")
		if name == "SHA256SUMS" {
			for n, sum := range sums {
				fmt.Fprintf(w, "%s  %s\n", sum, n)
			}
			return
		}
		body, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func sumOf(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func TestAnotherPlatformComesFromTheReleaseOnce(t *testing.T) {
	name := "sand-v2026.11.300-linux-amd64"
	srv, hits := release(t, map[string]string{name: "amd64 sand"}, map[string]string{name: sumOf("amd64 sand")})
	s, _ := selfStore(t)
	s.Released, s.Releases = true, srv.URL

	for i := 0; i < 2; i++ {
		b, err := s.Get(context.Background(), Platform{"linux", "amd64"})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		got, _ := os.ReadFile(b.Path)
		if string(got) != "amd64 sand" || b.Path != filepath.Join(s.Dir, name) {
			t.Fatalf("got %q at %s", got, b.Path)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("downloaded %d times, want once", hits.Load())
	}
}

// A download for this version clears out what older versions downloaded,
// and leaves this version's other platforms alone.
func TestADownloadRemovesOlderVersions(t *testing.T) {
	name := "sand-v2026.11.300-linux-amd64"
	srv, _ := release(t, map[string]string{name: "amd64 sand"}, map[string]string{name: sumOf("amd64 sand")})
	s, _ := selfStore(t)
	s.Released, s.Releases = true, srv.URL
	old := filepath.Join(s.Dir, "sand-v2026.10.5-linux-amd64")
	sibling := filepath.Join(s.Dir, "sand-v2026.11.300-darwin-arm64")
	os.WriteFile(old, []byte("old"), 0o700)
	os.WriteFile(sibling, []byte("sibling"), 0o700)

	if _, err := s.Get(context.Background(), Platform{"linux", "amd64"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("an older version's download is still there")
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Error("this version's other platform was removed")
	}
}

// A download that does not match the release's own checksum is thrown away,
// and nothing is left under the name a good one would have.
func TestADownloadThatDoesNotMatchItsChecksumIsRefused(t *testing.T) {
	name := "sand-v2026.11.300-linux-amd64"
	srv, _ := release(t, map[string]string{name: "tampered"}, map[string]string{name: sumOf("amd64 sand")})
	s, _ := selfStore(t)
	s.Released, s.Releases = true, srv.URL

	_, err := s.Get(context.Background(), Platform{"linux", "amd64"})
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(s.Dir); len(entries) != 0 {
		t.Fatalf("left %d files behind", len(entries))
	}
}

func TestAPlatformTheReleaseDoesNotPublish(t *testing.T) {
	srv, _ := release(t, nil, map[string]string{})
	s, _ := selfStore(t)
	s.Released, s.Releases = true, srv.URL
	_, err := s.Get(context.Background(), Platform{"darwin", "amd64"})
	if err == nil || !strings.Contains(err.Error(), "publishes no") {
		t.Fatalf("err = %v", err)
	}
}

func TestSumForReadsBothSha256sumForms(t *testing.T) {
	sum := sumOf("x")
	for _, line := range []string{sum + "  sand-a", sum + " *sand-a"} {
		got, err := sumFor([]byte("ffff  other\n"+line+"\n"), "sand-a")
		if err != nil || hex.EncodeToString(got[:]) != sum {
			t.Errorf("%q: %x, %v", line, got, err)
		}
	}
}

// A binary installed beside this one is used as it is: no download, even for
// a build that has a release to download from.
func TestABundledBinaryIsUsedFirst(t *testing.T) {
	name := "sand-v2026.11.300-linux-amd64"
	srv, hits := release(t, map[string]string{name: "from the release"}, map[string]string{name: sumOf("from the release")})
	s, _ := selfStore(t)
	s.Released, s.Releases = true, srv.URL
	s.Bundled = t.TempDir()
	os.WriteFile(filepath.Join(s.Bundled, name), []byte("installed beside"), 0o755)

	path, size, ok := s.Ready(Platform{"linux", "amd64"})
	if !ok || size != int64(len("installed beside")) || path != filepath.Join(s.Bundled, name) {
		t.Fatalf("Ready = %q, %d, %v", path, size, ok)
	}
	b, err := s.Get(context.Background(), Platform{"linux", "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(b.Path); string(got) != "installed beside" || hits.Load() != 0 {
		t.Fatalf("got %q after %d downloads", got, hits.Load())
	}
}

// A development build with the other platform installed beside it can copy
// it, though it has no release; one installed for another version is not it.
func TestABundledBinaryServesADevelopmentBuild(t *testing.T) {
	s, _ := selfStore(t)
	s.Bundled = t.TempDir()
	os.WriteFile(filepath.Join(s.Bundled, "sand-v2026.10.1-linux-amd64"), []byte("older"), 0o755)
	if err := s.CanProvide(Platform{"linux", "amd64"}); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("an older version's binary was taken for this one: %v", err)
	}
	os.WriteFile(filepath.Join(s.Bundled, "sand-v2026.11.300-linux-amd64"), []byte("this one"), 0o755)
	if err := s.CanProvide(Platform{"linux", "amd64"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), Platform{"linux", "amd64"}); err != nil {
		t.Fatal(err)
	}
}
