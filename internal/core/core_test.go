package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/device"
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

// SDR-P2-05: the two mode coordinates track unknown-ness independently — a
// partial systemctl answer must neither hide an unknown UnitFileState
// behind "idle" nor poison a known desired mode via ActiveState alone.
func TestDeviceModesIndependentUnknown(t *testing.T) {
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "active", Enabled: "unknown"},
	})
	actual, desired := DeviceModes(context.Background(), f.Client(), testDevice())
	if actual != "rtl-tcp" || desired != ModeUnknown {
		t.Errorf("unknown UnitFileState: got actual=%s desired=%s, want rtl-tcp/unknown", actual, desired)
	}

	f2 := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "unknown", Enabled: "enabled"},
	})
	actual, desired = DeviceModes(context.Background(), f2.Client(), testDevice())
	if actual != ModeUnknown || desired != "rtl-tcp" {
		t.Errorf("unknown ActiveState: got actual=%s desired=%s, want unknown/rtl-tcp", actual, desired)
	}
}

// SDR-P1-01: unknown breaks global ok — "cannot observe" is not "fine".
// The optional-device exception covers only CONFIRMED absence.
func TestAggregateUnknownBreaksOK(t *testing.T) {
	cases := []struct {
		name       string
		devices    []DeviceStatus
		wantOK     bool
		wantHealth string
	}{
		{
			"required unknown breaks ok",
			[]DeviceStatus{{Health: HealthUnknown}},
			false, HealthUnknown,
		},
		{
			"confirmed-absent optional stays neutral",
			[]DeviceStatus{
				{Health: HealthHealthy},
				{Optional: true, PresenceKnown: true, Present: false, Health: HealthMissing},
			},
			true, HealthHealthy,
		},
		{
			"unobservable optional is NOT neutral",
			[]DeviceStatus{
				{Health: HealthHealthy},
				{Optional: true, PresenceKnown: false, Health: HealthUnknown},
			},
			false, HealthUnknown,
		},
		{
			"conflict is louder than unknown",
			[]DeviceStatus{{Health: HealthConflict}, {Health: HealthUnknown}},
			false, HealthConflict,
		},
		{
			"unknown is louder than degraded",
			[]DeviceStatus{{Health: HealthDegraded}, {Health: HealthUnknown}},
			false, HealthUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, health := aggregate(tc.devices)
			if ok != tc.wantOK || health != tc.wantHealth {
				t.Errorf("got ok=%v health=%s, want %v/%s", ok, health, tc.wantOK, tc.wantHealth)
			}
		})
	}
}

// SDR-P1-01 (pack 2 review): the ACTUAL chain HealthFor → aggregate — an
// unobservable-sysfs device with perfectly known modes must break ok.
func TestUnknownPresenceBreaksOKThroughTheChain(t *testing.T) {
	d := DeviceStatus{PresenceKnown: false, Mode: "rtl-tcp", DesiredMode: "rtl-tcp"}
	d.Health = HealthFor(d)
	ok, health := aggregate([]DeviceStatus{d})
	if ok || health != HealthUnknown {
		t.Errorf("unknown presence produced ok=%v health=%s; want false/unknown", ok, health)
	}
}

// SDR-P2-05 (pack 2 review): one KNOWN mode must not mask a competitor
// whose state could not be read — unknown outranks a single known name.
func TestModeUnknownCompetitorIsNotMasked(t *testing.T) {
	// Competitor's ActiveState unknown, its enabled state known.
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "active", Enabled: "enabled"},
		"rtl-433.service": {Load: "loaded", Active: "unknown", Enabled: "disabled"},
	})
	actual, desired := DeviceModes(context.Background(), f.Client(), testDevice())
	if actual != ModeUnknown || desired != "rtl-tcp" {
		t.Errorf("unknown competitor ActiveState: got actual=%s desired=%s, want unknown/rtl-tcp", actual, desired)
	}

	// Competitor's UnitFileState unknown, its active state known.
	f2 := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "active", Enabled: "enabled"},
		"rtl-433.service": {Load: "loaded", Active: "inactive", Enabled: "unknown"},
	})
	actual, desired = DeviceModes(context.Background(), f2.Client(), testDevice())
	if actual != "rtl-tcp" || desired != ModeUnknown {
		t.Errorf("unknown competitor UnitFileState: got actual=%s desired=%s, want rtl-tcp/unknown", actual, desired)
	}
}

// SDR-P2-05 (pack 2 review): SetMode must not confirm success while a
// competitor's state cannot be read — the desired coordinate is unproven.
func TestSetModeNoFalseSuccessWithUnreadableCompetitor(t *testing.T) {
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
		"rtl-433.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
	})
	f.FailShowUnit("rtl-433.service", errors.New("dbus timeout for this unit"))

	_, err := SetMode(context.Background(), f.Client(), testDevice(), "rtl-tcp", 700*time.Millisecond)
	if err == nil {
		t.Fatal("SetMode reported success while the competitor's state was unreadable")
	}
}

// SDR-P1-05: a physical dongle satisfies AT MOST one configuration; every
// unresolvable attribution is an explicit ambiguity, never a guess.
func TestAllocateUSB(t *testing.T) {
	devA := config.DeviceConfig{ID: "a", USBVendorID: "0bda", USBProductID: "2838", Serial: "S1"}
	devB := config.DeviceConfig{ID: "b", USBVendorID: "0bda", USBProductID: "2838", Serial: "S2"}
	usb := func(serials ...string) []device.USBDevice {
		var out []device.USBDevice
		for i, s := range serials {
			out = append(out, device.USBDevice{
				SysName: fmt.Sprintf("1-%d", i+1), VendorID: "0bda", ProductID: "2838", Serial: s,
			})
		}
		return out
	}

	// Unique serials → clean one-to-one attribution.
	claims := allocateUSB([]config.DeviceConfig{devA, devB}, usb("S1", "S2"))
	if claims[0].dev == nil || claims[0].dev.Serial != "S1" ||
		claims[1].dev == nil || claims[1].dev.Serial != "S2" {
		t.Errorf("unique serials not attributed one-to-one: %+v", claims)
	}

	// One sysfs object matches BOTH configurations (equal serials in
	// config would be rejected by validate; here both match via one
	// physical dongle carrying S1 while dev b has empty serial).
	devBAny := devB
	devBAny.Serial = ""
	claims = allocateUSB([]config.DeviceConfig{devA, devBAny}, usb("S1"))
	if claims[0].dev != nil || !claims[0].ambiguous || claims[1].dev != nil || !claims[1].ambiguous {
		t.Errorf("contested dongle was granted instead of flagged: %+v", claims)
	}

	// One configuration, two matching dongles (factory-equal serials) →
	// ambiguous, not matched[0].
	claims = allocateUSB([]config.DeviceConfig{devA}, usb("S1", "S1"))
	if claims[0].dev != nil || !claims[0].ambiguous {
		t.Errorf("duplicate physical serials resolved by guessing: %+v", claims)
	}

	// No candidates at all → plain absence, not ambiguity.
	claims = allocateUSB([]config.DeviceConfig{devA}, usb("S9"))
	if claims[0].dev != nil || claims[0].ambiguous {
		t.Errorf("absence misreported: %+v", claims)
	}
}

// SDR-P1-05 (pack 4 review): an ambiguous OPTIONAL device must break
// global ok through the full allocateUSB → buildDevice → aggregate chain —
// the optional exception excuses confirmed absence, not ambiguity.
func TestAmbiguousOptionalDeviceBreaksOK(t *testing.T) {
	dc := config.DeviceConfig{
		ID: "rtl-sdr-01", Optional: true,
		USBVendorID: "0bda", USBProductID: "2838", Serial: "S1",
		Services: map[string]config.ServiceConfig{
			"rtl-tcp": {Systemd: "rtl-tcp.service"},
		},
	}
	// Two factory-equal dongles both match the single configuration.
	usb := []device.USBDevice{
		{SysName: "1-1", VendorID: "0bda", ProductID: "2838", Serial: "S1"},
		{SysName: "1-2", VendorID: "0bda", ProductID: "2838", Serial: "S1"},
	}
	claims := allocateUSB([]config.DeviceConfig{dc}, usb)
	if !claims[0].ambiguous {
		t.Fatal("two equal candidates must be ambiguous")
	}

	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
	})
	ds := buildDevice(context.Background(), dc, f.Client(), claims[0], true)
	if ds.Health != HealthConflict {
		t.Fatalf("ambiguous device health = %s, want conflict", ds.Health)
	}

	ok, health := aggregate([]DeviceStatus{ds})
	if ok || health != HealthConflict {
		t.Errorf("ambiguous optional device produced ok=%v health=%s; want false/conflict", ok, health)
	}

	// Genuinely absent optional device stays neutral — the exception the
	// fix must NOT destroy.
	claimsAbsent := allocateUSB([]config.DeviceConfig{dc}, nil)
	dsAbsent := buildDevice(context.Background(), dc, f.Client(), claimsAbsent[0], true)
	if dsAbsent.Health != HealthMissing {
		t.Fatalf("absent device health = %s, want missing", dsAbsent.Health)
	}
	if ok, health := aggregate([]DeviceStatus{dsAbsent}); !ok || health != HealthIdle {
		t.Errorf("confirmed-absent optional device broke ok: ok=%v health=%s", ok, health)
	}
}

// SDR-P1-01 end to end: with systemd unobservable the snapshot must say so
// instead of reporting a healthy idle node to the orchestrator.
func TestSnapshotUnknownWhenSystemdUnobservable(t *testing.T) {
	f := systemdtest.New(map[string]*systemdtest.Unit{})
	f.FailVerb("show", errors.New("dbus is down"))
	cfg := &config.Config{
		Node:    config.NodeConfig{ID: "test-node"},
		Devices: []config.DeviceConfig{*testDevice()},
	}
	snap := BuildSnapshot(context.Background(), cfg, f.Client())
	if snap.OK || snap.Health != HealthUnknown {
		t.Errorf("unobservable systemd reported ok=%v health=%s; want false/unknown", snap.OK, snap.Health)
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
		// SDR-P1-01 (pack 2 review): unobservable sysfs is not proof of
		// anything — unknown presence means unknown health, never healthy.
		{"unknown presence is unknown health", DeviceStatus{PresenceKnown: false, Mode: "rtl-tcp", DesiredMode: "rtl-tcp"}, HealthUnknown},
		{"confirmed conflict outranks unknown presence", DeviceStatus{PresenceKnown: false, Mode: ModeConflict, DesiredMode: "rtl-tcp"}, HealthConflict},
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

// Field finding: rtl_tcp exits 1 on SIGTERM, so the unit we just left is
// parked in "failed" and shows up that way in status. A deliberately
// stopped mode must not be reported as failed.
func TestSetModeClearsFailedStateOfLeftMode(t *testing.T) {
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
		"rtl-433.service": {Load: "loaded", Active: "active", Enabled: "enabled"},
	})

	if _, err := SetMode(context.Background(), f.Client(), testDevice(), "rtl-tcp", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(f.Calls(), "\n"), "reset-failed rtl-433.service") {
		t.Errorf("left-behind unit keeps its failed marker:\n%s", strings.Join(f.Calls(), "\n"))
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
	// The sweep verified the cleanup, so the failure is NOT ambiguous.
	if errors.Is(err, ErrCleanupUnverified) {
		t.Errorf("verified cleanup must not be reported as unverified: %v", err)
	}
}

// SDR-P1-03 re-review item 1: every path where cleanup cannot be CONFIRMED
// must surface ErrCleanupUnverified so the caller quarantines the device.
func TestSetModeReportsUnverifiedCleanup(t *testing.T) {
	cases := []struct {
		name string
		prep func(f *systemdtest.Fake)
	}{
		{"list-jobs fails", func(f *systemdtest.Fake) {
			f.FailVerb("list-jobs", errors.New("dbus is down"))
		}},
		{"cancel fails", func(f *systemdtest.Fake) {
			f.LingerJob("enable")
			f.FailVerb("cancel", errors.New("dbus is down"))
		}},
		{"job survives a successful cancel", func(f *systemdtest.Fake) {
			f.LingerJob("enable")
			f.KeepJobsOnCancel()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := systemdtest.New(map[string]*systemdtest.Unit{
				"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
			})
			f.HangVerb("enable")
			tc.prep(f)

			_, err := SetMode(context.Background(), f.Client(), testDevice(), "rtl-tcp", time.Second)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !errors.Is(err, ErrCleanupUnverified) {
				t.Errorf("unverifiable cleanup must wrap ErrCleanupUnverified, got: %v", err)
			}
		})
	}
}

func TestDeviceQuiescent(t *testing.T) {
	// Pending job → not quiescent.
	f := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
	})
	f.HangVerb("enable")
	f.LingerJob("enable")
	f.KeepJobsOnCancel()
	_, _ = SetMode(context.Background(), f.Client(), testDevice(), "rtl-tcp", 500*time.Millisecond)
	if quiet, _, err := DeviceQuiescent(context.Background(), f.Client(), testDevice()); err != nil || quiet {
		t.Errorf("device with a pending job reported quiescent=%v err=%v", quiet, err)
	}

	// Transitional ActiveState → not quiescent.
	f2 := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "activating", Enabled: "enabled"},
	})
	if quiet, _, err := DeviceQuiescent(context.Background(), f2.Client(), testDevice()); err != nil || quiet {
		t.Errorf("activating unit reported quiescent=%v err=%v", quiet, err)
	}

	// Stable state, no jobs → quiescent.
	f3 := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "active", Enabled: "enabled"},
	})
	if quiet, _, err := DeviceQuiescent(context.Background(), f3.Client(), testDevice()); err != nil || !quiet {
		t.Errorf("settled device reported quiescent=%v err=%v", quiet, err)
	}

	// Unobservable → error, never "quiescent".
	f4 := systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
	})
	f4.FailVerb("list-jobs", errors.New("dbus is down"))
	if _, _, err := DeviceQuiescent(context.Background(), f4.Client(), testDevice()); err == nil {
		t.Error("unobservable state must be an error, not a verdict")
	}
}
