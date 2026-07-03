package cli

import (
	"github.com/MichaelAbrosimov/sdrctl/internal/agentclient"
	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/core"
	"github.com/MichaelAbrosimov/sdrctl/internal/systemd"
)

// agentSnapshot prefers the agent's snapshot (via the local socket) over
// building one locally, so read commands show exactly what the agent — and
// therefore the network API and MQTT — sees. Reads fall back silently: a
// locally built snapshot derives from the same sources of truth (systemd,
// sysfs) and needs no privileges.
func agentSnapshot(cfg *config.Config) (core.Snapshot, bool) {
	if snap, err := agentclient.New(cfg.Socket.Path, cfg.ModeSetTimeout()).Status(); err == nil {
		return snap, true
	}
	return core.BuildSnapshot(cfg, systemd.New()), false
}
