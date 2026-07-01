# runbook-agent

An AI-powered incident response agent that turns runbooks into executable, safe state machines. Instead of copy-pasting commands from a Markdown file at 2 AM, the agent diagnoses the incident with Gemini, proposes a recovery action, and—after human approval—executes it.

## What it does

1. **Receives an alert** via webhook (`POST /api/alerts`)
2. **Loads a structured runbook** from YAML (diagnostic steps + recovery actions)
3. **Diagnoses the incident** using Gemini with read-only tools (`kubectl`, `curl`, `redis-cli`, Prometheus)
4. **Forms a structured hypothesis** (root cause, confidence, risk level)
5. **Checks safety gates** — forbidden actions are rejected; risky ones block for human approval
6. **Executes recovery** only after engineer approval via the dashboard
7. **Logs everything** to an audit trail (SQLite by default)

## Architecture

```
cmd/server          HTTP server (alerts, approvals, audit)
internal/agent      Gemini agent with read-only diagnostic tools
internal/runbook    YAML parser, executor, and Agent interface
internal/model      Shared types (Hypothesis) — cycle-free
internal/approval   Human-in-the-loop gate
internal/audit      SQLite audit store
internal/safe       Safety enforcer (read-only + approval lists)
```

## Safety model

The agent **only reads** during diagnosis. It cannot:

- Execute non-whitelisted commands
- Modify state without explicit approval
- Bypass the safety enforcer

Recovery actions are checked against the runbook's `safe_state`:

```yaml
safe_state:
  read_only:
    - delete_database
  requires_approval:
    - restart_service
```

## Quick start

### Prerequisites

- Go 1.26+
- `GOOGLE_API_KEY` environment variable (for Gemini)
- `kubectl` configured (optional, for K8s diagnostics)

### Run

```bash
go run ./cmd/server
```

The server listens on `:8080`.

### Trigger a runbook

```bash
curl -X POST http://localhost:8080/api/alerts \
  -H "Content-Type: application/json" \
  -d '{
    "alert": "PaymentGatewayErrorRate",
    "severity": "critical",
    "runbook": "payment-gateway-failure",
    "telemetry": "error_rate: 45%, p99_latency: 2.3s"
  }'
```

### Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/alerts` | Receive alert webhook |
| `GET`  | `/api/approvals` | List pending approvals |
| `POST` | `/api/approvals/:id/approve` | Approve an action |
| `POST` | `/api/approvals/:id/reject` | Reject an action |
| `GET`  | `/api/incidents/:id/history` | Audit trail |
| `GET`  | `/health` | Liveness probe |

## Runbook format

```yaml
name: payment-gateway-failure
version: "1.0"
severity: critical
mttr_target: 5m

triggers:
  - alert: PaymentGatewayErrorRate
    threshold: "> 10%"
    duration: 2m

safe_state:
  read_only:
    - delete_database
  requires_approval:
    - restart_service

diagnostic_steps:
  - step: 1
    name: Check pod status
    command: kubectl get pods -n payments
    ai_interpret: Look for CrashLoopBackOff or high restart counts

recovery_actions:
  - condition: stripe_down
    action: restart_service
    requires_approval: true
    command: kubectl rollout restart deployment/payment-gateway -n payments
    rollback_command: kubectl rollout undo deployment/payment-gateway -n payments
```

## Project layout

| Package | Responsibility |
|---------|---------------|
| `internal/agent` | Gemini agent, diagnostic tools, hypothesis generation |
| `internal/runbook` | YAML parsing, runbook execution engine, `Agent` interface |
| `internal/model` | Shared data types (avoids import cycles) |
| `internal/approval` | Human approval queue and blocking requests |
| `internal/audit` | SQLite-backed event log |
| `internal/safe` | Safety policy enforcement |

## Design decisions

- **Agent only reads** — all write operations happen in the executor, after safety checks
- **Interface-driven** — `runbook.Agent` decouples the executor from the concrete Gemini agent
- **Model package** — `internal/model` breaks the `agent ↔ runbook` import cycle
- **Genkit v1.10** — uses `ai.ToolContext`, `ai.WithConfig`, and `genkit.GenerateData` APIs
