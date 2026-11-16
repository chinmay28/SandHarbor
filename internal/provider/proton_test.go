package provider

import (
	"testing"
)

func TestProtonSessionEnvelope(t *testing.T) {
	stored := wrapProtonSession(ProtonRouteApp, "pixel", `{"v":1}`)
	route, machine, inner := unwrapProtonSession(stored, "ignored")
	if route != ProtonRouteApp || machine != "pixel" || inner != `{"v":1}` {
		t.Fatalf("unwrapped %s/%s/%s", route, machine, inner)
	}
	// A session from before the envelope is the CLI's, on the account's machine.
	route, machine, inner = unwrapProtonSession("the-cli-session", "raspberrypi")
	if route != ProtonRouteCLI || machine != "raspberrypi" || inner != "the-cli-session" {
		t.Fatalf("legacy unwrapped %s/%s/%s", route, machine, inner)
	}
	// The CLI's own session file is JSON too; it is not mistaken for an envelope.
	route, _, inner = unwrapProtonSession(`{"uid":"x","accessToken":"y"}`, "")
	if route != ProtonRouteCLI || inner != `{"uid":"x","accessToken":"y"}` {
		t.Fatalf("CLI JSON unwrapped %s/%s", route, inner)
	}
	if r, m, i := unwrapProtonSession("  ", "h"); r != "" || m != "" || i != "" {
		t.Fatal("no session should unwrap to nothing")
	}
}

func TestProtonRouteFollowsTheMachine(t *testing.T) {
	t.Setenv(AppBridgeEnv, "")
	t.Setenv(AppBridgeTokenEnv, "")
	if ProtonRouteHere() != ProtonRouteCLI {
		t.Fatal("a computer reaches Proton through the client")
	}
	p, err := New(Config{Kind: KindProtonCLI, Options: map[string]string{"session": wrapProtonSession(ProtonRouteCLI, "pi", "s")}})
	if err != nil {
		t.Fatal(err)
	}
	if cli, ok := p.(*protonCLIProvider); !ok || cli.session != "s" {
		t.Fatalf("built %T", p)
	}

	t.Setenv(AppBridgeEnv, "http://127.0.0.1:1")
	t.Setenv(AppBridgeTokenEnv, "t")
	if ProtonRouteHere() != ProtonRouteApp {
		t.Fatal("inside the app, Proton is reached through the app")
	}
	// The same account, with a computer's sign-in, on the phone: the app's
	// route, holding no session it could use.
	p, err = New(Config{Kind: KindProtonCLI, Options: map[string]string{"session": wrapProtonSession(ProtonRouteCLI, "pi", "s")}})
	if err != nil {
		t.Fatal(err)
	}
	app, ok := p.(*protonAppProvider)
	if !ok || app.session != "" {
		t.Fatalf("built %T with session %q", p, app.session)
	}
}

func TestProtonIsOneAccountSignedInPerMachine(t *testing.T) {
	t.Setenv(AppBridgeEnv, "")
	t.Setenv(AppBridgeTokenEnv, "")
	spec, ok := SpecFor(KindProtonCLI)
	if !ok || spec.Local || !spec.PerMachine || spec.SignedInOn == nil {
		t.Fatalf("spec = %+v", spec)
	}
	cases := []struct {
		name        string
		cfg         Config
		wantMachine string
		wantRoute   bool
	}{
		{"this route", Config{Kind: KindProtonCLI, Options: map[string]string{"session": wrapProtonSession(ProtonRouteCLI, "pi", "s")}}, "pi", true},
		{"the app's", Config{Kind: KindProtonCLI, Options: map[string]string{"session": wrapProtonSession(ProtonRouteApp, "pixel", "s")}}, "pixel", false},
		{"legacy", Config{Kind: KindProtonCLI, Host: "pi", Options: map[string]string{"session": "raw"}}, "pi", true},
		{"no sign-in", Config{Kind: KindProtonCLI, Options: map[string]string{}}, "", false},
	}
	for _, c := range cases {
		machine, routeHere, perMachine := SignedInOn(c.cfg)
		if !perMachine || machine != c.wantMachine || routeHere != c.wantRoute {
			t.Errorf("%s: SignedInOn = %q, %v, %v", c.name, machine, routeHere, perMachine)
		}
	}
	if _, _, perMachine := SignedInOn(Config{Kind: KindS3}); perMachine {
		t.Error("S3 is not a per-machine account")
	}
}

func TestAProtonSignInIsAdoptedOnlyByItsOwnRoute(t *testing.T) {
	t.Setenv(AppBridgeEnv, "")
	t.Setenv(AppBridgeTokenEnv, "")
	cfg := Config{Kind: KindProtonCLI, Options: map[string]string{"session": wrapProtonSession(ProtonRouteCLI, "old-pc", "s")}}
	adopted, ok := AdoptSignIn(cfg, "new-pc")
	if !ok {
		t.Fatal("the client's sign-in should pass to another computer")
	}
	if route, machine, inner := unwrapProtonSession(adopted.Options["session"], ""); route != ProtonRouteCLI || machine != "new-pc" || inner != "s" {
		t.Fatalf("adopted as %s/%s/%s", route, machine, inner)
	}
	if _, machine, _ := unwrapProtonSession(cfg.Options["session"], ""); machine != "old-pc" {
		t.Fatal("adopting changed the account it was given")
	}
	// A legacy session is the client's too.
	if _, ok := AdoptSignIn(Config{Kind: KindProtonCLI, Host: "old-pc", Options: map[string]string{"session": "raw"}}, "new-pc"); !ok {
		t.Fatal("a legacy client session should pass to another computer")
	}

	// On a phone, a computer's sign-in cannot be used.
	t.Setenv(AppBridgeEnv, "http://127.0.0.1:1")
	t.Setenv(AppBridgeTokenEnv, "t")
	if _, ok := AdoptSignIn(cfg, "phone"); ok {
		t.Fatal("the app's route cannot use the client's session")
	}
	// And every other account passes through untouched.
	s3 := Config{Kind: KindS3, Options: map[string]string{"bucket": "b"}}
	if got, ok := AdoptSignIn(s3, "phone"); !ok || got.Options["bucket"] != "b" {
		t.Fatal("a cloud has nothing to adopt")
	}
}
