package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelAbrosimov/sdrctl/internal/agent"
	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/core"
	"github.com/MichaelAbrosimov/sdrctl/internal/systemd"
)

type fakeUnit struct {
	load    string
	active  string
	enabled string
}

func fakeSystemd(units map[string]*fakeUnit) *systemd.Client {
	runner := func(name string, args ...string) (string, error) {
		if name != "systemctl" {
			return "", fmt.Errorf("unexpected command %s", name)
		}
		switch args[0] {
		case "show":
			u := units[args[1]]
			if u == nil {
				u = &fakeUnit{load: "not-found"}
			}
			return fmt.Sprintf("LoadState=%s\nActiveState=%s\nUnitFileState=%s\nNRestarts=0\nActiveEnterTimestamp=\n",
				u.load, u.active, u.enabled), nil
		case "enable":
			u := units[args[2]]
			if u == nil || u.load == "not-found" {
				return "", fmt.Errorf("unit %s not found", args[2])
			}
			u.enabled = "enabled"
			u.active = "active"
			return "", nil
		case "disable":
			u := units[args[2]]
			if u == nil || u.load == "not-found" {
				return "", fmt.Errorf("unit %s not found", args[2])
			}
			u.enabled = "disabled"
			u.active = "inactive"
			return "", nil
		case "reset-failed":
			return "", nil
		}
		return "", fmt.Errorf("unexpected systemctl %v", args)
	}
	return systemd.NewWithRunner(runner)
}

// newTestServer builds a server with the network write API fully disabled —
// the socket handler must accept writes regardless.
func newTestServer(units map[string]*fakeUnit) *Server {
	cfg := &config.Config{
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
	sd := fakeSystemd(units)
	return New(cfg, sd, agent.New(cfg, sd))
}

func idleUnits() map[string]*fakeUnit {
	return map[string]*fakeUnit{
		"rtl-tcp.service": {load: "loaded", active: "inactive", enabled: "disabled"},
	}
}

func TestNetworkWriteStaysTokenGated(t *testing.T) {
	srv := newTestServer(idleUnits())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/mode/rtl-tcp", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("network write without write_enabled: got %d, want 403", rec.Code)
	}
}

func TestSocketWriteNeedsNoToken(t *testing.T) {
	units := idleUnits()
	srv := newTestServer(units)

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
	if units["rtl-tcp.service"].enabled != "enabled" {
		t.Error("unit was not enabled through systemd")
	}
}

func TestSocketWriteIsIdempotent(t *testing.T) {
	srv := newTestServer(map[string]*fakeUnit{
		"rtl-tcp.service": {load: "loaded", active: "active", enabled: "enabled"},
	})
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
	srv := newTestServer(idleUnits())
	rec := httptest.NewRecorder()
	srv.SocketHandler().ServeHTTP(rec, httptest.NewRequest("POST", "/devices/rtl-sdr-01/mode/nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unknown mode") {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
}
