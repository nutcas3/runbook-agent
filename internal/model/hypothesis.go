package model

// Hypothesis is the structured output the agent produces after diagnosis.
// Every field is required — the agent cannot return a partial analysis.
type Hypothesis struct {
	RootCause  string  `json:"root_cause"`
	Confidence float64 `json:"confidence"` // 0.0 – 1.0
	Action     string  `json:"action"`     // matches a recovery_action condition
	RiskLevel  string  `json:"risk_level"` // "low" | "medium" | "high"
	Reasoning  string  `json:"reasoning"`  // evidence summary for the approval UI
}
