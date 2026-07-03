package agentclient

import (
	"errors"
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
