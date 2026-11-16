package provider

// One Proton Drive account, reached from whichever machine can reach it.
//
// Proton's own code is the only way to Proton — it publishes no Go SDK, and
// the protocol changes underneath clients that implement it themselves (see
// protoncli.go) — and there are two ways to run it: its command-line client,
// on a computer (protoncli.go), and its Drive SDK, inside the SAND Vault
// Android app (protonapp.go). Which a machine has is a fact about the
// machine, not about the account. So an account is one account, under one ID,
// on every machine that opens the vault, and each machine reaches it by the
// route it has.
//
// What each machine needs of its own is a sign-in. A Proton session's refresh
// token is single-use: two machines holding one session would each spend it
// in turn and sign the other out. And the two routes do not even share a
// session format. So the stored session is tagged with the machine that
// signed in and the route it signed in through, and a machine uses only a
// session that is its own. Every other machine sees the account as signed in
// elsewhere — not down, not pinged — and can sign in itself. Since credentials
// never travel between machines' vault files (only a recovery kit carries
// them, and then the tag says whose they are), each machine's file holds its
// own sign-in, and the parts on the account are readable from all of them.

import (
	"encoding/json"
	"strings"
)

// The two ways to Proton.
const (
	ProtonRouteCLI = "cli" // Proton's command-line client, on this machine's PATH
	ProtonRouteApp = "app" // Proton's Drive SDK, in the SAND Vault Android app
)

// ProtonRouteHere is the route this machine reaches Proton by: the app's,
// when this server was started by the app, and the client's otherwise.
func ProtonRouteHere() string {
	if AppBridgeAvailable() {
		return ProtonRouteApp
	}
	return ProtonRouteCLI
}

// protonSessionEnvelope is what the `session` secret holds: a route's own
// session, and whose it is.
type protonSessionEnvelope struct {
	V       int    `json:"sand_proton"`
	Route   string `json:"route"`
	Machine string `json:"machine"`
	Session string `json:"session"`
}

// wrapProtonSession tags a route's session with the machine it belongs to.
func wrapProtonSession(route, machine, session string) string {
	out, _ := json.Marshal(protonSessionEnvelope{V: 1, Route: route, Machine: machine, Session: session})
	return string(out)
}

// unwrapProtonSession reads a stored session. One written before sessions
// were tagged is the CLI's own, and belongs to the machine the account was
// connected on, which the account itself records (legacyMachine).
func unwrapProtonSession(stored, legacyMachine string) (route, machine, session string) {
	trimmed := strings.TrimSpace(stored)
	if trimmed == "" {
		return "", "", ""
	}
	var env protonSessionEnvelope
	if strings.HasPrefix(trimmed, "{") && json.Unmarshal([]byte(trimmed), &env) == nil && env.V == 1 && env.Route != "" {
		return env.Route, env.Machine, env.Session
	}
	return ProtonRouteCLI, legacyMachine, stored
}

// protonSignedInOn is the Spec.SignedInOn of a Proton account: the machine
// whose sign-in it holds, and whether this machine could use a sign-in made
// that way at all. Whether that machine is this one is the vault's to decide —
// it is what knows this machine's name.
func protonSignedInOn(cfg Config) (machine string, routeHere bool) {
	route, machine, session := unwrapProtonSession(cfg.Option("session"), cfg.Host)
	if session == "" {
		return "", false
	}
	return machine, route == ProtonRouteHere()
}

// protonAdoptSignIn is the Spec.AdoptSignIn of a Proton account: a sign-in
// made by this machine's route becomes this machine's, wherever it was made.
// One made by the other route — a computer's, restored onto a phone — cannot
// be used here at all, and needs a sign-in of this machine's own.
func protonAdoptSignIn(cfg Config, machine string) (Config, bool) {
	route, _, session := unwrapProtonSession(cfg.Option("session"), cfg.Host)
	if session == "" || route != ProtonRouteHere() {
		return cfg, false
	}
	out := cfg
	out.Options = make(map[string]string, len(cfg.Options))
	for k, v := range cfg.Options {
		out.Options[k] = v
	}
	out.Options["session"] = wrapProtonSession(route, machine, session)
	return out, true
}

// newProtonProvider builds a Proton account on the route this machine has.
// A session from the other route, or another machine, is not one this backend
// can use, and the vault does not ask it to (Spec.SignedInOn); built anyway,
// it answers as not signed in.
func newProtonProvider(cfg Config) (Provider, error) {
	route, machine, session := unwrapProtonSession(cfg.Option("session"), cfg.Host)
	here := ProtonRouteHere()
	if route != here {
		session = ""
	}
	wrap := func(s string) string { return wrapProtonSession(here, machine, s) }
	if here == ProtonRouteApp {
		return newProtonAppProvider(cfg, session, wrap)
	}
	return newProtonCLIRoute(cfg, session, wrap)
}
