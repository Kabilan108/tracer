package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// SessionState tracks the latest processed content for a provider session.
type SessionState struct {
	ProviderID  string
	SessionID   string
	ContentHash string
	OutputPath  string
	UpdatedAt   time.Time
}

// SourceState records the inputs a provider source was last ingested from and
// the sessions it produced, so unchanged sources can be skipped on startup.
type SourceState struct {
	ProviderID  string
	SourceKey   string
	Fingerprint string
	Version     int
	SessionIDs  []string
	UpdatedAt   time.Time
}

// StateStore persists runtime dedupe/checkpoint state for session processing.
type StateStore struct {
	db *sql.DB
}

// OpenStateStore opens or creates a SQLite state database.
func OpenStateStore(path string) (*StateStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create state db directory: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout=15000&_pragma=journal_mode(WAL)", filepath.ToSlash(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS session_state (
			provider_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			output_path TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (provider_id, session_id)
		);
		CREATE TABLE IF NOT EXISTS source_state (
			provider_id TEXT NOT NULL,
			source_key TEXT NOT NULL,
			fingerprint TEXT NOT NULL,
			version INTEGER NOT NULL,
			session_ids TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (provider_id, source_key)
		);
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ensure state schema: %w", err)
	}

	return &StateStore{db: db}, nil
}

// Close closes the state database.
func (s *StateStore) Close() error {
	return s.db.Close()
}

// Get returns persisted state for a provider session.
func (s *StateStore) Get(providerID, sessionID string) (SessionState, bool, error) {
	var state SessionState
	var updatedAtRaw string

	err := s.db.QueryRow(
		`SELECT provider_id, session_id, content_hash, output_path, updated_at
		 FROM session_state
		 WHERE provider_id = ? AND session_id = ?`,
		providerID,
		sessionID,
	).Scan(
		&state.ProviderID,
		&state.SessionID,
		&state.ContentHash,
		&state.OutputPath,
		&updatedAtRaw,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return SessionState{}, false, nil
		}
		return SessionState{}, false, fmt.Errorf("query session state: %w", err)
	}

	updatedAt, err := time.Parse(time.RFC3339Nano, updatedAtRaw)
	if err != nil {
		return SessionState{}, false, fmt.Errorf("parse state updated_at: %w", err)
	}
	state.UpdatedAt = updatedAt

	return state, true, nil
}

// Upsert saves state for a provider session.
func (s *StateStore) Upsert(state SessionState) error {
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = time.Now().UTC()
	}

	_, err := s.db.Exec(
		`INSERT INTO session_state (provider_id, session_id, content_hash, output_path, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(provider_id, session_id) DO UPDATE SET
		   content_hash = excluded.content_hash,
		   output_path = excluded.output_path,
		   updated_at = excluded.updated_at`,
		state.ProviderID,
		state.SessionID,
		state.ContentHash,
		state.OutputPath,
		state.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("upsert session state: %w", err)
	}

	return nil
}

// GetSource returns persisted state for a provider source.
func (s *StateStore) GetSource(providerID, sourceKey string) (SourceState, bool, error) {
	state := SourceState{ProviderID: providerID, SourceKey: sourceKey}
	var sessionIDsRaw string
	var updatedAtRaw string

	err := s.db.QueryRow(
		`SELECT fingerprint, version, session_ids, updated_at
		 FROM source_state
		 WHERE provider_id = ? AND source_key = ?`,
		providerID,
		sourceKey,
	).Scan(&state.Fingerprint, &state.Version, &sessionIDsRaw, &updatedAtRaw)
	if err != nil {
		if err == sql.ErrNoRows {
			return SourceState{}, false, nil
		}
		return SourceState{}, false, fmt.Errorf("query source state: %w", err)
	}

	if err := json.Unmarshal([]byte(sessionIDsRaw), &state.SessionIDs); err != nil {
		return SourceState{}, false, fmt.Errorf("parse source session ids: %w", err)
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, updatedAtRaw)
	if err != nil {
		return SourceState{}, false, fmt.Errorf("parse source updated_at: %w", err)
	}
	state.UpdatedAt = updatedAt

	return state, true, nil
}

// UpsertSource saves state for a provider source.
func (s *StateStore) UpsertSource(state SourceState) error {
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = time.Now().UTC()
	}
	sessionIDs := state.SessionIDs
	if sessionIDs == nil {
		sessionIDs = []string{}
	}
	sessionIDsRaw, err := json.Marshal(sessionIDs)
	if err != nil {
		return fmt.Errorf("encode source session ids: %w", err)
	}

	_, err = s.db.Exec(
		`INSERT INTO source_state (provider_id, source_key, fingerprint, version, session_ids, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(provider_id, source_key) DO UPDATE SET
		   fingerprint = excluded.fingerprint,
		   version = excluded.version,
		   session_ids = excluded.session_ids,
		   updated_at = excluded.updated_at`,
		state.ProviderID,
		state.SourceKey,
		state.Fingerprint,
		state.Version,
		string(sessionIDsRaw),
		state.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("upsert source state: %w", err)
	}
	return nil
}
