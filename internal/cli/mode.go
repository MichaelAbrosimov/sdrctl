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

func printMode(cfg *config.Config, dev *config.DeviceConfig) error {
	snap, _ := agentSnapshot(cfg)
	for _, d := range snap.Devices {
		if d.ID == dev.ID {
			fmt.Println(d.Mode)
			if d.DesiredMode != d.Mode {
				fmt.Printf("desired: %s\n", d.DesiredMode)
			}
			return nil
		}
	}
	return fmt.Errorf("device %s not in snapshot", dev.ID)
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
