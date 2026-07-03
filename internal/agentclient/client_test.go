package agentclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestMissingSocketIsUnavailable(t *testing.T) {
	c := New("/nonexistent/sdrctl.sock", 15*time.Second)
	_, err := c.Status()
	if err == nil {
		t.Fatal("expected an error for a missing socket")
	}
	if !IsUnavailable(err) {
		t.Errorf("missing socket must be Unavailable (fallback trigger), got %v", err)
	}
	if IsUnavailable(errors.New("agent: unknown mode")) {
		t.Error("plain API errors must not count as Unavailable")
	}
}

// SDR-P1-03: only errors proving the request never reached the agent may
// trigger the CLI's direct-systemctl fallback for writes.
func TestClassifySeparatesDialFromAmbiguous(t *testing.T) {
	dialErr := &url.Error{Op: "Post", URL: "http://sdrctl-agent/mode/rtl-tcp",
		Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}
	if !IsUnavailable(classify(http.MethodPost, dialErr)) {
		t.Error("dial error must be Unavailable: no bytes were sent, fallback is safe")
	}

	timeoutErr := &url.Error{Op: "Post", URL: "http://sdrctl-agent/mode/rtl-tcp",
		Err: context.DeadlineExceeded}
	if IsUnavailable(classify(http.MethodPost, timeoutErr)) {
		t.Error("timeout after a sent write must NOT be Unavailable: the transition may still run in the agent")
	}

	if !IsUnavailable(classify(http.MethodGet, timeoutErr)) {
		t.Error("read errors are always Unavailable: local read fallback is side-effect-free")
	}
}
