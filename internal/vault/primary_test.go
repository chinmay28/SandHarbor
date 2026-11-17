package vault

import (
	"errors"
	"testing"
	"time"
)

// A machine the register knows about, without it having to be opened here.
func addPeer(v *Vault, name string, phone bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.manifest.Instances == nil {
		v.manifest.Instances = map[string]*Instance{}
	}
	v.manifest.Instances[name] = &Instance{Name: name, LastSeen: time.Now(), Phone: phone}
}

// With no primary chosen, a computer runs the background work as it always
// did and a phone never does.
func TestWithNoPrimaryComputersRunTheWorkAndPhonesDoNot(t *testing.T) {
	computer, _ := newTestVaultOn(t, "pi", 1)
	if !computer.RunsBackgroundWork() {
		t.Error("a computer with no primary chosen does not run the background work")
	}

	phone, _ := newTestVaultOn(t, "pixel", 1)
	phone.SetPhone(true)
	if phone.RunsBackgroundWork() {
		t.Error("a phone runs the background work")
	}
	status, err := phone.PrimaryStatus()
	if err != nil || status.RunsHere || !status.Phone || status.Primary != "" {
		t.Errorf("phone status = %+v, %v", status, err)
	}
}

// Once a primary is chosen, it and only it runs the work.
func TestOnlyThePrimaryRunsTheWork(t *testing.T) {
	v, _ := newTestVaultOn(t, "pi", 1)
	addPeer(v, "desk", false)

	status, err := v.SetPrimary("desk")
	if err != nil {
		t.Fatalf("SetPrimary(desk): %v", err)
	}
	if status.Primary != "desk" || status.SetOn != "pi" || status.RunsHere {
		t.Errorf("after choosing desk: %+v", status)
	}
	if v.RunsBackgroundWork() {
		t.Error("pi runs the work with desk the primary")
	}

	if _, err := v.SetPrimary("pi"); err != nil {
		t.Fatalf("SetPrimary(pi): %v", err)
	}
	if !v.RunsBackgroundWork() {
		t.Error("the primary does not run the work")
	}

	// Taken back: every computer again.
	status, err = v.SetPrimary("")
	if err != nil || status.Primary != "" || !status.RunsHere {
		t.Errorf("after clearing: %+v, %v", status, err)
	}
}

// The choice survives the vault being locked and opened again, which is to
// say it is in the index.
func TestThePrimaryIsKeptInTheIndex(t *testing.T) {
	v, _ := newTestVaultOn(t, "pi", 1)
	addPeer(v, "desk", false)
	if _, err := v.SetPrimary("desk"); err != nil {
		t.Fatalf("SetPrimary: %v", err)
	}
	v.Lock()
	if v.RunsBackgroundWork() {
		t.Error("a locked vault runs background work")
	}
	if _, err := v.PrimaryStatus(); !errors.Is(err, ErrLocked) {
		t.Errorf("PrimaryStatus while locked: %v", err)
	}
	if err := v.Unlock(testPassword); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	status, err := v.PrimaryStatus()
	if err != nil || status.Primary != "desk" {
		t.Errorf("after unlocking again: %+v, %v", status, err)
	}
}

// A phone cannot be chosen, whether it is this machine or another, and
// neither can a machine nobody has heard of.
func TestAPhoneOrAStrangerCannotBeThePrimary(t *testing.T) {
	v, _ := newTestVaultOn(t, "pi", 1)
	addPeer(v, "pixel", true)

	if _, err := v.SetPrimary("pixel"); err == nil {
		t.Error("a phone in the register was made the primary")
	}
	if _, err := v.SetPrimary("nowhere"); err == nil {
		t.Error("a machine that never opened the vault was made the primary")
	}

	phone, _ := newTestVaultOn(t, "pixel", 1)
	phone.SetPhone(true)
	if _, err := phone.SetPrimary("pixel"); err == nil {
		t.Error("a phone made itself the primary")
	}
	// Even named in the index by a machine that did not know better.
	phone.mu.Lock()
	phone.manifest.Primary = &Primary{Name: "pixel", SetAt: time.Now()}
	phone.mu.Unlock()
	if phone.RunsBackgroundWork() {
		t.Error("a phone named the primary runs the work")
	}
}

// The register says which machines are phones, so the others can refuse them.
func TestTheRegisterSaysWhichMachinesArePhones(t *testing.T) {
	phone, _ := newTestVaultOn(t, "pixel", 1)
	phone.SetPhone(true)
	phone.mu.Lock()
	phone.registerSelfLocked(time.Now())
	inst := phone.manifest.Instances["pixel"]
	phone.mu.Unlock()
	if inst == nil || !inst.Phone {
		t.Fatalf("register entry = %+v", inst)
	}
	list, err := phone.Instances()
	if err != nil || len(list) == 0 || !list[0].Phone {
		t.Errorf("Instances() = %+v, %v", list, err)
	}
}

// Two machines that chose differently agree on the later choice, whichever
// way the merge goes — and a choice taken back beats an older one.
func TestMergeKeepsTheLaterPrimary(t *testing.T) {
	early := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)

	local := index(map[string]uint64{"pi": 2})
	local.Primary = &Primary{Name: "pi", SetAt: early}
	remote := index(map[string]uint64{"desk": 1})
	remote.Primary = &Primary{Name: "desk", SetAt: late}

	merged, _ := mergeIndexes(local, remote)
	if merged.Primary == nil || merged.Primary.Name != "desk" {
		t.Errorf("diverged merge chose %+v", merged.Primary)
	}
	merged, _ = mergeIndexes(remote, local)
	if merged.Primary == nil || merged.Primary.Name != "desk" {
		t.Errorf("the other way round chose %+v", merged.Primary)
	}

	// One side strictly ahead still carries the other's later choice.
	ahead := index(map[string]uint64{"pi": 3, "desk": 1})
	ahead.Primary = &Primary{Name: "pi", SetAt: early}
	behind := index(map[string]uint64{"pi": 1, "desk": 1})
	behind.Primary = &Primary{Name: "", SetAt: late}
	merged, _ = mergeIndexes(ahead, behind)
	if merged.Primary == nil || merged.Primary.Name != "" {
		t.Errorf("a later clearing was lost: %+v", merged.Primary)
	}
	if ahead.Primary.Name != "pi" {
		t.Error("the merge wrote into its input")
	}
}
