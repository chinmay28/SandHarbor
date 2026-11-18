package server

import (
	"net/http"
	"testing"
	"time"
)

// awaitThumbRun polls the run until want says so, or fails.
func awaitThumbRun(t *testing.T, c *testClient, want func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		w, status := c.json(http.MethodGet, "/api/thumbs/making", nil)
		if w.Code == http.StatusOK && want(status) {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run never got there: %d %v", w.Code, status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func finished(s map[string]any) bool { return s["running"] == false && s["result"] != nil }

// The whole path: count, start, finish on the server, every picture stored
// and every failure counted.
func TestThumbMakingRunsOnTheServer(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)
	c.mkdir("/photos")
	c.mkdir("/photos/sub")
	a := c.upload("a.png", "/photos", samplePNG(t, 600, 400))["id"].(string)
	b := c.upload("b.png", "/photos/sub", samplePNG(t, 300, 900))["id"].(string)
	c.upload("broken.jpg", "/photos", []byte("not a picture"))
	c.upload("notes.txt", "/photos", []byte("words"))

	w, plan := c.json(http.MethodGet, "/api/thumbs/plan?path=/photos", nil)
	if w.Code != http.StatusOK || plan["files"] != float64(3) || plan["folders"] != float64(2) {
		t.Fatalf("plan: %d %v", w.Code, plan)
	}

	w, started := c.json(http.MethodPost, "/api/thumbs/making", map[string]any{"path": "/photos"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: %d %v", w.Code, started)
	}
	done := awaitThumbRun(t, c, finished)
	result, _ := done["result"].(map[string]any)
	if result["made"] != float64(2) || result["failed"] != float64(1) || result["undrawn"] != float64(1) || result["done"] != float64(3) {
		t.Fatalf("result: %v", result)
	}
	for _, id := range []string{a, b} {
		if w := c.do(http.MethodGet, "/api/files/"+id+"/thumb", nil, ""); w.Code != http.StatusOK {
			t.Errorf("thumbnail of %s: %d", id, w.Code)
		}
	}

	if w, _ := c.json(http.MethodPost, "/api/thumbs/making", map[string]any{"path": "/nowhere"}); w.Code == http.StatusAccepted {
		t.Error("a run was started for a folder that does not exist")
	}
}

// Locking the vault by hand does not end a run or forget it, as it does every
// other job: the run is the same run after the unlock, and it finishes with
// every picture made. (That a run caught by the lock waits for the unlock is
// vault.TestMakeThumbsWaitsOutALock.)
func TestThumbMakingSurvivesALock(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)
	c.mkdir("/photos")
	for _, name := range []string{"a.png", "b.png", "c.png"} {
		c.upload(name, "/photos", samplePNG(t, 400, 300))
	}

	w, started := c.json(http.MethodPost, "/api/thumbs/making", map[string]any{"path": "/photos"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: %d %v", w.Code, started)
	}
	if w, _ := c.json(http.MethodPost, "/api/vault/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock: %d", w.Code)
	}
	if c.server.thumbMakes.status().ID != started["id"] {
		t.Fatal("locking the vault forgot the run")
	}
	if w, _ := c.json(http.MethodPost, "/api/vault/unlock", map[string]any{"password": "correct horse battery staple"}); w.Code != http.StatusOK {
		t.Fatalf("unlock: %d", w.Code)
	}

	done := awaitThumbRun(t, c, finished)
	if done["id"] != started["id"] {
		t.Errorf("a different run finished: %v", done["id"])
	}
	result, _ := done["result"].(map[string]any)
	if result["made"] != float64(3) || result["failed"] != float64(0) {
		t.Fatalf("result after the lock: %v", result)
	}
}

// Stop ends the run, and its counts stand as the result.
func TestThumbMakingStops(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 3)
	c.mkdir("/photos")
	c.upload("a.png", "/photos", samplePNG(t, 400, 300))

	if w, _ := c.json(http.MethodPost, "/api/thumbs/making", map[string]any{"path": "/photos"}); w.Code != http.StatusAccepted {
		t.Fatalf("start: %d", w.Code)
	}
	if w, _ := c.json(http.MethodDelete, "/api/thumbs/making", nil); w.Code != http.StatusOK {
		t.Fatalf("stop: %d", w.Code)
	}
	done := awaitThumbRun(t, c, finished)
	if result, _ := done["result"].(map[string]any); result["total"] != float64(1) {
		t.Errorf("result: %v", result)
	}
}
