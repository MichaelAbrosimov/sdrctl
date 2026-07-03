package agent

import (
	"context"
	"strings"
	"testing"

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

// SDR-P1-03 third-review item 3: auto-restore goes through the same guard
// as manual transitions — while a device is claimed (or quarantined), the
// observer must not touch systemd for it.
func TestAutoRestoreRespectsCoordinator(t *testing.T) {
	degraded := core.Snapshot{Devices: []core.DeviceStatus{{
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

	// Manual transition in flight → no restore.
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "enabled"},
	})
	coord := NewCoordinator()
	obs := New(coordTestConfig(), f.Client(), coord)
	if err := coord.BeginTransition("rtl-sdr-01"); err != nil {
		t.Fatal(err)
	}
	obs.autoRestore(context.Background(), degraded)
	if calls := strings.Join(f.Calls(), "\n"); strings.Contains(calls, "restart") {
		t.Errorf("auto-restore ran during a manual transition:\n%s", calls)
	}
	coord.EndTransition("rtl-sdr-01")

	// Quarantined device → no restore either.
	coord.Quarantine("rtl-sdr-01")
	obs.autoRestore(context.Background(), degraded)
	if calls := strings.Join(f.Calls(), "\n"); strings.Contains(calls, "restart") {
		t.Errorf("auto-restore created jobs inside a quarantined device:\n%s", calls)
	}

	// Free device → restore proceeds and releases the guard afterwards.
	f2 := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "enabled"},
	})
	coord2 := NewCoordinator()
	obs2 := New(coordTestConfig(), f2.Client(), coord2)
	obs2.autoRestore(context.Background(), degraded)
	if calls := strings.Join(f2.Calls(), "\n"); !strings.Contains(calls, "restart rtl-tcp.service") {
		t.Errorf("free device was not restored:\n%s", calls)
	}
	if err := coord2.BeginTransition("rtl-sdr-01"); err != nil {
		t.Errorf("guard was not released after auto-restore: %v", err)
	}
}
