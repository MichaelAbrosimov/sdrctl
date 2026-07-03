package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/core"
	"github.com/MichaelAbrosimov/sdrctl/internal/systemd"
)

var logLines int

var servicesCmd = &cobra.Command{
	Use:   "services",
	Short: "Show the state of known SDR systemd services",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		snap, _ := agentSnapshot(cfg)
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "DEVICE\tMODE\tUNIT\tSTATUS\tENABLED\tPORT")
		for _, d := range snap.Devices {
			names := make([]string, 0, len(d.ServiceInfo))
			for n := range d.ServiceInfo {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				si := d.ServiceInfo[n]
				port := ""
				if si.Port != 0 {
					port = fmt.Sprintf("%d", si.Port)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					d.ID, n, si.Unit, si.Status, si.Enabled, port)
			}
		}
		w.Flush()
		return nil
	},
}

var logsCmd = &cobra.Command{
	Use:   "logs [device-id]",
	Short: "Show journal of the active SDR service",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		var dev *config.DeviceConfig
		if len(args) == 1 {
			dev, err = cfg.DeviceByID(args[0])
		} else {
			dev, err = cfg.DefaultDevice()
		}
		if err != nil {
			return err
		}
		return deviceLogs(systemd.New(), dev, logLines)
	},
}

func init() {
	logsCmd.Flags().IntVarP(&logLines, "lines", "n", 80, "number of journal lines")
	rootCmd.AddCommand(servicesCmd)
	rootCmd.AddCommand(logsCmd)
}

func deviceLogs(sd *systemd.Client, dev *config.DeviceConfig, n int) error {
	ctx := context.Background()
	actual, _ := core.DeviceModes(ctx, sd, dev)
	switch actual {
	case core.ModeIdle, core.ModeConflict, core.ModeUnknown:
		units := make([]string, 0, len(dev.Services))
		for _, sc := range dev.Services {
			units = append(units, sc.Systemd)
		}
		sort.Strings(units)
		return fmt.Errorf("device %s has no single active SDR service (mode: %s); inspect directly: journalctl -u <unit> — units: %s",
			dev.ID, actual, strings.Join(units, ", "))
	}
	out, err := sd.Logs(ctx, dev.Services[actual].Systemd, n)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}
