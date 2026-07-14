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
	// Extract worker ID from path: /api/v1/workers/{id}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/workers/")
	id := strings.TrimRight(path, "/")
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
