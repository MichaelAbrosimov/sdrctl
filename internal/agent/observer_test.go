package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/core"
	"github.com/MichaelAbrosimov/sdrctl/internal/systemd"
)

// SDR-P2-02: the whole refresh cycle is serialized — concurrent refreshes
// must never overlap, and listeners must see snapshots in storage order
// (no backwards events).
func TestRefreshSerialized(t *testing.T) {
	cfg := coordTestConfig()
	obs := New(cfg, systemd.New(), NewCoordinator())

	var inBuild, maxConcurrent, seq int64
	obs.build = func(context.Context, *config.Config, *systemd.Client) core.Snapshot {
		n := atomic.AddInt64(&inBuild, 1)
		for {
			m := atomic.LoadInt64(&maxConcurrent)
			if n <= m || atomic.CompareAndSwapInt64(&maxConcurrent, m, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond) // widen any race window
		defer atomic.AddInt64(&inBuild, -1)
		return core.Snapshot{Node: "n", Version: time.Now().String(),
			Warnings: []string{time.Now().String()}, GeneratedAt: time.Now()}
	}

	var notifyMu sync.Mutex
	var order []int64
	obs.OnChange(func(prev, cur core.Snapshot) {
		notifyMu.Lock()
		order = append(order, atomic.AddInt64(&seq, 1))
		notifyMu.Unlock()
	})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			obs.Refresh()
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&maxConcurrent); got != 1 {
		t.Errorf("refresh cycles overlapped: max concurrency %d, want 1", got)
	}
	notifyMu.Lock()
	defer notifyMu.Unlock()
	for i := 1; i < len(order); i++ {
		if order[i] < order[i-1] {
			t.Errorf("onChange out of order: %v", order)
		}
	}
}
