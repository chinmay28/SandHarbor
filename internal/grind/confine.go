package grind

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sandsftp "github.com/chinmay28/SandHarbor/internal/sftp"
)

// openUnder opens a regular file at rel under root, refusing anything that
// is, or resolves to, somewhere outside root.
//
// The grinder runs as the login, which can read whatever the login can read,
// so the folder a machine is scoped to is a promise only this function keeps:
// the vault's SFTP client keeps it for browsing and plain imports, and this
// keeps it here, the same way — a path that climbs is refused as written, and
// a link is followed and the place it lands is what is checked.
func openUnder(root, rel string) (*os.File, os.FileInfo, error) {
	lexical, err := sandsftp.Under(root, rel)
	if err != nil {
		return nil, nil, err
	}

	realRoot, err := filepath.EvalSymlinks(filepath.FromSlash(sandsftp.CleanPath(root)))
	if err != nil {
		return nil, nil, fmt.Errorf("the folder %s: %w", root, err)
	}
	real, err := filepath.EvalSymlinks(filepath.FromSlash(lexical))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", rel, err)
	}
	if !within(realRoot, real) {
		return nil, nil, fmt.Errorf("%s leads outside %s", rel, root)
	}

	f, err := os.Open(real)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, errors.New(rel + " is not a regular file")
	}
	return f, info, nil
}

// within reports whether path is root or below it. Both are already resolved.
func within(root, path string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}
