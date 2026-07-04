package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/core"
	"github.com/MichaelAbrosimov/sdrctl/internal/systemd"
)

// ErrShuttingDown is returned by BeginTransition after shutdown has begun.
var ErrShuttingDown = errors.New("agent is shutting down")

type quarantineRec struct {
	since time.Time
	// capNotified: the hard-cap expiry was already reported to a caller;
	// the next write proceeds at the operator's explicit risk.
	capNotified bool
}

// Coordinator is the single per-device transition guard of the agent.
// EVERY control action — API writes and the observer's auto-restore alike —
// must pass through it; that is what makes "single executor" a process-wide
// invariant instead of an API-local one.
//
// It also owns the quarantine of devices whose failed transition could not
// be verified as cleaned up, and the "verified since agent start" flags:
// pending systemd jobs deliberately survive both the systemctl client and
// the agent process, so a fresh process must not trust a device until it is
// verified quiescent once.
type Coordinator struct {
	mu         sync.Mutex
	inflight   map[string]bool
	closing    bool
	quarantine map[string]quarantineRec
	verified   map[string]bool
	wg         sync.WaitGroup
}

func NewCoordinator() *Coordinator {
	return &Coordinator{
		inflight:   map[string]bool{},
		quarantine: map[string]quarantineRec{},
		verified:   map[string]bool{},
	}
}

// BeginTransition atomically checks the shutdown flag, the inflight guard
// AND the quarantine, then registers the transition in the drain group —
// one critical section, so neither WaitTransitions nor a quarantine set by
// a just-failed transition can be missed between an external gate check and
// the claim.
func (c *Coordinator) BeginTransition(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return ErrShuttingDown
	}
	if c.inflight[id] {
		return fmt.Errorf("mode change already in progress for device %s", id)
	}
	if _, q := c.quarantine[id]; q {
		return fmt.Errorf("device %s was quarantined concurrently; retry to verify its state", id)
	}
	c.inflight[id] = true
	c.wg.Add(1)
	return nil
}

// TryBeginRestore is the auto-restore entry: same guard, but it never
// verifies or lifts anything — it simply refuses while the device is being
// transitioned, is quarantined, or the agent is shutting down.
func (c *Coordinator) TryBeginRestore(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing || c.inflight[id] {
		return false
	}
	if _, q := c.quarantine[id]; q {
		return false
	}
	c.inflight[id] = true
	c.wg.Add(1)
	return true
}

// EndTransition releases the device guard. It must be the LAST thing the
// worker does, so the drain group covers the whole lifecycle.
func (c *Coordinator) EndTransition(id string) {
	c.mu.Lock()
	delete(c.inflight, id)
	c.mu.Unlock()
	c.wg.Done()
}

// Quarantine marks the device after an unverified cleanup. The entry time
// starts the hard-cap clock; re-entering does not reset it.
func (c *Coordinator) Quarantine(id string) {
	c.mu.Lock()
	if _, ok := c.quarantine[id]; !ok {
		c.quarantine[id] = quarantineRec{since: time.Now()}
	}
	c.mu.Unlock()
}

// GateWrite decides whether a write may proceed for the device. Three
// states exist per device in this process:
//
//   - verified: a previous check proved quiescence — writes flow freely;
//   - unverified (fresh agent start): pending jobs of a PREVIOUS process
//     may exist, so the first write must verify quiescence once;
//   - quarantined: an unverified cleanup happened — writes are refused
//     until quiescence is verified or the hard cap expires.
//
// The hard-cap exit is deliberately loud: the first request after expiry is
// refused with an explanation, the retry proceeds — the caller, not just
// journald, learns that the device was never verified.
func (c *Coordinator) GateWrite(ctx context.Context, sd *systemd.Client, dev *config.DeviceConfig, hardCap time.Duration) error {
	c.mu.Lock()
	rec, quarantined := c.quarantine[dev.ID]
	verified := c.verified[dev.ID]
	c.mu.Unlock()

	if !quarantined && verified {
		return nil
	}

	if quarantined && time.Since(rec.since) > hardCap {
		c.mu.Lock()
		rec, still := c.quarantine[dev.ID]
		switch {
		case !still:
			// lifted concurrently — fall through to the verified check below
		case rec.capNotified:
			delete(c.quarantine, dev.ID)
			c.verified[dev.ID] = true
			c.mu.Unlock()
			log.Printf("coordinator: device %s leaves quarantine by acknowledged hard cap (%s) WITHOUT verification", dev.ID, hardCap)
			return nil
		default:
			rec.capNotified = true
			c.quarantine[dev.ID] = rec
			c.mu.Unlock()
			return fmt.Errorf("device %s: quarantine hard cap (%s) expired but the state was never verified; repeat the request to proceed at operator's risk", dev.ID, hardCap)
		}
		c.mu.Unlock()
	}

	quiet, err := core.DeviceQuiescent(ctx, sd, dev)
	if err == nil && quiet {
		c.mu.Lock()
		delete(c.quarantine, dev.ID)
		c.verified[dev.ID] = true
		c.mu.Unlock()
		if quarantined {
			log.Printf("coordinator: device %s verified quiescent, quarantine lifted", dev.ID)
		}
		return nil
	}

	// A failed first-write verification enters quarantine so the hard-cap
	// clock runs from the first refusal, same as after a failed transition.
	if !quarantined {
		c.Quarantine(dev.ID)
	}
	if err != nil {
		return fmt.Errorf("device %s is quarantined and cannot be verified quiescent (%v); writes are refused until verification succeeds", dev.ID, err)
	}
	return fmt.Errorf("device %s is quarantined: pending systemd activity from an earlier transition has not settled; writes are refused", dev.ID)
}

// VerifyQuiescent is the automation-grade verification gate: unlike
// GateWrite it NEVER applies the hard-cap override — automation must not
// accept risk on the operator's behalf — and never lifts a quarantine.
// A device is trusted only after one proven quiescence per process; success
// marks it verified for everyone (the API's first write reuses the flag).
// Callers must hold the device guard.
func (c *Coordinator) VerifyQuiescent(ctx context.Context, sd *systemd.Client, dev *config.DeviceConfig) error {
	c.mu.Lock()
	_, quarantined := c.quarantine[dev.ID]
	verified := c.verified[dev.ID]
	c.mu.Unlock()
	if quarantined {
		return fmt.Errorf("device %s is quarantined", dev.ID)
	}
	if verified {
		return nil
	}
	quiet, err := core.DeviceQuiescent(ctx, sd, dev)
	if err != nil {
		return fmt.Errorf("device %s cannot be verified quiescent: %w", dev.ID, err)
	}
	if !quiet {
		return fmt.Errorf("device %s has pending systemd activity", dev.ID)
	}
	c.mu.Lock()
	c.verified[dev.ID] = true
	c.mu.Unlock()
	return nil
}

// Go runs fn asynchronously inside the drain group, unless shutdown has
// already begun.
func (c *Coordinator) Go(fn func()) {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return
	}
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		fn()
	}()
}

// Wait refuses new transitions, then blocks until every registered one
// (including tracked refreshes and restores) finishes. Setting closing
// under the same mutex as BeginTransition removes the Add-vs-Wait race.
func (c *Coordinator) Wait() {
	c.mu.Lock()
	c.closing = true
	c.mu.Unlock()
	c.wg.Wait()
}
