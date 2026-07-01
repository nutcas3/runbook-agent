// internal/runbook/types.go
// Defines the core runbook data model.
// A runbook is a structured, executable state machine — not a markdown file.
package runbook

import (
	"context"
	"time"

	"github.com/nutcas3/runbook-agent/internal/model"
)

// Runbook is the top-level structure parsed from YAML.
// Each runbook maps to one class of production incident.
type Runbook struct {
	Name        string           `yaml:"name"`
	Version     string           `yaml:"version"`
	Severity    string           `yaml:"severity"`
	MTTRTarget  string           `yaml:"mttr_target"`
	Triggers    []Trigger        `yaml:"triggers"`
	SafeState   SafeState        `yaml:"safe_state"`
	Diagnostics []DiagnosticStep `yaml:"diagnostic_steps"`
	Recovery    []RecoveryAction `yaml:"recovery_actions"`
}

// Trigger defines when this runbook activates.
type Trigger struct {
	Alert     string `yaml:"alert"`
	Threshold string `yaml:"threshold"`
	Duration  string `yaml:"duration"`
}

// SafeState defines the boundary between read-only observation
// and state-changing actions. This is the core safety contract.
type SafeState struct {
	// ReadOnly lists actions that are always forbidden, regardless of approval.
	ReadOnly []string `yaml:"read_only"`
	// RequiresApproval lists actions that block until a human approves.
	RequiresApproval []string `yaml:"requires_approval"`
}

// DiagnosticStep is a read-only observation step.
// The AI interprets the output; no state changes occur here.
type DiagnosticStep struct {
	Step        int           `yaml:"step"`
	Name        string        `yaml:"name"`
	Command     string        `yaml:"command"`
	Timeout     time.Duration `yaml:"timeout"`
	AIInterpret string        `yaml:"ai_interpret"`
}

// RecoveryAction is a state-changing operation.
// It may require human approval before execution.
type RecoveryAction struct {
	Condition        string `yaml:"condition"`
	Action           string `yaml:"action"`
	RequiresApproval bool   `yaml:"requires_approval"`
	Command          string `yaml:"command"`
	RollbackCommand  string `yaml:"rollback_command"`
}

// Agent is the interface implemented by the diagnostic AI agent.
type Agent interface {
	Diagnose(ctx context.Context, rb *Runbook, telemetry string) (*model.Hypothesis, error)
}
