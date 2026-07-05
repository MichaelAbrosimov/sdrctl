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
	"context"
	"encoding/json"
	"errors"
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
	ID            string `json:"id"`
	Type          string `json:"type"`
	Label         string `json:"label,omitempty"`
	Serial        string `json:"serial,omitempty"`
	Optional      bool   `json:"optional,omitempty"`
	Present       bool   `json:"present"`
	PresenceKnown bool   `json:"presence_known"`
	Mode          string `json:"mode"`
	DesiredMode   string `json:"desired_mode"`
	Health        string `json:"health"`
	// Quarantined: an earlier transition's cleanup could not be verified;
	// writes are refused until the device is proven quiescent. Set by the
	// agent's coordinator via Snapshot.ApplyQuarantine.
	Quarantined bool                     `json:"quarantined,omitempty"`
	Services    map[string]string        `json:"services"`
	ServiceInfo map[string]ServiceDetail `json:"service_details,omitempty"`
	Ports       map[string]int           `json:"ports,omitempty"`
	USB         *device.USBDevice        `json:"usb,omitempty"`
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
func BuildSnapshot(ctx context.Context, cfg *config.Config, sd *systemd.Client) Snapshot {
	snap := Snapshot{
		Node:        cfg.Node.ID,
		Role:        cfg.Node.Role,
		Version:     version.Version,
		GeneratedAt: time.Now().UTC(),
		LANIP:       LANIP(),
	}

	usb, usbErr := device.ScanUSB()
	presenceKnown := usbErr == nil
	claims := allocateUSB(cfg.Devices, usb)

	seenIDPairs := map[string]bool{}
	for i, dc := range cfg.Devices {
		ds := buildDevice(ctx, dc, sd, claims[i], presenceKnown)
		snap.Devices = append(snap.Devices, ds)

		if presenceKnown && claims[i].ambiguous {
			snap.Warnings = append(snap.Warnings, fmt.Sprintf(
				"device %s: cannot uniquely attribute a USB dongle (shared or duplicate serials) — refusing to guess; assign unique serials with rtl_eeprom",
				ds.ID))
		}

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

// usbClaim is the outcome of attributing physical dongles to one device
// configuration: exactly one owned sysfs object, nothing, or an explicit
// ambiguity that must not be silently resolved by guessing.
type usbClaim struct {
	dev       *device.USBDevice
	ambiguous bool
}

// allocateUSB attributes sysfs devices to configurations under one rule: a
// physical dongle may satisfy AT MOST one configuration. An object matching
// several configurations is granted to none of them (all get ambiguous),
// and a configuration matching several objects is ambiguous too — presence
// built on a guess would quietly control the wrong dongle (SDR-P1-05).
func allocateUSB(devs []config.DeviceConfig, usb []device.USBDevice) []usbClaim {
	candidates := make([][]device.USBDevice, len(devs))
	wantedBy := map[string]int{} // sysfs name → how many configurations match it
	for i, dc := range devs {
		candidates[i] = device.Match(usb, dc.USBVendorID, dc.USBProductID, dc.Serial)
		for _, u := range candidates[i] {
			wantedBy[u.SysName]++
		}
	}

	claims := make([]usbClaim, len(devs))
	for i := range devs {
		var sole []device.USBDevice
		contested := false
		for _, u := range candidates[i] {
			if wantedBy[u.SysName] > 1 {
				contested = true
				continue
			}
			sole = append(sole, u)
		}
		switch {
		case len(sole) == 1 && !contested:
			u := sole[0]
			claims[i] = usbClaim{dev: &u}
		case len(sole) > 1 || contested:
			claims[i] = usbClaim{ambiguous: true}
		}
	}
	return claims
}

func buildDevice(ctx context.Context, dc config.DeviceConfig, sd *systemd.Client, claim usbClaim, presenceKnown bool) DeviceStatus {
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

	// The two mode coordinates track unknown-ness INDEPENDENTLY: a partial
	// systemctl answer must neither hide an unknown UnitFileState behind
	// "idle" nor poison a perfectly known desired mode because only the
	// ActiveState read failed.
	var running, enabled []string
	activeUnknown, enabledUnknown := false, false
	for name, sc := range dc.Services {
		st := sd.UnitStatus(ctx, sc.Systemd)
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
			activeUnknown = true
		}
		if st.Enabled == "unknown" {
			enabledUnknown = true
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
	ds.Mode = modeFrom(running, activeUnknown)
	ds.DesiredMode = modeFrom(enabled, enabledUnknown)

	if presenceKnown && claim.dev != nil {
		ds.Present = true
		ds.USB = claim.dev
	}

	ds.Health = HealthFor(ds)
	if presenceKnown && claim.ambiguous {
		// Attribution is unresolvable — that is a conflict to surface, not
		// a guess to make; the snapshot carries the explaining warning.
		ds.Health = HealthConflict
	}
	return ds
}

// modeFrom folds unit observations into one mode. Priority: a confirmed
// conflict (two known owners) is the loudest; then unknown — one KNOWN
// running mode must not mask a competitor whose state could not be read,
// or SetMode would confirm success without proof; a single known name wins
// only when every other unit was read successfully.
func modeFrom(names []string, anyUnknown bool) string {
	switch {
	case len(names) > 1:
		return ModeConflict
	case anyUnknown:
		return ModeUnknown
	case len(names) == 1:
		return names[0]
	default:
		return ModeIdle
	}
}

// HealthFor derives device health from mode, desired mode and presence.
// Unknown presence (sysfs unobservable) makes the device unknown: health
// claims need proof of the dongle, and "could not look" is not proof —
// only a confirmed conflict outranks it.
func HealthFor(d DeviceStatus) string {
	if d.Mode == ModeConflict || d.DesiredMode == ModeConflict {
		return HealthConflict
	}
	if d.Mode == ModeUnknown || d.DesiredMode == ModeUnknown {
		return HealthUnknown
	}
	if !d.PresenceKnown {
		return HealthUnknown
	}
	if !d.Present {
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

// aggregate derives global ok/health. Contract (docs/api.md): ok is true
// only if every required device is healthy or idle — an UNKNOWN required
// device therefore breaks ok: "cannot observe" is not "fine". The optional-
// device exception applies only to CONFIRMED absence (presence known and
// not present), never to unobservability. Global health picks the loudest
// problem: conflict (definite, two owners of one dongle) over unknown
// (could hide anything) over degraded.
func aggregate(devices []DeviceStatus) (bool, string) {
	ok := true
	anyHealthy := false
	anyConflict := false
	anyUnknown := false
	for _, d := range devices {
		// Only CONFIRMED absence is neutral for optional devices — and
		// confirmed absence manifests as HealthMissing. An ambiguous
		// attribution also reports !Present, but its health is conflict:
		// that is a problem to surface, not an absence to excuse.
		if d.Optional && d.PresenceKnown && !d.Present && d.Health == HealthMissing {
			continue
		}
		switch d.Health {
		case HealthHealthy:
			anyHealthy = true
		case HealthIdle:
			// neutral
		case HealthUnknown:
			ok = false
			anyUnknown = true
		case HealthConflict:
			ok = false
			anyConflict = true
		default:
			ok = false
		}
	}
	switch {
	case anyConflict:
		return false, HealthConflict
	case anyUnknown:
		return false, HealthUnknown
	case !ok:
		return false, HealthDegraded
	case anyHealthy:
		return true, HealthHealthy
	default:
		return true, HealthIdle
	}
}

// ApplyQuarantine merges the agent coordinator's quarantine state into the
// snapshot: per-device flags, explaining warnings, and a re-aggregated
// global verdict — a quarantined REQUIRED device breaks ok (conscious
// decision: health-only monitoring must see the uncertainty, not just the
// 409 a refused write receives). Runs where snapshots are built, so HTTP,
// MQTT and change events all carry the same picture.
func (s *Snapshot) ApplyQuarantine(quarantined map[string]time.Time) {
	if len(quarantined) == 0 {
		return
	}
	any := false
	for i := range s.Devices {
		d := &s.Devices[i]
		since, ok := quarantined[d.ID]
		if !ok {
			continue
		}
		d.Quarantined = true
		any = true
		s.Warnings = append(s.Warnings, fmt.Sprintf(
			"device %s is quarantined since %s: an earlier transition could not be verified as cleaned up; writes are refused until the device is verified quiescent",
			d.ID, since.UTC().Format(time.RFC3339)))
	}
	if !any {
		return
	}
	// A quarantine breaks ok regardless of Optional: it is a control-plane
	// problem (writes refused), not a device absence to excuse.
	s.OK = false
	if s.Health == HealthHealthy || s.Health == HealthIdle {
		s.Health = HealthDegraded
	}
}

// DeviceModes returns the actual and desired mode of one device. The two
// coordinates track unknown-ness independently (see buildDevice).
func DeviceModes(ctx context.Context, sd *systemd.Client, dev *config.DeviceConfig) (actual, desired string) {
	var running, enabled []string
	activeUnknown, enabledUnknown := false, false
	for name, sc := range dev.Services {
		st := sd.UnitStatus(ctx, sc.Systemd)
		if st.Active == "unknown" {
			activeUnknown = true
		}
		if st.Enabled == "unknown" {
			enabledUnknown = true
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
	return modeFrom(running, activeUnknown), modeFrom(enabled, enabledUnknown)
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

// verifyPollInterval is the pause between verification reads in SetMode.
const verifyPollInterval = 300 * time.Millisecond

// SetMode switches a device to the target mode through systemd only:
// competing units are disabled (disable --now), the target unit is enabled
// (enable --now), then the outcome is verified. Desired state lives in the
// units' enabled flags — nothing is written anywhere else.
//
// The timeout is the caller-visible upper bound of the WHOLE call: a slice
// of it (a fifth, at most 2s) is reserved up front for failure cleanup, the
// rest bounds status reads, disable/enable and verification. A hung
// systemctl cannot hold the caller (and the agent's inflight guard) past the
// deadline: CommandContext kills the child. Killing systemctl does NOT
// remove a job it already enqueued in PID 1, so on every failure after the
// first mutation the cleanup slice sweeps pending jobs of this device's
// units and CONFIRMS they are gone (sweepPendingJobs). When confirmation is
// impossible — dead D-Bus, failing cancel, a job surviving cancel — the
// returned error wraps ErrCleanupUnverified and the caller must quarantine
// the device (refuse new writes) until it is verified quiescent.
//
// Success requires BOTH coordinates to converge: actual (ActiveState) and
// desired (enabled flags). A competitor left enabled, or a target that runs
// without being enabled, is a failed transition — it would resurrect the
// wrong mode after a reboot.
func SetMode(ctx context.Context, sd *systemd.Client, dev *config.DeviceConfig, target string, timeout time.Duration) (SetModeResult, error) {
	reserve := timeout / 5
	if reserve > 2*time.Second {
		reserve = 2 * time.Second
	}
	overall, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	transCtx, transCancel := context.WithDeadline(overall, time.Now().Add(timeout-reserve))
	defer transCancel()

	res := SetModeResult{Device: dev.ID, RequestedMode: target}

	var targetUnit string
	if target != ModeIdle {
		sc, ok := dev.Services[target]
		if !ok {
			return res, fmt.Errorf("unknown mode %q (available: %s)",
				target, strings.Join(ModeNames(dev), ", "))
		}
		targetUnit = sc.Systemd
		if sd.UnitStatus(transCtx, targetUnit).Load == "not-found" {
			return res, fmt.Errorf("mode %q is not installed: unit %s not found", target, targetUnit)
		}
	}

	actual, desired := DeviceModes(transCtx, sd, dev)
	if actual == target && desired == target {
		res.Mode = target
		return res, nil
	}

	// Stop and un-desire every other SDR service of this device. Missing
	// optional units are skipped; any other disable failure aborts the
	// transition — a competitor left enabled would bring the old mode back
	// after a reboot.
	for name, sc := range dev.Services {
		if name == target {
			continue
		}
		if sd.UnitStatus(transCtx, sc.Systemd).Load == "not-found" {
			continue
		}
		if err := sd.DisableNow(transCtx, sc.Systemd); err != nil {
			// Any mutation error may leave an enqueued job behind — not
			// only a context timeout: a D-Bus error after enqueue is just
			// as ambiguous. Sweep and verify before returning.
			if swErr := sweepPendingJobs(overall, sd, dev); swErr != nil {
				return res, fmt.Errorf("disable %s: %v; %w", sc.Systemd, err, swErr)
			}
			return res, fmt.Errorf("disable %s: %w", sc.Systemd, err)
		}
	}

	if target != ModeIdle {
		// Clear a possible StartLimit throttle from earlier failures.
		_ = sd.ResetFailed(transCtx, targetUnit)
		if err := sd.EnableNow(transCtx, targetUnit); err != nil {
			if swErr := sweepPendingJobs(overall, sd, dev); swErr != nil {
				return res, fmt.Errorf("enable %s: %v; %w", targetUnit, err, swErr)
			}
			return res, fmt.Errorf("enable %s: %w", targetUnit, err)
		}
	}
	res.Changed = true

	for {
		actual, desired := DeviceModes(transCtx, sd, dev)
		if actual == target && desired == target {
			res.Mode = target
			return res, nil
		}
		if target != ModeIdle && sd.UnitStatus(transCtx, targetUnit).Active == "failed" {
			return res, fmt.Errorf("unit %s failed to start; see: journalctl -u %s -n 50",
				targetUnit, targetUnit)
		}
		select {
		case <-transCtx.Done():
			// Cleanup within the reserved slice of the SAME deadline:
			// sweep whatever this transition may have left queued in
			// PID 1, then take one diagnostic read for the error message.
			swErr := sweepPendingJobs(overall, sd, dev)
			cur, curDesired := DeviceModes(overall, sd, dev)
			res.Mode = cur
			if swErr != nil {
				return res, fmt.Errorf("timed out waiting for mode %q (current: %s, desired: %s); %w",
					target, cur, curDesired, swErr)
			}
			return res, fmt.Errorf("timed out waiting for mode %q (current: %s, desired: %s)",
				target, cur, curDesired)
		case <-time.After(verifyPollInterval):
		}
	}
}

// ErrCleanupUnverified marks a failed transition whose cleanup could not be
// CONFIRMED: pending systemd jobs of the device may still exist and land
// later. Callers owning a concurrency guard must treat the device as
// ambiguous and refuse new writes until the state is verified quiescent.
var ErrCleanupUnverified = errors.New("cleanup after failed transition is unverified: pending systemd jobs may still apply")

// sweepPendingJobs cancels queued systemd jobs of the device's units and
// CONFIRMS they are gone by re-listing. Every failure path — listing,
// cancelling, or a job surviving a formally successful cancel — returns an
// error wrapping ErrCleanupUnverified; nil means "verified: nothing of this
// device is pending in PID 1".
func sweepPendingJobs(ctx context.Context, sd *systemd.Client, dev *config.DeviceConfig) error {
	jobs, err := sd.PendingJobs(ctx)
	if err != nil {
		return fmt.Errorf("%w: list-jobs: %v", ErrCleanupUnverified, err)
	}
	for _, sc := range dev.Services {
		if id, ok := jobs[sc.Systemd]; ok {
			if err := sd.CancelJob(ctx, id); err != nil {
				return fmt.Errorf("%w: cancel job %s (%s): %v", ErrCleanupUnverified, id, sc.Systemd, err)
			}
		}
	}
	jobs, err = sd.PendingJobs(ctx)
	if err != nil {
		return fmt.Errorf("%w: recheck after cancel: %v", ErrCleanupUnverified, err)
	}
	for _, sc := range dev.Services {
		if id, ok := jobs[sc.Systemd]; ok {
			return fmt.Errorf("%w: job %s (%s) still pending after cancel", ErrCleanupUnverified, id, sc.Systemd)
		}
	}
	return nil
}

// DeviceQuiescent reports whether the device is verifiably at rest: no
// pending systemd jobs touch its units and no unit is in a transitional
// ActiveState. An unobservable state (list-jobs failing, unit state
// unknown) is an error, not "quiescent" — absence of evidence is not
// evidence of absence here.
//
// The returned fingerprint is a deterministic digest of every unit's
// (active, enabled) pair, taken from the SAME reads that produced the
// verdict: callers demanding stability across N probes compare the
// fingerprints — a device flapping between two quiescent-looking states is
// not settled.
func DeviceQuiescent(ctx context.Context, sd *systemd.Client, dev *config.DeviceConfig) (bool, string, error) {
	jobs, err := sd.PendingJobs(ctx)
	if err != nil {
		return false, "", err
	}
	for _, sc := range dev.Services {
		if _, ok := jobs[sc.Systemd]; ok {
			return false, "", nil
		}
	}
	parts := make([]string, 0, len(dev.Services))
	for _, sc := range dev.Services {
		st := sd.UnitStatus(ctx, sc.Systemd)
		switch st.Active {
		case "activating", "deactivating", "reloading":
			return false, "", nil
		case "unknown":
			return false, "", fmt.Errorf("unit %s state is unobservable", sc.Systemd)
		}
		// Unknown in EITHER fingerprint coordinate is unobservability, not
		// rest: a partial systemd answer must never certify quiescence.
		if st.Enabled == "unknown" {
			return false, "", fmt.Errorf("unit %s enabled state is unobservable", sc.Systemd)
		}
		parts = append(parts, sc.Systemd+"="+st.Active+"/"+st.Enabled)
	}
	sort.Strings(parts)
	return true, strings.Join(parts, ";"), nil
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
