package vault

import (
	"context"
	"fmt"
	"testing"

	"github.com/chinmay28/SandHarbor/internal/provider"
)

// One Proton Drive account serves every machine that opens the vault, each by
// its own sign-in (provider/proton.go). These pin what the vault does with
// that: reach it where this machine's sign-in is, and only there.

func protonSession(route, machine string) string {
	return fmt.Sprintf(`{"sand_proton":1,"route":%q,"machine":%q,"session":"s"}`, route, machine)
}

func TestAProtonAccountIsReachedWhereItIsSignedIn(t *testing.T) {
	// A computer: no app bridge, so Proton's route here is the client's.
	t.Setenv(provider.AppBridgeEnv, "")
	t.Setenv(provider.AppBridgeTokenEnv, "")

	v, _ := newTestVaultOn(t, "pi", 1)
	proton := provider.Config{
		ID: "proton-1", Kind: provider.KindProtonCLI, Name: "proton",
		Options: map[string]string{"folder": "/my-files/sand", "session": protonSession(provider.ProtonRouteCLI, "pi")},
	}
	v.mu.Lock()
	v.providers = append(v.providers, proton)
	v.mu.Unlock()

	if !v.reachableHere(proton) || v.livesElsewhere(proton) {
		t.Fatal("on the machine that signed in, the account is reachable")
	}
	if _, ok := v.SignInElsewhere(proton); ok {
		t.Fatal("nothing to offer on the machine that signed in")
	}

	// The same vault file opened on another computer, as a kit carries it:
	// the pi's sign-in is not this machine's to spend.
	v.SetInstance("desktop")
	if v.reachableHere(proton) || !v.livesElsewhere(proton) {
		t.Fatal("another machine must not use the pi's sign-in")
	}
	on, ok := v.SignInElsewhere(proton)
	if !ok || on != "pi" {
		t.Fatalf("SignInElsewhere = %q, %v; want pi, true", on, ok)
	}
	v.mu.RLock()
	away := v.awayLocked(proton)
	v.mu.RUnlock()
	if away != "pi" {
		t.Fatalf("away = %q, want pi", away)
	}

	// Not pinged, not down: offered a sign-in here.
	statuses, err := v.ProviderStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got *ProviderStatus
	for i := range statuses {
		if statuses[i].ID == "proton-1" {
			got = &statuses[i]
		}
	}
	if got == nil || !got.SignInHere || got.Elsewhere != "pi" || got.Error != "" {
		t.Fatalf("status = %+v", got)
	}

	// This machine signs in for itself: reachable here from then on.
	v.mu.Lock()
	for i := range v.providers {
		if v.providers[i].ID == "proton-1" {
			v.providers[i].Options["session"] = protonSession(provider.ProtonRouteCLI, "desktop")
			proton = v.providers[i]
		}
	}
	v.mu.Unlock()
	if !v.reachableHere(proton) || v.livesElsewhere(proton) {
		t.Fatal("after signing in here, the account is reachable here")
	}
}

func TestAProtonSignInByTheOtherRouteIsNotUsedHere(t *testing.T) {
	t.Setenv(provider.AppBridgeEnv, "")
	t.Setenv(provider.AppBridgeTokenEnv, "")
	v, _ := newTestVaultOn(t, "pixel", 1)
	// Signed in by the app, on this very machine's name, but this server was
	// not started by the app: the session is the app's format, not the
	// client's, and cannot be used here.
	proton := provider.Config{ID: "p", Kind: provider.KindProtonCLI,
		Options: map[string]string{"session": protonSession(provider.ProtonRouteApp, "pixel")}}
	if v.reachableHere(proton) || !v.livesElsewhere(proton) {
		t.Fatal("a session for the other route is not usable here")
	}
}

func TestALegacyProtonAccountStaysWithItsMachine(t *testing.T) {
	t.Setenv(provider.AppBridgeEnv, "")
	t.Setenv(provider.AppBridgeTokenEnv, "")
	v, _ := newTestVaultOn(t, "pi", 1)
	// Connected by a build from before the envelope: the client's raw session,
	// and the machine stamped on the account.
	legacy := provider.Config{ID: "p", Kind: provider.KindProtonCLI, Host: "pi",
		Options: map[string]string{"session": "raw-cli-session"}}
	if !v.reachableHere(legacy) {
		t.Fatal("an existing account must keep working on its machine")
	}
	v.SetInstance("phone")
	if v.reachableHere(legacy) {
		t.Fatal("and nowhere else")
	}
	if on, ok := v.SignInElsewhere(legacy); !ok || on != "pi" {
		t.Fatalf("SignInElsewhere = %q, %v", on, ok)
	}
	// From before machine names: taken to be this machine's, as before.
	unnamed := legacy
	unnamed.Host = ""
	if !v.reachableHere(unnamed) {
		t.Fatal("an account from before machine names is this machine's")
	}
}

func TestOtherAccountsAreUnaffected(t *testing.T) {
	v, _ := newTestVaultOn(t, "pi", 1)
	s3 := provider.Config{ID: "b", Kind: provider.KindS3}
	if !v.reachableHere(s3) || v.livesElsewhere(s3) {
		t.Fatal("a cloud is reachable everywhere")
	}
	if _, ok := v.SignInElsewhere(s3); ok {
		t.Fatal("a cloud has no per-machine sign-in")
	}
}
