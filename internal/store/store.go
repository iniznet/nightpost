package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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
	Channel        string
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
	channel TEXT NOT NULL DEFAULT '',
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
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrate applies additive migrations idempotently: pre-channel databases
// (volumes created before M3) get the issues.channel column and the stats
// indexes without touching any existing row.
func migrate(db *sql.DB) error {
	for _, m := range []struct{ name, stmt string }{
		{"issues.channel", `ALTER TABLE issues ADD COLUMN channel TEXT NOT NULL DEFAULT ''`},
		{"idx_issues_type", `CREATE INDEX IF NOT EXISTS idx_issues_type ON issues(type, last_seen_at DESC)`},
		{"idx_occurrences_seen", `CREATE INDEX IF NOT EXISTS idx_occurrences_seen ON occurrences(seen_at)`},
	} {
		if _, err := db.Exec(m.stmt); err != nil {
			// Idempotent: only "already applied" failures may pass through.
			if strings.Contains(err.Error(), "duplicate column") || strings.Contains(err.Error(), "already exists") {
				continue
			}
			return fmt.Errorf("migrate %s: %w", m.name, err)
		}
	}
	return nil
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

// RecordLog stores one Monolog record occurrence. The channel is lifted into
// its own column so the logs view can filter without touching payload JSON.
func (s *Store) RecordLog(projectKey, channel, level, message string, context map[string]any, environment string) (int64, error) {
	fp := ForLog(channel, level, message)
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO issues
		(project_key, fingerprint, type, title, exception_class, level, environment, channel, count, first_seen_at, last_seen_at)
		VALUES (?, ?, 'log', ?, NULL, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(project_key, fingerprint) DO UPDATE SET
			count = count + 1, last_seen_at = excluded.last_seen_at`,
		projectKey, fp, truncate(message, 240), level, environment, channel, now, now)
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

// IssueQuery selects issues for the UI: every zero-value field is a no-op
// filter, Status is "open", "resolved" or "all" (empty = all), BeforeID is
// the keyset cursor for "older than" pagination.
type IssueQuery struct {
	Project, Type, Level, Env, Channel, Status, Q string
	BeforeID                                      int64
	Limit                                         int
}

func (s *Store) Issues(q IssueQuery) ([]Issue, error) {
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id, project_key, fingerprint, type, title, exception_class, level, environment, channel, count, first_seen_at, last_seen_at, resolved
		FROM issues
		WHERE (? = '' OR project_key = ?)
		  AND (? = '' OR type = ?)
		  AND (? = '' OR level = ?)
		  AND (? = '' OR environment = ?)
		  AND (? = '' OR channel = ?)
		  AND (? = '' OR title LIKE '%' || ? || '%')
		  AND (CASE ? WHEN 'open' THEN resolved = 0 WHEN 'resolved' THEN resolved = 1 ELSE 1 END)
		  AND (? <= 0 OR id < ?)
		ORDER BY last_seen_at DESC, id DESC LIMIT ?`,
		q.Project, q.Project,
		q.Type, q.Type,
		q.Level, q.Level,
		q.Env, q.Env,
		q.Channel, q.Channel,
		q.Q, q.Q,
		q.Status,
		q.BeforeID, q.BeforeID,
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Issue
	for rows.Next() {
		var i Issue
		var first, last int64
		var exc, level, env, channel sql.NullString
		if err := rows.Scan(&i.ID, &i.ProjectKey, &i.Fingerprint, &i.Type, &i.Title, &exc, &level, &env, &channel, &i.Count, &first, &last, &i.Resolved); err != nil {
			return nil, err
		}
		i.ExceptionClass, i.Level, i.Environment, i.Channel = exc.String, level.String, env.String, channel.String
		i.FirstSeenAt, i.LastSeenAt = time.Unix(first, 0), time.Unix(last, 0)
		out = append(out, i)
	}
	return out, rows.Err()
}

// Projects lists every project key that has ever been seen on either table.
func (s *Store) Projects() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT project_key FROM (
		SELECT project_key FROM issues
		UNION ALL
		SELECT project_key FROM profiles
	) ORDER BY project_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Environments lists distinct non-empty environments for the filter dropdown.
func (s *Store) Environments(project string) ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT environment FROM issues
		WHERE environment IS NOT NULL AND environment <> '' AND (? = '' OR project_key = ?)
		ORDER BY environment LIMIT 20`, project, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) Issue(id int64) (*Issue, error) {
	var i Issue
	var first, last int64
	var exc, level, env, channel sql.NullString
	err := s.db.QueryRow(`SELECT id, project_key, fingerprint, type, title, exception_class, level, environment, channel, count, first_seen_at, last_seen_at, resolved
		FROM issues WHERE id = ?`, id).
		Scan(&i.ID, &i.ProjectKey, &i.Fingerprint, &i.Type, &i.Title, &exc, &level, &env, &channel, &i.Count, &first, &last, &i.Resolved)
	if err != nil {
		return nil, err
	}
	i.ExceptionClass, i.Level, i.Environment, i.Channel = exc.String, level.String, env.String, channel.String
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
