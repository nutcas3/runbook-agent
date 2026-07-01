// cmd/server/main.go
//
// Production HTTP server for the runbook agent.
//
// Endpoints:
//   POST /api/alerts        — Receives Prometheus/PagerDuty webhook, triggers runbook
//   GET  /api/approvals     — Dashboard polls for pending approvals
//   POST /api/approvals/:id/approve  — Engineer approves an action
//   POST /api/approvals/:id/reject   — Engineer rejects an action
//   GET  /api/incidents/:id/history  — Fetch audit trail for an incident
//   GET  /health            — Liveness probe
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/firebase/genkit/go/plugins/googlegenai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/nutcas3/runbook-agent/internal/agent"
	"github.com/nutcas3/runbook-agent/internal/approval"
	"github.com/nutcas3/runbook-agent/internal/audit"
	"github.com/nutcas3/runbook-agent/internal/runbook"
)

type Server struct {
	executor    *runbook.Executor
	approvalSvc *approval.Service
	auditStore  *audit.Store
}

// AlertPayload is the webhook body from Prometheus or PagerDuty.
type AlertPayload struct {
	Alert     string `json:"alert"`
	Severity  string `json:"severity"`
	Runbook   string `json:"runbook"` // e.g. "payment-gateway-failure"
	Telemetry string `json:"telemetry"`
}

// DecisionPayload is posted by the dashboard Approve/Reject buttons.
type DecisionPayload struct {
	ApprovedBy    string `json:"approved_by"`
	Justification string `json:"justification"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Wire up dependencies
	g := genkit.Init(ctx,
		genkit.WithPlugins(&googlegenai.GoogleAI{}),
		genkit.WithDefaultModel("googleai/gemini-2.5-flash"),
	)
	_ = g // used by agent internally

	ag, err := agent.New(ctx)
	if err != nil {
		log.Fatalf("agent init: %v", err)
	}

	approvalSvc := approval.New()

	auditStore, err := audit.New("runbook-audit.db")
	if err != nil {
		log.Fatalf("audit store: %v", err)
	}

	executor := runbook.NewExecutor(ag, approvalSvc, auditStore)

	srv := &Server{
		executor:    executor,
		approvalSvc: approvalSvc,
		auditStore:  auditStore,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/alerts", srv.handleAlert)
	mux.HandleFunc("GET /api/approvals", srv.handleListApprovals)
	mux.HandleFunc("POST /api/approvals/", srv.handleDecision)
	mux.HandleFunc("GET /api/incidents/", srv.handleIncidentHistory)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	httpServer := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Minute, // long: approval handler blocks up to 5m
	}

	go func() {
		log.Println("runbook-agent listening on :8080")
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down gracefully…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

// handleAlert is called by Prometheus Alertmanager (or any webhook source).
// It loads the runbook, starts execution in a goroutine, and returns immediately.
func (s *Server) handleAlert(w http.ResponseWriter, r *http.Request) {
	var payload AlertPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}

	rb, err := runbook.Parse("runbooks/" + payload.Runbook + ".yaml")
	if err != nil {
		http.Error(w, "runbook not found: "+err.Error(), http.StatusNotFound)
		return
	}

	// Run the runbook asynchronously — the HTTP response returns immediately.
	// Status updates are delivered via the approval dashboard and audit trail.
	go func() {
		result, err := s.executor.Execute(context.Background(), rb, payload.Telemetry)
		if err != nil {
			log.Printf("runbook execution error [%s]: %v", payload.Runbook, err)
			return
		}
		log.Printf("runbook %s completed: resolved=%v duration=%s",
			result.RunbookName, result.Resolved, result.Duration)
	}()

	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
}

// handleListApprovals returns pending approval requests for the dashboard.
func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	pending := s.approvalSvc.GetPending()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(pending)
}

// handleDecision routes to Approve or Reject based on the URL path.
// POST /api/approvals/{id}/approve
// POST /api/approvals/{id}/reject
func (s *Server) handleDecision(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/approvals/"), "/")
	if len(parts) != 2 {
		http.Error(w, "path must be /api/approvals/{id}/approve or /reject", http.StatusBadRequest)
		return
	}
	id, verb := parts[0], parts[1]

	var payload DecisionPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}

	var err error
	switch verb {
	case "approve":
		err = s.approvalSvc.Approve(id, payload.ApprovedBy, payload.Justification)
	case "reject":
		err = s.approvalSvc.Reject(id, payload.ApprovedBy, payload.Justification)
	default:
		http.Error(w, "unknown verb: "+verb, http.StatusBadRequest)
		return
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": verb + "d"})
}

// handleIncidentHistory returns the full audit trail for one incident.
func (s *Server) handleIncidentHistory(w http.ResponseWriter, r *http.Request) {
	incidentID := strings.TrimPrefix(r.URL.Path, "/api/incidents/")
	incidentID = strings.TrimSuffix(incidentID, "/history")

	events, err := s.auditStore.History(r.Context(), incidentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events)
}
