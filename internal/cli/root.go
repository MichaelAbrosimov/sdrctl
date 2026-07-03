// Package cli wires the sdrctl commands.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/version"
)

var cfgPath string

var rootCmd = &cobra.Command{
	Use:   "sdrctl",
	Short: "Local control plane for an SDR node",
	Long: `sdrctl is the local control agent and CLI of a headless SDR radio node.

systemd owns every SDR process; sdrctl only drives systemctl/journalctl and
reports state. The future sdr-manager talks to this node exclusively through
the HTTP API served by 'sdrctl agent'.`,
	SilenceUsage: true,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgPath, "config", config.DefaultPath, "path to config file")
	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print sdrctl version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(version.Version)
		},
	})
}

func loadConfig() (*config.Config, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	if !cfg.Loaded {
		fmt.Fprintf(os.Stderr, "warning: config %s not found, using defaults\n", cfgPath)
	}
	return cfg, nil
}
