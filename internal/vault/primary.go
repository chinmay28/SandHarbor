package vault

import (
	"fmt"
	"strings"
	"time"
)

// One machine does the background work.
//
// A vault opened on a Raspberry Pi and on a phone has one set of standing
// instructions — replicate these folders, check those parts every Sunday,
// move the files still on the old key — and they live in the shared index, so
// every machine that unlocks the vault sees them. Left to itself, every one of
// those machines would carry them out: the same replica made twice, the same
// folder checked twice, and a phone, which can reach none of the disks plugged
// into the Pi, failing on every file whose parts are on them.
//
// So the index names a primary: the one machine that runs the background
// work. Every other machine leaves it alone. What somebody starts by hand —
// a check, a repair, a sweep — still runs wherever they started it; this is
// only about the work nobody is watching.
//
// Until a primary is chosen, every machine that is not a phone runs it, as
// before. A phone never does, and cannot be chosen: it sleeps, it roams onto
// mobile data, and the disks are elsewhere.

// Primary is the choice of machine, as the index carries it.
type Primary struct {
	// Name is the machine's instance name (InstanceName). Empty records that
	// the choice was taken back: dated like any other choice, so that a copy
	// of the index from before it does not bring the old primary back.
	Name string `json:"name"`

	// SetAt is when it was chosen, which is how two machines that chose
	// differently agree: the later choice wins (newerPrimary).
	SetAt time.Time `json:"set_at"`

	// SetOn is the machine the choice was made on, for the panel to say.
	SetOn string `json:"set_on,omitempty"`
}

// PrimaryStatus is what the machines panel shows about background work.
type PrimaryStatus struct {
	// Primary is the machine chosen, "" when none has been.
	Primary string `json:"primary"`
	// SetAt and SetOn say when and where it was chosen.
	SetAt time.Time `json:"set_at,omitempty"`
	SetOn string    `json:"set_on,omitempty"`
	// RunsHere says whether this machine runs the background work.
	RunsHere bool `json:"runs_here"`
	// Phone says whether this machine is a phone.
	Phone bool `json:"phone"`
}

// SetPhone marks this machine as a phone, before the vault is unlocked: the
// Android app's server is started so (sand serve --phone).
func (v *Vault) SetPhone(phone bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.phone = phone
}

// Phone reports whether this machine is a phone.
func (v *Vault) Phone() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.phone
}

// RunsBackgroundWork reports whether this machine should carry out the
// vault's standing instructions now. False while the vault is locked.
func (v *Vault) RunsBackgroundWork() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil || v.manifest == nil {
		return false
	}
	return v.runsBackgroundWorkLocked()
}

func (v *Vault) runsBackgroundWorkLocked() bool {
	if v.phone {
		return false
	}
	if p := v.manifest.Primary; p != nil && p.Name != "" {
		return p.Name == v.instance
	}
	return true
}

// PrimaryStatus says which machine runs the background work, and whether it
// is this one.
func (v *Vault) PrimaryStatus() (PrimaryStatus, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dataKey == nil {
		return PrimaryStatus{}, ErrLocked
	}
	out := PrimaryStatus{RunsHere: v.runsBackgroundWorkLocked(), Phone: v.phone}
	if p := v.manifest.Primary; p != nil {
		out.Primary, out.SetAt, out.SetOn = p.Name, p.SetAt, p.SetOn
	}
	return out, nil
}

// SetPrimary makes the machine called name the one that runs the background
// work, or, with name empty, goes back to having none chosen. The machine must
// be this one or one in the register, and must not be a phone.
func (v *Vault) SetPrimary(name string) (PrimaryStatus, error) {
	name = strings.TrimSpace(name)

	v.mu.Lock()
	if v.dataKey == nil {
		v.mu.Unlock()
		return PrimaryStatus{}, ErrLocked
	}
	if name != "" {
		phone, known := v.phone, name == v.instance
		if !known {
			if inst := v.manifest.Instances[name]; inst != nil {
				phone, known = inst.Phone, true
			}
		}
		if !known {
			v.mu.Unlock()
			return PrimaryStatus{}, fmt.Errorf("no machine called %q has opened this vault", name)
		}
		if phone {
			v.mu.Unlock()
			return PrimaryStatus{}, fmt.Errorf("%s is a phone, and a phone cannot be the primary", name)
		}
	}

	previous := v.manifest.Primary
	v.manifest.Primary = &Primary{Name: name, SetAt: time.Now().UTC(), SetOn: v.instance}
	if err := v.persistLocked(); err != nil {
		v.manifest.Primary = previous
		v.mu.Unlock()
		return PrimaryStatus{}, err
	}
	v.mu.Unlock()
	return v.PrimaryStatus()
}

// newerPrimary is the choice two copies of the index agree on: the later,
// a choice taken back included.
func newerPrimary(a, b *Primary) *Primary {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case b.SetAt.After(a.SetAt):
		return b
	default:
		return a
	}
}
