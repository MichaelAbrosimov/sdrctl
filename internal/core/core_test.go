package core

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/systemd"
)

// fakeUnit models one systemd unit for the fake systemctl runner.
type fakeUnit struct {
	load    string // loaded | not-found
	active  string
	enabled string
}

// fakeSystemd returns a systemd client backed by an in-memory unit table and
// a log of executed commands. enable/disable mutate the table like the real
// systemctl --now would.
func fakeSystemd(units map[string]*fakeUnit) (*systemd.Client, *[]string) {
	var calls []string
	runner := func(name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name != "systemctl" {
			return "", fmt.Errorf("unexpected command %s", name)
		}
		switch args[0] {
		case "show":
			unit := args[1]
			u := units[unit]
			if u == nil {
				u = &fakeUnit{load: "not-found"}
			}
			return fmt.Sprintf("LoadState=%s\nActiveState=%s\nUnitFileState=%s\nNRestarts=0\nActiveEnterTimestamp=\n",
				u.load, u.active, u.enabled), nil
		case "enable":
			u := units[args[2]]
			if u == nil || u.load == "not-found" {
				return "", fmt.Errorf("unit %s not found", args[2])
			}
			u.enabled = "enabled"
			u.active = "active"
			return "", nil
		case "disable":
			u := units[args[2]]
			if u == nil || u.load == "not-found" {
				return "", fmt.Errorf("unit %s not found", args[2])
			}
			u.enabled = "disabled"
			u.active = "inactive"
			return "", nil
		case "reset-failed":
			return "", nil
		}
		return "", fmt.Errorf("unexpected systemctl %v", args)
	}
	return systemd.NewWithRunner(runner), &calls
}

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
		units   map[string]*fakeUnit
		actual  string
		desired string
	}{
		{
			name: "idle when nothing runs, missing optional units ignored",
			units: map[string]*fakeUnit{
				"rtl-tcp.service": {load: "loaded", active: "inactive", enabled: "disabled"},
			},
			actual: ModeIdle, desired: ModeIdle,
		},
		{
			name: "single active service defines the mode",
			units: map[string]*fakeUnit{
				"rtl-tcp.service": {load: "loaded", active: "active", enabled: "enabled"},
				"rtl-433.service": {load: "loaded", active: "inactive", enabled: "disabled"},
			},
			actual: "rtl-tcp", desired: "rtl-tcp",
		},
		{
			name: "two active services is a conflict",
			units: map[string]*fakeUnit{
				"rtl-tcp.service": {load: "loaded", active: "active", enabled: "enabled"},
				"rtl-433.service": {load: "loaded", active: "active", enabled: "disabled"},
			},
			actual: ModeConflict, desired: "rtl-tcp",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sd, _ := fakeSystemd(tc.units)
			actual, desired := DeviceModes(sd, testDevice())
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
	units := map[string]*fakeUnit{
		"rtl-tcp.service": {load: "loaded", active: "inactive", enabled: "disabled"},
		"rtl-433.service": {load: "loaded", active: "active", enabled: "enabled"},
	}
	sd, calls := fakeSystemd(units)

	res, err := SetMode(sd, testDevice(), "rtl-tcp", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Mode != "rtl-tcp" {
		t.Errorf("unexpected result: %+v", res)
	}
	joined := strings.Join(*calls, "\n")
	if !strings.Contains(joined, "systemctl disable --now rtl-433.service") {
		t.Errorf("competing service was not disabled:\n%s", joined)
	}
	if !strings.Contains(joined, "systemctl enable --now rtl-tcp.service") {
		t.Errorf("target service was not enabled:\n%s", joined)
	}
	if units["rtl-433.service"].active != "inactive" {
		t.Error("rtl-433 still active")
	}
}

func TestSetModeIdempotent(t *testing.T) {
	units := map[string]*fakeUnit{
		"rtl-tcp.service": {load: "loaded", active: "active", enabled: "enabled"},
	}
	sd, calls := fakeSystemd(units)

	res, err := SetMode(sd, testDevice(), "rtl-tcp", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Errorf("expected no-op, got %+v", res)
	}
	for _, c := range *calls {
		if strings.Contains(c, "enable") || strings.Contains(c, "disable") {
			t.Errorf("no-op still mutated systemd: %s", c)
		}
	}
}

func TestSetModeIdleStopsEverything(t *testing.T) {
	units := map[string]*fakeUnit{
		"rtl-tcp.service": {load: "loaded", active: "active", enabled: "enabled"},
		"rtl-433.service": {load: "loaded", active: "inactive", enabled: "disabled"},
	}
	sd, _ := fakeSystemd(units)

	res, err := SetMode(sd, testDevice(), ModeIdle, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Mode != ModeIdle {
		t.Errorf("unexpected result: %+v", res)
	}
	if units["rtl-tcp.service"].active != "inactive" || units["rtl-tcp.service"].enabled != "disabled" {
		t.Errorf("rtl-tcp not fully stopped/disabled: %+v", units["rtl-tcp.service"])
	}
}

func TestSetModeRejectsUnknownAndMissing(t *testing.T) {
	sd, _ := fakeSystemd(map[string]*fakeUnit{
		"rtl-tcp.service": {load: "loaded", active: "inactive", enabled: "disabled"},
	})
	if _, err := SetMode(sd, testDevice(), "nope", time.Second); err == nil {
		t.Error("unknown mode accepted")
	}
	// spyserver.service is not installed in the fake table
	if _, err := SetMode(sd, testDevice(), "spyserver", time.Second); err == nil {
		t.Error("not-installed mode accepted")
	}
}
