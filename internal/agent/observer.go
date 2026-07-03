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
	"log"
	"sync"
	"time"

	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/core"
	"github.com/MichaelAbrosimov/sdrctl/internal/systemd"
)

type Observer struct {
	cfg *config.Config
	sd  *systemd.Client

	mu       sync.RWMutex
	last     core.Snapshot
	haveLast bool

	onChange    []func(prev, cur core.Snapshot)
	lastRestore map[string]time.Time
}

func New(cfg *config.Config, sd *systemd.Client) *Observer {
	return &Observer{cfg: cfg, sd: sd, lastRestore: map[string]time.Time{}}
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
// Reads use a background context: a snapshot in progress is cheap and may
// finish even while the agent is shutting down.
func (o *Observer) Refresh() core.Snapshot {
	cur := core.BuildSnapshot(context.Background(), o.cfg, o.sd)

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
		if d.PresenceKnown && !d.Present {
			continue // dongle is gone; nothing to restore until it returns
		}
		switch d.DesiredMode {
		case core.ModeIdle, core.ModeConflict, core.ModeUnknown:
			continue
		}
		if time.Since(o.lastRestore[d.ID]) < o.cfg.RestoreCooldown() {
			continue
		}
		det, ok := d.ServiceInfo[d.DesiredMode]
		if !ok {
			continue
		}
		o.lastRestore[d.ID] = time.Now()
		log.Printf("supervisor: device %s degraded (desired %s), restarting %s",
			d.ID, d.DesiredMode, det.Unit)
		// Each attempt is bounded like a mode transition, so a hung
		// systemctl cannot stall the observer loop.
		attemptCtx, cancel := context.WithTimeout(ctx, o.cfg.ModeSetTimeout())
		_ = o.sd.ResetFailed(attemptCtx, det.Unit)
		if err := o.sd.Restart(attemptCtx, det.Unit); err != nil {
			log.Printf("supervisor: restart %s: %v", det.Unit, err)
		}
		cancel()
	}
}
