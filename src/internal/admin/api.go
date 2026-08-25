package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"openmodel/sp-state-agent/internal/worker"
)

// Server serves the admin REST API for worker management.
type Server struct {
	httpServer *http.Server
	mux        *http.ServeMux
	registry   *worker.Registry
	poller     *worker.Poller
	logger     *slog.Logger
	adminToken string
	banDefault time.Duration // ban duration when a ban request omits duration_sec
}

// SetBanDefault overrides the default routing-ban duration (config ban.default_duration_sec).
func (s *Server) SetBanDefault(d time.Duration) {
	if d > 0 {
		s.banDefault = d
	}
}

// NewServer creates a new admin API server.
// adminToken protects /api/v1/* endpoints. /health and /metrics are always public.
// If adminToken is empty, auth is DISABLED (caller should log a warning).
func NewServer(port int, adminToken string, registry *worker.Registry, poller *worker.Poller, logger *slog.Logger) *Server {
	mux := http.NewServeMux()

	s := &Server{
		registry:   registry,
		poller:     poller,
		logger:     logger,
		adminToken: adminToken,
		banDefault: 7 * 24 * time.Hour,
	}

	mux.HandleFunc("/api/v1/workers/register", s.handleRegister)
	mux.HandleFunc("/api/v1/workers", s.handleWorkers)
	mux.HandleFunc("/api/v1/workers/", s.handleWorkerByID)
	mux.HandleFunc("/api/v1/stats", s.handleStats)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ready", s.handleReady)
	mux.Handle("/metrics", promhttp.Handler())

	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: authMiddleware(adminToken, mux),
	}
	s.mux = mux

	return s
}

// RegisterSettlementAPI adds settlement endpoints to this admin server.
func (s *Server) RegisterSettlementAPI(sa *SettlementAPI) {
	sa.RegisterRoutes(s.mux)
}

// Start begins serving. Blocks until the server shuts down.
func (s *Server) Start() error {
	s.logger.Info("admin API starting", "addr", s.httpServer.Addr)
	return s.httpServer.ListenAndServe()
}

// Stop gracefully shuts down the server, waiting up to timeout for in-flight requests to complete.
func (s *Server) Stop(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var reg worker.WorkerRegistration
	if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	result, err := s.registry.Register(reg)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"worker_id":        result.Worker.ID,
		"registered":       true,
		"created":          result.Created,
		"endpoint_changed": result.EndpointChanged,
	})
}

func (s *Server) handleWorkers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	workers := s.registry.List()
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"workers": workers,
		"total":   len(workers),
	})
}

func (s *Server) handleWorkerByID(w http.ResponseWriter, r *http.Request) {
	// Extract worker ID from path: /api/v1/workers/{id}[/ban]
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/workers/")
	path = strings.TrimRight(path, "/")
	if id, isBan := strings.CutSuffix(path, "/ban"); isBan {
		s.handleWorkerBan(w, r, id)
		return
	}
	id := path
	if id == "" {
		http.Error(w, "worker ID required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		wk, ok := s.registry.Get(id)
		if !ok {
			jsonError(w, "worker not found", http.StatusNotFound)
			return
		}
		jsonResponse(w, http.StatusOK, wk)

	case http.MethodDelete:
		_, ok := s.registry.Deregister(id)
		if !ok {
			jsonError(w, "worker not found", http.StatusNotFound)
			return
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"worker_id":    id,
			"deregistered": true,
		})

	default:
		http.Error(w, "GET or DELETE only", http.StatusMethodNotAllowed)
	}
}

// handleWorkerBan drives the routing-ban punishment lever:
//
//	POST   /api/v1/workers/{id}/ban {duration_sec?, reason?} → ban (default duration when omitted)
//	DELETE /api/v1/workers/{id}/ban                          → lift the ban early
//
// A banned worker keeps being polled (observable) but receives no inference
// tasks until the ban expires. Pair with the on-chain frozen-earnings
// confiscation for the full misbehavior response (see runbook).
func (s *Server) handleWorkerBan(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" {
		http.Error(w, "worker ID required", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPost:
		var req struct {
			DurationSec int64  `json:"duration_sec"`
			Reason      string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
			jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		d := time.Duration(req.DurationSec) * time.Second
		if d <= 0 {
			d = s.banDefault
		}
		until := time.Now().Add(d)
		if !s.registry.SetBan(id, until, req.Reason) {
			jsonError(w, "worker not found", http.StatusNotFound)
			return
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"worker_id":    id,
			"banned":       true,
			"banned_until": until.UTC().Format(time.RFC3339),
			"reason":       req.Reason,
		})

	case http.MethodDelete:
		if !s.registry.SetBan(id, time.Time{}, "") {
			jsonError(w, "worker not found", http.StatusNotFound)
			return
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"worker_id": id,
			"banned":    false,
		})

	default:
		http.Error(w, "POST or DELETE only", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	jsonResponse(w, http.StatusOK, s.registry.Stats())
}

// handleHealth is a lightweight liveness probe — returns OK as long as the
// server is accepting HTTP. Suitable for k8s livenessProbe / Docker healthcheck.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, `{"status":"ok"}`)
}

// handleReady is a readiness probe.
// 200: at least one worker can serve requests (or no workers registered yet).
// 503: all workers are mining/offline.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	type check struct {
		TotalWorkers     int  `json:"total_workers"`
		AvailableWorkers int  `json:"available_workers"`
		Ready            bool `json:"ready"`
	}

	stats := s.registry.Stats()
	c := check{
		TotalWorkers:     stats.TotalWorkers,
		AvailableWorkers: stats.IdleWorkers + stats.BusyWorkers,
	}
	c.Ready = c.TotalWorkers == 0 || c.AvailableWorkers > 0

	status := http.StatusOK
	if !c.Ready {
		status = http.StatusServiceUnavailable
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(c)
}

func jsonResponse(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func jsonError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
