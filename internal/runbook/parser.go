// internal/runbook/parser.go
// Parses a YAML runbook file into a typed Runbook struct.
package runbook

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Parse reads a YAML runbook file from disk and returns a validated Runbook.
func Parse(path string) (*Runbook, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading runbook %q: %w", path, err)
	}

	var rb Runbook
	if err := yaml.Unmarshal(data, &rb); err != nil {
		return nil, fmt.Errorf("parsing runbook %q: %w", path, err)
	}

	if err := validate(&rb); err != nil {
		return nil, fmt.Errorf("invalid runbook %q: %w", path, err)
	}

	return &rb, nil
}

func validate(rb *Runbook) error {
	if rb.Name == "" {
		return fmt.Errorf("runbook must have a name")
	}
	if rb.Version == "" {
		return fmt.Errorf("runbook must have a version")
	}
	if len(rb.Triggers) == 0 {
		return fmt.Errorf("runbook must have at least one trigger")
	}
	if len(rb.Diagnostics) == 0 {
		return fmt.Errorf("runbook must have at least one diagnostic step")
	}
	return nil
}
