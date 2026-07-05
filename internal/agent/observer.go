// Package agent contains the long-running observer used by `sdrctl agent`.
//
// The observer is deliberately NOT a reconcile actor: systemd owns process
// supervision (Restart=, StartLimit*, Conflicts=). The observer only watches
// state for the API/MQTT and performs one narrow recovery action that systemd
// cannot do alone: restarting a desired-but-stopped unit after its StartLimit
// was exhausted (typically when a dongle returns after re-plug).
package agent

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/core"
	"github.com/MichaelAbrosimov/sdrctl/internal/systemd"
)

type Observer struct {
	cfg   *config.Config
	sd    *systemd.Client
	coord *Coordinator
	// build is core.BuildSnapshot, injectable for concurrency tests.
	build func(context.Context, *config.Config, *systemd.Client) core.Snapshot

	// refreshMu serializes the WHOLE refresh cycle (build → compare →
	// store → notify): concurrent refreshes — the poll ticker vs the
	// post-transition refresh — must not let an older-but-slower snapshot
	// overwrite a newer one and emit a backwards MQTT event. It also
	// guarantees onChange callbacks observe snapshots in storage order.
	refreshMu sync.Mutex

	mu       sync.RWMutex
	last     core.Snapshot
	haveLast bool

	onChange    []func(prev, cur core.Snapshot)
	lastRestore map[string]time.Time
}

func New(cfg *config.Config, sd *systemd.Client, coord *Coordinator) *Observer {
	return &Observer{
		cfg: cfg, sd: sd, coord: coord,
		build:       core.BuildSnapshot,
		lastRestore: map[string]time.Time{},
	}
}

// OnChange registers a listener; must be called before Run.
func (o *Observer) OnChange(f func(prev, cur core.Snapshot)) {
	o.onChange = append(o.onChange, f)
}

// Latest returns the most recent snapshot, building one if none exists yet.
func (o *Observer) Latest() core.Snapshot {
	o.mu.RLock()
	if o.haveLast {
		s := o.last
		o.mu.RUnlock()
		return s
	}
	o.mu.RUnlock()
	return o.Refresh()
}

// Refresh rebuilds the snapshot and notifies listeners when it changed.
// The read is bounded so a hung systemctl cannot stall the observer loop or
// a post-transition refresh (which runs on the write path of the API): on
// timeout the affected fields degrade to "unknown", which after SDR-P1-01
// will surface as non-ok health rather than a silent hang.
func (o *Observer) Refresh() core.Snapshot {
	// One refresh at a time, start to finish (see refreshMu).
	o.refreshMu.Lock()
	defer o.refreshMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), o.cfg.ModeSetTimeout())
	defer cancel()
	cur := o.build(ctx, o.cfg, o.sd)

	o.mu.Lock()
	prev, had := o.last, o.haveLast
	o.last, o.haveLast = cur, true
	o.mu.Unlock()

	if had && !core.Equal(prev, cur) {
		for _, f := range o.onChange {
			f(prev, cur)
		}
	}
	return cur
}

// Run polls until the context is cancelled.
func (o *Observer) Run(ctx context.Context) {
	interval := time.Duration(o.cfg.Observer.IntervalSec) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		snap := o.Refresh()
		o.autoRestore(ctx, snap)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (o *Observer) autoRestore(ctx context.Context, s core.Snapshot) {
	if !o.cfg.Observer.AutoRestore {
		return
	}
	for _, d := range s.Devices {
		if d.Health != core.HealthDegraded {
			continue
		}
		if !d.PresenceKnown || !d.Present {
			// A control action needs POSITIVE evidence of the dongle: a
			// failed sysfs read must not be treated as presence. On hosts
			// without USB detection auto-restore is deliberately inert —
			// the conservative side of the error.
			continue
		}
		switch d.DesiredMode {
		case core.ModeIdle, core.ModeConflict, core.ModeUnknown:
			continue
		}
		if time.Since(o.lastRestore[d.ID]) < o.cfg.RestoreCooldown() {
			continue
		}
		dev, err := o.cfg.DeviceByID(d.ID)
		if err != nil {
			continue
		}
		// Auto-restore is a control action like any other: it must own the
		// same per-device guard as API writes, or it races a manual
		// transition and can enqueue jobs into a quarantined device.
		if !o.coord.TryBeginRestore(d.ID) {
			continue
		}
		o.restoreDevice(ctx, dev)
		o.coord.EndTransition(d.ID)
	}
}

// restoreDevice runs one restore attempt while holding the device guard.
// The snapshot only NOMINATES a device; every decision here is made from
// fresh reads, and the actual work goes through the same core.SetMode
// primitive as a manual transition — one code path owns disable ordering,
// the pending-job sweep and the ErrCleanupUnverified contract.
func (o *Observer) restoreDevice(ctx context.Context, dev *config.DeviceConfig) {
	opCtx, cancel := context.WithTimeout(ctx, o.cfg.ModeSetTimeout())
	defer cancel()

	// Automation gate: before its first action in this process the device
	// must be proven quiescent, exactly like the API's first write — but
	// with NO hard-cap override: automation never accepts operator risk.
	if err := o.coord.VerifyQuiescent(opCtx, o.sd, dev); err != nil {
		log.Printf("supervisor: skipping restore: %v", err)
		return
	}

	// Re-read under the guard: the snapshot that nominated this device may
	// predate a manual transition that already changed the desired mode.
	actual, desired := core.DeviceModes(opCtx, o.sd, dev)
	switch desired {
	case core.ModeIdle, core.ModeConflict, core.ModeUnknown:
		return
	}
	if actual == desired {
		return // recovered on its own (or by the manual transition)
	}

	o.lastRestore[dev.ID] = time.Now()
	log.Printf("supervisor: device %s degraded (desired %s), restoring", dev.ID, desired)
	if _, err := core.SetMode(opCtx, o.sd, dev, desired, o.cfg.ModeSetTimeout()); err != nil {
		log.Printf("supervisor: restore %s/%s: %v", dev.ID, desired, err)
		// Quarantine BEFORE the guard is released (EndTransition runs in
		// the caller), so no write slips into the ambiguous window.
		if errors.Is(err, core.ErrCleanupUnverified) {
			o.coord.Quarantine(dev.ID)
		}
	}
}
