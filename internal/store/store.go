package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Issue struct {
	ID             int64
	ProjectKey     string
	Fingerprint    string
	Type           string
	Title          string
	ExceptionClass string
	Level          string
	Environment    string
	Count          int64
	FirstSeenAt    time.Time
	LastSeenAt     time.Time
	Resolved       bool
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Single writer process; a single connection keeps SQLite serialization simple.
	db.SetMaxOpenConns(1)
	schema := `
CREATE TABLE IF NOT EXISTS issues (
	id INTEGER PRIMARY KEY,
	project_key TEXT NOT NULL,
	fingerprint TEXT NOT NULL,
	type TEXT NOT NULL DEFAULT 'error',
	title TEXT NOT NULL,
	exception_class TEXT,
	level TEXT,
	environment TEXT,
	count INTEGER NOT NULL DEFAULT 1,
	first_seen_at INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL,
	resolved INTEGER NOT NULL DEFAULT 0,
	UNIQUE(project_key, fingerprint)
);
CREATE INDEX IF NOT EXISTS idx_issues_last_seen ON issues(last_seen_at DESC);
CREATE TABLE IF NOT EXISTS profiles (
	id INTEGER PRIMARY KEY,
	project_key TEXT NOT NULL,
	type TEXT NOT NULL DEFAULT 'request',
	name TEXT NOT NULL,
	duration_ms REAL NOT NULL,
	environment TEXT,
	spans TEXT,
	hotspots TEXT,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_profiles_created ON profiles(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_profiles_type ON profiles(type, project_key, created_at DESC);
CREATE TABLE IF NOT EXISTS occurrences (
	id INTEGER PRIMARY KEY,
	issue_id INTEGER NOT NULL,
	type TEXT NOT NULL,
	payload TEXT,
	seen_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_occurrences_issue ON occurrences(issue_id, seen_at DESC);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// RecordError stores one exception occurrence, folding it into an existing
// issue with the same fingerprint (count+1, last_seen_at bumped) or creating
// a new issue row.
func (s *Store) RecordError(projectKey, exceptionClass, message, fingerprint, environment, level string, payload any) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO issues
		(project_key, fingerprint, type, title, exception_class, level, environment, count, first_seen_at, last_seen_at)
		VALUES (?, ?, 'error', ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(project_key, fingerprint) DO UPDATE SET
			count = count + 1, last_seen_at = excluded.last_seen_at`,
		projectKey, fingerprint, truncate(message, 240), exceptionClass, level, environment, now, now)
	if err != nil {
		return 0, fmt.Errorf("upsert issue: %w", err)
	}
	issueID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := s.insertOccurrence(issueID, "error", payload, now); err != nil {
		return issueID, err
	}
	return issueID, nil
}

// RecordLog stores one Monolog record occurrence.
func (s *Store) RecordLog(projectKey, channel, level, message string, context map[string]any, environment string) (int64, error) {
	fp := ForLog(channel, level, message)
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO issues
		(project_key, fingerprint, type, title, exception_class, level, environment, count, first_seen_at, last_seen_at)
		VALUES (?, ?, 'log', ?, NULL, ?, ?, 1, ?, ?)
		ON CONFLICT(project_key, fingerprint) DO UPDATE SET
			count = count + 1, last_seen_at = excluded.last_seen_at`,
		projectKey, fp, truncate(message, 240), level, environment, now, now)
	if err != nil {
		return 0, fmt.Errorf("upsert issue: %w", err)
	}
	issueID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	payload := map[string]any{"channel": channel, "context": context}
	if err := s.insertOccurrence(issueID, "log", payload, now); err != nil {
		return issueID, err
	}
	return issueID, nil
}

func (s *Store) insertOccurrence(issueID int64, kind string, payload any, now int64) error {
	var raw string
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			raw = `{"error": "payload not serializable"}`
		} else {
			raw = string(b)
		}
	}
	_, err := s.db.Exec(`INSERT INTO occurrences (issue_id, type, payload, seen_at) VALUES (?, ?, ?, ?)`,
		issueID, kind, raw, now)
	if err != nil {
		return fmt.Errorf("insert occurrence: %w", err)
	}
	return nil
}

func (s *Store) Issues(projectKey string, limit int) ([]Issue, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, project_key, fingerprint, type, title, exception_class, level, environment, count, first_seen_at, last_seen_at, resolved
		FROM issues WHERE (? = '' OR project_key = ?)
		ORDER BY resolved ASC, last_seen_at DESC LIMIT ?`, projectKey, projectKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Issue
	for rows.Next() {
		var i Issue
		var first, last int64
		var exc, level, env sql.NullString
		if err := rows.Scan(&i.ID, &i.ProjectKey, &i.Fingerprint, &i.Type, &i.Title, &exc, &level, &env, &i.Count, &first, &last, &i.Resolved); err != nil {
			return nil, err
		}
		i.ExceptionClass, i.Level, i.Environment = exc.String, level.String, env.String
		i.FirstSeenAt, i.LastSeenAt = time.Unix(first, 0), time.Unix(last, 0)
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Store) Issue(id int64) (*Issue, error) {
	var i Issue
	var first, last int64
	var exc, level, env sql.NullString
	err := s.db.QueryRow(`SELECT id, project_key, fingerprint, type, title, exception_class, level, environment, count, first_seen_at, last_seen_at, resolved
		FROM issues WHERE id = ?`, id).
		Scan(&i.ID, &i.ProjectKey, &i.Fingerprint, &i.Type, &i.Title, &exc, &level, &env, &i.Count, &first, &last, &i.Resolved)
	if err != nil {
		return nil, err
	}
	i.ExceptionClass, i.Level, i.Environment = exc.String, level.String, env.String
	i.FirstSeenAt, i.LastSeenAt = time.Unix(first, 0), time.Unix(last, 0)
	return &i, nil
}

func (s *Store) Occurrences(issueID int64, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT payload, seen_at FROM occurrences WHERE issue_id = ? ORDER BY seen_at DESC, id DESC LIMIT ?`, issueID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw sql.NullString
		var seen int64
		if err := rows.Scan(&raw, &seen); err != nil {
			return nil, err
		}
		entry := map[string]any{"seen_at": time.Unix(seen, 0).UTC().Format(time.RFC3339)}
		if raw.Valid && raw.String != "" {
			var payload any
			if json.Unmarshal([]byte(raw.String), &payload) == nil {
				entry["payload"] = payload
			}
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

func (s *Store) SetResolved(id int64, resolved bool) error {
	_, err := s.db.Exec(`UPDATE issues SET resolved = ? WHERE id = ?`, resolved, id)
	return err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}