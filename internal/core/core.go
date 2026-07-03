// Package core builds the node/device state model.
//
// Sources of truth:
//
//	actual mode       = systemd ActiveState of SDR units
//	desired mode      = systemd UnitFileState (enabled/disabled)
//	physical presence = USB sysfs
//
// sdrctl keeps no state files; a snapshot is always derived live.
package core

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/device"
	"github.com/MichaelAbrosimov/sdrctl/internal/systemd"
	"github.com/MichaelAbrosimov/sdrctl/internal/version"
)

const (
	ModeIdle     = "idle"
	ModeConflict = "conflict"
	ModeUnknown  = "unknown"

	HealthHealthy  = "healthy"
	HealthIdle     = "idle"
	HealthMissing  = "missing"
	HealthDegraded = "degraded"
	HealthConflict = "conflict"
	HealthUnknown  = "unknown"
)

type ServiceDetail struct {
	Unit     string `json:"unit"`
	Status   string `json:"status"`
	Enabled  string `json:"enabled"`
	Port     int    `json:"port,omitempty"`
	Restarts string `json:"restarts,omitempty"`
	Since    string `json:"since,omitempty"`
	Optional bool   `json:"optional,omitempty"`
}

type DeviceStatus struct {
	ID            string                   `json:"id"`
	Type          string                   `json:"type"`
	Label         string                   `json:"label,omitempty"`
	Serial        string                   `json:"serial,omitempty"`
	Optional      bool                     `json:"optional,omitempty"`
	Present       bool                     `json:"present"`
	PresenceKnown bool                     `json:"presence_known"`
	Mode          string                   `json:"mode"`
	DesiredMode   string                   `json:"desired_mode"`
	Health        string                   `json:"health"`
	Services      map[string]string        `json:"services"`
	ServiceInfo   map[string]ServiceDetail `json:"service_details,omitempty"`
	Ports         map[string]int           `json:"ports,omitempty"`
	USB           *device.USBDevice        `json:"usb,omitempty"`
}

type Snapshot struct {
	Node        string         `json:"node"`
	Role        string         `json:"role,omitempty"`
	Version     string         `json:"version"`
	GeneratedAt time.Time      `json:"generated_at"`
	LANIP       string         `json:"lan_ip,omitempty"`
	OK          bool           `json:"ok"`
	Health      string         `json:"health"`
	Warnings    []string       `json:"warnings,omitempty"`
	Devices     []DeviceStatus `json:"devices"`
}

// BuildSnapshot derives the full node state from systemd and sysfs.
func BuildSnapshot(cfg *config.Config, sd *systemd.Client) Snapshot {
	snap := Snapshot{
		Node:        cfg.Node.ID,
		Role:        cfg.Node.Role,
		Version:     version.Version,
		GeneratedAt: time.Now().UTC(),
		LANIP:       LANIP(),
	}

	usb, usbErr := device.ScanUSB()
	presenceKnown := usbErr == nil

	seenIDPairs := map[string]bool{}
	for _, dc := range cfg.Devices {
		ds := buildDevice(dc, sd, usb, presenceKnown)
		snap.Devices = append(snap.Devices, ds)

		if ds.Mode != ModeIdle && ds.Mode != ModeConflict && ds.Mode != ModeUnknown &&
			ds.DesiredMode == ModeIdle {
			snap.Warnings = append(snap.Warnings, fmt.Sprintf(
				"device %s: mode %s is active but not enabled — it will not survive a reboot",
				ds.ID, ds.Mode))
		}

		if presenceKnown && len(cfg.Devices) > 1 {
			pair := dc.USBVendorID + ":" + dc.USBProductID
			if !seenIDPairs[pair] {
				seenIDPairs[pair] = true
				for _, serial := range device.DuplicateSerials(usb, dc.USBVendorID, dc.USBProductID) {
					snap.Warnings = append(snap.Warnings, fmt.Sprintf(
						"several dongles %s share USB serial %q — assign unique serials with rtl_eeprom",
						pair, serial))
				}
			}
		}
	}

	snap.OK, snap.Health = aggregate(snap.Devices)
	return snap
}

func buildDevice(dc config.DeviceConfig, sd *systemd.Client, usb []device.USBDevice, presenceKnown bool) DeviceStatus {
	ds := DeviceStatus{
		ID:            dc.ID,
		Type:          dc.Type,
		Label:         dc.Label,
		Serial:        dc.Serial,
		Optional:      dc.Optional,
		PresenceKnown: presenceKnown,
		Services:      map[string]string{},
		ServiceInfo:   map[string]ServiceDetail{},
		Ports:         map[string]int{},
	}

	var running, enabled []string
	anyUnknown := false
	for name, sc := range dc.Services {
		st := sd.UnitStatus(sc.Systemd)
		ds.Services[name] = st.Active
		ds.ServiceInfo[name] = ServiceDetail{
			Unit:     sc.Systemd,
			Status:   st.Active,
			Enabled:  st.Enabled,
			Port:     sc.Port,
			Restarts: st.Restarts,
			Since:    st.Since,
			Optional: sc.Optional,
		}
		if sc.Port != 0 {
			ds.Ports[name] = sc.Port
		}
		if st.Active == "unknown" {
			anyUnknown = true
		}
		if st.IsRunning() {
			running = append(running, name)
		}
		if st.IsEnabled() {
			enabled = append(enabled, name)
		}
	}
	sort.Strings(running)
	sort.Strings(enabled)
	ds.Mode = modeFrom(running, anyUnknown)
	ds.DesiredMode = modeFrom(enabled, anyUnknown)

	if presenceKnown {
		matched := device.Match(usb, dc.USBVendorID, dc.USBProductID, dc.Serial)
		if len(matched) > 0 {
			ds.Present = true
			ds.USB = &matched[0]
		}
	}

	ds.Health = HealthFor(ds)
	return ds
}

func modeFrom(names []string, anyUnknown bool) string {
	switch {
	case len(names) == 1:
		return names[0]
	case len(names) > 1:
		return ModeConflict
	case anyUnknown:
		return ModeUnknown
	default:
		return ModeIdle
	}
}

// HealthFor derives device health from mode, desired mode and presence.
func HealthFor(d DeviceStatus) string {
	if d.Mode == ModeConflict || d.DesiredMode == ModeConflict {
		return HealthConflict
	}
	if d.Mode == ModeUnknown || d.DesiredMode == ModeUnknown {
		return HealthUnknown
	}
	if d.PresenceKnown && !d.Present {
		return HealthMissing
	}
	switch {
	case d.DesiredMode == ModeIdle && d.Mode == ModeIdle:
		return HealthIdle
	case d.Mode == d.DesiredMode:
		return HealthHealthy
	case d.DesiredMode == ModeIdle:
		// Something runs without being enabled: it works, but a warning is
		// attached at snapshot level.
		return HealthHealthy
	default:
		return HealthDegraded
	}
}

func aggregate(devices []DeviceStatus) (bool, string) {
	ok := true
	anyHealthy := false
	anyConflict := false
	for _, d := range devices {
		if d.Optional && d.PresenceKnown && !d.Present {
			continue // optional missing devices never break global health
		}
		switch d.Health {
		case HealthHealthy:
			anyHealthy = true
		case HealthIdle, HealthUnknown:
			// neutral
		case HealthConflict:
			ok = false
			anyConflict = true
		default:
			ok = false
		}
	}
	switch {
	case !ok && anyConflict:
		return false, HealthConflict
	case !ok:
		return false, HealthDegraded
	case anyHealthy:
		return true, HealthHealthy
	default:
		return true, HealthIdle
	}
}

// DeviceModes returns the actual and desired mode of one device.
func DeviceModes(sd *systemd.Client, dev *config.DeviceConfig) (actual, desired string) {
	var running, enabled []string
	anyUnknown := false
	for name, sc := range dev.Services {
		st := sd.UnitStatus(sc.Systemd)
		if st.Active == "unknown" {
			anyUnknown = true
		}
		if st.IsRunning() {
			running = append(running, name)
		}
		if st.IsEnabled() {
			enabled = append(enabled, name)
		}
	}
	sort.Strings(running)
	sort.Strings(enabled)
	return modeFrom(running, anyUnknown), modeFrom(enabled, anyUnknown)
}

type SetModeResult struct {
	Device        string `json:"device"`
	RequestedMode string `json:"requested_mode"`
	Mode          string `json:"mode"`
	Changed       bool   `json:"changed"`
}

// ModeNames lists selectable modes of a device (services + idle).
func ModeNames(dev *config.DeviceConfig) []string {
	names := make([]string, 0, len(dev.Services)+1)
	for name := range dev.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return append(names, ModeIdle)
}

// SetMode switches a device to the target mode through systemd only:
// competing units are disabled (disable --now), the target unit is enabled
// (enable --now), then the outcome is verified. Desired state lives in the
// units' enabled flags — nothing is written anywhere else.
func SetMode(sd *systemd.Client, dev *config.DeviceConfig, target string, timeout time.Duration) (SetModeResult, error) {
	res := SetModeResult{Device: dev.ID, RequestedMode: target}

	var targetUnit string
	if target != ModeIdle {
		sc, ok := dev.Services[target]
		if !ok {
			return res, fmt.Errorf("unknown mode %q (available: %s)",
				target, strings.Join(ModeNames(dev), ", "))
		}
		targetUnit = sc.Systemd
		if sd.UnitStatus(targetUnit).Load == "not-found" {
			return res, fmt.Errorf("mode %q is not installed: unit %s not found", target, targetUnit)
		}
	}

	actual, desired := DeviceModes(sd, dev)
	if actual == target && desired == target {
		res.Mode = target
		return res, nil
	}

	// Stop and un-desire every other SDR service of this device. Missing
	// optional units are skipped, they are not an error.
	for name, sc := range dev.Services {
		if name == target {
			continue
		}
		st := sd.UnitStatus(sc.Systemd)
		if st.Load == "not-found" {
			continue
		}
		if err := sd.DisableNow(sc.Systemd); err != nil && st.IsRunning() {
			return res, fmt.Errorf("stop %s: %w", sc.Systemd, err)
		}
	}

	if target != ModeIdle {
		// Clear a possible StartLimit throttle from earlier failures.
		_ = sd.ResetFailed(targetUnit)
		if err := sd.EnableNow(targetUnit); err != nil {
			return res, fmt.Errorf("enable %s: %w", targetUnit, err)
		}
	}
	res.Changed = true

	deadline := time.Now().Add(timeout)
	for {
		if target == ModeIdle {
			allStopped := true
			for _, sc := range dev.Services {
				st := sd.UnitStatus(sc.Systemd)
				if st.IsRunning() {
					allStopped = false
					break
				}
			}
			if allStopped {
				res.Mode = ModeIdle
				return res, nil
			}
		} else {
			st := sd.UnitStatus(targetUnit)
			switch st.Active {
			case "active":
				res.Mode = target
				return res, nil
			case "failed":
				return res, fmt.Errorf("unit %s failed to start; see: journalctl -u %s -n 50",
					targetUnit, targetUnit)
			}
		}
		if time.Now().After(deadline) {
			cur, _ := DeviceModes(sd, dev)
			res.Mode = cur
			return res, fmt.Errorf("timed out waiting for mode %q (current: %s)", target, cur)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// LANIP returns the first global unicast IPv4 address of the host.
func LANIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		return ip.String()
	}
	return ""
}

// Equal compares two snapshots ignoring the generation timestamp. Map keys
// are sorted by encoding/json, so the comparison is deterministic.
func Equal(a, b Snapshot) bool {
	a.GeneratedAt = time.Time{}
	b.GeneratedAt = time.Time{}
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}
