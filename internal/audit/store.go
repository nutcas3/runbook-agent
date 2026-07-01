// internal/audit/store.go
//
// The AuditStore records every action taken during an incident response:
// who approved what, when, and why. This is the compliance record.
//
// Every write action executed by the system is attributed:
//   - actor: "gemini-agent" for AI actions, engineer name for approvals
//   - action_type: the operation performed
//   - incident_id: ties all events to a single incident
//   - details: structured JSON with the full context
//
// This answers the post-mortem question: "what exactly happened and who authorised it?"
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
)

// Event is one record in the audit log.
type Event struct {
	ID         string         `json:"id"`
	IncidentID string         `json:"incident_id"`
	Timestamp  time.Time      `json:"timestamp"`
	Actor      string         `json:"actor"`
	ActionType string         `json:"action_type"`
	Details    map[string]any `json:"details"`
}

// Store persists audit events to SQLite.
// In production, swap the driver for Postgres or Cloud Spanner.
type Store struct {
	db *sql.DB
}

// New opens (or creates) the audit database at dbPath.
func New(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening audit db: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrating audit db: %w", err)
	}
	return s, nil
}

// Log records a single event. Call this for every meaningful system action.
func (s *Store) Log(
	ctx context.Context,
	incidentID string,
	actor string,
	actionType string,
	details map[string]any,
) error {
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("marshalling details: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO audit_log (id, incident_id, timestamp, actor, action_type, details)
		VALUES (?, ?, ?, ?, ?, ?)
	`, uuid.New().String(), incidentID, time.Now().UTC(), actor, actionType, string(detailsJSON))
	return err
}

// History returns all events for an incident, ordered chronologically.
func (s *Store) History(ctx context.Context, incidentID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, incident_id, timestamp, actor, action_type, details
		FROM audit_log
		WHERE incident_id = ?
		ORDER BY timestamp ASC
	`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var detailsJSON string
		if err := rows.Scan(
			&e.ID, &e.IncidentID, &e.Timestamp,
			&e.Actor, &e.ActionType, &detailsJSON,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(detailsJSON), &e.Details); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS audit_log (
			id          TEXT PRIMARY KEY,
			incident_id TEXT NOT NULL,
			timestamp   DATETIME NOT NULL,
			actor       TEXT NOT NULL,
			action_type TEXT NOT NULL,
			details     TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_incident_id ON audit_log(incident_id);
	`)
	return err
}
