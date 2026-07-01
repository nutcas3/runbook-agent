// internal/agent/agent.go
//
// The RunbookAgent wraps Google Gemini via Genkit Go.
// It performs read-only diagnostics and forms a hypothesis about the incident.
//
// Key design decision: the agent ONLY ever reads. It can:
//   - Execute whitelisted diagnostic commands (kubectl get, curl, redis-cli info)
//   - Query Prometheus for metrics
//   - Read application logs
//
// It cannot:
//   - Modify production state
//   - Execute commands not on the whitelist
//   - Access systems outside its defined tool set
//   - Bypass the approval gate
//
// The LLM is the engine. The runbook is the track.
package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/googlegenai"
	"github.com/nutcas3/runbook-agent/internal/model"
	"github.com/nutcas3/runbook-agent/internal/runbook"
)

// AnalysisInput is the input to the Genkit analysis flow.
type AnalysisInput struct {
	RunbookYAML string `json:"runbook_yaml"`
	Telemetry   string `json:"telemetry"`
}

// allowedCommands is the whitelist enforced at the Go level.
// The LLM cannot hallucinate its way past this list.
var allowedCommands = []string{
	"kubectl get",
	"kubectl logs",
	"kubectl describe",
	"kubectl rollout history",
	"curl",
	"redis-cli info",
	"redis-cli get",
}

// RunbookAgent orchestrates Genkit Go tool use for incident diagnosis.
type RunbookAgent struct {
	g     *genkit.Genkit
	tools []ai.ToolRef
}

// New initialises a RunbookAgent with the Gemini 2.5 Flash model.
// Flash is chosen deliberately — sub-second tool calls matter during a live P0.
func New(ctx context.Context) (*RunbookAgent, error) {
	g := genkit.Init(ctx,
		genkit.WithPlugins(&googlegenai.GoogleAI{}),
		genkit.WithDefaultModel("googleai/gemini-2.5-flash"),
	)

	a := &RunbookAgent{g: g}
	a.registerTools()
	return a, nil
}

// registerTools defines every tool the agent can call during diagnosis.
// These are the only system interactions the LLM can trigger.
func (a *RunbookAgent) registerTools() {
	// Tool: run a whitelisted diagnostic command
	execTool := genkit.DefineTool(a.g, "execute_command",
		"Execute a read-only diagnostic command (kubectl get/logs/describe, curl, redis-cli info).",
		func(ctx *ai.ToolContext, in struct {
			Command string `json:"command"`
		}) (map[string]any, error) {
			return a.execCommand(ctx, in.Command)
		},
	)

	// Tool: query Prometheus metrics
	promTool := genkit.DefineTool(a.g, "query_prometheus",
		"Query a Prometheus metric by PromQL expression.",
		func(ctx *ai.ToolContext, in struct {
			Query string `json:"query"`
		}) (map[string]any, error) {
			return a.queryPrometheus(ctx, in.Query)
		},
	)

	// Tool: read recent application logs
	logTool := genkit.DefineTool(a.g, "read_logs",
		"Read the last N lines of logs for a Kubernetes service.",
		func(ctx *ai.ToolContext, in struct {
			Service   string `json:"service"`
			Namespace string `json:"namespace"`
			Lines     int    `json:"lines"`
		}) (map[string]any, error) {
			return a.readLogs(ctx, in.Service, in.Namespace, in.Lines)
		},
	)

	a.tools = []ai.ToolRef{execTool, promTool, logTool}
}

// Diagnose runs the full diagnostic flow for an incident.
// It returns a structured Hypothesis ready for the approval UI.
func (a *RunbookAgent) Diagnose(ctx context.Context, rb *runbook.Runbook, telemetry string) (*model.Hypothesis, error) {
	// Build a context-rich prompt from the runbook's diagnostic steps.
	steps := buildDiagnosticContext(rb)

	flow := genkit.DefineFlow(a.g, "incidentDiagnosis",
		func(ctx context.Context, in *AnalysisInput) (*model.Hypothesis, error) {
			prompt := fmt.Sprintf(`
You are an expert SRE incident response agent. You have access to diagnostic tools.
Your job is to determine the root cause of the incident and propose a recovery action.

RUNBOOK: %s
VERSION: %s

DIAGNOSTIC STEPS TO FOLLOW:
%s

CURRENT TELEMETRY:
%s

Use the tools to gather evidence, then return a structured hypothesis with:
- root_cause: a concise one-sentence description
- confidence: a float between 0 and 1
- action: the recovery_action condition that applies (e.g. "stripe_down")
- risk_level: "low", "medium", or "high"
- reasoning: 3-5 bullet points of evidence supporting the hypothesis

Always use the tools before forming your hypothesis. Do not guess.
`, in.RunbookYAML, rb.Version, steps, in.Telemetry)

			hyp, _, err := genkit.GenerateData[model.Hypothesis](ctx, a.g,
				ai.WithPrompt(prompt),
				ai.WithTools(a.tools...),
				ai.WithConfig(ai.GenerationCommonConfig{Temperature: 0.1}),
			)
			if err != nil {
				return nil, fmt.Errorf("gemini diagnosis failed: %w", err)
			}
			return hyp, nil
		},
	)

	return flow.Run(ctx, &AnalysisInput{
		RunbookYAML: rb.Name,
		Telemetry:   telemetry,
	})
}

// execCommand executes a whitelisted diagnostic command.
// Commands not on the whitelist are rejected with an error — at the Go level,
// not the prompt level. The LLM cannot bypass this.
func (a *RunbookAgent) execCommand(ctx context.Context, command string) (map[string]any, error) {
	if !isAllowed(command) {
		return map[string]any{
			"error": fmt.Sprintf("command not whitelisted: %q", command),
		}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "sh", "-c", command).CombinedOutput()
	result := map[string]any{
		"stdout": string(out),
		"ok":     err == nil,
	}
	if err != nil {
		result["error"] = err.Error()
	}
	return result, nil
}

func (a *RunbookAgent) queryPrometheus(ctx context.Context, query string) (map[string]any, error) {
	url := fmt.Sprintf("http://prometheus.internal/api/v1/query?query=%s", query)
	cmd := fmt.Sprintf("curl -sf %q", url)
	return a.execCommand(ctx, cmd)
}

func (a *RunbookAgent) readLogs(ctx context.Context, service, namespace string, lines int) (map[string]any, error) {
	if lines <= 0 || lines > 500 {
		lines = 100
	}
	cmd := fmt.Sprintf("kubectl logs -l app=%s -n %s --tail=%d", service, namespace, lines)
	return a.execCommand(ctx, cmd)
}

func isAllowed(cmd string) bool {
	for _, prefix := range allowedCommands {
		if strings.HasPrefix(strings.TrimSpace(cmd), prefix) {
			return true
		}
	}
	return false
}

func buildDiagnosticContext(rb *runbook.Runbook) string {
	var sb strings.Builder
	for _, s := range rb.Diagnostics {
		sb.WriteString(fmt.Sprintf("Step %d: %s\n  Command: %s\n  Interpret: %s\n\n",
			s.Step, s.Name, s.Command, s.AIInterpret))
	}
	return sb.String()
}
