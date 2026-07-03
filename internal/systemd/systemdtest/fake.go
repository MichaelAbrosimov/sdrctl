// Package systemdtest provides a programmable fake systemctl runner shared
// by core and api tests: per-verb errors, per-verb hangs (block until the
// context is cancelled) and a "sticky enabled" mode simulating a disable
// that reports success without clearing the enabled flag.
package systemdtest

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/MichaelAbrosimov/sdrctl/internal/systemd"
)

// Unit models one systemd unit in the fake table.
type Unit struct {
	Load    string // loaded | not-found
	Active  string
	Enabled string
}

type Fake struct {
	mu            sync.Mutex
	units         map[string]*Unit
	calls         []string
	errs          map[string]error
	hang          map[string]bool
	stickyEnabled map[string]bool
}

func New(units map[string]*Unit) *Fake {
	return &Fake{
		units:         units,
		errs:          map[string]error{},
		hang:          map[string]bool{},
		stickyEnabled: map[string]bool{},
	}
}

// Client wraps the fake into a systemd.Client.
func (f *Fake) Client() *systemd.Client { return systemd.NewWithRunner(f.run) }

// FailVerb makes every call of the given systemctl verb ("show", "enable",
// "disable", "reset-failed") return err.
func (f *Fake) FailVerb(verb string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[verb] = err
}

// HangVerb makes every call of the verb block until the context is
// cancelled, imitating a hung systemctl.
func (f *Fake) HangVerb(verb string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hang[verb] = true
}

// StickyEnabled makes disable of the unit report success and stop it while
// silently leaving the enabled flag set — the failure mode SetMode must
// catch via desired-state verification.
func (f *Fake) StickyEnabled(unit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stickyEnabled[unit] = true
}

// Calls returns a copy of the executed command log.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Unit returns a snapshot of one unit's state.
func (f *Fake) Unit(name string) Unit {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u := f.units[name]; u != nil {
		return *u
	}
	return Unit{Load: "not-found"}
}

func (f *Fake) run(ctx context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if name != "systemctl" {
		f.mu.Unlock()
		return "", fmt.Errorf("unexpected command %s", name)
	}
	verb := args[0]
	hang := f.hang[verb]
	verbErr := f.errs[verb]
	f.mu.Unlock()

	if hang {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if verbErr != nil {
		return "", verbErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	switch verb {
	case "show":
		u := f.units[args[1]]
		if u == nil {
			u = &Unit{Load: "not-found"}
		}
		return fmt.Sprintf("LoadState=%s\nActiveState=%s\nUnitFileState=%s\nNRestarts=0\nActiveEnterTimestamp=\n",
			u.Load, u.Active, u.Enabled), nil
	case "enable":
		u := f.units[args[2]]
		if u == nil || u.Load == "not-found" {
			return "", fmt.Errorf("unit %s not found", args[2])
		}
		u.Enabled = "enabled"
		u.Active = "active"
		return "", nil
	case "disable":
		u := f.units[args[2]]
		if u == nil || u.Load == "not-found" {
			return "", fmt.Errorf("unit %s not found", args[2])
		}
		u.Active = "inactive"
		if !f.stickyEnabled[args[2]] {
			u.Enabled = "disabled"
		}
		return "", nil
	case "reset-failed":
		return "", nil
	}
	return "", fmt.Errorf("unexpected systemctl %v", args)
}
