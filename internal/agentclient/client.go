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
	// writeTimeout must exceed the agent's synchronous mode-set timeout (15s).
	writeTimeout = 30 * time.Second
)

// Unavailable marks transport-level failures where the agent could not be
// reached at all.
type Unavailable struct{ Err error }

func (u Unavailable) Error() string { return u.Err.Error() }
func (u Unavailable) Unwrap() error { return u.Err }

// IsUnavailable reports whether err means "agent unreachable" (fallback is
// appropriate) as opposed to an answer from a running agent.
func IsUnavailable(err error) bool {
	var u Unavailable
	return errors.As(err, &u)
}

type Client struct {
	read  *http.Client
	write *http.Client
}

func New(socketPath string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		read:  &http.Client{Transport: tr, Timeout: readTimeout},
		write: &http.Client{Transport: tr, Timeout: writeTimeout},
	}
}

// Status returns the agent's current node snapshot.
func (c *Client) Status() (core.Snapshot, error) {
	var snap core.Snapshot
	err := c.do(c.read, http.MethodGet, "/status", &snap)
	return snap, err
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
		return Unavailable{Err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Unavailable{Err: err}
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
