package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/core"
	"github.com/MichaelAbrosimov/sdrctl/internal/device"
	"github.com/MichaelAbrosimov/sdrctl/internal/systemd"
)

var devicesCmd = &cobra.Command{
	Use:   "devices",
	Short: "List configured SDR devices",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		snap := core.BuildSnapshot(cfg, systemd.New())
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "DEVICE\tTYPE\tLABEL\tSERIAL\tPRESENT\tMODE\tHEALTH\tDEFAULT")
		for i, d := range snap.Devices {
			def := ""
			if cfg.Devices[i].Default {
				def = "yes"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				d.ID, d.Type, d.Label, d.Serial, presentString(d), d.Mode, d.Health, def)
		}
		w.Flush()
		for _, warn := range snap.Warnings {
			fmt.Printf("\nwarning: %s\n", warn)
		}
		return nil
	},
}

var deviceCmd = &cobra.Command{
	Use:   "device [id] [status|logs|mode [set <mode>]]",
	Short: "Inspect or control a single SDR device",
	Long: `Without arguments shows the default device. Examples:

  sdrctl device rtl-sdr-01
  sdrctl device rtl-sdr-01 status
  sdrctl device rtl-sdr-01 logs
  sdrctl device rtl-sdr-01 mode
  sdrctl device rtl-sdr-01 mode set rtl-tcp`,
	Args: cobra.ArbitraryArgs,
	RunE: deviceDispatch,
}

func init() {
	rootCmd.AddCommand(devicesCmd)
	rootCmd.AddCommand(deviceCmd)
}

func deviceDispatch(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	var dev *config.DeviceConfig
	rest := args
	if len(args) == 0 {
		if dev, err = cfg.DefaultDevice(); err != nil {
			return err
		}
	} else {
		if dev, err = cfg.DeviceByID(args[0]); err != nil {
			return fmt.Errorf("%v (configured: %s)", err, strings.Join(deviceIDs(cfg), ", "))
		}
		rest = args[1:]
	}

	sd := systemd.New()
	switch {
	case len(rest) == 0, rest[0] == "status":
		return printDeviceDetail(cfg, sd, dev)
	case rest[0] == "logs":
		return deviceLogs(sd, dev, logLines)
	case rest[0] == "mode" && len(rest) == 1:
		return printMode(dev)
	case rest[0] == "mode" && len(rest) == 3 && rest[1] == "set":
		return runModeSet(dev, rest[2])
	default:
		return fmt.Errorf("unknown device subcommand %q", strings.Join(rest, " "))
	}
}

func deviceIDs(cfg *config.Config) []string {
	ids := make([]string, len(cfg.Devices))
	for i, d := range cfg.Devices {
		ids[i] = d.ID
	}
	return ids
}

func printDeviceDetail(cfg *config.Config, sd *systemd.Client, dev *config.DeviceConfig) error {
	snap := core.BuildSnapshot(cfg, sd)
	var ds *core.DeviceStatus
	for i := range snap.Devices {
		if snap.Devices[i].ID == dev.ID {
			ds = &snap.Devices[i]
			break
		}
	}
	if ds == nil {
		return fmt.Errorf("device %s not in snapshot", dev.ID)
	}

	fmt.Printf("Device:   %s (%s)\n", ds.ID, ds.Type)
	if ds.Label != "" {
		fmt.Printf("Label:    %s\n", ds.Label)
	}
	if ds.Serial != "" {
		fmt.Printf("Serial:   %s (configured)\n", ds.Serial)
	}
	fmt.Printf("Mode:     %s (desired: %s)\n", ds.Mode, ds.DesiredMode)
	fmt.Printf("Health:   %s\n", ds.Health)

	switch {
	case !ds.PresenceKnown:
		fmt.Println("USB:      detection unavailable on this host (no sysfs)")
	case ds.USB != nil:
		fmt.Printf("USB:      %s %s:%s serial=%s (%s)\n",
			ds.USB.Product, ds.USB.VendorID, ds.USB.ProductID, ds.USB.Serial, ds.USB.SysName)
	default:
		fmt.Printf("USB:      device not found (looking for %s:%s",
			dev.USBVendorID, dev.USBProductID)
		if dev.Serial != "" {
			fmt.Printf(" serial=%s", dev.Serial)
		}
		fmt.Println(")")
	}

	fmt.Println("\nServices:")
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "  MODE\tUNIT\tSTATUS\tENABLED\tPORT\tRESTARTS")
	names := make([]string, 0, len(ds.ServiceInfo))
	for n := range ds.ServiceInfo {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		si := ds.ServiceInfo[n]
		port := ""
		if si.Port != 0 {
			port = fmt.Sprintf("%d", si.Port)
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\n",
			n, si.Unit, si.Status, si.Enabled, port, si.Restarts)
	}
	w.Flush()

	if ds.Mode != core.ModeIdle && ds.Mode != core.ModeUnknown && ds.Mode != core.ModeConflict {
		if si, ok := ds.ServiceInfo[ds.Mode]; ok {
			fmt.Printf("\nnote: dongle is busy (%s). Do not run rtl_test while the service is active.\n", si.Unit)
		}
	}

	if lsusb, err := device.Lsusb(); err == nil {
		fmt.Println("\nlsusb reference:")
		for _, line := range strings.Split(strings.TrimSpace(lsusb), "\n") {
			fmt.Println("  " + line)
		}
	}

	for _, warn := range snap.Warnings {
		fmt.Printf("\nwarning: %s\n", warn)
	}
	return nil
}
