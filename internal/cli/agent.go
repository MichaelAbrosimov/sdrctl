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
		obs := agent.New(cfg, sd)

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

		if cfg.API.Enabled {
			srv := api.New(cfg, sd, obs)
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
