// internal/approval/service.go
//
// The ApprovalService is the human-in-the-loop gate.
//
// When the agent forms a hypothesis and proposes a state-changing action,
// execution is suspended here. The coroutine parks on a Go channel and
// waits for a signal from the dashboard. Nothing happens until a human
// clicks Approve or Reject.
//
// This is not optional. The runbook engine calls RequireApproval() for
// every action flagged requires_approval: true, and the engine will not
// proceed until this function returns.
package approval

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nutcas3/runbook-agent/internal/model"
)

// Status represents the lifecycle of an approval request.
type Status string

const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusRejected Status = "rejected"
	StatusTimeout  Status = "timeout"
)

// Request is created when the engine reaches an approval gate.
// It is surfaced on the dashboard for the on-call engineer to review.
type Request struct {
	ID         string     `json:"id"`
	IncidentID string     `json:"incident_id"`
	Action     string     `json:"action"`
	Confidence float64    `json:"confidence"`
	RiskLevel  string     `json:"risk_level"`
	Reasoning  string     `json:"reasoning"`
	Status     Status     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	DecidedAt  *time.Time `json:"decided_at,omitempty"`
	// ApprovedBy captures the engineer's identity for the audit trail.
	ApprovedBy    string `json:"approved_by,omitempty"`
	Justification string `json:"justification,omitempty"`
}

// Result is sent on the channel when the engineer decides.
type Result struct {
	Approved      bool
	ApprovedBy    string
	Justification string
}

// Service manages the lifecycle of approval requests.
// It is safe for concurrent use — multiple incidents can be in-flight simultaneously.
type Service struct {
	mu       sync.RWMutex
	pending  map[string]chan Result
	requests map[string]*Request
}

func New() *Service {
	return &Service{
		pending:  make(map[string]chan Result),
		requests: make(map[string]*Request),
	}
}

// Request creates an approval request for the given hypothesis and blocks
// until the engineer approves, rejects, or the 5-minute timeout expires.
//
// The calling goroutine is parked here — the incident state machine is frozen
// at this gate. The HTTP handler (Approve/Reject) sends on the channel to unblock.
func (s *Service) Request(
	ctx context.Context,
	incidentID string,
	hyp *model.Hypothesis,
) (bool, string, error) {
	id := uuid.New().String()

	req := &Request{
		ID:         id,
		IncidentID: incidentID,
		Action:     hyp.Action,
		Confidence: hyp.Confidence,
		RiskLevel:  hyp.RiskLevel,
		Reasoning:  hyp.Reasoning,
		Status:     StatusPending,
		CreatedAt:  time.Now().UTC(),
	}

	ch := make(chan Result, 1)

	s.mu.Lock()
	s.pending[id] = ch
	s.requests[id] = req
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	// Block until decision or timeout.
	// 5 minutes: enough for a groggy engineer to open Slack and click a button.
	select {
	case result := <-ch:
		return result.Approved, result.Justification, nil
	case <-time.After(5 * time.Minute):
		s.updateStatus(id, StatusTimeout, "", "")
		return false, "approval timeout — escalating to secondary on-call", nil
	case <-ctx.Done():
		return false, "", ctx.Err()
	}
}

// Approve unblocks the waiting Request goroutine with an approval.
// The engineer's identity is recorded for the audit trail.
func (s *Service) Approve(id, approvedBy, justification string) error {
	return s.decide(id, approvedBy, justification, true)
}

// Reject unblocks the waiting Request goroutine with a rejection.
func (s *Service) Reject(id, approvedBy, justification string) error {
	return s.decide(id, approvedBy, justification, false)
}

// GetPending returns all requests currently awaiting a decision.
// The dashboard polls this to display the approval queue.
func (s *Service) GetPending() []*Request {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*Request, 0, len(s.pending))
	for id := range s.pending {
		if req, ok := s.requests[id]; ok {
			out = append(out, req)
		}
	}
	return out
}

func (s *Service) decide(id, approvedBy, justification string, approved bool) error {
	s.mu.RLock()
	ch, ok := s.pending[id]
	s.mu.RUnlock()

	if !ok {
		return fmt.Errorf("approval %q not found or already decided", id)
	}

	status := StatusApproved
	if !approved {
		status = StatusRejected
	}
	s.updateStatus(id, status, approvedBy, justification)

	ch <- Result{
		Approved:      approved,
		ApprovedBy:    approvedBy,
		Justification: justification,
	}
	return nil
}

func (s *Service) updateStatus(id string, status Status, approvedBy, justification string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if req, ok := s.requests[id]; ok {
		now := time.Now().UTC()
		req.Status = status
		req.DecidedAt = &now
		req.ApprovedBy = approvedBy
		req.Justification = justification
	}
}
