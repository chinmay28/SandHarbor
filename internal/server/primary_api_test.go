package server

import (
	"net/http"
	"path/filepath"
	"testing"
)

// A server started with --phone tells its vault so, before anything else.
func TestThePhoneFlagReachesTheVault(t *testing.T) {
	s := &Server{VaultPath: filepath.Join(t.TempDir(), "vault.sand"), Phone: true}
	v, err := s.Vault()
	if err != nil {
		t.Fatalf("Vault: %v", err)
	}
	if !v.Phone() {
		t.Error("the vault of a --phone server is not a phone")
	}
}

// The machines panel says who runs the background work and lets it be
// chosen; the replication panel says whether it is this machine.
func TestChoosingThePrimary(t *testing.T) {
	c := newTestClient(t)
	c.setup("correct horse battery staple", 1)
	here := c.server.vault.Instance()

	w, list := c.json(http.MethodGet, "/api/vault/instances", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("instances: %d %v", w.Code, list)
	}
	primary, _ := list["primary"].(map[string]any)
	if primary["primary"] != "" || primary["runs_here"] != true || primary["phone"] != false {
		t.Errorf("before choosing: %v", primary)
	}

	w, chosen := c.json(http.MethodPost, "/api/vault/primary", map[string]any{"name": here})
	if w.Code != http.StatusOK || chosen["primary"] != here || chosen["runs_here"] != true {
		t.Fatalf("choosing this machine: %d %v", w.Code, chosen)
	}

	if w, bad := c.json(http.MethodPost, "/api/vault/primary", map[string]any{"name": "nowhere"}); w.Code != http.StatusBadRequest {
		t.Errorf("choosing a stranger: %d %v", w.Code, bad)
	}

	w, rep := c.json(http.MethodGet, "/api/vault/replication", nil)
	if w.Code != http.StatusOK || rep["runs_here"] != true || rep["primary"] != here {
		t.Errorf("replication status: %d %v", w.Code, rep)
	}

	// On a phone, nothing runs here and the phone cannot take it on.
	c.server.vault.SetPhone(true)
	w, rep = c.json(http.MethodGet, "/api/vault/replication", nil)
	if w.Code != http.StatusOK || rep["runs_here"] != false {
		t.Errorf("replication status on a phone: %d %v", w.Code, rep)
	}
	if w, _ := c.json(http.MethodPost, "/api/vault/primary", map[string]any{"name": here}); w.Code != http.StatusBadRequest {
		t.Errorf("a phone made itself the primary: %d", w.Code)
	}
	if c.server.vault.RunsBackgroundWork() {
		t.Error("a phone runs background work")
	}
}
