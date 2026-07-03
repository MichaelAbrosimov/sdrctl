package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/core"
	"github.com/MichaelAbrosimov/sdrctl/internal/systemd"
)

const modeSetTimeout = 15 * time.Second

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
		return printMode(dev)
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
		return runModeSet(dev, args[0])
	},
}

func init() {
	modeCmd.AddCommand(modeSetCmd)
	rootCmd.AddCommand(modeCmd)
}

func printMode(dev *config.DeviceConfig) error {
	actual, desired := core.DeviceModes(systemd.New(), dev)
	fmt.Println(actual)
	if desired != actual {
		fmt.Printf("desired: %s\n", desired)
	}
	return nil
}

func runModeSet(dev *config.DeviceConfig, target string) error {
	res, err := core.SetMode(systemd.New(), dev, target, modeSetTimeout)
	if err != nil {
		return fmt.Errorf("%s: %w", dev.ID, err)
	}
	if !res.Changed {
		fmt.Printf("%s: already in mode %s — nothing to do\n", dev.ID, res.Mode)
		return nil
	}
	fmt.Printf("%s: mode set to %s\n", dev.ID, res.Mode)
	return nil
}
