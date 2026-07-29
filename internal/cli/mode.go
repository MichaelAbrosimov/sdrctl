package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/MichaelAbrosimov/sdrctl/internal/agentclient"
	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/core"
	"github.com/MichaelAbrosimov/sdrctl/internal/systemd"
)

var modeCmd = &cobra.Command{
	Use:   "mode [mode]",
	Short: "Show the current SDR mode, or switch to the given one",
	Long: `Without arguments prints the current mode of the default device.

With a mode name it switches to it — 'sdrctl mode rtl-tcp' is shorthand for
'sdrctl mode set rtl-tcp', because that is what the hand types.`,
	// An explicit Args also stops cobra from rejecting a mode name as an
	// "unknown command" just because this command has subcommands.
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		dev, err := cfg.DefaultDevice()
		if err != nil {
			return err
		}
		if len(args) == 1 {
			return runModeSet(cfg, dev, args[0])
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

var (
	followMode bool
	quietMode  bool
)

func init() {
	// Both spellings switch modes, so both carry the switch flags.
	for _, c := range []*cobra.Command{modeCmd, modeSetCmd} {
		c.Flags().BoolVarP(&followMode, "follow", "f", false,
			"stream the transition log even when output is not a terminal")
		c.Flags().BoolVarP(&quietMode, "quiet", "q", false,
			"print only the result line")
	}
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
//
// A transition takes seconds and is worth watching, so at a terminal the
// journal of the device's units is streamed while it runs; piped output
// stays the single result line scripts expect. --follow/--quiet override.
func runModeSet(cfg *config.Config, dev *config.DeviceConfig, target string) error {
	live := followMode || (stdoutIsTTY() && !quietMode)
	// The unit linger waits for; empty for idle, which starts nothing.
	targetUnit := ""
	if sc, ok := dev.Services[target]; ok {
		targetUnit = sc.Systemd
	}

	var fol *follower
	started := time.Now()
	if live {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var err error
		if fol, err = startFollow(ctx, dev, os.Stdout); err != nil {
			// Journal access is a nicety, not a precondition: carry on quietly.
			fmt.Fprintf(os.Stderr, "%s\n", dim(fmt.Sprintf("(live log unavailable: %v)", err)))
			live = false
		} else {
			fmt.Printf("%s  %-9s %s\n", dim(started.Format("15:04:05")), accent("sdrctl"),
				fmt.Sprintf("requesting mode %s", bold(target)))
		}
	}

	res, err := agentclient.New(cfg.Socket.Path, cfg.ModeSetTimeout()).SetMode(dev.ID, target)
	switch {
	case err == nil:
		return reportModeSet(res, fol, started, live, targetUnit)
	case agentclient.IsUnavailable(err):
		fmt.Fprintf(os.Stderr, "warning: agent socket %s unreachable (%v)\n", cfg.Socket.Path, err)
		fmt.Fprintf(os.Stderr, "warning: falling back to driving systemctl directly\n")
	default:
		// The agent answered with an error: it is the authority, no fallback.
		fol.stop()
		return fmt.Errorf("%s: %w", dev.ID, err)
	}

	res, err = core.SetMode(context.Background(), systemd.New(), dev, target, cfg.ModeSetTimeout())
	if err != nil {
		fol.stop()
		return fmt.Errorf("%s: %w", dev.ID, err)
	}
	return reportModeSet(res, fol, started, live, targetUnit)
}

// reportModeSet prints the outcome. The follower is stopped and drained
// first so the verdict never lands between journal lines.
func reportModeSet(res core.SetModeResult, fol *follower, started time.Time, live bool, targetUnit string) error {
	if live && res.Changed {
		// Let the new unit's startup output land before the verdict.
		fol.linger(targetUnit, 400*time.Millisecond, 2500*time.Millisecond)
	}
	fol.stop()

	if !live {
		if !res.Changed {
			fmt.Printf("%s: already in mode %s — nothing to do\n", res.Device, res.Mode)
			return nil
		}
		fmt.Printf("%s: mode set to %s\n", res.Device, res.Mode)
		return nil
	}

	stamp := dim(time.Now().Format("15:04:05"))
	if !res.Changed {
		fmt.Printf("%s  %-9s %s\n", stamp, accent("sdrctl"),
			fmt.Sprintf("already in mode %s — nothing to do", bold(res.Mode)))
		return nil
	}
	// Both coordinates are verified by SetMode; say so explicitly — that is
	// the difference between "command sent" and "mode is actually on".
	fmt.Printf("%s  %-9s %s %s\n", stamp, accent("sdrctl"),
		green("✓ verified"),
		dim(fmt.Sprintf("active=%s desired=%s  %.1fs",
			res.Mode, res.Mode, time.Since(started).Seconds())))
	return nil
}
