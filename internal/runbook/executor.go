// internal/runbook/executor.go
//
// The Executor is the state machine that runs a runbook end-to-end.
//
// Execution flow:
//  1. Run diagnostic steps (read-only, agent tool use)
//  2. Form a hypothesis (Gemini analysis)
//  3. For each applicable recovery action:
//     a. Check safe state (forbidden?) → error if yes
//     b. Check approval gate → block if required
//     c. Execute on human approval
//     d. Log everything to the audit trail
//
// This is what replaces the copy-paste-from-markdown-at-2AM workflow.
package runbook

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/google/uuid"
	"github.com/nutcas3/runbook-agent/internal/approval"
	"github.com/nutcas3/runbook-agent/internal/audit"
	"github.com/nutcas3/runbook-agent/internal/model"
	"github.com/nutcas3/runbook-agent/internal/safe"
)

// ExecutionResult is returned when a runbook completes (or is rejected/timed out).
type ExecutionResult struct {
	IncidentID  string            `json:"incident_id"`
	RunbookName string            `json:"runbook_name"`
	Hypothesis  *model.Hypothesis `json:"hypothesis"`
	Resolved    bool              `json:"resolved"`
	Duration    time.Duration     `json:"duration"`
	AuditEvents []audit.Event     `json:"audit_events"`
}

// Executor runs a Runbook against a live incident.
type Executor struct {
	agent    Agent
	approval *approval.Service
	audit    *audit.Store
}

// NewExecutor creates an Executor with all required dependencies.
func NewExecutor(
	ag Agent,
	ap *approval.Service,
	au *audit.Store,
) *Executor {
	return &Executor{agent: ag, approval: ap, audit: au}
}

// Execute runs the full runbook lifecycle for one incident.
func (e *Executor) Execute(ctx context.Context, rb *Runbook, telemetry string) (*ExecutionResult, error) {
	incidentID := uuid.New().String()
	start := time.Now()

	_ = e.audit.Log(ctx, incidentID, "system", "runbook_started", map[string]any{
		"runbook": rb.Name,
		"version": rb.Version,
	})

	// Phase 1: AI diagnosis (read-only)
	_ = e.audit.Log(ctx, incidentID, "gemini-agent", "diagnosis_started", nil)

	hyp, err := e.agent.Diagnose(ctx, rb, telemetry)
	if err != nil {
		return nil, fmt.Errorf("diagnosis failed: %w", err)
	}

	_ = e.audit.Log(ctx, incidentID, "gemini-agent", "hypothesis_formed", map[string]any{
		"root_cause": hyp.RootCause,
		"confidence": hyp.Confidence,
		"action":     hyp.Action,
		"risk_level": hyp.RiskLevel,
	})

	// Phase 2: Find and execute the matching recovery action
	enforcer := safe.New(rb.SafeState.ReadOnly, rb.SafeState.RequiresApproval)

	for _, action := range rb.Recovery {
		if action.Condition != hyp.Action {
			continue
		}

		// Safety check: is this action forbidden?
		if err := enforcer.Validate(action.Action); err != nil {
			_ = e.audit.Log(ctx, incidentID, "system", "action_blocked", map[string]any{
				"action": action.Action,
				"reason": err.Error(),
			})
			return nil, err
		}

		// Approval gate: park here until human decides
		if enforcer.RequiresApproval(action.Action) || action.RequiresApproval {
			_ = e.audit.Log(ctx, incidentID, "system", "approval_requested", map[string]any{
				"action": action.Action,
			})

			approved, justification, err := e.approval.Request(ctx, incidentID, hyp)
			if err != nil {
				return nil, fmt.Errorf("approval gate error: %w", err)
			}

			if !approved {
				_ = e.audit.Log(ctx, incidentID, "engineer", "action_rejected", map[string]any{
					"action":        action.Action,
					"justification": justification,
				})
				return &ExecutionResult{
					IncidentID:  incidentID,
					RunbookName: rb.Name,
					Hypothesis:  hyp,
					Resolved:    false,
					Duration:    time.Since(start),
				}, nil
			}

			_ = e.audit.Log(ctx, incidentID, "engineer", "action_approved", map[string]any{
				"action":        action.Action,
				"justification": justification,
			})
		}

		// Execute the recovery command
		if err := e.executeCommand(ctx, action.Command); err != nil {
			// Auto-rollback on failure
			_ = e.audit.Log(ctx, incidentID, "system", "execution_failed_rolling_back", map[string]any{
				"error": err.Error(),
			})
			_ = e.executeCommand(ctx, action.RollbackCommand)
			return nil, fmt.Errorf("action %q failed (rollback attempted): %w", action.Action, err)
		}

		_ = e.audit.Log(ctx, incidentID, "system", "action_executed", map[string]any{
			"action":  action.Action,
			"command": action.Command,
		})

		history, _ := e.audit.History(ctx, incidentID)
		return &ExecutionResult{
			IncidentID:  incidentID,
			RunbookName: rb.Name,
			Hypothesis:  hyp,
			Resolved:    true,
			Duration:    time.Since(start),
			AuditEvents: history,
		}, nil
	}

	return nil, fmt.Errorf("no recovery action matched condition %q", hyp.Action)
}

func (e *Executor) executeCommand(ctx context.Context, command string) error {
	if command == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "sh", "-c", command).Run()
}
