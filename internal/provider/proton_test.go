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
