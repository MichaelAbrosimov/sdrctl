package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/MichaelAbrosimov/sdrctl/internal/config"
)

// follower streams the journal of a device's units while a mode transition
// runs, so the operator SEES the switch happen — systemd stopping the old
// unit, the new process opening the dongle — instead of waiting for one
// summary line. sdrctl's own steps are printed around it by the caller.
//
// It reads journalctl JSON rather than a text format: the fields are then
// unambiguous (source, message) without parsing a human layout.
type follower struct {
	cmd  *exec.Cmd
	done chan struct{}

	mu   sync.Mutex
	last time.Time       // when the most recent line was printed
	seen map[string]bool // units heard from, for linger's target check
}

func (f *follower) note(unit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = time.Now()
	if unit != "" {
		f.seen[unit] = true
	}
}

// startFollow tails every unit of the device from now on. A failure to
// start (no journalctl, no journal access) is not fatal — the transition
// itself does not depend on it, so the caller degrades to quiet mode.
func startFollow(ctx context.Context, dev *config.DeviceConfig, out io.Writer) (*follower, error) {
	units := make([]string, 0, len(dev.Services))
	for _, sc := range dev.Services {
		units = append(units, sc.Systemd)
	}
	sort.Strings(units) // deterministic argv, nicer to debug

	args := []string{"-f", "--since", "now", "-o", "json", "--no-pager"}
	for _, u := range units {
		args = append(args, "-u", u)
	}

	cmd := exec.CommandContext(ctx, "journalctl", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	f := &follower{cmd: cmd, done: make(chan struct{}), seen: map[string]bool{}}
	go func() {
		defer close(f.done)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line, unit := formatJournal(scanner.Bytes())
			if line == "" {
				continue
			}
			fmt.Fprintln(out, line)
			f.note(unit)
		}
	}()
	return f, nil
}

// linger keeps the tail running after the agent reports success. The agent
// verifies through systemd, which calls a Type=simple unit "active" as soon
// as the process is forked — its interesting startup output ("RTL-SDR Blog
// V4 Detected", "Tuned to …") reaches the journal milliseconds later, and
// cutting the log there hides exactly what the operator switched modes to
// see. Waiting for silence alone is not enough (the old unit's stop lines
// look like activity), so wait for evidence FROM the target unit, then for
// the log to settle. The cap bounds a target that never says anything.
func (f *follower) linger(targetUnit string, quietFor, max time.Duration) {
	if f == nil {
		return
	}
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		f.mu.Lock()
		heard := targetUnit == "" || f.seen[targetUnit]
		quiet := !f.last.IsZero() && time.Since(f.last) >= quietFor
		f.mu.Unlock()
		if heard && quiet {
			return
		}
	}
}

// stop ends the tail and waits for the pending output to drain, so the
// final result line is never printed in the middle of journal lines.
func (f *follower) stop() {
	if f == nil {
		return
	}
	if f.cmd.Process != nil {
		_ = f.cmd.Process.Kill()
	}
	select {
	case <-f.done:
	case <-time.After(time.Second):
	}
	_ = f.cmd.Wait()
}

// formatJournal renders one journalctl JSON record as
// "HH:MM:SS  source  message" and reports which unit it belongs to.
// Records without a message (journal bookkeeping) render empty and are
// skipped. Two unit fields matter: a service's own output carries
// _SYSTEMD_UNIT, while systemd's messages ABOUT a unit carry UNIT.
func formatJournal(raw []byte) (line, unit string) {
	var rec struct {
		Timestamp string `json:"__REALTIME_TIMESTAMP"`
		Ident     string `json:"SYSLOG_IDENTIFIER"`
		OwnUnit   string `json:"_SYSTEMD_UNIT"`
		AboutUnit string `json:"UNIT"`
		Message   any    `json:"MESSAGE"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return "", ""
	}
	msg := journalMessage(rec.Message)
	if msg == "" {
		return "", ""
	}
	unit = rec.OwnUnit
	if rec.AboutUnit != "" {
		unit = rec.AboutUnit
	}

	stamp := "        "
	if us, err := strconv.ParseInt(rec.Timestamp, 10, 64); err == nil {
		stamp = time.UnixMicro(us).Format("15:04:05")
	}
	src := rec.Ident
	if src == "" {
		src = unit
	}
	return fmt.Sprintf("%s  %-9s %s",
		dim(stamp), blue(truncate(src, 9)), severity(msg)(msg)), unit
}

// journalMessage handles both textual messages and the byte-array form
// journald uses for non-UTF-8 payloads.
func journalMessage(v any) string {
	switch m := v.(type) {
	case string:
		return m
	case []any:
		b := make([]byte, 0, len(m))
		for _, n := range m {
			f, ok := n.(float64)
			if !ok {
				return ""
			}
			b = append(b, byte(f))
		}
		return string(b)
	default:
		return ""
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
