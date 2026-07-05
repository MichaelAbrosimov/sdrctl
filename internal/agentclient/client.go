// Package agentclient is the CLI side of the agent's local control socket.
//
// The agent is the single executor of mode transitions; every CLI command
// prefers this channel. Transport-level failures (agent down, socket missing,
// no group membership) are wrapped in Unavailable so callers can fall back to
// driving systemctl directly. API-level errors come from the running agent —
// it IS the authority, so they are returned as-is and never trigger fallback.
package agentclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/MichaelAbrosimov/sdrctl/internal/core"
)

const (
	readTimeout = 3 * time.Second
	// writeMargin is added on top of the agent's synchronous mode-set
	// timeout, so the client always outwaits the agent's own deadline.
	writeMargin = 5 * time.Second
)

// Unavailable marks failures where it is safe for the caller to fall back:
// the request provably never reached the agent (dial error), or it was a
// read — reads are side-effect-free and the local fallback is equivalent.
type Unavailable struct{ Err error }

func (u Unavailable) Error() string { return u.Err.Error() }
func (u Unavailable) Unwrap() error { return u.Err }

// IsUnavailable reports whether err means "agent unreachable, fallback safe"
// as opposed to an answer from a running agent or an ambiguous write outcome.
func IsUnavailable(err error) bool {
	var u Unavailable
	return errors.As(err, &u)
}

// classify decides what a transport error means for the caller. Dial errors
// are always Unavailable — no bytes were sent, no mutation started. Errors
// after that (timeout awaiting the reply, connection dropped mid-response)
// are Unavailable only for reads; for writes the transition may still be
// running inside the agent, so falling back would create a second executor —
// exactly the situation the socket-first design exists to prevent.
func classify(method string, err error) error {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return Unavailable{Err: err}
	}
	if method == http.MethodGet {
		return Unavailable{Err: err}
	}
	return fmt.Errorf(
		"agent: request outcome unknown (%v) — the transition may still be running; NOT falling back, check 'sdrctl status' before retrying", err)
}

type Client struct {
	read  *http.Client
	write *http.Client
}

// New builds a client for the agent socket. modeSetTimeout is the agent's
// synchronous transition deadline (config mode_set_timeout_sec): writes wait
// that long plus a margin.
func New(socketPath string, modeSetTimeout time.Duration) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		read:  &http.Client{Transport: tr, Timeout: readTimeout},
		write: &http.Client{Transport: tr, Timeout: modeSetTimeout + writeMargin},
	}
}

// Status returns the agent's current node snapshot.
func (c *Client) Status() (core.Snapshot, error) {
	var snap core.Snapshot
	err := c.do(c.read, http.MethodGet, "/status", &snap)
	return snap, err
}

// DeviceMode reads the LIVE mode pair of one device — the agent serves the
// mode endpoints from fresh systemd reads, not the observer cache, so the
// answer is correct immediately after SetMode.
func (c *Client) DeviceMode(device string) (actual, desired string, err error) {
	var out struct {
		Mode        string `json:"mode"`
		DesiredMode string `json:"desired_mode"`
	}
	err = c.do(c.read, http.MethodGet, "/devices/"+url.PathEscape(device)+"/mode", &out)
	return out.Mode, out.DesiredMode, err
}

// SetMode switches a device mode through the agent. On the socket this
// endpoint is synchronous: the response carries the final result.
func (c *Client) SetMode(device, mode string) (core.SetModeResult, error) {
	var res core.SetModeResult
	err := c.do(c.write, http.MethodPost,
		"/devices/"+url.PathEscape(device)+"/mode/"+url.PathEscape(mode), &res)
	return res, err
}

func (c *Client) do(hc *http.Client, method, path string, out any) error {
	// The host is a placeholder: the transport always dials the unix socket.
	req, err := http.NewRequest(method, "http://sdrctl-agent"+path, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return classify(method, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return classify(method, err)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return fmt.Errorf("agent: %s", e.Error)
		}
		return fmt.Errorf("agent: HTTP %s", resp.Status)
	}
	return json.Unmarshal(body, out)
}
