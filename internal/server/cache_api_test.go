package server

import (
	"net/http"
	"testing"
	"time"
)

// Keeping files on this machine, over HTTP: pin, fill, look, let go.

func TestCacheKeepFillAndStop(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	c.mkdir("/films")
	c.upload("film.mkv", "/films", []byte("a film that should open without the clouds"))

	// No reserve, so the test does not depend on how full this disk is.
	if w, _ := c.json(http.MethodPut, "/api/cache/limits", map[string]any{"limit": 0, "reserve": -1}); w.Code != http.StatusOK {
		t.Fatalf("PUT limits: %d %s", w.Code, w.Body.String())
	}

	w, pin := c.json(http.MethodPost, "/api/cache", map[string]any{"path": "/films", "for": "7d"})
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/cache: %d %s", w.Code, w.Body.String())
	}
	if pin["folder"] != true || pin["path"] != "/films" {
		t.Fatalf("pin = %v", pin)
	}
	until, _ := time.Parse(time.RFC3339Nano, pin["until"].(string))
	if d := time.Until(until); d < 6*24*time.Hour || d > 8*24*time.Hour {
		t.Fatalf("7d kept until %v", until)
	}

	c.server.fillCache()

	_, status := c.json(http.MethodGet, "/api/cache", nil)
	pins := status["pins"].([]any)
	if len(pins) != 1 {
		t.Fatalf("pins = %v", pins)
	}
	row := pins[0].(map[string]any)
	if row["complete"] != true || row["files"] != float64(1) {
		t.Fatalf("after the fill: %v", row)
	}
	if used := status["usage"].(map[string]any)["used"].(float64); used == 0 {
		t.Fatalf("usage after the fill: %v", status["usage"])
	}

	id := pin["id"].(string)
	if w := c.do(http.MethodDelete, "/api/cache/"+id, nil, ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body.String())
	}
	if w := c.do(http.MethodDelete, "/api/cache/"+id, nil, ""); w.Code != http.StatusNotFound {
		t.Fatalf("DELETE twice: %d, want 404", w.Code)
	}
	_, status = c.json(http.MethodGet, "/api/cache", nil)
	if used := status["usage"].(map[string]any)["used"].(float64); used != 0 {
		t.Fatalf("usage after letting go: %v", status["usage"])
	}
}

func TestCacheKeepRefusesWhatItCannotDo(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 3)
	c.upload("a.txt", "/", []byte("a"))

	for name, body := range map[string]map[string]any{
		"no length":   {"path": "/a.txt"},
		"both":        {"path": "/a.txt", "for": "1d", "until": time.Now().Add(time.Hour)},
		"bad length":  {"path": "/a.txt", "for": "soon"},
		"in the past": {"path": "/a.txt", "until": time.Now().Add(-time.Hour)},
		"no path":     {"for": "1d"},
		"sub vault":   {"path": "/", "for": "1d", "vault_id": "some-sub-vault"},
	} {
		if w, _ := c.json(http.MethodPost, "/api/cache", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, w.Code, w.Body.String())
		}
	}
	if w, _ := c.json(http.MethodPost, "/api/cache", map[string]any{"path": "/nowhere", "for": "1d"}); w.Code != http.StatusNotFound {
		t.Errorf("a missing path: %d, want 404", w.Code)
	}
	if w, _ := c.json(http.MethodPut, "/api/cache/limits", map[string]any{"limit": -5}); w.Code != http.StatusBadRequest {
		t.Errorf("a negative limit: %d, want 400", w.Code)
	}
}

func TestCacheNeedsASession(t *testing.T) {
	c := newTestClient(t)
	c.setup("the vault password", 1)
	c.cookies = nil
	if w := c.do(http.MethodGet, "/api/cache", nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/cache without a session: %d", w.Code)
	}
}
