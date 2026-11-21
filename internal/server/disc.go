package server

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/chinmay28/SandHarbor/internal/bluray"
	"github.com/chinmay28/SandHarbor/internal/vault"
)

// Playing the film inside a disc image.
//
// A Blu-ray kept as an .iso is a filesystem with a film somewhere in it, and
// a player handed the image's address gets the filesystem. VLC on a desktop
// may make sense of that; VLC on a phone mostly does not, and the TV's player
// reads containers, not discs. So a stream link for a disc image stands for
// the film rather than the file: the main title's clips, found once when the
// link is minted, served as the transport stream they already are (see
// internal/bluray). The player is given an address ending in .ts and never
// learns there was a disc.
//
// Nothing is unpacked or stored. The link's ticket carries where the title
// lies in the image, and each request reads those ranges out of the vault —
// a seek costs the chunks it lands in, exactly as it does in any other film.

// discStreamMIME is the type a disc's film is served as.
const discStreamMIME = "video/mp2t"

// discProbeTimeout bounds finding the title in a disc image. It is a handful
// of small reads, but each can be a chunk gathered from cloud accounts.
var discProbeTimeout = 2 * time.Minute

// discFilm is the film a stream link plays out of a disc image.
type discFilm struct {
	title *bluray.Title
	// hash is the image the title was found in. A title is offsets into one
	// particular image, so a link is good for that image and no other.
	hash string
}

// isDiscImage reports whether a file is named as a disc image.
func isDiscImage(entry *vault.Entry) bool {
	return strings.EqualFold(path.Ext(entry.Name), ".iso")
}

// discStreamName is the name a disc's film is served under: the image's own,
// with the extension of what is actually sent, because a player picks its
// demuxer off the extension.
func discStreamName(name string) string {
	return strings.TrimSuffix(name, path.Ext(name)) + ".ts"
}

// unplayableDisc reports whether err is a verdict on the image — not a
// Blu-ray, nothing on it to play, still encrypted — rather than a failure to
// read it. A verdict is the same every time for the same bytes.
func unplayableDisc(err error) bool {
	return errors.Is(err, bluray.ErrNotBluray) ||
		errors.Is(err, bluray.ErrNoTitle) ||
		errors.Is(err, bluray.ErrEncrypted)
}

// discCacheSize is how many images' verdicts are remembered. Each is a title's
// extents or an error: a few hundred bytes for a film in one piece.
const discCacheSize = 256

// discCache remembers what was found in each disc image, by the image's hash,
// so that a film is looked for once rather than every time it is played.
// Memory only, and emptied when the vault locks: where a film lies inside an
// image is a fact about the plaintext.
type discCache struct {
	mu    sync.Mutex
	found map[string]discVerdict
}

type discVerdict struct {
	title *bluray.Title
	err   error
}

func (c *discCache) get(hash string) (discVerdict, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.found[hash]
	return v, ok
}

func (c *discCache) put(hash string, v discVerdict) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.found == nil {
		c.found = map[string]discVerdict{}
	}
	// Full: drop one, whichever the map gives up. Finding a title again
	// costs a few reads, which is not worth an ordering to avoid.
	if _, ok := c.found[hash]; !ok && len(c.found) >= discCacheSize {
		for old := range c.found {
			delete(c.found, old)
			break
		}
	}
	c.found[hash] = v
}

func (c *discCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.found = nil
}

// discFilmOf finds the film in a disc image, reading the image from the given
// placement.
//
// The error is one of bluray's three verdicts (see unplayableDisc) when the
// image was read and has no film to give, and anything else when it could not
// be read.
func (s *Server) discFilmOf(ctx context.Context, v *vault.Vault, entry *vault.Entry, source vault.ReadSource) (*discFilm, error) {
	if found, ok := s.discs.get(entry.Hash); ok {
		if found.err != nil {
			return nil, found.err
		}
		return &discFilm{title: found.title, hash: entry.Hash}, nil
	}

	var opts []vault.ReadOption
	if source != "" && source != vault.ReadParts {
		opts = append(opts, vault.ReadFrom(source))
	}
	image, opened, err := v.OpenReaderAt(ctx, entry.ID, opts...)
	if err != nil {
		return nil, err
	}
	title, err := bluray.Open(image, opened.Size)
	if err != nil && !unplayableDisc(err) {
		return nil, err
	}
	// The hash is the one of the file that was opened, which is the one the
	// title's offsets are good for.
	s.discs.put(opened.Hash, discVerdict{title: title, err: err})
	if err != nil {
		return nil, err
	}
	return &discFilm{title: title, hash: opened.Hash}, nil
}

// discErrorResponse answers a request to play a disc image that has no film
// to give, in words for whoever is holding the remote.
func discErrorResponse(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, bluray.ErrEncrypted):
		writeError(w, http.StatusUnprocessableEntity,
			"This disc image is still encrypted, so it cannot be played. Copy the disc again with a tool that decrypts it.",
			"DISC_ENCRYPTED")
	case errors.Is(err, bluray.ErrNotBluray):
		writeError(w, http.StatusUnprocessableEntity,
			"This disc image is not a Blu-ray. Only Blu-ray images can be played.",
			"DISC_UNPLAYABLE")
	default:
		writeError(w, http.StatusUnprocessableEntity,
			"No film could be found on this disc image.",
			"DISC_UNPLAYABLE")
	}
}
