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

// kitWithProton is a vault on "pc" with three folders, a file, and a Proton
// account signed in on the pc by the client, exported as a kit.
func kitWithProton(t *testing.T) (*Kit, string) {
	t.Helper()
	ctx := context.Background()
	pc, _ := newTestVaultOn(t, "pc", 3)
	if _, _, err := pc.Upload(ctx, MainScope, "/", "notes.txt", []byte("x"), UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	pc.mu.Lock()
	pc.providers = append(pc.providers, provider.Config{
		ID: "proton-1", Kind: provider.KindProtonCLI, Name: "proton",
		Options: map[string]string{"folder": "/my-files/sand", "session": protonSession(provider.ProtonRouteCLI, "pc")},
	})
	if err := pc.persistLocked(); err != nil {
		t.Fatal(err)
	}
	pc.mu.Unlock()
	fingerprint, zipped := exportTestKit(t, pc)
	pc.AwaitBackupSync()
	return openTestKit(t, zipped, fingerprint.Code), "proton-1"
}

func protonResult(t *testing.T, report *KitImportReport, id string) KitAccountResult {
	t.Helper()
	for _, a := range report.Accounts {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no result for %s in %+v", id, report.Accounts)
	return KitAccountResult{}
}

func storedConfig(t *testing.T, v *Vault, id string) provider.Config {
	t.Helper()
	v.mu.RLock()
	defer v.mu.RUnlock()
	cfg, ok := v.configForLocked(id)
	if !ok {
		t.Fatalf("no account %s", id)
	}
	return cfg
}

// Restored in place of the machine the kit came from: that machine is gone,
// so its Proton sign-in is this machine's now — the restore works as it
// always has, as best it can.
func TestARestoreInPlaceTakesOverTheProtonSignIn(t *testing.T) {
	t.Setenv(provider.AppBridgeEnv, "")
	t.Setenv(provider.AppBridgeTokenEnv, "")
	kit, id := kitWithProton(t)

	restored := freshVault(t)
	restored.SetInstance("new-pc")
	report, err := restored.ImportKit(context.Background(), kit, KitImportOptions{Password: "new password"})
	if err != nil {
		t.Fatalf("ImportKit: %v", err)
	}
	if got := protonResult(t, report, id); got.Status == KitAccountSignInHere || got.Status == KitAccountElsewhere {
		t.Fatalf("a restore in place left the Proton account %q: %s", got.Status, got.Detail)
	}
	cfg := storedConfig(t, restored, id)
	if !restored.reachableHere(cfg) {
		t.Fatal("the restored Proton account is not this machine's")
	}
	if machine, _, _ := provider.SignedInOn(cfg); machine != "new-pc" {
		t.Fatalf("the sign-in is %q's, want the restoring machine's", machine)
	}
}

// Joined as a second machine: the pc goes on using its sign-in, so this one
// is told to sign in for itself, with the button to do it.
func TestASecondMachineIsToldToSignInToProton(t *testing.T) {
	t.Setenv(provider.AppBridgeEnv, "")
	t.Setenv(provider.AppBridgeTokenEnv, "")
	kit, id := kitWithProton(t)

	pi := freshVault(t)
	pi.SetInstance("pi")
	report, err := pi.ImportKit(context.Background(), kit, KitImportOptions{Password: testPassword, Sibling: true})
	if err != nil {
		t.Fatalf("ImportKit: %v", err)
	}
	got := protonResult(t, report, id)
	if got.Status != KitAccountSignInHere || got.Repair != KitRepairSignIn {
		t.Fatalf("Proton on a second machine reads %q / %q: %s", got.Status, got.Repair, got.Detail)
	}
	if machine, _, _ := provider.SignedInOn(storedConfig(t, pi, id)); machine != "pc" {
		t.Fatalf("the pc's sign-in was taken over (now %q's); the pc would be signed out", machine)
	}
	// The local folders are the pc's, as before.
	for _, a := range report.Accounts {
		if a.ID != id && a.Status != KitAccountElsewhere {
			t.Errorf("folder %s reads %q, want elsewhere", a.Name, a.Status)
		}
	}
}

// A computer's sign-in restored onto a phone cannot be used there at all —
// the app's route does not read the client's session — so even a restore in
// place asks for a sign-in.
func TestAProtonSignInFromTheOtherRouteAsksForASignIn(t *testing.T) {
	t.Setenv(provider.AppBridgeEnv, "")
	t.Setenv(provider.AppBridgeTokenEnv, "")
	kit, id := kitWithProton(t)

	t.Setenv(provider.AppBridgeEnv, "http://127.0.0.1:1")
	t.Setenv(provider.AppBridgeTokenEnv, "t")
	phone := freshVault(t)
	phone.SetInstance("phone")
	report, err := phone.ImportKit(context.Background(), kit, KitImportOptions{Password: "new password", SkipCloudIndex: true})
	if err != nil {
		t.Fatalf("ImportKit: %v", err)
	}
	got := protonResult(t, report, id)
	if got.Status != KitAccountSignInHere || got.Repair != KitRepairSignIn {
		t.Fatalf("Proton from a computer on a phone reads %q / %q: %s", got.Status, got.Repair, got.Detail)
	}
}
