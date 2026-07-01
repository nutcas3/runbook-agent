// internal/safe/enforcer.go
// The SafeStateEnforcer is the core safety contract of the system.
//
// It answers two questions for every proposed action:
//   1. Is this action forbidden outright? (read_only list)
//   2. Does this action require human approval before execution?
//
// These checks happen at the framework level — not in the runbook YAML,
// not in the LLM prompt, but in compiled Go code. The agent literally
// cannot call a forbidden action; the function returns an error.
package safe

import (
	"fmt"
)

// Enforcer separates safe read-only diagnostics from state-changing operations.
type Enforcer struct {
	forbidden        map[string]bool
	requiresApproval map[string]bool
}

// New creates an Enforcer from the runbook's safe_state configuration.
func New(forbidden []string, requiresApproval []string) *Enforcer {
	e := &Enforcer{
		forbidden:        make(map[string]bool, len(forbidden)),
		requiresApproval: make(map[string]bool, len(requiresApproval)),
	}
	for _, a := range forbidden {
		e.forbidden[a] = true
	}
	for _, a := range requiresApproval {
		e.requiresApproval[a] = true
	}
	return e
}

// Validate returns an error if the action is outright forbidden.
// Call this before executing any action, diagnostic or recovery.
func (e *Enforcer) Validate(action string) error {
	if e.forbidden[action] {
		return fmt.Errorf(
			"action %q is in the read-only list and can never be executed by the agent",
			action,
		)
	}
	return nil
}

// RequiresApproval returns true if the action must be approved by a human
// before execution. The agent will pause and wait for a dashboard click.
func (e *Enforcer) RequiresApproval(action string) bool {
	return e.requiresApproval[action]
}
