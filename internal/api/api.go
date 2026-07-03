// Package api implements the HTTP integration boundary of sdrctl.
//
// Read endpoints are always available when the API is enabled. Write
// endpoints follow the accepted-async pattern: they validate, return
// 202 Accepted immediately and perform the transition in the background;
// the outcome is observable via GET /status and MQTT.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
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

// preflightTimeout bounds the network API's synchronous no-op probe, so a
// hung systemctl cannot hold the handler: on timeout the probe reads
// unknown, the request proceeds to the (itself bounded) transition.
const preflightTimeout = 5 * time.Second

// ambiguousHardCap bounds the write quarantine after an unverified cleanup.
// Rationale (see the settling design note in CODE_REVIEW.md): everything
// systemd does on its own settles within StartLimitIntervalSec-scale time;
// past 2× that, keeping the operator locked out is worse than letting an
// informed retry through.
const ambiguousHardCap = 4 * time.Minute

type Server struct {
	cfg *config.Config
	sd  *systemd.Client
	obs *agent.Observer

	mu       sync.Mutex
	inflight map[string]bool
	closing  bool
	// ambiguous quarantines devices whose failed transition could not be
	// verified as cleaned up (core.ErrCleanupUnverified): a pending systemd
	// job may still land. Writes are refused until the device is verified
	// quiescent or the hard cap expires. Value = quarantine entry time.
	ambiguous map[string]time.Time
	// wg tracks in-flight transitions (including their post-transition
	// snapshot refresh) so agent shutdown can drain them; each part is
	// bounded, so the wait is finite.
	wg sync.WaitGroup
}

func New(cfg *config.Config, sd *systemd.Client, obs *agent.Observer) *Server {
	return &Server{
		cfg: cfg, sd: sd, obs: obs,
		inflight:  map[string]bool{},
		ambiguous: map[string]time.Time{},
	}
}

// WaitTransitions blocks until every in-flight transition (and its
// snapshot refresh) finishes, refusing new ones first. Called on agent
// shutdown: started transitions are deliberately not cancelled (systemd
// would complete their jobs anyway), and each is self-bounded, so this
// returns within that bound. Setting closing under the same mutex that
// beginTransition uses removes the Add-vs-Wait race: any transition either
// registered before this point or is rejected.
func (s *Server) WaitTransitions() {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.wg.Wait()
}

// Handler returns the network API: read endpoints plus token-gated async
// write endpoints (202 Accepted).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.readRoutes(mux)
	mux.HandleFunc("POST /mode/{mode}", s.handleSetModeDefault)
	mux.HandleFunc("POST /devices/{id}/mode/{mode}", s.handleSetModeDevice)
	return mux
}

// SocketHandler returns the local-socket API: the same read endpoints plus
// write endpoints that are always enabled and synchronous. Access control is
// the socket file's ownership (root:<group> 0660), not a token.
func (s *Server) SocketHandler() http.Handler {
	mux := http.NewServeMux()
	s.readRoutes(mux)
	mux.HandleFunc("POST /mode/{mode}", s.handleSocketSetModeDefault)
	mux.HandleFunc("POST /devices/{id}/mode/{mode}", s.handleSocketSetModeDevice)
	return mux
}

func (s *Server) readRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /mode", s.handleMode)
	mux.HandleFunc("GET /devices", s.handleDevices)
	mux.HandleFunc("GET /devices/{id}", s.handleDevice)
	mux.HandleFunc("GET /devices/{id}/mode", s.handleDeviceMode)
	mux.HandleFunc("GET /devices/{id}/health", s.handleDeviceHealth)
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

func (s *Server) handleMode(w http.ResponseWriter, r *http.Request) {
	dev, err := s.cfg.DefaultDevice()
	if err != nil {
		writeErr(w, http.StatusConflict, "%v", err)
		return
	}
	actual, desired := core.DeviceModes(r.Context(), s.sd, dev)
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

func (s *Server) handleSocketSetModeDefault(w http.ResponseWriter, r *http.Request) {
	dev, err := s.cfg.DefaultDevice()
	if err != nil {
		writeErr(w, http.StatusConflict, "%v", err)
		return
	}
	s.setModeSync(w, dev.ID, r.PathValue("mode"))
}

func (s *Server) handleSocketSetModeDevice(w http.ResponseWriter, r *http.Request) {
	s.setModeSync(w, r.PathValue("id"), r.PathValue("mode"))
}

// resolveTarget validates the device id and mode name, writing the error
// response itself when validation fails.
func (s *Server) resolveTarget(w http.ResponseWriter, id, mode string) *config.DeviceConfig {
	dev, err := s.cfg.DeviceByID(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "%v", err)
		return nil
	}
	if mode != core.ModeIdle {
		if _, ok := dev.Services[mode]; !ok {
			writeErr(w, http.StatusBadRequest, "unknown mode %q for device %s", mode, id)
			return nil
		}
	}
	return dev
}

// beginTransition atomically checks the shutdown flag and the inflight
// guard, then registers the transition in the drain group — one critical
// section, so WaitTransitions can never miss a started transition. The
// returned status code is 0 on success.
func (s *Server) beginTransition(id string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return http.StatusServiceUnavailable, fmt.Errorf("agent is shutting down")
	}
	if s.inflight[id] {
		return http.StatusConflict, fmt.Errorf("mode change already in progress for device %s", id)
	}
	s.inflight[id] = true
	s.wg.Add(1)
	return 0, nil
}

// endTransition is the LAST thing a transition worker does — after the
// transition itself and its snapshot refresh — so WaitTransitions covers
// the whole lifecycle.
func (s *Server) endTransition(id string) {
	s.mu.Lock()
	delete(s.inflight, id)
	s.mu.Unlock()
	s.wg.Done()
}

// noteAmbiguous quarantines the device when a failed transition's cleanup
// could not be verified.
func (s *Server) noteAmbiguous(id string, err error) {
	if !errors.Is(err, core.ErrCleanupUnverified) {
		return
	}
	s.mu.Lock()
	if _, ok := s.ambiguous[id]; !ok {
		s.ambiguous[id] = time.Now()
	}
	s.mu.Unlock()
	log.Printf("api: device %s quarantined (unverified cleanup): %v", id, err)
}

// gateAmbiguous refuses writes for a quarantined device until it is
// verified quiescent (no pending jobs, no transitional unit states) or the
// hard cap expires. Verification runs on demand right here — no background
// loop can race the write path this way.
func (s *Server) gateAmbiguous(dev *config.DeviceConfig) error {
	s.mu.Lock()
	since, quarantined := s.ambiguous[dev.ID]
	s.mu.Unlock()
	if !quarantined {
		return nil
	}

	if time.Since(since) > ambiguousHardCap {
		log.Printf("api: device %s leaves quarantine by hard cap (%s) without verification — proceeding on operator responsibility",
			dev.ID, ambiguousHardCap)
		s.clearAmbiguous(dev.ID)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), preflightTimeout)
	quiet, err := core.DeviceQuiescent(ctx, s.sd, dev)
	cancel()
	if err == nil && quiet {
		log.Printf("api: device %s verified quiescent, quarantine lifted", dev.ID)
		s.clearAmbiguous(dev.ID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("device %s is quarantined after an unverified cleanup and still cannot be verified (%v); retry later or wait out the cap (%s left)",
			dev.ID, err, (ambiguousHardCap - time.Since(since)).Round(time.Second))
	}
	return fmt.Errorf("device %s is quarantined: pending systemd activity from a failed transition has not settled yet (%s until hard cap)",
		dev.ID, (ambiguousHardCap - time.Since(since)).Round(time.Second))
}

func (s *Server) clearAmbiguous(id string) {
	s.mu.Lock()
	delete(s.ambiguous, id)
	s.mu.Unlock()
}

// refreshAsync publishes a fresh snapshot without holding the caller: the
// socket response must not pay for a second ModeSetTimeout-bounded read
// pass. The refresh is registered in the drain group unless shutdown has
// already begun (then the final state is systemd's to keep).
func (s *Server) refreshAsync() {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		s.obs.Refresh()
	}()
}

// setMode is the network write path: token-gated, async 202 Accepted.
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

	dev := s.resolveTarget(w, id, mode)
	if dev == nil {
		return
	}

	if err := s.gateAmbiguous(dev); err != nil {
		writeErr(w, http.StatusConflict, "%v", err)
		return
	}

	// Idempotency probe for the 200-no-op/202-accepted contract, bounded so
	// a hung systemctl cannot hold the handler. On probe timeout the modes
	// read unknown and the request just proceeds to the bounded transition.
	probeCtx, probeCancel := context.WithTimeout(r.Context(), preflightTimeout)
	actual, desired := core.DeviceModes(probeCtx, s.sd, dev)
	probeCancel()
	if actual == mode && desired == mode {
		writeJSON(w, http.StatusOK, core.SetModeResult{
			Device: id, RequestedMode: mode, Mode: mode, Changed: false,
		})
		return
	}

	if code, err := s.beginTransition(id); code != 0 {
		writeErr(w, code, "%v", err)
		return
	}

	go func() {
		// endTransition runs after the refresh below: the drain group
		// covers the worker's whole lifecycle, not just SetMode.
		defer s.endTransition(id)
		// Deliberately not the request context: an accepted transition must
		// finish even if the caller disconnects; SetMode bounds itself.
		if _, err := core.SetMode(context.Background(), s.sd, dev, mode, s.cfg.ModeSetTimeout()); err != nil {
			log.Printf("api: mode set %s/%s failed: %v", id, mode, err)
			s.noteAmbiguous(id, err)
		} else {
			log.Printf("api: device %s switched to mode %s", id, mode)
		}
		// Publish the outcome (MQTT + fresh /status) right away.
		s.obs.Refresh()
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"accepted":       true,
		"device":         id,
		"requested_mode": mode,
	})
}

// setModeSync is the socket write path: trusted (file permissions instead of
// a token) and synchronous — the CLI wants the final result, not a ticket.
func (s *Server) setModeSync(w http.ResponseWriter, id, mode string) {
	dev := s.resolveTarget(w, id, mode)
	if dev == nil {
		return
	}

	if err := s.gateAmbiguous(dev); err != nil {
		writeErr(w, http.StatusConflict, "%v", err)
		return
	}

	// No idempotency preflight here: SetMode performs the same check inside
	// its own deadline. A separate unbounded probe would let a hung
	// systemctl hold this handler before it ever reaches claim/SetMode.
	if code, err := s.beginTransition(id); code != 0 {
		writeErr(w, code, "%v", err)
		return
	}
	defer s.endTransition(id)

	// Not the request context: even on the synchronous socket path a started
	// transition must run to completion if the CLI disconnects — the client
	// timeout is longer than SetMode's own deadline, which bounds this call.
	res, err := core.SetMode(context.Background(), s.sd, dev, mode, s.cfg.ModeSetTimeout())
	// Asynchronous on purpose: a second synchronous ModeSetTimeout-bounded
	// read pass would let the full handler take ~2× the configured timeout,
	// past what the socket client waits.
	s.refreshAsync()
	if err != nil {
		log.Printf("socket: mode set %s/%s failed: %v", id, mode, err)
		s.noteAmbiguous(id, err)
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	log.Printf("socket: device %s switched to mode %s", id, mode)
	writeJSON(w, http.StatusOK, res)
}
