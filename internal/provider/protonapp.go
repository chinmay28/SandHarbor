package provider

// Proton Drive, reached through the SAND Vault Android app.
//
// The route a Proton Drive account takes on a phone running the vault (see
// proton.go for how a route is chosen). Proton's command-line client runs on
// Bun, which has no Android build; what a phone does have is the app SAND runs
// inside, and Proton publishes its Drive SDK for Android (me.proton.drive:sdk).
// So the app carries the SDK and this route asks the app.
//
// The app listens on loopback and hands its address and a token to the server
// it starts (SAND_APP_BRIDGE, SAND_APP_BRIDGE_TOKEN), so the conversation is
// between two processes of one app on one phone. The protocol is in
// docs/design/proton-android.md; the app's side is in apps/android.
//
// The session is the vault's, not the app's: it is kept as this account's
// secret, encrypted under the vault password, and sent with every request.
// The app holds it only in memory. When Proton's access token is refreshed —
// its refresh token is single-use — the app answers with the new session and
// it goes back to the vault through CredentialRotator, as Box's does.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// AppBridgeEnv and AppBridgeTokenEnv are how the Android app tells the
	// server it starts where to find it. Read once per backend built, so a
	// test can set them.
	AppBridgeEnv      = "SAND_APP_BRIDGE"
	AppBridgeTokenEnv = "SAND_APP_BRIDGE_TOKEN"

	// ProtonAppPingBudget allows for the app building its Proton client on
	// the first request after a start, which fetches and unlocks the
	// account's keys before anything else can happen.
	ProtonAppPingBudget = 90 * time.Second

	// ProtonAppSignInTimeout is how long somebody has to follow the sign-in
	// link. Proton's own CLI gives up after ten minutes of polling; the
	// server's flow lasts a little longer than the app's wait.
	ProtonAppSignInTimeout = 12 * time.Minute

	protonAppSessionHeader = "X-Sand-Proton-Session"
	protonAppFolderHeader  = "X-Sand-Proton-Folder"
)

// AppBridgeAvailable reports whether this server was started by the Android
// app, with a bridge to ask.
func AppBridgeAvailable() bool {
	_, _, ok := appBridgeFromEnv()
	return ok
}

func appBridgeFromEnv() (base, token string, ok bool) {
	base = strings.TrimRight(strings.TrimSpace(os.Getenv(AppBridgeEnv)), "/")
	token = strings.TrimSpace(os.Getenv(AppBridgeTokenEnv))
	return base, token, base != "" && token != ""
}

// appBridgeClient talks to the app over loopback, so it never goes through a
// proxy, and its own timeout is generous: a part is uploaded to Proton and
// confirmed before the app answers. Each request also carries its context.
var appBridgeClient = &http.Client{
	Timeout:   10 * time.Minute,
	Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4},
}

// ErrProtonSignedOut is what a session Proton no longer accepts becomes.
// It is the one failure a person has to act on, so it says how.
var ErrProtonSignedOut = errors.New("Proton signed this account out — sign in to it again from its Edit panel")

type protonAppProvider struct {
	base

	bridge string
	token  string
	folder string

	// mu guards session and sink. Requests are not serialized here: the app
	// keeps one Proton client per account and the SDK handles concurrency.
	mu      sync.Mutex
	session string
	sink    func(map[string]string)

	// wrap tags a session with whose it is before it goes back to the vault.
	wrap func(string) string
}

// newProtonAppProvider is the app's route, with the session already taken out
// of its envelope (proton.go); wrap puts a refreshed one back in.
func newProtonAppProvider(cfg Config, session string, wrap func(string) string) (Provider, error) {
	folder := protonCLINormalizeFolder(cfg.Option("folder"))
	if folder == "" {
		folder = protonCLIDefaultFolder
	}
	bridge, token, _ := appBridgeFromEnv()
	return &protonAppProvider{
		base:    base{cfg},
		bridge:  bridge,
		token:   token,
		folder:  folder,
		session: session,
		wrap:    wrap,
	}, nil
}

// OnCredentialChange registers where a refreshed session goes. It satisfies
// CredentialRotator.
func (p *protonAppProvider) OnCredentialChange(fn func(map[string]string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sink = fn
}

func (p *protonAppProvider) currentSession() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.session
}

// rotated takes a session the app refreshed, and passes it to the vault once.
func (p *protonAppProvider) rotated(session string) {
	if session == "" {
		return
	}
	p.mu.Lock()
	if session == p.session {
		p.mu.Unlock()
		return
	}
	p.session = session
	sink := p.sink
	p.mu.Unlock()
	if sink != nil {
		sink(map[string]string{"session": p.wrap(session)})
	}
}

// --- talking to the app ------------------------------------------------------

// appBridgeError is the app's own account of a failure.
type appBridgeError struct {
	Message string `json:"error"`
	Code    string `json:"code"`
}

// call sends one request to the app, with this account's session and folder,
// and returns the answer's body. A refreshed session in the answer is taken
// whatever the status, since a refresh can happen on the way to a failure.
func (p *protonAppProvider) call(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, error) {
	session := p.currentSession()
	if strings.TrimSpace(session) == "" {
		return nil, ErrProtonSignedOut
	}
	headers := http.Header{}
	headers.Set(protonAppSessionHeader, base64.StdEncoding.EncodeToString([]byte(session)))
	headers.Set(protonAppFolderHeader, p.folder)

	out, resp, err := appBridgeDo(ctx, p.bridge, p.token, method, path, query, headers, body)
	if resp != nil {
		if fresh := resp.Header.Get(protonAppSessionHeader); fresh != "" {
			if decoded, derr := base64.StdEncoding.DecodeString(fresh); derr == nil {
				p.rotated(string(decoded))
			}
		}
	}
	return out, err
}

// appBridgeDo is one request to the app, with the errors it can answer
// turned into ones the vault understands.
func appBridgeDo(ctx context.Context, bridge, token, method, path string, query url.Values, headers http.Header, body []byte) ([]byte, *http.Response, error) {
	if bridge == "" || token == "" {
		return nil, nil, errors.New("Proton Drive (this phone) is reached through the SAND Vault Android app, " +
			"and this server was not started by it")
	}
	target := bridge + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range headers {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}

	resp, err := appBridgeClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, fmt.Errorf("the SAND Vault app did not answer: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp, fmt.Errorf("reading the app's answer: %w", err)
	}
	if resp.StatusCode < 300 {
		return data, resp, nil
	}

	var failure appBridgeError
	_ = json.Unmarshal(data, &failure)
	msg := strings.TrimSpace(failure.Message)
	if msg == "" {
		msg = strings.TrimSpace(string(data))
	}
	if msg == "" {
		msg = resp.Status
	}
	switch {
	case failure.Code == "not_found" || (failure.Code == "" && resp.StatusCode == http.StatusNotFound):
		return nil, resp, ErrNotFound
	case failure.Code == "signed_out":
		return nil, resp, fmt.Errorf("%w (%s)", ErrProtonSignedOut, msg)
	case failure.Code == "proton":
		return nil, resp, fmt.Errorf("Proton Drive: %s", msg)
	default:
		return nil, resp, fmt.Errorf("the SAND Vault app: %s", msg)
	}
}

func objectPath(key string) string {
	return "/proton/v1/objects/" + url.PathEscape(key)
}

// --- the object store --------------------------------------------------------

func (p *protonAppProvider) Put(ctx context.Context, key string, data []byte) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if data == nil {
		data = []byte{}
	}
	_, err := p.call(ctx, http.MethodPut, objectPath(key), nil, data)
	return err
}

func (p *protonAppProvider) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	return p.call(ctx, http.MethodGet, objectPath(key), nil, nil)
}

func (p *protonAppProvider) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := ValidateKey(key); err != nil {
		return ObjectInfo{}, err
	}
	out, err := p.call(ctx, http.MethodGet, objectPath(key), url.Values{"stat": {"1"}}, nil)
	if err != nil {
		return ObjectInfo{}, err
	}
	var info ObjectInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return ObjectInfo{}, fmt.Errorf("the SAND Vault app said something unexpected: %w", err)
	}
	if info.Key == "" {
		info.Key = key
	}
	return info, nil
}

func (p *protonAppProvider) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	_, err := p.call(ctx, http.MethodDelete, objectPath(key), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (p *protonAppProvider) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	out, err := p.call(ctx, http.MethodGet, "/proton/v1/objects", url.Values{"prefix": {prefix}}, nil)
	if err != nil {
		return nil, err
	}
	var objects []ObjectInfo
	if err := json.Unmarshal(out, &objects); err != nil {
		return nil, fmt.Errorf("the SAND Vault app said something unexpected: %w", err)
	}
	// The app filters by prefix; this is the backstop that keeps a listing
	// honest if it ever does not.
	kept := objects[:0]
	for _, o := range objects {
		if strings.HasPrefix(o.Key, prefix) {
			kept = append(kept, o)
		}
	}
	return kept, nil
}

// Ping checks the app is there, the session is good and the folder exists,
// creating it if not — which is how an account connects for the first time.
func (p *protonAppProvider) Ping(ctx context.Context) error {
	_, err := p.call(ctx, http.MethodGet, "/proton/v1/ping", nil, nil)
	return err
}

// PingBudget satisfies PingBudgeter; see ProtonAppPingBudget.
func (p *protonAppProvider) PingBudget() time.Duration { return ProtonAppPingBudget }

// Usage is the account's own quota, which Proton reports — unlike the CLI,
// which has no quota command. It satisfies UsageReporter.
func (p *protonAppProvider) Usage(ctx context.Context) (Usage, error) {
	out, err := p.call(ctx, http.MethodGet, "/proton/v1/usage", nil, nil)
	if err != nil {
		return Usage{}, err
	}
	var u struct {
		Used  int64 `json:"used"`
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(out, &u); err != nil {
		return Usage{}, fmt.Errorf("the SAND Vault app said something unexpected: %w", err)
	}
	return Usage{Used: u.Used, Total: u.Total}, nil
}

// Account names the signed-in address. It satisfies Identifier.
func (p *protonAppProvider) Account(ctx context.Context) (string, error) {
	out, err := p.call(ctx, http.MethodGet, "/proton/v1/account", nil, nil)
	if err != nil {
		return "", err
	}
	var a struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(out, &a); err != nil {
		return "", fmt.Errorf("the SAND Vault app said something unexpected: %w", err)
	}
	if strings.TrimSpace(a.Email) == "" {
		return "", errors.New("the SAND Vault app did not say which Proton account this is")
	}
	return a.Email, nil
}

// --- signing in --------------------------------------------------------------

// ProtonAppSignInResult is what a finished sign-in gives the vault.
type ProtonAppSignInResult struct {
	Options map[string]string // the session, as this account's secret
	Email   string            // whom it signed in as, to name the account
}

// ProtonAppSignIn has the app start Proton's sign-in by link, reports the
// link through onURL as soon as there is one, and waits for somebody to
// follow it. The app does the waiting on Proton; this asks the app how it is
// going until it says done, failed, or ctx ends.
func ProtonAppSignIn(ctx context.Context, machine string, onURL func(string)) (ProtonAppSignInResult, error) {
	bridge, token, _ := appBridgeFromEnv()
	out, _, err := appBridgeDo(ctx, bridge, token, http.MethodPost, "/proton/v1/signin", nil, nil, []byte("{}"))
	if err != nil {
		return ProtonAppSignInResult{}, err
	}
	var started struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(out, &started); err != nil || started.ID == "" || started.URL == "" {
		return ProtonAppSignInResult{}, errors.New("the SAND Vault app did not start a Proton sign-in")
	}
	if onURL != nil {
		onURL(started.URL)
	}

	for {
		// The app holds each of these open while nothing has happened, so
		// this is a long poll rather than a busy one.
		out, _, err := appBridgeDo(ctx, bridge, token, http.MethodGet,
			"/proton/v1/signin/"+url.PathEscape(started.ID), nil, nil, nil)
		if err != nil {
			if ctx.Err() != nil {
				return ProtonAppSignInResult{}, fmt.Errorf("the Proton sign-in was not finished in time — start it again")
			}
			return ProtonAppSignInResult{}, err
		}
		var state struct {
			State   string `json:"state"`
			Session string `json:"session"`
			Email   string `json:"email"`
			Error   string `json:"error"`
		}
		if err := json.Unmarshal(out, &state); err != nil {
			return ProtonAppSignInResult{}, fmt.Errorf("the SAND Vault app said something unexpected: %w", err)
		}
		switch state.State {
		case "pending":
			// Not a busy loop even if the app answers at once: wait a beat.
			select {
			case <-ctx.Done():
				return ProtonAppSignInResult{}, fmt.Errorf("the Proton sign-in was not finished in time — start it again")
			case <-time.After(protonAppPollGap):
			}
		case "done":
			if strings.TrimSpace(state.Session) == "" {
				return ProtonAppSignInResult{}, errors.New("the Proton sign-in finished without a session")
			}
			return ProtonAppSignInResult{
				// This machine's own sign-in (proton.go).
				Options: map[string]string{"session": wrapProtonSession(ProtonRouteApp, machine, state.Session)},
				Email:   strings.TrimSpace(state.Email),
			}, nil
		case "failed":
			msg := strings.TrimSpace(state.Error)
			if msg == "" {
				msg = "Proton did not say why"
			}
			return ProtonAppSignInResult{}, fmt.Errorf("the Proton sign-in failed: %s", msg)
		default:
			return ProtonAppSignInResult{}, fmt.Errorf("the SAND Vault app reported a sign-in state %q", state.State)
		}
	}
}

// protonAppPollGap is the pause between two answers of "pending"; a variable
// so a test need not wait it out.
var protonAppPollGap = time.Second
