package grindbin

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// DefaultReleases is where a release's files are downloaded from: the
// project's GitHub releases, by tag.
const DefaultReleases = "https://github.com/chinmay28/SandHarbor/releases/download"

// maxBinary bounds a download. A sand binary is about 30 MB; a body much past
// that is not one.
const maxBinary = 200 << 20

// ErrNoRelease is a development build asked for another platform's binary:
// there is no release cut from it to fetch one from.
var ErrNoRelease = errors.New("this sand is a development build, so there is no release to fetch another platform's sand from")

// Binary is a sand executable on this machine, ready to be copied.
type Binary struct {
	Path string
	Size int64
	Sum  [32]byte
}

// Store hands out a sand binary for a platform. It is safe for concurrent
// use; two imports wanting the same platform download it once.
type Store struct {
	// Bundled is a folder of binaries for other platforms installed beside
	// this one, named as a release names them: the installer puts the other
	// Linux architecture there, so copying sand to a machine of either kind is
	// a copy and nothing more. Looked in before anything is downloaded; a
	// name carries its version, so what an older install left is never used.
	Bundled string

	// Dir is where downloaded binaries are kept. They are named by version and
	// platform, so a binary there is only ever the one that name says.
	Dir string

	// Version is this build's version, and Released whether it is a release
	// (stamped) rather than a development build.
	Version  string
	Released bool

	// Releases is the base URL releases are downloaded from: Version and the
	// asset name are appended. Empty is DefaultReleases.
	Releases string

	// Client makes the downloads. Nil is a client with no overall deadline;
	// the context bounds each call.
	Client *http.Client

	// Executable and Here are this process's own binary and platform. Nil and
	// zero are os.Executable and Self; tests set them.
	Executable func() (string, error)
	Here       Platform

	mu sync.Mutex
}

// Native is the platform this store's own binary is for.
func (s *Store) Native() Platform { return s.here() }

func (s *Store) here() Platform {
	if s.Here == (Platform{}) {
		return Self()
	}
	return s.Here
}

// CanProvide says, without downloading anything, whether Get could be asked
// for this platform: always for this process's own, and for another only
// from a release build. A release build can still fail to download; that is
// for Get to say.
func (s *Store) CanProvide(p Platform) error {
	if _, _, ok := s.Ready(p); ok || s.Released {
		return nil
	}
	return ErrNoRelease
}

// Ready reports a binary for the platform that is at hand without the
// network — this process's own, one installed beside it, or one downloaded
// before — and its size. It reads no file's contents.
func (s *Store) Ready(p Platform) (path string, size int64, ok bool) {
	var candidates []string
	if p == s.here() {
		exe := s.Executable
		if exe == nil {
			exe = os.Executable
		}
		if path, err := exe(); err == nil {
			candidates = append(candidates, path)
		}
	} else {
		name := AssetName(s.Version, p)
		if s.Bundled != "" {
			candidates = append(candidates, filepath.Join(s.Bundled, name))
		}
		candidates = append(candidates, filepath.Join(s.Dir, name))
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.Mode().IsRegular() {
			return c, info.Size(), true
		}
	}
	return "", 0, false
}

// Get returns a sand binary for the platform.
func (s *Store) Get(ctx context.Context, p Platform) (Binary, error) {
	if p == s.here() {
		exe := s.Executable
		if exe == nil {
			exe = os.Executable
		}
		path, err := exe()
		if err != nil {
			return Binary{}, fmt.Errorf("finding this sand's own binary: %w", err)
		}
		return describe(path)
	}
	if s.Bundled != "" {
		if b, err := describe(filepath.Join(s.Bundled, AssetName(s.Version, p))); err == nil {
			return b, nil
		}
	}
	if !s.Released {
		return Binary{}, ErrNoRelease
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	name := AssetName(s.Version, p)
	path := filepath.Join(s.Dir, name)
	// Only a verified download is ever renamed to this name, so a file here
	// is the one the release published.
	if b, err := describe(path); err == nil {
		return b, nil
	}
	return s.download(ctx, name, path)
}

// describe sizes and hashes a binary on disk.
func describe(path string) (Binary, error) {
	f, err := os.Open(path)
	if err != nil {
		return Binary{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return Binary{}, err
	}
	b := Binary{Path: path, Size: n}
	copy(b.Sum[:], h.Sum(nil))
	return b, nil
}

// download fetches one release asset, checks it against the release's
// SHA256SUMS, and keeps it under its name.
func (s *Store) download(ctx context.Context, name, dest string) (Binary, error) {
	base := strings.TrimSuffix(s.Releases, "/")
	if base == "" {
		base = DefaultReleases
	}
	release := base + "/" + s.Version

	sums, err := s.fetch(ctx, release+"/SHA256SUMS", 1<<20)
	if err != nil {
		return Binary{}, fmt.Errorf("the checksums of release %s: %w", s.Version, err)
	}
	want, err := sumFor(sums, name)
	if err != nil {
		return Binary{}, fmt.Errorf("release %s: %w", s.Version, err)
	}

	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return Binary{}, err
	}
	tmp, err := os.CreateTemp(s.Dir, ".download-*")
	if err != nil {
		return Binary{}, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	body, err := s.open(ctx, release+"/"+name)
	if err != nil {
		return Binary{}, fmt.Errorf("downloading %s: %w", name, err)
	}
	defer body.Close()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(body, maxBinary+1))
	if err != nil {
		return Binary{}, fmt.Errorf("downloading %s: %w", name, err)
	}
	if n > maxBinary {
		return Binary{}, fmt.Errorf("%s is larger than any sand binary", name)
	}
	var got [32]byte
	copy(got[:], h.Sum(nil))
	if got != want {
		return Binary{}, fmt.Errorf("%s does not match the checksum its release publishes", name)
	}
	if err := tmp.Close(); err != nil {
		return Binary{}, err
	}
	if err := os.Chmod(tmp.Name(), 0o700); err != nil {
		return Binary{}, err
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return Binary{}, err
	}
	s.pruneOlder()
	return Binary{Path: dest, Size: n, Sum: got}, nil
}

func (s *Store) open(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			// A build from source between releases has a version no release
			// was cut as, and lands here too.
			return nil, errors.New("there is no such release, or it does not publish it")
		}
		return nil, fmt.Errorf("the server answered %s", resp.Status)
	}
	return resp.Body, nil
}

func (s *Store) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	body, err := s.open(ctx, url)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, limit))
}

// sumFor reads one file's checksum out of a sha256sum listing.
func sumFor(listing []byte, name string) ([32]byte, error) {
	var sum [32]byte
	sc := bufio.NewScanner(strings.NewReader(string(listing)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		raw, err := hex.DecodeString(fields[0])
		if err != nil || len(raw) != len(sum) {
			return sum, fmt.Errorf("its checksum for %s does not parse", name)
		}
		copy(sum[:], raw)
		return sum, nil
	}
	return sum, fmt.Errorf("it publishes no %s", name)
}

// pruneOlder removes binaries an older version of this vault downloaded: each
// is tens of megabytes, and nothing will copy them again.
func (s *Store) pruneOlder() {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return
	}
	current := "sand-" + s.Version + "-"
	for _, e := range entries {
		if name := e.Name(); strings.HasPrefix(name, "sand-") && !strings.HasPrefix(name, current) {
			_ = os.Remove(filepath.Join(s.Dir, name))
		}
	}
}
