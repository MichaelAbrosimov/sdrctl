package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/MichaelAbrosimov/sdrctl/internal/agentclient"
	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/core"
	"github.com/MichaelAbrosimov/sdrctl/internal/systemd"
)

var modeCmd = &cobra.Command{
	Use:   "mode",
	Short: "Show the current SDR mode of the default device",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		dev, err := cfg.DefaultDevice()
		if err != nil {
			return err
		}
		return printMode(cfg, dev)
	},
}

var modeSetCmd = &cobra.Command{
	Use:   "set <mode>",
	Short: "Switch the default device to a mode (idle, rtl-tcp, ...)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		dev, err := cfg.DefaultDevice()
		if err != nil {
			return err
		}
		return runModeSet(cfg, dev, args[0])
	},
}

func init() {
	modeCmd.AddCommand(modeSetCmd)
	rootCmd.AddCommand(modeCmd)
}

// printMode reads LIVE — via the agent's uncached mode endpoint, or
// directly from systemd when the agent is down. The observer snapshot is
// refreshed asynchronously after a transition, so a cached read right
// after `mode set` would show the previous mode.
func printMode(cfg *config.Config, dev *config.DeviceConfig) error {
	actual, desired, err := agentclient.New(cfg.Socket.Path, cfg.ModeSetTimeout()).DeviceMode(dev.ID)
	if err != nil {
		if !agentclient.IsUnavailable(err) {
			return fmt.Errorf("%s: %w", dev.ID, err)
		}
		actual, desired = core.DeviceModes(context.Background(), systemd.New(), dev)
	}
	fmt.Println(actual)
	if desired != actual {
		fmt.Printf("desired: %s\n", desired)
	}
	return nil
}

// runModeSet is socket-first: the agent is the single executor of mode
// transitions. Only when the agent is unreachable does the CLI drive
// systemctl directly (via the polkit rule) — the node must stay controllable
// while the agent is down.
func runModeSet(cfg *config.Config, dev *config.DeviceConfig, target string) error {
	res, err := agentclient.New(cfg.Socket.Path, cfg.ModeSetTimeout()).SetMode(dev.ID, target)
	switch {
	case err == nil:
		return reportModeSet(res)
	case agentclient.IsUnavailable(err):
		fmt.Fprintf(os.Stderr, "warning: agent socket %s unreachable (%v)\n", cfg.Socket.Path, err)
		fmt.Fprintf(os.Stderr, "warning: falling back to driving systemctl directly\n")
	default:
		// The agent answered with an error: it is the authority, no fallback.
		return fmt.Errorf("%s: %w", dev.ID, err)
	}

	res, err = core.SetMode(context.Background(), systemd.New(), dev, target, cfg.ModeSetTimeout())
	if err != nil {
		return fmt.Errorf("%s: %w", dev.ID, err)
	}
	return reportModeSet(res)
}

func reportModeSet(res core.SetModeResult) error {
	if !res.Changed {
		fmt.Printf("%s: already in mode %s — nothing to do\n", res.Device, res.Mode)
		return nil
	}
	fmt.Printf("%s: mode set to %s\n", res.Device, res.Mode)
	return nil
}
