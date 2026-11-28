package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// Uploading one file a piece at a time, for the file too large for a request.
//
// The tests make the request ceiling tiny rather than the file huge: what is
// being shown is that a file the one-request upload refuses goes in this way.

// beginPieces opens an upload and returns its ID.
func (c *testClient) beginPieces(req map[string]any) string {
	c.t.Helper()
	w, body := c.json(http.MethodPost, "/api/uploads", req)
	if w.Code != http.StatusCreated {
		c.t.Fatalf("begin: %d %v", w.Code, body)
	}
	return body["id"].(string)
}

// sendPiece sends one piece and returns the status and the parsed answer.
func (c *testClient) sendPiece(id string, offset int, piece []byte) (int, map[string]any) {
	c.t.Helper()
	w := c.do(http.MethodPut, fmt.Sprintf("/api/uploads/%s?offset=%d", id, offset),
		bytes.NewReader(piece), "application/octet-stream")
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

// finishPieces finishes an upload and returns the status and its one result.
func (c *testClient) finishPieces(id string) (int, map[string]any) {
	c.t.Helper()
	w, body := c.json(http.MethodPost, "/api/uploads/"+id+"/finish", nil)
	results, _ := body["results"].([]any)
	if len(results) != 1 {
		return w.Code, body
	}
	return w.Code, results[0].(map[string]any)
}

// inPieces uploads content whole, size bytes at a time.
func (c *testClient) inPieces(req map[string]any, content []byte, size int) (int, map[string]any) {
	c.t.Helper()
	req["size"] = len(content)
	id := c.beginPieces(req)
	for at := 0; at < len(content); at += size {
		end := min(at+size, len(content))
		if code, body := c.sendPiece(id, at, content[at:end]); code != http.StatusOK || body["received"] != float64(end) {
			c.t.Fatalf("piece at %d: %d %v", at, code, body)
		}
	}
	return c.finishPieces(id)
}

func (c *testClient) spools() []string {
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(c.server.VaultPath), ".sand-upload-*"))
	return left
}

func TestAFileOverTheRequestCeilingGoesInPieces(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.server.MaxUploadSize = 4 << 10

	film := make([]byte, 100<<10)
	rand.New(rand.NewSource(7)).Read(film)

	// The one-request upload turns it away, which is the problem.
	var buf bytes.Buffer
	if w := c.do(http.MethodPost, "/api/files", &buf, "multipart/form-data; boundary=x"); w.Code == http.StatusCreated {
		t.Fatal("setup: an empty upload was accepted")
	}

	code, result := c.inPieces(map[string]any{"path": "/", "name": "film.bin", "mod": 1559390400000}, film, 16<<10)
	if code != http.StatusCreated || result["ok"] != true {
		t.Fatalf("finish: %d %v", code, result)
	}
	file := result["file"].(map[string]any)
	w := c.do(http.MethodGet, "/api/files/"+file["id"].(string)+"/content", nil, "")
	if !bytes.Equal(w.Body.Bytes(), film) {
		t.Errorf("what came back is not what was sent (%d bytes of %d)", w.Body.Len(), len(film))
	}
	if got := c.modified("/", "film.bin"); got.Year() != 2019 {
		t.Errorf("modified = %v, want the file's own time", got)
	}
	if left := c.spools(); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestAFileInPiecesKeepsThePathItHadInAFolder(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)

	code, result := c.inPieces(map[string]any{
		"path": "/", "name": "big.bin", "rel": "trip/day1/big.bin",
	}, []byte("0123456789"), 4)
	if code != http.StatusCreated || result["name"] != "trip/day1/big.bin" {
		t.Fatalf("finish: %d %v", code, result)
	}
	if got := c.names("/trip/day1"); got != "big.bin" {
		t.Errorf("/trip/day1 holds %q", got)
	}
}

// The screenshot this was written for: an exported archive over the ceiling,
// sent to be extracted.
func TestAnArchiveOverTheRequestCeilingIsExtractedFromPieces(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.server.MaxUploadSize = 1 << 10

	noise := make([]byte, 20<<10)
	rand.New(rand.NewSource(3)).Read(noise)
	archive := zipOf(t, map[string]string{
		"Takeout/Photos/a.bin": string(noise),
		"Takeout/Photos/b.txt": "small",
	})
	if len(archive) < 8<<10 {
		t.Fatalf("setup: the archive is only %d bytes", len(archive))
	}

	code, result := c.inPieces(map[string]any{"path": "/", "name": "takeout-001.zip", "extract": true}, archive, 3<<10)
	if code != http.StatusCreated {
		t.Fatalf("finish: %d %v", code, result)
	}
	run := c.extracted(result)
	if run["dir"] != "/takeout-001" || reportOf(run)["extracted"] != float64(2) || run["error"] != nil {
		t.Errorf("run = %v", run)
	}
	if got := c.names("/takeout-001/Takeout/Photos"); got != "a.bin b.txt" {
		t.Errorf("holds %q", got)
	}
	if got := c.names("/"); got != "" {
		t.Errorf("/ holds %q — the archive itself is not stored", got)
	}
	if left := c.spools(); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestAPieceSentTwiceIsToldWhereTheUploadHasGotTo(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	id := c.beginPieces(map[string]any{"path": "/", "name": "a.bin", "size": 10})

	c.sendPiece(id, 0, []byte("01234"))
	// The answer was lost, so the sender tries again — and learns it landed.
	code, body := c.sendPiece(id, 0, []byte("01234"))
	if code != http.StatusConflict || body["code"] != "OFFSET" || body["received"] != float64(5) {
		t.Fatalf("resend: %d %v", code, body)
	}
	c.sendPiece(id, 5, []byte("56789"))
	_, result := c.finishPieces(id)
	file := result["file"].(map[string]any)
	if w := c.do(http.MethodGet, "/api/files/"+file["id"].(string)+"/content", nil, ""); w.Body.String() != "0123456789" {
		t.Errorf("stored %q", w.Body.String())
	}
}

func TestFinishingBeforeEverythingArrivedIsRefusedAndCanBeResumed(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	id := c.beginPieces(map[string]any{"path": "/", "name": "a.bin", "size": 10})
	c.sendPiece(id, 0, []byte("01234"))

	code, body := c.finishPieces(id)
	if code != http.StatusConflict || body["code"] != "INCOMPLETE" || body["received"] != float64(5) {
		t.Fatalf("early finish: %d %v", code, body)
	}
	if got := c.names("/"); got != "" {
		t.Errorf("%q was stored from half a file", got)
	}
	c.sendPiece(id, 5, []byte("56789"))
	if code, result := c.finishPieces(id); code != http.StatusCreated || result["ok"] != true {
		t.Errorf("finish: %d %v", code, result)
	}
}

func TestOpeningAPieceUploadRefusesWhatItCanBeforeAByteIsSent(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	c.server.MaxPieceUploadSize = 1000

	cases := []struct {
		why  string
		req  map[string]any
		code string
	}{
		{"no size", map[string]any{"path": "/", "name": "a.bin"}, "BAD_REQUEST"},
		{"too large", map[string]any{"path": "/", "name": "a.bin", "size": 1001}, "TOO_LARGE"},
		{"climbs out", map[string]any{"path": "/", "name": "a.bin", "rel": "../a.bin", "size": 5}, "BAD_PATH"},
		{"not an archive", map[string]any{"path": "/", "name": "a.txt", "size": 5, "extract": true}, "NOT_AN_ARCHIVE"},
		{"bad scheme", map[string]any{"path": "/", "name": "a.bin", "size": 5, "scheme": "nonsense"}, "BAD_SCHEME"},
	}
	for _, tc := range cases {
		w, body := c.json(http.MethodPost, "/api/uploads", tc.req)
		if w.Code < 400 || body["code"] != tc.code {
			t.Errorf("%s: %d %v, want %s", tc.why, w.Code, body, tc.code)
		}
	}
	if left := c.spools(); len(left) != 0 {
		t.Errorf("a refused upload left a file behind: %v", left)
	}
}

func TestAPieceUploadCanBeGivenUp(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	id := c.beginPieces(map[string]any{"path": "/", "name": "a.bin", "size": 10})
	c.sendPiece(id, 0, []byte("01234"))
	if len(c.spools()) != 1 {
		t.Fatal("setup: nothing was being kept")
	}

	if w, _ := c.json(http.MethodDelete, "/api/uploads/"+id, nil); w.Code != http.StatusOK {
		t.Fatalf("abort: %d", w.Code)
	}
	if left := c.spools(); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	for _, try := range []func() int{
		func() int { code, _ := c.sendPiece(id, 5, []byte("56789")); return code },
		func() int { code, _ := c.finishPieces(id); return code },
		func() int { w, _ := c.json(http.MethodDelete, "/api/uploads/"+id, nil); return w.Code },
	} {
		if code := try(); code != http.StatusNotFound {
			t.Errorf("after giving up: %d, want 404", code)
		}
	}
}

func TestMoreThanWasPromisedIsRefused(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	id := c.beginPieces(map[string]any{"path": "/", "name": "a.bin", "size": 4})
	if code, body := c.sendPiece(id, 0, []byte(strings.Repeat("x", 9))); code != http.StatusBadRequest || body["code"] != "TOO_LONG" {
		t.Errorf("%d %v", code, body)
	}
}

func TestLockingTheVaultGivesUpPieceUploads(t *testing.T) {
	c := newTestClient(t)
	c.setup("pw", 3)
	id := c.beginPieces(map[string]any{"path": "/", "name": "a.bin", "size": 10})
	c.sendPiece(id, 0, []byte("01234"))

	if w, _ := c.json(http.MethodPost, "/api/vault/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock: %d", w.Code)
	}
	if left := c.spools(); len(left) != 0 {
		t.Errorf("plaintext left on the disk of a locked vault: %v", left)
	}
	if c.server.pieces.count() != 0 {
		t.Error("the upload was remembered past the lock")
	}
}
