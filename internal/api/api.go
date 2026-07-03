// Package api implements the HTTP integration boundary of sdrctl.
//
// Read endpoints are always available when the API is enabled. Write
// endpoints follow the accepted-async pattern: they validate, return
// 202 Accepted immediately and perform the transition in the background;
// the outcome is observable via GET /status and MQTT.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/MichaelAbrosimov/sdrctl/internal/agent"
	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/core"
	"github.com/MichaelAbrosimov/sdrctl/internal/systemd"
)

const setModeTimeout = 15 * time.Second

type Server struct {
	cfg *config.Config
	sd  *systemd.Client
	obs *agent.Observer

	mu       sync.Mutex
	inflight map[string]bool
}

func New(cfg *config.Config, sd *systemd.Client, obs *agent.Observer) *Server {
	return &Server{cfg: cfg, sd: sd, obs: obs, inflight: map[string]bool{}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /mode", s.handleMode)
	mux.HandleFunc("GET /devices", s.handleDevices)
	mux.HandleFunc("GET /devices/{id}", s.handleDevice)
	mux.HandleFunc("GET /devices/{id}/mode", s.handleDeviceMode)
	mux.HandleFunc("GET /devices/{id}/health", s.handleDeviceHealth)
	mux.HandleFunc("POST /mode/{mode}", s.handleSetModeDefault)
	mux.HandleFunc("POST /devices/{id}/mode/{mode}", s.handleSetModeDevice)
	return mux
}

// Run serves until the context is cancelled.
func (s *Server) Run(ctx interface{ Done() <-chan struct{} }) error {
	srv := &http.Server{
		Addr:              s.cfg.API.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		_ = srv.Close()
		return nil
	case err := <-errc:
		return err
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	snap := s.obs.Latest()
	devices := map[string]string{}
	for _, d := range snap.Devices {
		devices[d.ID] = d.Health
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      snap.OK,
		"node":    snap.Node,
		"service": "sdrctl-agent",
		"health":  snap.Health,
		"devices": devices,
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.obs.Latest())
}

func (s *Server) handleMode(w http.ResponseWriter, _ *http.Request) {
	dev, err := s.cfg.DefaultDevice()
	if err != nil {
		writeErr(w, http.StatusConflict, "%v", err)
		return
	}
	actual, desired := core.DeviceModes(s.sd, dev)
	writeJSON(w, http.StatusOK, map[string]string{
		"device": dev.ID, "mode": actual, "desired_mode": desired,
	})
}

func (s *Server) handleDevices(w http.ResponseWriter, _ *http.Request) {
	snap := s.obs.Latest()
	writeJSON(w, http.StatusOK, map[string]any{"node": snap.Node, "devices": snap.Devices})
}

func (s *Server) findDevice(w http.ResponseWriter, id string) *core.DeviceStatus {
	snap := s.obs.Latest()
	for i := range snap.Devices {
		if snap.Devices[i].ID == id {
			return &snap.Devices[i]
		}
	}
	writeErr(w, http.StatusNotFound, "unknown device %q", id)
	return nil
}

func (s *Server) handleDevice(w http.ResponseWriter, r *http.Request) {
	if d := s.findDevice(w, r.PathValue("id")); d != nil {
		writeJSON(w, http.StatusOK, d)
	}
}

func (s *Server) handleDeviceMode(w http.ResponseWriter, r *http.Request) {
	if d := s.findDevice(w, r.PathValue("id")); d != nil {
		writeJSON(w, http.StatusOK, map[string]string{
			"device": d.ID, "mode": d.Mode, "desired_mode": d.DesiredMode,
		})
	}
}

func (s *Server) handleDeviceHealth(w http.ResponseWriter, r *http.Request) {
	if d := s.findDevice(w, r.PathValue("id")); d != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"device": d.ID, "health": d.Health, "present": d.Present,
		})
	}
}

func (s *Server) handleSetModeDefault(w http.ResponseWriter, r *http.Request) {
	dev, err := s.cfg.DefaultDevice()
	if err != nil {
		writeErr(w, http.StatusConflict, "%v", err)
		return
	}
	s.setMode(w, r, dev.ID, r.PathValue("mode"))
}

func (s *Server) handleSetModeDevice(w http.ResponseWriter, r *http.Request) {
	s.setMode(w, r, r.PathValue("id"), r.PathValue("mode"))
}

func (s *Server) setMode(w http.ResponseWriter, r *http.Request, id, mode string) {
	if !s.cfg.API.WriteEnabled {
		writeErr(w, http.StatusForbidden, "write API is disabled (set api.write_enabled: true)")
		return
	}
	if s.cfg.API.Token == "" {
		writeErr(w, http.StatusForbidden, "write API requires api.token to be configured")
		return
	}
	want := "Bearer " + s.cfg.API.Token
	got := r.Header.Get("Authorization")
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		writeErr(w, http.StatusUnauthorized, "invalid or missing bearer token")
		return
	}

	dev, err := s.cfg.DeviceByID(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return
	}
	if mode != core.ModeIdle {
		if _, ok := dev.Services[mode]; !ok {
			writeErr(w, http.StatusBadRequest, "unknown mode %q for device %s", mode, id)
			return
		}
	}

	// Idempotency: requesting the current desired+actual mode is a no-op.
	actual, desired := core.DeviceModes(s.sd, dev)
	if actual == mode && desired == mode {
		writeJSON(w, http.StatusOK, core.SetModeResult{
			Device: id, RequestedMode: mode, Mode: mode, Changed: false,
		})
		return
	}

	s.mu.Lock()
	if s.inflight[id] {
		s.mu.Unlock()
		writeErr(w, http.StatusConflict, "mode change already in progress for device %s", id)
		return
	}
	s.inflight[id] = true
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.inflight, id)
			s.mu.Unlock()
			// Publish the outcome (MQTT + fresh /status) right away.
			s.obs.Refresh()
		}()
		if _, err := core.SetMode(s.sd, dev, mode, setModeTimeout); err != nil {
			log.Printf("api: mode set %s/%s failed: %v", id, mode, err)
		} else {
			log.Printf("api: device %s switched to mode %s", id, mode)
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"accepted":       true,
		"device":         id,
		"requested_mode": mode,
	})
}
