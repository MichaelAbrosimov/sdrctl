package config

import (
	"errors"
	"os"
	"path/filepath"
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
    services:
      rtl-tcp: {systemd: "rtl-tcp@rtl-sdr-01.service"}
  - id: rtl-sdr-02
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

func TestTimingHelpersFallBackOnHandBuiltConfig(t *testing.T) {
	cfg := &Config{} // built directly, no normalize()
	if cfg.ModeSetTimeout().Seconds() != 15 || cfg.RestoreCooldown().Seconds() != 30 {
		t.Errorf("helpers must fall back to defaults: %v %v",
			cfg.ModeSetTimeout(), cfg.RestoreCooldown())
	}
}
