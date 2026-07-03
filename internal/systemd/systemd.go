// Package systemd wraps systemctl/journalctl invocations.
//
// systemd is the single owner of SDR processes and of both state dimensions:
// ActiveState is the actual mode, UnitFileState (enabled/disabled) is the
// desired mode. sdrctl never starts SDR binaries directly and keeps no state
// files of its own.
package systemd

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// NotInstalled is the status reported for units systemd does not know about.
const NotInstalled = "not-installed"

// Runner executes a command and returns its combined output. It exists so
// tests can substitute a fake systemctl.
type Runner func(name string, args ...string) (string, error)

type Client struct {
	run Runner
}

func New() *Client { return &Client{run: runCommand} }

func NewWithRunner(r Runner) *Client { return &Client{run: r} }

func runCommand(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), fmt.Errorf("%s %s: %w: %s",
			name, strings.Join(args, " "), err, strings.TrimSpace(buf.String()))
	}
	return buf.String(), nil
}

// UnitStatus is a point-in-time view of one systemd unit.
type UnitStatus struct {
	Unit     string
	Load     string // loaded | not-found | unknown
	Active   string // active | inactive | failed | activating | ... | not-installed | unknown
	Enabled  string // enabled | disabled | static | ... | not-installed | unknown
	Restarts string // NRestarts counter
	Since    string // ActiveEnterTimestamp
}

// UnitStatus queries a single unit. Errors degrade to "unknown" fields so a
// host without systemd (e.g. a dev machine) still gets a usable answer.
func (c *Client) UnitStatus(unit string) UnitStatus {
	st := UnitStatus{Unit: unit, Load: "unknown", Active: "unknown", Enabled: "unknown"}
	out, err := c.run("systemctl", "show", unit, "--no-pager",
		"-p", "LoadState,ActiveState,UnitFileState,NRestarts,ActiveEnterTimestamp")
	if err != nil {
		return st
	}
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "LoadState":
			st.Load = val
		case "ActiveState":
			st.Active = val
		case "UnitFileState":
			st.Enabled = val
		case "NRestarts":
			st.Restarts = val
		case "ActiveEnterTimestamp":
			st.Since = val
		}
	}
	if st.Load == "not-found" {
		st.Active = NotInstalled
		st.Enabled = NotInstalled
	}
	if st.Enabled == "" {
		st.Enabled = "-"
	}
	return st
}

// IsEnabled reports whether the unit carries the desired mode.
func (u UnitStatus) IsEnabled() bool {
	return u.Enabled == "enabled" || u.Enabled == "enabled-runtime"
}

// IsRunning reports whether the unit currently owns the dongle.
func (u UnitStatus) IsRunning() bool {
	return u.Active == "active" || u.Active == "activating" || u.Active == "reloading"
}

// EnableNow marks the unit as desired and starts it (enable --now).
func (c *Client) EnableNow(unit string) error {
	_, err := c.run("systemctl", "enable", "--now", unit)
	return err
}

// DisableNow clears the desired flag and stops the unit (disable --now).
func (c *Client) DisableNow(unit string) error {
	_, err := c.run("systemctl", "disable", "--now", unit)
	return err
}

func (c *Client) Restart(unit string) error {
	_, err := c.run("systemctl", "restart", unit)
	return err
}

// ResetFailed clears the failed state so a unit throttled by StartLimit can
// be started again.
func (c *Client) ResetFailed(unit string) error {
	_, err := c.run("systemctl", "reset-failed", unit)
	return err
}

// Logs returns the last n journal lines of a unit.
func (c *Client) Logs(unit string, n int) (string, error) {
	return c.run("journalctl", "-u", unit, "-n", strconv.Itoa(n), "--no-pager")
}
