package cli

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/MichaelAbrosimov/sdrctl/internal/agent"
	"github.com/MichaelAbrosimov/sdrctl/internal/api"
	"github.com/MichaelAbrosimov/sdrctl/internal/core"
	"github.com/MichaelAbrosimov/sdrctl/internal/mqtt"
	"github.com/MichaelAbrosimov/sdrctl/internal/systemd"
	"github.com/MichaelAbrosimov/sdrctl/internal/version"
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Run the sdrctl agent (HTTP API, state observer, MQTT telemetry)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		sd := systemd.New()
		// One coordinator = one per-device transition guard for the whole
		// process: API writes and auto-restore share it.
		coord := agent.NewCoordinator()
		obs := agent.New(cfg, sd, coord)

		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		if cfg.MQTT.Enabled {
			pub := mqtt.New(cfg.MQTT, obs.Latest)
			obs.OnChange(pub.PublishSnapshot)
			pub.Start()
			defer pub.Close()
			log.Printf("mqtt: publishing to %s (prefix %s)", cfg.MQTT.Broker, cfg.MQTT.TopicPrefix)
			if cfg.MQTT.HeartbeatSec > 0 {
				go heartbeat(ctx, pub, obs, time.Duration(cfg.MQTT.HeartbeatSec)*time.Second)
			}
		}

		srv := api.New(cfg, sd, obs, coord)

		// The local socket is the primary control channel: always on while
		// the agent runs, independent of the network API below. socketDone
		// lets shutdown wait for the socket file to be removed.
		socketDone := make(chan struct{})
		go func() {
			defer close(socketDone)
			log.Printf("socket: listening on %s (group %s, read/write)",
				cfg.Socket.Path, cfg.Socket.Group)
			if err := srv.RunSocket(ctx, cfg.Socket.Path, cfg.Socket.Group); err != nil &&
				!errors.Is(err, http.ErrServerClosed) {
				log.Printf("socket: %v", err)
				stop()
			}
		}()

		if cfg.API.Enabled {
			go func() {
				log.Printf("api: listening on %s (write_enabled=%v)",
					cfg.API.Listen, cfg.API.WriteEnabled)
				if err := srv.Run(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Printf("api: %v", err)
					stop()
				}
			}()
		}

		log.Printf("sdrctl agent %s started (node %s, observer every %ds)",
			version.Version, cfg.Node.ID, cfg.Observer.IntervalSec)
		obs.Run(ctx)
		<-socketDone
		// Drain in-flight transitions before exiting: they are deliberately
		// not cancelled (systemd would complete their queued jobs anyway)
		// and each is self-bounded by ModeSetTimeout.
		srv.WaitTransitions()
		log.Printf("sdrctl agent stopped")
		return nil
	},
}

func init() { rootCmd.AddCommand(agentCmd) }

func heartbeat(ctx context.Context, pub *mqtt.Publisher, obs *agent.Observer, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pub.PublishSnapshot(core.Snapshot{}, obs.Latest())
		}
	}
}
