package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/systemd/systemdtest"
)

func testDevice() *config.DeviceConfig {
	return &config.DeviceConfig{
		ID: "rtl-sdr-01",
		Services: map[string]config.ServiceConfig{
			"rtl-tcp":   {Systemd: "rtl-tcp.service", Port: 1234},
			"rtl-433":   {Systemd: "rtl-433.service", Optional: true},
			"spyserver": {Systemd: "spyserver.service", Optional: true},
		},
	}
}

func TestDeviceModesDetection(t *testing.T) {
	cases := []struct {
		name    string
		units   map[string]*systemdtest.Unit
		actual  string
		desired string
	}{
		{
			name: "idle when nothing runs, missing optional units ignored",
			units: map[string]*systemdtest.Unit{
				"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
			},
			actual: ModeIdle, desired: ModeIdle,
		},
		{
			name: "single active service defines the mode",
			units: map[string]*systemdtest.Unit{
				"rtl-tcp.service": {Load: "loaded", Active: "active", Enabled: "enabled"},
				"rtl-433.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
			},
			actual: "rtl-tcp", desired: "rtl-tcp",
		},
		{
			name: "two active services is a conflict",
			units: map[string]*systemdtest.Unit{
				"rtl-tcp.service": {Load: "loaded", Active: "active", Enabled: "enabled"},
				"rtl-433.service": {Load: "loaded", Active: "active", Enabled: "disabled"},
			},
			actual: ModeConflict, desired: "rtl-tcp",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := systemdtest.New(tc.units)
			actual, desired := DeviceModes(context.Background(), f.Client(), testDevice())
			if actual != tc.actual || desired != tc.desired {
				t.Errorf("got actual=%s desired=%s, want %s/%s", actual, desired, tc.actual, tc.desired)
			}
		})
	}
}

func TestHealthFor(t *testing.T) {
	cases := []struct {
		name string
		d    DeviceStatus
		want string
	}{
		{"healthy", DeviceStatus{PresenceKnown: true, Present: true, Mode: "rtl-tcp", DesiredMode: "rtl-tcp"}, HealthHealthy},
		{"idle", DeviceStatus{PresenceKnown: true, Present: true, Mode: ModeIdle, DesiredMode: ModeIdle}, HealthIdle},
		{"missing", DeviceStatus{PresenceKnown: true, Present: false, Mode: ModeIdle, DesiredMode: ModeIdle}, HealthMissing},
		{"degraded", DeviceStatus{PresenceKnown: true, Present: true, Mode: ModeIdle, DesiredMode: "rtl-tcp"}, HealthDegraded},
		{"conflict", DeviceStatus{PresenceKnown: true, Present: true, Mode: ModeConflict, DesiredMode: "rtl-tcp"}, HealthConflict},
		{"unknown presence falls back to services", DeviceStatus{PresenceKnown: false, Mode: "rtl-tcp", DesiredMode: "rtl-tcp"}, HealthHealthy},
	}
	for _, tc := range cases {
		if got := HealthFor(tc.d); got != tc.want {
			t.Errorf("%s: HealthFor = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestSetModeSwitchesThroughSystemd(t *testing.T) {
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
		"rtl-433.service": {Load: "loaded", Active: "active", Enabled: "enabled"},
	})

	res, err := SetMode(context.Background(), f.Client(), testDevice(), "rtl-tcp", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Mode != "rtl-tcp" {
		t.Errorf("unexpected result: %+v", res)
	}
	joined := strings.Join(f.Calls(), "\n")
	if !strings.Contains(joined, "systemctl disable --now rtl-433.service") {
		t.Errorf("competing service was not disabled:\n%s", joined)
	}
	if !strings.Contains(joined, "systemctl enable --now rtl-tcp.service") {
		t.Errorf("target service was not enabled:\n%s", joined)
	}
	if f.Unit("rtl-433.service").Active != "inactive" {
		t.Error("rtl-433 still active")
	}
}

func TestSetModeIdempotent(t *testing.T) {
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "active", Enabled: "enabled"},
	})

	res, err := SetMode(context.Background(), f.Client(), testDevice(), "rtl-tcp", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Errorf("expected no-op, got %+v", res)
	}
	for _, c := range f.Calls() {
		if strings.Contains(c, "enable") || strings.Contains(c, "disable") {
			t.Errorf("no-op still mutated systemd: %s", c)
		}
	}
}

func TestSetModeIdleStopsEverything(t *testing.T) {
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "active", Enabled: "enabled"},
		"rtl-433.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
	})

	res, err := SetMode(context.Background(), f.Client(), testDevice(), ModeIdle, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Mode != ModeIdle {
		t.Errorf("unexpected result: %+v", res)
	}
	if u := f.Unit("rtl-tcp.service"); u.Active != "inactive" || u.Enabled != "disabled" {
		t.Errorf("rtl-tcp not fully stopped/disabled: %+v", u)
	}
}

func TestSetModeRejectsUnknownAndMissing(t *testing.T) {
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
	})
	if _, err := SetMode(context.Background(), f.Client(), testDevice(), "nope", time.Second); err == nil {
		t.Error("unknown mode accepted")
	}
	// spyserver.service is not installed in the fake table
	if _, err := SetMode(context.Background(), f.Client(), testDevice(), "spyserver", time.Second); err == nil {
		t.Error("not-installed mode accepted")
	}
}

// SDR-P1-02: a failing disable of an installed unit must abort the
// transition even when that unit is not running.
func TestSetModeAbortsWhenDisableFails(t *testing.T) {
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
		"rtl-433.service": {Load: "loaded", Active: "inactive", Enabled: "enabled"},
	})
	f.FailVerb("disable", errors.New("dbus timeout"))

	_, err := SetMode(context.Background(), f.Client(), testDevice(), "rtl-tcp", time.Second)
	if err == nil {
		t.Fatal("transition succeeded despite failing disable of an enabled competitor")
	}
	if !strings.Contains(err.Error(), "disable rtl-433.service") {
		t.Errorf("error should name the failing disable, got: %v", err)
	}
}

// SDR-P1-02: success requires the desired coordinate too. A disable that
// reports success but leaves the competitor enabled must fail the
// transition instead of handing back a mode that flips after reboot.
func TestSetModeDetectsCompetitorLeftEnabled(t *testing.T) {
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
		"rtl-433.service": {Load: "loaded", Active: "active", Enabled: "enabled"},
	})
	f.StickyEnabled("rtl-433.service")

	_, err := SetMode(context.Background(), f.Client(), testDevice(), "rtl-tcp", 700*time.Millisecond)
	if err == nil {
		t.Fatal("transition succeeded while the competitor is still enabled")
	}
	if !strings.Contains(err.Error(), "desired") {
		t.Errorf("error should expose the desired-state mismatch, got: %v", err)
	}
}

// SDR-P1-02: idle means every unit inactive AND disabled.
func TestSetModeIdleRequiresDisabled(t *testing.T) {
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "active", Enabled: "enabled"},
	})
	f.StickyEnabled("rtl-tcp.service")

	_, err := SetMode(context.Background(), f.Client(), testDevice(), ModeIdle, 700*time.Millisecond)
	if err == nil {
		t.Fatal("idle transition succeeded while a unit is still enabled")
	}
}

// SDR-P1-03: the timeout bounds the whole transition, including a hung
// systemctl during enable — the caller must get control back promptly.
func TestSetModeDeadlineCoversHungSystemctl(t *testing.T) {
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
	})
	f.HangVerb("enable")

	start := time.Now()
	_, err := SetMode(context.Background(), f.Client(), testDevice(), "rtl-tcp", 300*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("transition with hung systemctl reported success")
	}
	if elapsed > 2*time.Second {
		t.Errorf("SetMode held the caller for %v; the deadline did not cover the hung call", elapsed)
	}
}

// SDR-P1-03 review item 3: the configured timeout is the upper bound of the
// whole call — cleanup and the diagnostic read live INSIDE it, not on top.
func TestSetModeRespectsConfiguredUpperBound(t *testing.T) {
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
	})
	f.HangVerb("enable")

	timeout := 500 * time.Millisecond
	start := time.Now()
	_, err := SetMode(context.Background(), f.Client(), testDevice(), "rtl-tcp", timeout)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error")
	}
	// Generous slack for scheduler jitter only — NOT for extra I/O windows.
	if elapsed > timeout+300*time.Millisecond {
		t.Errorf("SetMode took %v, exceeding the configured upper bound %v", elapsed, timeout)
	}
}

// SDR-P1-03 review item 2: killing the systemctl client does not remove a
// job it already enqueued in PID 1 — SetMode must cancel pending jobs of
// the device before reporting failure, or a queued start could land after
// the inflight guard is released.
func TestSetModeCancelsPendingJobsOnTimeout(t *testing.T) {
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
	})
	f.HangVerb("enable")
	f.LingerJob("enable") // the enqueued start job survives the killed client

	_, err := SetMode(context.Background(), f.Client(), testDevice(), "rtl-tcp", time.Second)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(strings.Join(f.Calls(), "\n"), "systemctl cancel") {
		t.Error("SetMode did not try to cancel the pending job")
	}

	// Whatever PID 1 still holds now lands; a cancelled job must not.
	f.CompleteJobs()
	if u := f.Unit("rtl-tcp.service"); u.Active == "active" || u.Enabled == "enabled" {
		t.Errorf("queued job landed after the reported failure: %+v", u)
	}
}
