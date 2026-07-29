package cli

import (
	"fmt"
	"os"
	"strings"
)

// Minimal ANSI helpers — deliberately not a dependency. Colour is applied
// only when stdout is a terminal and the user has not opted out, so piped
// output stays clean for scripts.
const (
	ansiReset  = "\033[0m"
	ansiDim    = "\033[2m"
	ansiBold   = "\033[1m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiBlue   = "\033[36m"
	ansiAccent = "\033[38;5;173m" // warm terracotta
)

// stdoutIsTTY reports whether stdout is an interactive terminal. It decides
// the default verbosity of a mode switch: a human watching gets the live
// transition log, a pipe gets the single result line.
func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// colorEnabled follows the NO_COLOR convention (https://no-color.org).
func colorEnabled() bool {
	if _, off := os.LookupEnv("NO_COLOR"); off {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return stdoutIsTTY()
}

func paint(code, s string) string {
	if !colorEnabled() {
		return s
	}
	return code + s + ansiReset
}

func dim(s string) string    { return paint(ansiDim, s) }
func bold(s string) string   { return paint(ansiBold, s) }
func green(s string) string  { return paint(ansiGreen, s) }
func yellow(s string) string { return paint(ansiYellow, s) }
func red(s string) string    { return paint(ansiRed, s) }
func blue(s string) string   { return paint(ansiBlue, s) }
func accent(s string) string { return paint(ansiAccent, s) }

// severity colours a journal line by its content: systemd's own failure
// wording and the words a unit uses when it dies are worth spotting in a
// scrolling transition log.
func severity(msg string) func(string) string {
	l := strings.ToLower(msg)
	switch {
	case strings.Contains(l, "failed"), strings.Contains(l, "error"),
		strings.Contains(l, "sigkill"), strings.Contains(l, "timed out"):
		return yellow
	case strings.Contains(l, "started"), strings.Contains(l, "detected"),
		strings.Contains(l, "listening"), strings.Contains(l, "tuned"):
		return green
	default:
		return dim
	}
}

func errorf(format string, args ...any) string {
	return red(fmt.Sprintf(format, args...))
}
