// Package transfer synchronizes archived transcripts between Tracer hosts.
package transfer

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// CursorStore persists the last file hash successfully pushed to each remote.
type CursorStore struct {
	db *sql.DB
}

// CursorEntry is one path/hash pair committed after a successful push.
type CursorEntry struct {
	RelPath     string
	ContentHash string
}

// OpenCursorStore opens the engine state database and creates only the push cursor table.
func OpenCursorStore(path string) (*CursorStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create state db directory: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout=15000&_pragma=journal_mode(WAL)", filepath.ToSlash(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open push state db: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	schema := "CREATE TABLE IF NOT EXISTS push_state (" +
		"remote TEXT NOT NULL," +
		"rel_path TEXT NOT NULL," +
		"content_hash TEXT NOT NULL," +
		"pushed_at TEXT NOT NULL," +
		"PRIMARY KEY (remote, rel_path)" +
		");"
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ensure push state schema: %w", err)
	}

	return &CursorStore{db: db}, nil
}

// LoadCursorHashesReadOnly reads an existing push cursor without creating a database or table.
func LoadCursorHashesReadOnly(path, remote string) (map[string]string, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("inspect push state db: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout=15000", filepath.ToSlash(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open push state db read-only: %w", err)
	}
	defer db.Close()
	var tableCount int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'push_state'",
	).Scan(&tableCount); err != nil {
		return nil, fmt.Errorf("inspect push state schema: %w", err)
	}
	if tableCount == 0 {
		return map[string]string{}, nil
	}
	store := &CursorStore{db: db}
	return store.LoadHashes(remote)
}

// Close closes the state database.
func (s *CursorStore) Close() error {
	return s.db.Close()
}

// LoadHashes returns the last successfully pushed hash for every path at a remote.
func (s *CursorStore) LoadHashes(remote string) (map[string]string, error) {
	rows, err := s.db.Query("SELECT rel_path, content_hash FROM push_state WHERE remote = ?", remote)
	if err != nil {
		return nil, fmt.Errorf("query push state: %w", err)
	}
	defer rows.Close()

	hashes := make(map[string]string)
	for rows.Next() {
		var relPath string
		var contentHash string
		if err := rows.Scan(&relPath, &contentHash); err != nil {
			return nil, fmt.Errorf("scan push state: %w", err)
		}
		hashes[relPath] = contentHash
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate push state: %w", err)
	}
	return hashes, nil
}

// Upsert records one successfully pushed file.
func (s *CursorStore) Upsert(remote, relPath, contentHash string, pushedAt time.Time) error {
	if pushedAt.IsZero() {
		pushedAt = time.Now().UTC()
	}
	query := "INSERT INTO push_state (remote, rel_path, content_hash, pushed_at) VALUES (?, ?, ?, ?) " +
		"ON CONFLICT(remote, rel_path) DO UPDATE SET " +
		"content_hash = excluded.content_hash, pushed_at = excluded.pushed_at"
	_, err := s.db.Exec(
		query,
		remote,
		relPath,
		contentHash,
		pushedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("upsert push state: %w", err)
	}
	return nil
}

// CommitPush atomically upserts stable transfers and prunes paths absent from the scan.
func (s *CursorStore) CommitPush(remote string, entries []CursorEntry, presentPaths []string, pushedAt time.Time) error {
	if pushedAt.IsZero() {
		pushedAt = time.Now().UTC()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin push state commit: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()

	query := "INSERT INTO push_state (remote, rel_path, content_hash, pushed_at) VALUES (?, ?, ?, ?) " +
		"ON CONFLICT(remote, rel_path) DO UPDATE SET " +
		"content_hash = excluded.content_hash, pushed_at = excluded.pushed_at"
	for _, entry := range entries {
		if _, err := tx.Exec(
			query,
			remote,
			entry.RelPath,
			entry.ContentHash,
			pushedAt.UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("upsert push state: %w", err)
		}
	}

	present := make(map[string]struct{}, len(presentPaths))
	for _, relPath := range presentPaths {
		present[relPath] = struct{}{}
	}
	rows, err := tx.Query("SELECT rel_path FROM push_state WHERE remote = ?", remote)
	if err != nil {
		return fmt.Errorf("query push state for pruning: %w", err)
	}
	stale := make([]string, 0)
	for rows.Next() {
		var relPath string
		if err := rows.Scan(&relPath); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan push state for pruning: %w", err)
		}
		if _, ok := present[relPath]; !ok {
			stale = append(stale, relPath)
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close push state pruning query: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate push state for pruning: %w", err)
	}
	for _, relPath := range stale {
		if _, err := tx.Exec("DELETE FROM push_state WHERE remote = ? AND rel_path = ?", remote, relPath); err != nil {
			return fmt.Errorf("prune push state: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit push state: %w", err)
	}
	rollback = false
	return nil
}
