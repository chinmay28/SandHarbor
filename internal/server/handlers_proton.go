package server

// Signing in to Proton Drive from the browser.
//
// Every other backend that signs in does it by redirect: SAND sends the browser
// to the provider, the provider sends it back with a code, and the code becomes
// credentials. Proton's client does not work that way. It prints a URL and
// blocks until somebody has been to it — there is no redirect to catch, and the
// URL can perfectly well be opened on a different device from the one running
// the browser SAND is being driven from, which is exactly what a headless box
// needs.
//
// So the shape here is: start it, show the link, and poll. The polling, the
// flow store and the two endpoints that turn a finished sign-in into an account
// are the OAuth ones unchanged — a finished flow is a finished flow whatever
// produced it, and reusing them means a Proton account is connected, named and
// repaired by the same code as every other.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/chinmay28/SandHarbor/internal/provider"
)

// handleProtonSignIn starts a Proton sign-in and returns straight away with a
// flow to poll. The client is left running in the background: it is waiting for
// somebody to visit the URL, and that wait is measured in minutes.
func (s *Server) handleProtonSignIn(w http.ResponseWriter, r *http.Request) {
	// An account signing back in does it with its own settings: its folder, its
	// client, and above all its own state directory, so that the session lands
	// where that account's client will look for it.
	cfg, ok := s.linkSignInConfig(w, r, provider.KindProtonCLI, "folder", "binary", "state_dir")
	if !ok {
		return
	}

	// The sign-in is this machine's own (provider/proton.go): tagged with its
	// name, so that another machine opening the vault never uses it.
	machine := ""
	if v, err := s.Vault(); err == nil {
		machine = v.Instance()
	}

	// Inside the Android app, Proton is reached through the app, which does
	// the signing in itself.
	if provider.ProtonRouteHere() == provider.ProtonRouteApp {
		s.startLinkSignIn(w, r, cfg, provider.ProtonAppSignInTimeout, func(flowID string) {
			ctx, cancel := context.WithTimeout(context.Background(), provider.ProtonAppSignInTimeout)
			defer cancel()
			res, err := provider.ProtonAppSignIn(ctx, machine, func(url string) {
				s.oauthFlows.setSignInURL(flowID, url)
			})
			if err != nil {
				s.oauthFlows.finish(flowID, nil, "", err.Error())
				return
			}
			s.oauthFlows.finish(flowID, res.Options, res.Email, "")
		})
		return
	}

	// Fail before the flow exists if the client is not installed, so the answer
	// is a sentence naming the fix rather than a flow that polls its way to the
	// same news a moment later.
	if err := protonClientAvailable(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "PROTON_NO_CLIENT")
		return
	}

	s.startLinkSignIn(w, r, cfg, provider.ProtonCLILoginTimeout, func(flowID string) {
		s.runProtonSignIn(flowID, cfg, machine)
	})
}

// linkSignInConfig reads a request to start a sign-in by link: the settings
// the connect form has so far, or — for an account signing back in — that
// account's own, as named by carry. It answers the request itself when it
// cannot go on.
func (s *Server) linkSignInConfig(w http.ResponseWriter, r *http.Request, kind provider.Kind, carry ...string) (provider.Config, bool) {
	var req struct {
		// Options are what the connect form has been filled in with so far.
		// They travel with the flow so that finishing it does not have to ask
		// again.
		Options map[string]string `json:"options"`

		// ProviderID turns this into a repair of an account already connected,
		// which is the way back from a session Proton has expired or the
		// account holder has revoked.
		ProviderID string `json:"provider_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "BAD_REQUEST")
		return provider.Config{}, false
	}

	options := map[string]string{}
	for key, value := range req.Options {
		options[key] = value
	}

	if req.ProviderID != "" {
		v, err := s.Vault()
		if err != nil {
			vaultErrorResponse(w, err)
			return provider.Config{}, false
		}
		accounts, err := v.Providers()
		if err != nil {
			vaultErrorResponse(w, err)
			return provider.Config{}, false
		}
		var found *provider.Config
		for i := range accounts {
			if accounts[i].ID == req.ProviderID {
				found = &accounts[i]
				break
			}
		}
		if found == nil {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("no connected account with id %s", req.ProviderID), "BAD_REQUEST")
			return provider.Config{}, false
		}
		if found.Kind != kind {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(
				"%s is a %s account, and only a %s one signs in this way",
				found.Name, found.Kind, kind), "BAD_REQUEST")
			return provider.Config{}, false
		}
		for _, key := range carry {
			if value := found.Option(key); value != "" {
				options[key] = value
			}
		}
	}

	// The session an account already has is never carried into a sign-in. A
	// client would see it, decide it is signed in and return without asking
	// anybody anything — which is precisely wrong when the reason for signing
	// in again is that the old session stopped working. It is also a secret,
	// and this one arrived from a browser that was only ever shown a
	// placeholder.
	delete(options, "session")

	return provider.Config{ID: req.ProviderID, Kind: kind, Options: options}, true
}

// startLinkSignIn starts the flow the browser polls, runs the sign-in in the
// background, and answers with the flow.
func (s *Server) startLinkSignIn(w http.ResponseWriter, r *http.Request, cfg provider.Config, lasts time.Duration, run func(flowID string)) {
	flow, err := s.oauthFlows.start(&oauthFlow{
		Kind:       cfg.Kind,
		ProviderID: cfg.ID,
		Session:    sessionToken(r),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "INTERNAL")
		return
	}

	// Deliberately not the request's context. The request is answered in a
	// moment and the sign-in goes on for as long as somebody takes to find
	// their password manager; tying the two together would cancel the sign-in
	// the instant the browser got its flow ID.
	go run(flow.ID)

	writeJSON(w, http.StatusOK, map[string]any{
		"flow_id":    flow.ID,
		"expires_in": int(lasts.Seconds()),
	})
}

// runProtonSignIn drives the client and records what came of it against the
// flow, for the browser to pick up on its next poll.
func (s *Server) runProtonSignIn(flowID string, cfg provider.Config, machine string) {
	ctx, cancel := context.WithTimeout(context.Background(), provider.ProtonCLILoginTimeout)
	defer cancel()

	options, err := provider.ProtonCLISignIn(ctx, cfg, machine, func(url string) {
		s.oauthFlows.setSignInURL(flowID, url)
	})
	if err != nil {
		s.oauthFlows.finish(flowID, nil, "", err.Error())
		return
	}

	// Ask the account what it is called, so the connect dialog can suggest a
	// name rather than making somebody invent one. A failure here is not worth
	// losing a sign-in over — the session is good, and an unnamed account is a
	// smaller problem than no account.
	account := protonAccountName(ctx, cfg, options)

	s.oauthFlows.finish(flowID, options, account, "")
}

// protonAccountName is the signed-in address, or nothing.
func protonAccountName(ctx context.Context, cfg provider.Config, options map[string]string) string {
	merged := map[string]string{}
	for key, value := range cfg.Options {
		merged[key] = value
	}
	for key, value := range options {
		merged[key] = value
	}

	live, err := provider.New(provider.Config{ID: cfg.ID, Kind: provider.KindProtonCLI, Options: merged})
	if err != nil {
		return ""
	}
	namer, ok := live.(provider.Identifier)
	if !ok {
		return ""
	}
	// One question to the client, which costs what a Ping costs — see
	// provider.ProtonCLIPingBudget. Cut shorter, the account was named "Proton
	// Drive 2" on any machine where the client takes its time.
	ctx, cancel := context.WithTimeout(ctx, provider.ProtonCLIPingBudget)
	defer cancel()

	name, err := namer.Account(ctx)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}

// protonClientAvailable reports whether there is a Proton client to sign in
// with, by asking the backend to find the one this account is configured for.
func protonClientAvailable(cfg provider.Config) error {
	_, err := provider.ProtonCLIClientPath(cfg)
	return err
}
