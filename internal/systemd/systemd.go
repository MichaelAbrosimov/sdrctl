// Package systemd wraps systemctl/journalctl invocations.
//
// systemd is the single owner of SDR processes and of both state dimensions:
// ActiveState is the actual mode, UnitFileState (enabled/disabled) is the
// desired mode. sdrctl never starts SDR binaries directly and keeps no state
// files of its own.
package systemd

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NotInstalled is the status reported for units systemd does not know about.
const NotInstalled = "not-installed"

// Runner executes a command and returns its combined output. It exists so
// tests can substitute a fake systemctl. The context bounds the execution:
// a hung systemctl is killed when the context expires.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

type Client struct {
	run Runner
}

func New() *Client { return &Client{run: runCommand} }

func NewWithRunner(r Runner) *Client { return &Client{run: r} }

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		// Prefer the context error: "deadline exceeded" says more than
		// "signal: killed" left behind by CommandContext.
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
		err = fmt.Errorf("%s %s: %w: %s",
			name, strings.Join(args, " "), err, strings.TrimSpace(buf.String()))
		// The public state model deliberately degrades errors to "unknown";
		// the CAUSE still belongs in the journal — rate-limited, because
		// the observer re-reads every few seconds and a dead D-Bus would
		// otherwise flood it with identical lines.
		logRateLimited(name+" "+args[0], "systemd: %v", err)
		return buf.String(), err
	}
	return buf.String(), nil
}

const logRateInterval = 30 * time.Second

var (
	logMu   sync.Mutex
	lastLog = map[string]time.Time{}
)

func logRateLimited(key, format string, args ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	if time.Since(lastLog[key]) < logRateInterval {
		return
	}
	lastLog[key] = time.Now()
	log.Printf(format, args...)
}

// UnitStatus is a point-in-time view of one systemd unit.
type UnitStatus struct {
	Unit     string
	Load     string // loaded | not-found | unknown
	Active   string // active | inactive | failed | activating | ... | not-installed | unknown
	Enabled  string // enabled | disabled | static | ... | not-installed | unknown
	Restarts string // NRestarts counter
	Since    string // ActiveEnterTimestamp
	MainPID  int    // 0 when the unit is not running
}

// UnitStatus queries a single unit. Errors degrade to "unknown" fields so a
// host without systemd (e.g. a dev machine) still gets a usable answer.
func (c *Client) UnitStatus(ctx context.Context, unit string) UnitStatus {
	st := UnitStatus{Unit: unit, Load: "unknown", Active: "unknown", Enabled: "unknown"}
	out, err := c.run(ctx, "systemctl", "show", unit, "--no-pager",
		"-p", "LoadState,ActiveState,UnitFileState,NRestarts,ActiveEnterTimestamp,MainPID")
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
		case "MainPID":
			st.MainPID, _ = strconv.Atoi(val)
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
func (c *Client) EnableNow(ctx context.Context, unit string) error {
	_, err := c.run(ctx, "systemctl", "enable", "--now", unit)
	return err
}

// DisableNow clears the desired flag and stops the unit (disable --now).
func (c *Client) DisableNow(ctx context.Context, unit string) error {
	_, err := c.run(ctx, "systemctl", "disable", "--now", unit)
	return err
}

func (c *Client) Restart(ctx context.Context, unit string) error {
	_, err := c.run(ctx, "systemctl", "restart", unit)
	return err
}

// ResetFailed clears the failed state so a unit throttled by StartLimit can
// be started again.
func (c *Client) ResetFailed(ctx context.Context, unit string) error {
	_, err := c.run(ctx, "systemctl", "reset-failed", unit)
	return err
}

// PendingJobs returns systemd's queued/running jobs as unit → job id.
// Killing a systemctl client does not remove a job it already enqueued in
// PID 1; this is how such jobs are found for cancellation.
func (c *Client) PendingJobs(ctx context.Context) (map[string]string, error) {
	out, err := c.run(ctx, "systemctl", "list-jobs", "--no-pager", "--no-legend")
	if err != nil {
		return nil, err
	}
	jobs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 {
			jobs[fields[1]] = fields[0]
		}
	}
	return jobs, nil
}

// CancelJob cancels one queued systemd job by id.
func (c *Client) CancelJob(ctx context.Context, id string) error {
	_, err := c.run(ctx, "systemctl", "cancel", id)
	return err
}

// CmdLine returns the actual command line of a running process, read from
// /proc. `systemctl show -p ExecStart` is not a substitute: it reports the
// unit file literally, with `$VARS` unexpanded, whereas this is what the
// process is REALLY running with — including args from an EnvironmentFile
// that may have been edited without a restart. Empty when unavailable.
//
// Caveat worth knowing when the output looks odd: this is the process's
// CURRENT argv, and a process may rewrite it in place. rtl_433 parses
// `-F mqtt://host:port` destructively, overwriting the colon with a NUL,
// so /proc reports host and port as two arguments (byte length unchanged —
// the giveaway). We report what the kernel reports rather than guessing.
func CmdLine(pid int) string {
	if pid <= 0 {
		return ""
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	parts := strings.FieldsFunc(string(raw), func(r rune) bool { return r == 0 })
	return strings.Join(parts, " ")
}

// Logs returns the last n journal lines of a unit.
func (c *Client) Logs(ctx context.Context, unit string, n int) (string, error) {
	return c.run(ctx, "journalctl", "-u", unit, "-n", strconv.Itoa(n), "--no-pager")
}
