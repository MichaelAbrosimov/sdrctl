package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSingleDeviceShorthand(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
node:
  id: wyse-sdr-01
services:
  rtl-tcp:
    systemd: rtl-tcp.service
    port: 1234
  rtl-433:
    systemd: rtl-433.service
    optional: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Devices) != 1 {
		t.Fatalf("expected 1 implicit device, got %d", len(cfg.Devices))
	}
	d := cfg.Devices[0]
	if d.ID != "rtl-sdr-01" || !d.Default {
		t.Errorf("implicit device should be rtl-sdr-01 and default, got %+v", d)
	}
	if d.USBVendorID != "0bda" || d.USBProductID != "2838" {
		t.Errorf("rtl-sdr usb ids not defaulted: %+v", d)
	}
	if d.Services["rtl-tcp"].Port != 1234 {
		t.Errorf("service port lost: %+v", d.Services)
	}
	dev, err := cfg.DefaultDevice()
	if err != nil || dev.ID != "rtl-sdr-01" {
		t.Errorf("DefaultDevice = %v, %v", dev, err)
	}
}

func TestMultipleDevicesRequireExplicitDefault(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
node:
  id: test
devices:
  - id: rtl-sdr-01
    serial: "00000001"
    services:
      rtl-tcp: {systemd: "rtl-tcp@rtl-sdr-01.service"}
  - id: rtl-sdr-02
    serial: "00000002"
    services:
      rtl-tcp: {systemd: "rtl-tcp@rtl-sdr-02.service"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.DefaultDevice(); !errors.As(err, &ErrNoDefaultDevice{}) {
		t.Errorf("expected ErrNoDefaultDevice, got %v", err)
	}
	if _, err := cfg.DeviceByID("rtl-sdr-02"); err != nil {
		t.Errorf("DeviceByID failed: %v", err)
	}
}

func TestValidateRejectsDuplicatesAndReservedNames(t *testing.T) {
	if _, err := Load(writeConfig(t, `
devices:
  - id: a
    services: {rtl-tcp: {systemd: x.service}}
  - id: a
    services: {rtl-tcp: {systemd: y.service}}
`)); err == nil {
		t.Error("duplicate device id accepted")
	}

	if _, err := Load(writeConfig(t, `
services:
  idle: {systemd: idle.service}
`)); err == nil {
		t.Error("reserved mode name accepted")
	}
}

// SDR-P1-05: devices sharing a VID/PID pair need non-empty unique serials —
// otherwise physical attribution is guesswork.
func TestValidateRequiresUniqueSerialsForSharedIDs(t *testing.T) {
	if _, err := Load(writeConfig(t, `
devices:
  - id: a
    services: {rtl-tcp: {systemd: x.service}}
  - id: b
    services: {rtl-tcp: {systemd: y.service}}
`)); err == nil {
		t.Error("two devices with shared USB ids and no serials accepted")
	}

	if _, err := Load(writeConfig(t, `
devices:
  - id: a
    serial: "00000001"
    services: {rtl-tcp: {systemd: x.service}}
  - id: b
    serial: "00000001"
    services: {rtl-tcp: {systemd: y.service}}
`)); err == nil {
		t.Error("duplicate serials on a shared VID/PID pair accepted")
	}

	if _, err := Load(writeConfig(t, `
devices:
  - id: a
    serial: "00000001"
    services: {rtl-tcp: {systemd: x.service}}
  - id: b
    serial: "00000002"
    services: {rtl-tcp: {systemd: y.service}}
`)); err != nil {
		t.Errorf("unique serials rejected: %v", err)
	}

	// A single device may keep an empty serial — nothing to confuse it with.
	if _, err := Load(writeConfig(t, `
services:
  rtl-tcp: {systemd: x.service}
`)); err != nil {
		t.Errorf("single device without serial rejected: %v", err)
	}
}

// SDR-P3-01: a typo must fail loudly, not silently enable a default — the
// CLI and the agent have to read the SAME config.
func TestStrictParsingAndOperationalValidation(t *testing.T) {
	if _, err := Load(writeConfig(t, `
mode_set_timout_sec: 20
services:
  rtl-tcp: {systemd: x.service}
`)); err == nil {
		t.Error("typo'd field accepted silently")
	}

	if _, err := Load(writeConfig(t, `
mqtt:
  enabled: true
services:
  rtl-tcp: {systemd: x.service}
`)); err == nil {
		t.Error("mqtt.enabled without broker accepted")
	}

	if _, err := Load(writeConfig(t, `
mqtt:
  qos: 3
services:
  rtl-tcp: {systemd: x.service}
`)); err == nil {
		t.Error("qos 3 accepted")
	}

	if _, err := Load(writeConfig(t, `
api:
  listen: "no-port-here"
services:
  rtl-tcp: {systemd: x.service}
`)); err == nil {
		t.Error("unparseable api.listen accepted")
	}

	if _, err := Load(writeConfig(t, `
services:
  rtl-tcp: {systemd: x.service, port: 70000}
`)); err == nil {
		t.Error("out-of-range port accepted")
	}

	if _, err := Load(writeConfig(t, `
services:
  rtl-tcp: {systemd: same.service}
  rtl-433: {systemd: same.service}
`)); err == nil {
		t.Error("two modes sharing one systemd unit accepted")
	}
}

// Pack-5 review: a second YAML document would be decoded into nowhere —
// reject it instead of silently ignoring everything after '---'.
func TestRejectsMultiDocumentYAML(t *testing.T) {
	if _, err := Load(writeConfig(t, `
services:
  rtl-tcp: {systemd: x.service}
---
mode_set_timeout_sec: 20
`)); err == nil {
		t.Error("second YAML document in config accepted silently")
	}

	cfgPath := writeConfig(t, `
services:
  rtl-tcp: {systemd: x.service}
`)
	secrets := filepath.Join(filepath.Dir(cfgPath), "secrets.yaml")
	if err := os.WriteFile(secrets, []byte("api:\n  token: a\n---\nmqtt:\n  password: b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.LoadSecrets(); err == nil {
		t.Error("second YAML document in secrets accepted silently")
	}
}

func TestMissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Loaded {
		t.Error("Loaded should be false for a missing file")
	}
	if cfg.API.Listen != "0.0.0.0:8081" || cfg.Observer.IntervalSec != 5 {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if cfg.MQTT.TopicPrefix == "" || cfg.MQTT.ClientID == "" {
		t.Errorf("mqtt defaults not derived: %+v", cfg.MQTT)
	}
	if cfg.Socket.Path != "/run/sdrctl/sdrctl.sock" || cfg.Socket.Group != "sdrctl" {
		t.Errorf("socket defaults not applied: %+v", cfg.Socket)
	}
	if cfg.ModeSetTimeoutSec != 15 || cfg.Observer.RestoreCooldownSec != 30 {
		t.Errorf("timing defaults not applied: mode_set=%d cooldown=%d",
			cfg.ModeSetTimeoutSec, cfg.Observer.RestoreCooldownSec)
	}
}

// SDR-P2-01: the daemon refuses defaults — an empty agent reporting ok=true
// misleads monitoring; the soft path stays for read-only CLI use.
func TestRequireForAgent(t *testing.T) {
	missing, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.RequireForAgent(); err == nil {
		t.Error("agent accepted a missing config file")
	}

	empty, err := Load(writeConfig(t, "node:\n  id: x\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.RequireForAgent(); err == nil {
		t.Error("agent accepted a config without devices")
	}

	good, err := Load(writeConfig(t, `
services:
  rtl-tcp:
    systemd: rtl-tcp.service
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := good.RequireForAgent(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

// SDR-P1-04: the agent-only secrets overlay separates "CLI must read the
// config" from "the agent must hide credentials".
func TestSecretsOverlay(t *testing.T) {
	cfgPath := writeConfig(t, `
api:
  write_enabled: true
services:
  rtl-tcp:
    systemd: rtl-tcp.service
`)
	secrets := filepath.Join(filepath.Dir(cfgPath), "secrets.yaml")
	if err := os.WriteFile(secrets, []byte("api:\n  token: sekret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API.Token != "" {
		t.Error("main config must not have picked the token up on its own")
	}
	if err := cfg.LoadSecrets(); err != nil {
		t.Fatal(err)
	}
	if cfg.API.Token != "sekret" {
		t.Errorf("token not merged from overlay: %q", cfg.API.Token)
	}
	if err := cfg.CheckSecretPerms(); err != nil {
		t.Errorf("0600 overlay rejected: %v", err)
	}

	// A group-readable secret file must be refused while its secret is in use.
	if err := os.Chmod(secrets, 0o640); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg2.LoadSecrets(); err != nil {
		t.Fatal(err)
	}
	if err := cfg2.CheckSecretPerms(); err == nil {
		t.Error("group-readable secrets file with write API enabled was accepted")
	}
}

// Pack-3 review item 1: safe mode bits are NOT enough — a 0600 secrets
// file owned by the wrong user can be read and replaced by that user. The
// expected owner is the agent's effective UID (root under systemd);
// simulated here via the effectiveUID indirection, since a non-root test
// cannot chown to another user.
func TestCheckSecretPermsWrongOwner(t *testing.T) {
	cfgPath := writeConfig(t, `
api:
  write_enabled: true
services:
  rtl-tcp:
    systemd: rtl-tcp.service
`)
	secrets := filepath.Join(filepath.Dir(cfgPath), "secrets.yaml")
	if err := os.WriteFile(secrets, []byte("api:\n  token: sekret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.LoadSecrets(); err != nil {
		t.Fatal(err)
	}

	orig := effectiveUID
	effectiveUID = func() int { return orig() + 1 } // pretend the agent runs as someone else
	defer func() { effectiveUID = orig }()

	err = cfg.CheckSecretPerms()
	if err == nil {
		t.Fatal("0600 secrets file owned by a different user was accepted")
	}
	if !strings.Contains(err.Error(), "owned by uid") {
		t.Errorf("refusal should explain the owner mismatch, got: %v", err)
	}
}

// Secrets in the MAIN config are refused too when readable beyond owner —
// but only while the corresponding subsystem actually uses them.
func TestCheckSecretPermsMainConfig(t *testing.T) {
	cfgPath := writeConfig(t, `
api:
  write_enabled: true
  token: in-main-file
services:
  rtl-tcp:
    systemd: rtl-tcp.service
`)
	// writeConfig creates 0644.
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.CheckSecretPerms(); err == nil {
		t.Error("world-readable main config carrying an active token was accepted")
	}

	// Same file, write API off: the token is inert, the agent may run.
	cfgPathOff := writeConfig(t, `
api:
  write_enabled: false
  token: in-main-file
services:
  rtl-tcp:
    systemd: rtl-tcp.service
`)
	cfgOff, err := Load(cfgPathOff)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfgOff.CheckSecretPerms(); err != nil {
		t.Errorf("inert token blocked agent start: %v", err)
	}
}

func TestTimingHelpersFallBackOnHandBuiltConfig(t *testing.T) {
	cfg := &Config{} // built directly, no normalize()
	if cfg.ModeSetTimeout().Seconds() != 15 || cfg.RestoreCooldown().Seconds() != 30 {
		t.Errorf("helpers must fall back to defaults: %v %v",
			cfg.ModeSetTimeout(), cfg.RestoreCooldown())
	}
}

// UX finding (2026-08-03): the underlying binaries are called rtl_tcp and
// rtl_433, so users type underscores; and a unique shorthand should work.
// An unknown name must list what IS available — an error that only says
// "unknown" makes the user guess twice.
func TestResolveMode(t *testing.T) {
	dev := &DeviceConfig{ID: "rtl-sdr-01", Services: map[string]ServiceConfig{
		"rtl-tcp": {Systemd: "rtl-tcp.service"}, "rtl-433": {Systemd: "rtl-433.service"},
		"spyserver": {Systemd: "spyserver.service"},
	}}
	for in, want := range map[string]string{
		"rtl-tcp": "rtl-tcp", "rtl_tcp": "rtl-tcp", "RTL_TCP": "rtl-tcp", " rtl-tcp ": "rtl-tcp",
		"tcp": "rtl-tcp", "433": "rtl-433", "spy": "spyserver", "idle": "idle", "i": "idle",
	} {
		got, err := dev.ResolveMode(in)
		if err != nil || got != want {
			t.Errorf("ResolveMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// "rtl" prefixes two modes — guessing would be worse than asking.
	if _, err := dev.ResolveMode("rtl"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("ambiguous shorthand accepted or unexplained: %v", err)
	}
	// The unknown-mode error must list the alternatives.
	_, err := dev.ResolveMode("nope")
	if err == nil {
		t.Fatal("unknown mode accepted")
	}
	for _, m := range []string{"rtl-tcp", "rtl-433", "spyserver", "idle"} {
		if !strings.Contains(err.Error(), m) {
			t.Errorf("error does not list %q: %v", m, err)
		}
	}
}

// Template instances are ordinary mode names, so the shorthand resolver must
// behave predictably around them: the family alone is ambiguous (refusing is
// the point — "rtl-433" would hide which frequency the node went to), while
// anything that picks one instance resolves.
func TestResolveModeTemplateInstances(t *testing.T) {
	dev := &DeviceConfig{ID: "rtl-sdr-01", Services: map[string]ServiceConfig{
		"rtl-tcp":     {Systemd: "rtl-tcp.service"},
		"rtl-433@433": {Systemd: "rtl-433@433.service"},
		"rtl-433@868": {Systemd: "rtl-433@868.service"},
	}}

	for in, want := range map[string]string{
		"rtl-433@868": "rtl-433@868", // exact
		"RTL-433@868": "rtl-433@868", // case-insensitive
		"rtl_433@868": "rtl-433@868", // underscore is a dash
		"868":         "rtl-433@868", // unique substring
		"@433":        "rtl-433@433", // the instance suffix alone
		"tcp":         "rtl-tcp",     // unaffected by the instances
	} {
		got, err := dev.ResolveMode(in)
		if err != nil || got != want {
			t.Errorf("ResolveMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	// "rtl-433" prefixes both instances, and "433" is a substring of both
	// ("rtl-433@868" contains it in the family part). Both must be refused
	// naming the candidates, never silently resolved to one of them.
	for _, in := range []string{"rtl-433", "433"} {
		got, err := dev.ResolveMode(in)
		if err == nil {
			t.Fatalf("ResolveMode(%q) = %q; want an ambiguity error", in, got)
		}
		for _, want := range []string{"rtl-433@433", "rtl-433@868"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ResolveMode(%q) error %q does not name %q", in, err, want)
			}
		}
	}
}
