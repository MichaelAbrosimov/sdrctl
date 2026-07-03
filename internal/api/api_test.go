package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MichaelAbrosimov/sdrctl/internal/agent"
	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/core"
	"github.com/MichaelAbrosimov/sdrctl/internal/systemd/systemdtest"
)

// testConfig has the network write API fully disabled — the socket handler
// must accept writes regardless.
func testConfig() *config.Config {
	return &config.Config{
		Node: config.NodeConfig{ID: "test-node"},
		Devices: []config.DeviceConfig{{
			ID:      "rtl-sdr-01",
			Type:    "rtl-sdr",
			Default: true,
			Services: map[string]config.ServiceConfig{
				"rtl-tcp": {Systemd: "rtl-tcp.service"},
			},
		}},
	}
}

func newServer(cfg *config.Config, f *systemdtest.Fake) *Server {
	sd := f.Client()
	return New(cfg, sd, agent.New(cfg, sd))
}

func idleUnits() map[string]*systemdtest.Unit {
	return map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "inactive", Enabled: "disabled"},
	}
}

func TestNetworkWriteStaysTokenGated(t *testing.T) {
	srv := newServer(testConfig(), systemdtest.New(idleUnits()))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/mode/rtl-tcp", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("network write without write_enabled: got %d, want 403", rec.Code)
	}
}

func TestSocketWriteNeedsNoToken(t *testing.T) {
	f := systemdtest.New(idleUnits())
	srv := newServer(testConfig(), f)

	rec := httptest.NewRecorder()
	srv.SocketHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/devices/rtl-sdr-01/mode/rtl-tcp", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("socket write: got %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var res core.SetModeResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Mode != "rtl-tcp" {
		t.Errorf("expected synchronous switch to rtl-tcp, got %+v", res)
	}
	if f.Unit("rtl-tcp.service").Enabled != "enabled" {
		t.Error("unit was not enabled through systemd")
	}
}

func TestSocketWriteIsIdempotent(t *testing.T) {
	srv := newServer(testConfig(), systemdtest.New(map[string]*systemdtest.Unit{
		"rtl-tcp.service": {Load: "loaded", Active: "active", Enabled: "enabled"},
	}))
	rec := httptest.NewRecorder()
	srv.SocketHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/mode/rtl-tcp", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	var res core.SetModeResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Errorf("no-op switch reported Changed=true: %+v", res)
	}
}

func TestSocketWriteRejectsUnknownMode(t *testing.T) {
	srv := newServer(testConfig(), systemdtest.New(idleUnits()))
	rec := httptest.NewRecorder()
	srv.SocketHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/devices/rtl-sdr-01/mode/nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unknown mode") {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
}

// SDR-P1-03: a hung systemctl must not hold the inflight guard past the
// transition deadline — the handler returns an error and the device stays
// controllable (the next request is NOT rejected with 409).
func TestSocketWriteReleasesInflightAfterTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.ModeSetTimeoutSec = 1
	f := systemdtest.New(idleUnits())
	f.HangVerb("enable")
	srv := newServer(cfg, f)

	rec := httptest.NewRecorder()
	srv.SocketHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/mode/rtl-tcp", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("hung transition: got %d (%s), want 500", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.SocketHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/mode/rtl-tcp", nil))
	if rec.Code == http.StatusConflict {
		t.Fatal("inflight guard was not released after the timed-out transition")
	}
}

// SDR-P1-03 re-review item 3: the FULL socket handler — SetMode plus the
// post-transition refresh — must answer within the configured timeout (plus
// scheduler slack), not within 2× of it. The refresh is asynchronous now.
func TestSocketWriteBoundedWhenShowHangs(t *testing.T) {
	cfg := testConfig()
	cfg.ModeSetTimeoutSec = 1
	f := systemdtest.New(idleUnits())
	f.HangVerb("show")
	srv := newServer(cfg, f)

	start := time.Now()
	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		srv.SocketHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/mode/rtl-tcp", nil))
		done <- rec.Code
	}()

	select {
	case code := <-done:
		if code == http.StatusOK {
			t.Errorf("write with hung systemctl reported success (%d)", code)
		}
		// 1s configured + slack; the old synchronous refresh added a whole
		// second ModeSetTimeout here.
		if elapsed := time.Since(start); elapsed > 1900*time.Millisecond {
			t.Errorf("full handler took %v — the caller-visible bound exceeds the configured timeout", elapsed)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("socket write handler is not bounded: still blocked with a hung systemctl show")
	}
}

// SDR-P1-03 re-review item 1: an unverifiable cleanup quarantines the
// device; writes are refused until the state is verified quiescent, then
// allowed again.
func TestAmbiguousOutcomeQuarantinesDevice(t *testing.T) {
	cfg := testConfig()
	cfg.ModeSetTimeoutSec = 1
	f := systemdtest.New(idleUnits())
	f.HangVerb("enable")
	f.LingerJob("enable")
	f.KeepJobsOnCancel() // cancel "succeeds" but the job stays → unverified

	srv := newServer(cfg, f)

	rec := httptest.NewRecorder()
	srv.SocketHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/mode/rtl-tcp", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("ambiguous transition: got %d (%s), want 500", rec.Code, rec.Body.String())
	}

	// The pending job is still there → the device must be quarantined.
	rec = httptest.NewRecorder()
	srv.SocketHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/mode/rtl-tcp", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("write on quarantined device: got %d (%s), want 409", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "quarantined") {
		t.Errorf("409 should explain the quarantine, got: %s", rec.Body.String())
	}

	// PID 1 finishes the job; the device becomes verifiably quiescent and
	// the next write goes through (idempotent 200: the job landed rtl-tcp).
	f.CompleteJobs()
	rec = httptest.NewRecorder()
	srv.SocketHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/mode/rtl-tcp", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("write after quiescence: got %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var res core.SetModeResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Errorf("landed job already put the device in rtl-tcp; expected a no-op, got %+v", res)
	}
}

// SDR-P1-03 re-review item 2: after WaitTransitions has begun, no new
// transition may start — beginTransition and the closing flag share one
// critical section, so the WaitGroup can never be raised from zero after
// Wait started.
func TestShutdownRejectsNewTransitions(t *testing.T) {
	f := systemdtest.New(idleUnits())
	srv := newServer(testConfig(), f)

	srv.WaitTransitions() // no transitions in flight: sets closing, returns

	rec := httptest.NewRecorder()
	srv.SocketHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/mode/rtl-tcp", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("write after shutdown began: got %d (%s), want 503", rec.Code, rec.Body.String())
	}
	for _, c := range f.Calls() {
		if strings.Contains(c, "enable") || strings.Contains(c, "disable") {
			t.Errorf("shutdown-rejected request still mutated systemd: %s", c)
		}
	}
}
