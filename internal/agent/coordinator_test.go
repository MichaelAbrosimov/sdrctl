package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/core"
	"github.com/MichaelAbrosimov/sdrctl/internal/systemd/systemdtest"
)

func coordTestConfig() *config.Config {
	return &config.Config{
		Node: config.NodeConfig{ID: "test-node"},
		Observer: config.ObserverConfig{
			AutoRestore:        true,
			RestoreCooldownSec: 1,
		},
		// Keep hand-built-config fallbacks (15s) out of test runtimes.
		ModeSetTimeoutSec: 1,
		Devices: []config.DeviceConfig{{
			ID:      "rtl-sdr-01",
			Type:    "rtl-sdr",
			Default: true,
			Services: map[string]config.ServiceConfig{
				"rtl-tcp": {Systemd: "rtl-tcp.service"},
			},
		}},
	}
}

// degradedSnapshot nominates rtl-sdr-01 for restore: desired rtl-tcp, not
// running.
func degradedSnapshot() core.Snapshot {
	return core.Snapshot{Devices: []core.DeviceStatus{{
		ID:            "rtl-sdr-01",
		PresenceKnown: true,
		Present:       true,
		Mode:          core.ModeIdle,
		DesiredMode:   "rtl-tcp",
		Health:        core.HealthDegraded,
		ServiceInfo: map[string]core.ServiceDetail{
			"rtl-tcp": {Unit: "rtl-tcp.service"},
		},
	}}}
}

// SDR-P1-03 third-review item 1: a quarantine set after an external gate
// check must still be caught — BeginTransition re-checks it inside the same
// critical section as the claim.
func TestBeginTransitionRejectsQuarantinedDevice(t *testing.T) {
	c := NewCoordinator()
	c.Quarantine("rtl-sdr-01")
	if err := c.BeginTransition("rtl-sdr-01"); err == nil {
		t.Fatal("BeginTransition accepted a quarantined device")
	}
	if err := c.BeginTransition("rtl-sdr-02"); err != nil {
		t.Fatalf("unrelated device must not be affected: %v", err)
	}
}

// SDR-P1-03 third-review item 5: leaving quarantine via the hard cap is a
// two-step informed exit — the first request is refused with an
// explanation, only the explicit retry proceeds.
func TestGateWriteHardCapIsInformedExit(t *testing.T) {
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
	})
	// Quiescence verification would fail: list-jobs is unobservable.
	f.FailVerb("list-jobs", context.DeadlineExceeded)

	c := NewCoordinator()
	dev := &coordTestConfig().Devices[0]
	c.Quarantine(dev.ID)

	// Cap of zero: expired immediately, but the exit must still be loud.
	err := c.GateWrite(context.Background(), f.Client(), dev, 0)
	if err == nil {
		t.Fatal("first request after cap expiry must be refused with an explanation")
	}
	if !strings.Contains(err.Error(), "repeat the request") {
		t.Errorf("refusal should instruct the caller how to proceed, got: %v", err)
	}
	if err := c.GateWrite(context.Background(), f.Client(), dev, 0); err != nil {
		t.Fatalf("acknowledged retry must proceed, got: %v", err)
	}
}

// mutated reports whether any systemctl call in the log changes state.
func mutated(calls []string) bool {
	joined := strings.Join(calls, "\n")
	return strings.Contains(joined, "enable") || strings.Contains(joined, "disable") ||
		strings.Contains(joined, "restart")
}

// SDR-P1-03 third-review item 3: auto-restore goes through the same guard
// as manual transitions — while a device is claimed (or quarantined), the
// observer must not touch systemd for it.
func TestAutoRestoreRespectsCoordinator(t *testing.T) {
	// Manual transition in flight → no restore.
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "enabled"},
	})
	coord := NewCoordinator()
	obs := New(coordTestConfig(), f.Client(), coord)
	if err := coord.BeginTransition("rtl-sdr-01"); err != nil {
		t.Fatal(err)
	}
	obs.autoRestore(context.Background(), degradedSnapshot())
	if mutated(f.Calls()) {
		t.Errorf("auto-restore ran during a manual transition:\n%s", strings.Join(f.Calls(), "\n"))
	}
	coord.EndTransition("rtl-sdr-01")

	// Quarantined device → no restore either.
	coord.Quarantine("rtl-sdr-01")
	obs.autoRestore(context.Background(), degradedSnapshot())
	if mutated(f.Calls()) {
		t.Errorf("auto-restore created jobs inside a quarantined device:\n%s", strings.Join(f.Calls(), "\n"))
	}

	// Free, verifiably quiescent device → restore proceeds through the
	// shared SetMode primitive and releases the guard afterwards.
	f2 := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "enabled"},
	})
	coord2 := NewCoordinator()
	obs2 := New(coordTestConfig(), f2.Client(), coord2)
	obs2.autoRestore(context.Background(), degradedSnapshot())
	if !strings.Contains(strings.Join(f2.Calls(), "\n"), "enable --now rtl-tcp.service") {
		t.Errorf("free device was not restored:\n%s", strings.Join(f2.Calls(), "\n"))
	}
	if u := f2.Unit("rtl-tcp.service"); u.Active != "active" {
		t.Errorf("restore did not actually start the unit: %+v", u)
	}
	if err := coord2.BeginTransition("rtl-sdr-01"); err != nil {
		t.Errorf("guard was not released after auto-restore: %v", err)
	}
}

// SDR-P1-03 fourth-review item 1: automation must prove quiescence before
// its first action in a fresh process — a pending job from the previous
// process blocks auto-restore just like it blocks the first API write.
func TestAutoRestoreRequiresStartupVerification(t *testing.T) {
	cfg := coordTestConfig()
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "enabled"},
	})
	// Seed a pending job from the "previous process".
	f.HangVerb("enable")
	f.LingerJob("enable")
	f.KeepJobsOnCancel()
	_, _ = core.SetMode(context.Background(), f.Client(), &cfg.Devices[0], "rtl-tcp", cfg.ModeSetTimeout())
	f.UnhangVerb("enable")
	before := len(f.Calls())

	coord := NewCoordinator()
	obs := New(cfg, f.Client(), coord)
	obs.autoRestore(context.Background(), degradedSnapshot())
	if mutated(f.Calls()[before:]) {
		t.Errorf("auto-restore mutated systemd despite an unverified device:\n%s",
			strings.Join(f.Calls()[before:], "\n"))
	}
}

// SDR-P1-03 fourth-review item 2: the snapshot only nominates; the decision
// is re-made from fresh reads under the guard. A manual idle that landed
// after the snapshot must cancel the restore.
func TestAutoRestoreReChecksDesiredUnderGuard(t *testing.T) {
	// The snapshot claims desired rtl-tcp, but by now a manual transition
	// has disabled everything (desired = idle).
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
	})
	coord := NewCoordinator()
	obs := New(coordTestConfig(), f.Client(), coord)
	obs.autoRestore(context.Background(), degradedSnapshot())
	if mutated(f.Calls()) {
		t.Errorf("auto-restore acted on a stale snapshot decision:\n%s", strings.Join(f.Calls(), "\n"))
	}
}

// Third-review item 4 / settling (в): leaving quarantine requires N
// consecutive stable observations, not one lucky read.
func TestQuarantineExitRequiresStableQuiescence(t *testing.T) {
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "active", Enabled: "enabled"},
	})
	c := NewCoordinator()
	dev := &coordTestConfig().Devices[0]
	c.Quarantine(dev.ID)

	before := len(f.Calls())
	if err := c.GateWrite(context.Background(), f.Client(), dev, time.Hour); err != nil {
		t.Fatalf("stable device did not leave quarantine: %v", err)
	}
	listJobs := 0
	for _, call := range f.Calls()[before:] {
		if strings.Contains(call, "list-jobs") {
			listJobs++
		}
	}
	if listJobs < quiescenceConfirmations {
		t.Errorf("quarantine lifted after %d quiescence reads, want at least %d", listJobs, quiescenceConfirmations)
	}
}

// Pack-3 review settling blocker: N quiescent-looking instants are not
// stability — the (active, enabled) fingerprints must be IDENTICAL across
// the probes. A unit flapping between calm states must not leave
// quarantine.
func TestQuarantineExitDetectsFlappingState(t *testing.T) {
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "active", Enabled: "enabled"},
	})
	// Non-transitional on every probe, but not the SAME state twice.
	f.ShowSequence("rtl-tcp.service", "active", "inactive", "active")

	c := NewCoordinator()
	dev := &coordTestConfig().Devices[0]
	c.Quarantine(dev.ID)

	err := c.GateWrite(context.Background(), f.Client(), dev, time.Hour)
	if err == nil {
		t.Fatal("flapping device left quarantine")
	}
	if !strings.Contains(err.Error(), "changed between probes") &&
		!strings.Contains(err.Error(), "quarantined") {
		t.Errorf("refusal should explain the instability, got: %v", err)
	}
}

// Pack-4 settling review: unknown in EITHER fingerprint coordinate is
// unobservability, not rest — a partial systemd answer (Active known,
// Enabled unknown) must not lift the quarantine.
func TestQuarantineExitRejectsUnknownEnabled(t *testing.T) {
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "active", Enabled: "unknown"},
	})
	c := NewCoordinator()
	dev := &coordTestConfig().Devices[0]
	c.Quarantine(dev.ID)

	if err := c.GateWrite(context.Background(), f.Client(), dev, time.Hour); err == nil {
		t.Fatal("partial systemd answer (Enabled unknown) lifted the quarantine")
	}
}

// SDR-P2-04: a control action needs POSITIVE evidence of the dongle — a
// failed sysfs read (presence unknown) must not trigger a restore.
func TestAutoRestoreNeedsConfirmedPresence(t *testing.T) {
	snap := degradedSnapshot()
	snap.Devices[0].PresenceKnown = false
	snap.Devices[0].Present = false

	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "enabled"},
	})
	coord := NewCoordinator()
	obs := New(coordTestConfig(), f.Client(), coord)
	obs.autoRestore(context.Background(), snap)
	if mutated(f.Calls()) {
		t.Errorf("auto-restore acted on unknown presence:\n%s", strings.Join(f.Calls(), "\n"))
	}
}

// SDR-P1-03 fourth-review item 3: a restore whose cleanup cannot be
// verified must quarantine the device BEFORE releasing the guard — the
// same ErrCleanupUnverified contract as a manual transition.
func TestAutoRestoreQuarantinesOnUnverifiedCleanup(t *testing.T) {
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "enabled"},
	})
	coord := NewCoordinator()
	obs := New(coordTestConfig(), f.Client(), coord)

	// The restore's own enable hangs, leaves a job, and cancel lies.
	f.HangVerb("enable")
	f.LingerJob("enable")
	f.KeepJobsOnCancel()
	obs.autoRestore(context.Background(), degradedSnapshot())

	if err := coord.BeginTransition("rtl-sdr-01"); err == nil {
		t.Fatal("device with unverified restore cleanup was not quarantined")
	}
	// The late-landing job is contained by the quarantine, not by luck:
	// even after it lands, writes stay gated until verification.
	f.CompleteJobs()
	if got := coord.TryBeginRestore("rtl-sdr-01"); got {
		t.Error("auto-restore may not re-enter a quarantined device")
	}
}
