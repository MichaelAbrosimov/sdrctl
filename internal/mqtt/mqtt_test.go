package mqtt

import (
	"testing"
	"time"
)

// stuckToken models a broker that accepted the publish but never acks.
type stuckToken struct{}

func (stuckToken) Wait() bool                     { select {} }
func (stuckToken) WaitTimeout(time.Duration) bool { return false }
func (stuckToken) Done() <-chan struct{}          { return make(chan struct{}) }
func (stuckToken) Error() error                   { return nil }

// Pack-5 review: the diagnostic wait on a publish token must be bounded —
// a broker that never PUBACKs must not accumulate one goroutine per
// heartbeat forever.
func TestWaitAndLogIsBounded(t *testing.T) {
	orig := publishWaitTimeout
	publishWaitTimeout = 50 * time.Millisecond
	defer func() { publishWaitTimeout = orig }()

	done := make(chan struct{})
	go func() {
		waitAndLog("sdr/test/status", stuckToken{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitAndLog blocked on a never-acking token")
	}
}
