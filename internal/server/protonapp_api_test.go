package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/chinmay28/SandHarbor/internal/provider"
)

// fakeAndroidApp is just enough of the app's bridge to sign in and connect
// an account (docs/design/proton-android.md has the whole protocol).
func fakeAndroidApp(t *testing.T) {
	t.Helper()
	var mu sync.Mutex
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/proton/v1/signin":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "f1", "url": "https://account.proton.me/desktop/login#payload=x"})
		case r.URL.Path == "/proton/v1/signin/f1":
			polls++
			if polls < 2 {
				_ = json.NewEncoder(w).Encode(map[string]string{"state": "pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"state": "done", "session": "the-session", "email": "someone@proton.me"})
		case r.URL.Path == "/proton/v1/ping":
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/proton/v1/account":
			_ = json.NewEncoder(w).Encode(map[string]string{"email": "someone@proton.me"})
		case r.URL.Path == "/proton/v1/usage":
			_ = json.NewEncoder(w).Encode(map[string]int64{"used": 0, "total": 1 << 30})
		case r.URL.Path == "/proton/v1/objects":
			_, _ = w.Write([]byte("[]"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv(provider.AppBridgeEnv, srv.URL)
	t.Setenv(provider.AppBridgeTokenEnv, "tok")
}

// TestProtonAppSignInConnectsAnAccount is the phone's whole path, through the
// same status and complete endpoints every other sign-in uses.
func TestProtonAppSignInConnectsAnAccount(t *testing.T) {
	fakeAndroidApp(t)
	c := newTestClient(t)
	c.setup("correct horse battery staple", 0)

	w, start := c.json(http.MethodPost, "/api/providers/protonapp/signin", map[string]any{
		"options": map[string]string{"folder": "/my-files/sand"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("start: %d %v", w.Code, start)
	}
	flowID, _ := start["flow_id"].(string)

	shown := awaitFlow(t, c, flowID, func(s map[string]any) bool { return s["sign_in_url"] != nil })
	if url, _ := shown["sign_in_url"].(string); !strings.HasPrefix(url, "https://account.proton.me/") {
		t.Fatalf("sign_in_url = %v", shown["sign_in_url"])
	}
	ready := awaitFlow(t, c, flowID, func(s map[string]any) bool {
		return s["status"] == "ready" || s["status"] == "error"
	})
	if ready["status"] != "ready" || ready["account"] != "someone@proton.me" {
		t.Fatalf("sign-in finished as %v", ready)
	}

	w, done := c.json(http.MethodPost, "/api/providers/oauth/complete", map[string]any{
		"flow_id": flowID,
		"name":    "proton",
		"options": map[string]string{"folder": "/my-files/sand"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("complete: %d %v", w.Code, done)
	}
	account, _ := done["provider"].(map[string]any)
	if account["kind"] != string(provider.KindProtonApp) {
		t.Fatalf("connected a %v", account["kind"])
	}
	options, _ := account["options"].(map[string]any)
	if options["session"] == "the-session" {
		t.Fatal("the Proton session was handed back to the browser in the clear")
	}
}

// TestProtonAppSignInOutsideTheAppSaysSo: a Pi has no app to sign in through.
func TestProtonAppSignInOutsideTheAppSaysSo(t *testing.T) {
	t.Setenv(provider.AppBridgeEnv, "")
	t.Setenv(provider.AppBridgeTokenEnv, "")
	c := newTestClient(t)
	c.setup("correct horse battery staple", 0)

	w, body := c.json(http.MethodPost, "/api/providers/protonapp/signin", map[string]any{})
	if w.Code != http.StatusBadRequest || body["code"] != "PROTON_NO_APP" {
		t.Fatalf("start outside the app: %d %v", w.Code, body)
	}
}
