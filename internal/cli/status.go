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
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show node and device status",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		snap, viaAgent := agentSnapshot(cfg)
		printStatus(cfg, snap, viaAgent)
		return nil
	},
}

func init() { rootCmd.AddCommand(statusCmd) }

func printStatus(cfg *config.Config, snap core.Snapshot, viaAgent bool) {
	role := ""
	if snap.Role != "" {
		role = " (" + snap.Role + ")"
	}
	fmt.Printf("Node:     %s%s\n", snap.Node, role)
	fmt.Printf("Version:  %s\n", snap.Version)
	note := ""
	if !cfg.Loaded {
		note = " (not found, defaults in use)"
	}
	fmt.Printf("Config:   %s%s\n", cfg.Path, note)
	if snap.LANIP != "" {
		fmt.Printf("LAN IP:   %s\n", snap.LANIP)
	}
	fmt.Printf("Agent:    %s\n", agentLine(cfg, viaAgent))
	fmt.Printf("API:      %s\n", apiLine(cfg))
	fmt.Printf("MQTT:     %s\n", mqttLine(cfg))
	fmt.Printf("Health:   %s (ok=%v)\n", snap.Health, snap.OK)

	if len(snap.Devices) > 0 {
		fmt.Println()
		printDevicesTable(snap)
	}
	for _, warn := range snap.Warnings {
		fmt.Printf("\nwarning: %s\n", warn)
	}
}

func agentLine(cfg *config.Config, viaAgent bool) string {
	if viaAgent {
		return "running — " + cfg.Socket.Path
	}
	return "unreachable (" + cfg.Socket.Path + ") — state read directly from systemd"
}

func apiLine(cfg *config.Config) string {
	if !cfg.API.Enabled {
		return "disabled"
	}
	url := "http://" + cfg.API.Listen
	if cfg.API.WriteEnabled {
		return url + " (read/write)"
	}
	return url + " (read-only)"
}

func mqttLine(cfg *config.Config) string {
	if !cfg.MQTT.Enabled {
		return "disabled"
	}
	return cfg.MQTT.Broker + " (publish-only, prefix " + cfg.MQTT.TopicPrefix + ")"
}

func printDevicesTable(snap core.Snapshot) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "DEVICE\tLABEL\tPRESENT\tMODE\tDESIRED\tHEALTH\tSERVICES")
	for _, d := range snap.Devices {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			d.ID, d.Label, presentString(d), d.Mode, d.DesiredMode, d.Health, servicesSummary(d))
	}
	w.Flush()
}

func presentString(d core.DeviceStatus) string {
	switch {
	case !d.PresenceKnown:
		return "unknown"
	case d.Present:
		return "yes"
	default:
		return "no"
	}
}

func servicesSummary(d core.DeviceStatus) string {
	names := make([]string, 0, len(d.Services))
	for n := range d.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		s := n + ":" + d.Services[n]
		if port, ok := d.Ports[n]; ok && d.Services[n] == "active" {
			s += fmt.Sprintf("(:%d)", port)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}
