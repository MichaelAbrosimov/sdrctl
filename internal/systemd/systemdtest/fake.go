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

// pendingJob models a job enqueued in PID 1 that outlived its killed
// systemctl client.
type pendingJob struct {
	id   int
	unit string
	verb string // enable | disable | restart
}

// unitArg extracts the unit name from a systemctl argument list: verbs with
// --now carry it third, plain verbs (restart, reset-failed) second.
func unitArg(verb string, args []string) string {
	switch verb {
	case "enable", "disable":
		if len(args) >= 3 {
			return args[2]
		}
	case "restart", "reset-failed":
		if len(args) >= 2 {
			return args[1]
		}
	}
	return ""
}

type Fake struct {
	mu            sync.Mutex
	units         map[string]*Unit
	calls         []string
	errs          map[string]error
	hang          map[string]bool
	linger        map[string]bool
	stickyEnabled map[string]bool
	failShow      map[string]error
	showSequence  map[string][]string
	keepOnCancel  bool
	jobs          []pendingJob
	nextJobID     int
}

func New(units map[string]*Unit) *Fake {
	return &Fake{
		units:         units,
		errs:          map[string]error{},
		hang:          map[string]bool{},
		linger:        map[string]bool{},
		stickyEnabled: map[string]bool{},
		failShow:      map[string]error{},
		showSequence:  map[string][]string{},
		nextJobID:     1,
	}
}

// ShowSequence makes consecutive `systemctl show` calls of the unit report
// the given ActiveState values in order (the last one repeats) — a unit
// flapping between otherwise calm-looking states.
func (f *Fake) ShowSequence(unit string, actives ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.showSequence[unit] = actives
}

// FailShowUnit makes `systemctl show` of ONE unit fail while other units
// stay readable — the mixed known+unknown scenario a single-mode read must
// not mask.
func (f *Fake) FailShowUnit(unit string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failShow[unit] = err
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

// UnhangVerb removes a previously configured hang for the verb, e.g. to
// model "systemd responds again in the next process".
func (f *Fake) UnhangVerb(verb string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.hang, verb)
}

// LingerJob makes a hung verb leave a pending job behind when its context
// is cancelled — modelling systemd semantics where killing the systemctl
// client does not remove the job it already enqueued in PID 1. The job is
// visible via list-jobs, removable via cancel, and applies its effect when
// CompleteJobs is called.
func (f *Fake) LingerJob(verb string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.linger[verb] = true
}

// CompleteJobs lets every still-pending job land, mutating the unit table —
// what PID 1 would eventually do unless the job was cancelled.
func (f *Fake) CompleteJobs() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, j := range f.jobs {
		u := f.units[j.unit]
		if u == nil || u.Load == "not-found" {
			continue
		}
		switch j.verb {
		case "enable":
			u.Enabled = "enabled"
			u.Active = "active"
		case "disable":
			u.Active = "inactive"
			if !f.stickyEnabled[j.unit] {
				u.Enabled = "disabled"
			}
		case "restart":
			u.Active = "active"
		}
	}
	f.jobs = nil
}

// KeepJobsOnCancel makes `systemctl cancel` report success without removing
// the job — the failure mode where only the post-cancel re-listing can
// prove that cleanup did not actually happen.
func (f *Fake) KeepJobsOnCancel() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keepOnCancel = true
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
		f.mu.Lock()
		if f.linger[verb] {
			if unit := unitArg(verb, args); unit != "" {
				f.jobs = append(f.jobs, pendingJob{id: f.nextJobID, unit: unit, verb: verb})
				f.nextJobID++
			}
		}
		f.mu.Unlock()
		return "", ctx.Err()
	}
	// Like exec.CommandContext, a command is never started with an already
	// cancelled context — side effects must not leak past the deadline.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if verbErr != nil {
		return "", verbErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	switch verb {
	case "show":
		if err := f.failShow[args[1]]; err != nil {
			return "", err
		}
		u := f.units[args[1]]
		if u == nil {
			u = &Unit{Load: "not-found"}
		}
		active := u.Active
		if seq := f.showSequence[args[1]]; len(seq) > 0 {
			active = seq[0]
			if len(seq) > 1 {
				f.showSequence[args[1]] = seq[1:]
			}
		}
		return fmt.Sprintf("LoadState=%s\nActiveState=%s\nUnitFileState=%s\nNRestarts=0\nActiveEnterTimestamp=\n",
			u.Load, active, u.Enabled), nil
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
	case "restart":
		u := f.units[args[1]]
		if u == nil || u.Load == "not-found" {
			return "", fmt.Errorf("unit %s not found", args[1])
		}
		u.Active = "active"
		return "", nil
	case "reset-failed":
		return "", nil
	case "list-jobs":
		var b strings.Builder
		for _, j := range f.jobs {
			fmt.Fprintf(&b, "%d %s start running\n", j.id, j.unit)
		}
		return b.String(), nil
	case "cancel":
		if f.keepOnCancel {
			return "", nil
		}
		id := args[1]
		kept := f.jobs[:0]
		for _, j := range f.jobs {
			if fmt.Sprint(j.id) != id {
				kept = append(kept, j)
			}
		}
		f.jobs = kept
		return "", nil
	}
	return "", fmt.Errorf("unexpected systemctl %v", args)
}
