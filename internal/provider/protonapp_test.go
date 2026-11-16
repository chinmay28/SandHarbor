package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const fakeBridgeToken = "test-token"

// fakeBridge is the app's side of the protocol in docs/design/proton-android.md,
// over a map instead of Proton.
type fakeBridge struct {
	t *testing.T

	mu       sync.Mutex
	objects  map[string][]byte
	sessions []string // the session each account request arrived with
	folders  []string

	// rotateTo, when set, is handed back once as a refreshed session.
	rotateTo string
	// signedOut makes every account request answer signed_out.
	signedOut bool

	// sign-in
	signInStates []string // answered in turn to GET /signin/{id}
	polls        int
}

func newFakeBridge(t *testing.T) (*fakeBridge, *httptest.Server) {
	t.Helper()
	f := &fakeBridge{t: t, objects: map[string][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv(AppBridgeEnv, srv.URL)
	t.Setenv(AppBridgeTokenEnv, fakeBridgeToken)
	return f, srv
}

func (f *fakeBridge) fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}

func (f *fakeBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+fakeBridgeToken {
		f.fail(w, http.StatusUnauthorized, "bad_request", "wrong token")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	path := r.URL.EscapedPath()
	if strings.HasPrefix(path, "/proton/v1/signin") {
		f.serveSignIn(w, r, path)
		return
	}

	raw, err := base64.StdEncoding.DecodeString(r.Header.Get(protonAppSessionHeader))
	if err != nil || len(raw) == 0 {
		f.fail(w, http.StatusBadRequest, "bad_request", "no session")
		return
	}
	f.sessions = append(f.sessions, string(raw))
	f.folders = append(f.folders, r.Header.Get(protonAppFolderHeader))
	if f.rotateTo != "" {
		w.Header().Set(protonAppSessionHeader, base64.StdEncoding.EncodeToString([]byte(f.rotateTo)))
		f.rotateTo = ""
	}
	if f.signedOut {
		f.fail(w, http.StatusUnauthorized, "signed_out", "refresh token revoked")
		return
	}

	switch {
	case path == "/proton/v1/ping":
		w.WriteHeader(http.StatusNoContent)
	case path == "/proton/v1/usage":
		_ = json.NewEncoder(w).Encode(map[string]int64{"used": 1234, "total": 5_000_000_000})
	case path == "/proton/v1/account":
		_ = json.NewEncoder(w).Encode(map[string]string{"email": "someone@proton.me"})
	case path == "/proton/v1/objects":
		prefix := r.URL.Query().Get("prefix")
		out := []ObjectInfo{}
		for k, v := range f.objects {
			if strings.HasPrefix(k, prefix) {
				out = append(out, ObjectInfo{Key: k, Size: int64(len(v))})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
		_ = json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(path, "/proton/v1/objects/"):
		key, _ := url.PathUnescape(strings.TrimPrefix(path, "/proton/v1/objects/"))
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			f.objects[key] = body
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			if _, ok := f.objects[key]; !ok {
				f.fail(w, http.StatusNotFound, "not_found", "no such part")
				return
			}
			delete(f.objects, key)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			data, ok := f.objects[key]
			if !ok {
				f.fail(w, http.StatusNotFound, "not_found", "no such part")
				return
			}
			if r.URL.Query().Get("stat") == "1" {
				_ = json.NewEncoder(w).Encode(ObjectInfo{Key: key, Size: int64(len(data))})
				return
			}
			_, _ = w.Write(data)
		}
	default:
		f.fail(w, http.StatusNotFound, "bad_request", "no such endpoint "+path)
	}
}

func (f *fakeBridge) serveSignIn(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method == http.MethodPost && path == "/proton/v1/signin" {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "flow-1", "url": "https://account.proton.me/desktop/login#payload=x"})
		return
	}
	if path != "/proton/v1/signin/flow-1" {
		f.fail(w, http.StatusNotFound, "not_found", "no such sign-in")
		return
	}
	state := "pending"
	if f.polls < len(f.signInStates) {
		state = f.signInStates[f.polls]
	}
	f.polls++
	switch state {
	case "done":
		_ = json.NewEncoder(w).Encode(map[string]string{"state": "done", "session": `{"v":1,"uid":"u"}`, "email": "someone@proton.me"})
	case "failed":
		_ = json.NewEncoder(w).Encode(map[string]string{"state": "failed", "error": "the link expired"})
	default:
		_ = json.NewEncoder(w).Encode(map[string]string{"state": state})
	}
}

func newProtonAppForTest(t *testing.T, session string) *protonAppProvider {
	t.Helper()
	p, err := New(Config{ID: "acct", Kind: KindProtonApp, Options: map[string]string{"session": session}})
	if err != nil {
		t.Fatal(err)
	}
	return p.(*protonAppProvider)
}

func TestProtonAppStoresThroughTheApp(t *testing.T) {
	fake, _ := newFakeBridge(t)
	p := newProtonAppForTest(t, `{"v":1,"uid":"u1"}`)
	ctx := context.Background()

	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := p.Put(ctx, "abc-p1.sand", []byte("part one")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := p.Put(ctx, "abd-p2.sand", []byte("two")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := p.Get(ctx, "abc-p1.sand")
	if err != nil || string(got) != "part one" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	info, err := p.Stat(ctx, "abc-p1.sand")
	if err != nil || info.Size != 8 || info.Key != "abc-p1.sand" {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	list, err := p.List(ctx, "abc")
	if err != nil || len(list) != 1 || list[0].Key != "abc-p1.sand" {
		t.Fatalf("List(abc) = %+v, %v", list, err)
	}
	if err := p.Delete(ctx, "abc-p1.sand"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.Get(ctx, "abc-p1.sand"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
	if _, err := p.Stat(ctx, "abc-p1.sand"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat after Delete = %v, want ErrNotFound", err)
	}
	// Deleting what is not there is not an error, as for every backend.
	if err := p.Delete(ctx, "abc-p1.sand"); err != nil {
		t.Fatalf("Delete of a missing part = %v", err)
	}

	for i, s := range fake.sessions {
		if s != `{"v":1,"uid":"u1"}` {
			t.Errorf("request %d carried session %q", i, s)
		}
		if fake.folders[i] != "/my-files/sand" {
			t.Errorf("request %d carried folder %q", i, fake.folders[i])
		}
	}
}

func TestProtonAppReportsQuotaAndAccount(t *testing.T) {
	newFakeBridge(t)
	p := newProtonAppForTest(t, "s")
	u, err := p.Usage(context.Background())
	if err != nil || u.Used != 1234 || u.Total != 5_000_000_000 || u.Measured {
		t.Fatalf("Usage = %+v, %v", u, err)
	}
	who, err := p.Account(context.Background())
	if err != nil || who != "someone@proton.me" {
		t.Fatalf("Account = %q, %v", who, err)
	}
}

func TestProtonAppPassesARefreshedSessionToTheVaultOnce(t *testing.T) {
	fake, _ := newFakeBridge(t)
	p := newProtonAppForTest(t, "old")
	var saved []string
	p.OnCredentialChange(func(u map[string]string) { saved = append(saved, u["session"]) })

	fake.rotateTo = "new"
	if err := p.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0] != "new" {
		t.Fatalf("sink got %v, want [new]", saved)
	}
	if fake.sessions[1] != "new" {
		t.Fatalf("the next request carried %q, want the refreshed session", fake.sessions[1])
	}
}

func TestProtonAppTakesARefreshEvenOnAFailure(t *testing.T) {
	fake, _ := newFakeBridge(t)
	p := newProtonAppForTest(t, "old")
	var saved []string
	p.OnCredentialChange(func(u map[string]string) { saved = append(saved, u["session"]) })
	fake.rotateTo = "new"
	if _, err := p.Get(context.Background(), "missing-p1.sand"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get = %v", err)
	}
	if len(saved) != 1 {
		t.Fatalf("a refresh on the way to a 404 was dropped: %v", saved)
	}
}

func TestProtonAppSignedOutSaysSo(t *testing.T) {
	fake, _ := newFakeBridge(t)
	fake.signedOut = true
	p := newProtonAppForTest(t, "s")
	err := p.Ping(context.Background())
	if !errors.Is(err, ErrProtonSignedOut) {
		t.Fatalf("Ping = %v, want ErrProtonSignedOut", err)
	}
}

func TestProtonAppWithoutASessionDoesNotAsk(t *testing.T) {
	fake, _ := newFakeBridge(t)
	p := newProtonAppForTest(t, "")
	if err := p.Ping(context.Background()); !errors.Is(err, ErrProtonSignedOut) {
		t.Fatalf("Ping = %v, want ErrProtonSignedOut", err)
	}
	if len(fake.sessions) != 0 {
		t.Fatal("asked the app with no session to send")
	}
}

func TestProtonAppRefusesKeysOutsideTheFolder(t *testing.T) {
	newFakeBridge(t)
	p := newProtonAppForTest(t, "s")
	for _, key := range []string{"", "a/b", "..", "."} {
		if err := p.Put(context.Background(), key, []byte("x")); err == nil {
			t.Errorf("Put(%q) succeeded", key)
		}
	}
}

func TestProtonAppNeedsTheApp(t *testing.T) {
	t.Setenv(AppBridgeEnv, "")
	t.Setenv(AppBridgeTokenEnv, "")
	p := newProtonAppForTest(t, "s")
	err := p.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Android app") {
		t.Fatalf("Ping without the app = %v", err)
	}
}

func TestProtonAppIsOfferedOnlyInsideTheApp(t *testing.T) {
	offered := func() bool {
		for _, s := range Specs() {
			if s.Kind == KindProtonApp {
				return true
			}
		}
		return false
	}
	t.Setenv(AppBridgeEnv, "")
	t.Setenv(AppBridgeTokenEnv, "")
	if offered() {
		t.Fatal("offered on a machine with no app")
	}
	if _, ok := SpecFor(KindProtonApp); !ok {
		t.Fatal("an account already connected must still be known")
	}
	if !IsLocal(KindProtonApp) {
		t.Fatal("the phone's Proton must be local to the phone")
	}
	t.Setenv(AppBridgeEnv, "http://127.0.0.1:1")
	t.Setenv(AppBridgeTokenEnv, "t")
	if !offered() {
		t.Fatal("not offered inside the app")
	}
}

func TestProtonAppSignIn(t *testing.T) {
	old := protonAppPollGap
	protonAppPollGap = time.Millisecond
	t.Cleanup(func() { protonAppPollGap = old })

	fake, _ := newFakeBridge(t)
	fake.signInStates = []string{"pending", "pending", "done"}
	var link string
	res, err := ProtonAppSignIn(context.Background(), func(u string) { link = u })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, "https://account.proton.me/") {
		t.Errorf("link = %q", link)
	}
	if res.Options["session"] != `{"v":1,"uid":"u"}` || res.Email != "someone@proton.me" {
		t.Errorf("result = %+v", res)
	}
	if fake.polls != 3 {
		t.Errorf("polled %d times, want 3", fake.polls)
	}
}

func TestProtonAppSignInFailure(t *testing.T) {
	fake, _ := newFakeBridge(t)
	fake.signInStates = []string{"failed"}
	_, err := ProtonAppSignIn(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "the link expired") {
		t.Fatalf("err = %v", err)
	}
}

func TestProtonAppSignInGivesUp(t *testing.T) {
	old := protonAppPollGap
	protonAppPollGap = 5 * time.Millisecond
	t.Cleanup(func() { protonAppPollGap = old })

	newFakeBridge(t) // pending for ever
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := ProtonAppSignIn(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "not finished in time") {
		t.Fatalf("err = %v", err)
	}
}
