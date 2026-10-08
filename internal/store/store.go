package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// Store keeps two pools on the same WAL database: one writer connection
// (SQLite allows exactly one concurrent writer) and a small reader pool
// (WAL readers never block the writer or each other). UI queries run on
// the reader pool so ingest writes can never starve a dashboard render.
type Store struct {
	wdb *sql.DB
	rdb *sql.DB

	limMu        sync.Mutex
	limWindows   map[string]*limWindow
	MaxOccPerMin int

	dropped atomic.Int64
}

type limWindow struct {
	minute   int64
	recorded int
}

// allowOccurrence admits at most MaxOccPerMin occurrence rows per fingerprint
// per minute; past the cap the occurrence insert is skipped while the issue
// upsert still runs, so issue counts stay accurate without row flooding.
func (s *Store) allowOccurrence(fingerprint string) bool {
	capPerMin := s.MaxOccPerMin
	if capPerMin <= 0 {
		return true
	}
	minute := time.Now().Unix() / 60
	s.limMu.Lock()
	defer s.limMu.Unlock()
	w := s.limWindows[fingerprint]
	if w == nil || w.minute != minute {
		w = &limWindow{minute: minute}
		s.limWindows[fingerprint] = w
	}
	if w.recorded >= capPerMin {
		return false
	}
	w.recorded++
	if len(s.limWindows) > 4096 {
		for k, lw := range s.limWindows {
			if lw.minute != minute {
				delete(s.limWindows, k)
			}
		}
	}
	return true
}

// DroppedOccurrences reports occurrence inserts skipped by the cap.
func (s *Store) DroppedOccurrences() int64 { return s.dropped.Load() }

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

const dsnPragmas = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=wal_autocheckpoint(1000)"

func Open(path string) (*Store, error) {
	wdb, err := sql.Open("sqlite", path+dsnPragmas)
	if err != nil {
		return nil, fmt.Errorf("open sqlite writer: %w", err)
	}
	// SQLite allows exactly one concurrent writer: one connection.
	wdb.SetMaxOpenConns(1)
	rdb, err := sql.Open("sqlite", path+dsnPragmas+"&_pragma=cache_size(-65536)")
	if err != nil {
		wdb.Close()
		return nil, fmt.Errorf("open sqlite reader: %w", err)
	}
	// WAL readers are safe concurrently; four connections cover the UI pages.
	rdb.SetMaxOpenConns(4)
	rdb.SetMaxIdleConns(4)
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
	if _, err := wdb.Exec(schema); err != nil {
		wdb.Close()
		rdb.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(wdb); err != nil {
		wdb.Close()
		rdb.Close()
		return nil, err
	}
	s := &Store{wdb: wdb, rdb: rdb, limWindows: map[string]*limWindow{}, MaxOccPerMin: 30}
	// Auto-provision the projects table from existing event rows so upgraded
	// volumes carry their producers as unsigned legacy projects (#157).
	if err := s.ProvisionProjects(); err != nil {
		s.Close()
		return nil, fmt.Errorf("provision projects: %w", err)
	}
	return s, nil
}

// migrate applies additive migrations idempotently: pre-channel databases
// (volumes created before M3) get the issues.channel column and the stats
// indexes without touching any existing row.
func migrate(db *sql.DB) error {
	for _, m := range []struct{ name, stmt string }{
		{"issues.channel", `ALTER TABLE issues ADD COLUMN channel TEXT NOT NULL DEFAULT ''`},
		{"idx_issues_type", `CREATE INDEX IF NOT EXISTS idx_issues_type ON issues(type, last_seen_at DESC)`},
		{"idx_occurrences_seen", `CREATE INDEX IF NOT EXISTS idx_occurrences_seen ON occurrences(seen_at)`},
		{"profiles.trace_id", `ALTER TABLE profiles ADD COLUMN trace_id TEXT NOT NULL DEFAULT ''`},
		{"idx_profiles_trace", `CREATE INDEX IF NOT EXISTS idx_profiles_trace ON profiles(trace_id, created_at DESC)`},
		{"profiles.tree", `ALTER TABLE profiles ADD COLUMN tree TEXT`},
		{"projects", `CREATE TABLE IF NOT EXISTS projects (
	key TEXT PRIMARY KEY,
	ingest_key TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL DEFAULT 0,
	rotated_at INTEGER NOT NULL DEFAULT 0)`},
		{"users", `CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY,
	email TEXT NOT NULL UNIQUE,
	pw_hash TEXT NOT NULL,
	created_at INTEGER NOT NULL DEFAULT 0,
	sessions_valid_after INTEGER NOT NULL DEFAULT 0)`},
		{"issues.resolved_at", `ALTER TABLE issues ADD COLUMN resolved_at INTEGER NOT NULL DEFAULT 0`},
		{"alerts_log", `CREATE TABLE IF NOT EXISTS alerts_log (
	id INTEGER PRIMARY KEY,
	channel TEXT NOT NULL,
	rule TEXT NOT NULL,
	target_key TEXT NOT NULL,
	sent_at INTEGER NOT NULL)`},
		{"idx_alerts_target", `CREATE INDEX IF NOT EXISTS idx_alerts_target ON alerts_log(rule, target_key, sent_at DESC)`},
		{"idx_alerts_channel", `CREATE INDEX IF NOT EXISTS idx_alerts_channel ON alerts_log(channel, sent_at DESC)`},
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

func (s *Store) Close() error {
	err := s.wdb.Close()
	if err2 := s.rdb.Close(); err == nil {
		err = err2
	}
	return err
}

// RecordError stores one exception occurrence, folding it into an existing
// issue with the same fingerprint (count+1, last_seen_at bumped) or creating
// a new issue row.
func (s *Store) RecordError(projectKey, exceptionClass, message, fingerprint, environment, level string, payload any) (int64, error) {
	now := time.Now().Unix()
	// RETURNING id, not LastInsertId: an upsert that took the conflict
	// branch performs no insert, so the connection-wide last-insert rowid
	// still points at the previous occurrence row from another table.
	var issueID int64
	err := s.wdb.QueryRow(`INSERT INTO issues
		(project_key, fingerprint, type, title, exception_class, level, environment, count, first_seen_at, last_seen_at)
		VALUES (?, ?, 'error', ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(project_key, fingerprint) DO UPDATE SET
			count = count + 1, last_seen_at = excluded.last_seen_at
		RETURNING id`,
		projectKey, fingerprint, truncate(message, 240), exceptionClass, level, environment, now, now).Scan(&issueID)
	if err != nil {
		return 0, fmt.Errorf("upsert issue: %w", err)
	}
	if !s.allowOccurrence(fingerprint) {
		s.dropped.Add(1)
		return issueID, nil
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
	var issueID int64
	err := s.wdb.QueryRow(`INSERT INTO issues
		(project_key, fingerprint, type, title, exception_class, level, environment, channel, count, first_seen_at, last_seen_at)
		VALUES (?, ?, 'log', ?, NULL, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(project_key, fingerprint) DO UPDATE SET
			count = count + 1, last_seen_at = excluded.last_seen_at
		RETURNING id`,
		projectKey, fp, truncate(message, 240), level, environment, channel, now, now).Scan(&issueID)
	if err != nil {
		return 0, fmt.Errorf("upsert issue: %w", err)
	}
	payload := map[string]any{"channel": channel, "context": context}
	if !s.allowOccurrence(fp) {
		s.dropped.Add(1)
		return issueID, nil
	}
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
	_, err := s.wdb.Exec(`INSERT INTO occurrences (issue_id, type, payload, seen_at) VALUES (?, ?, ?, ?)`,
		issueID, kind, truncate(raw, 32<<10), now)
	if err != nil {
		return fmt.Errorf("insert occurrence: %w", err)
	}
	return nil
}

// IssueQuery selects issues for the UI: every zero-value field is a no-op
// filter, Status is "open", "resolved" or "all" (empty = all), Sort is
// "recent" (default, last_seen_at DESC) or "pain" (count DESC — recurring
// noise ranks above one-off noise), and BeforeID/BeforeCount form the
// matching keyset cursor for "older than" pagination.
type IssueQuery struct {
	Project, Type, Level, Env, Channel, Status, Q string
	Sort                                          string
	BeforeID                                      int64
	BeforeCount                                   int64
	Limit                                         int
}

func (s *Store) Issues(q IssueQuery) ([]Issue, error) {
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	cursor := `AND (? <= 0 OR id < ?)`
	order := `ORDER BY last_seen_at DESC, id DESC`
	cursorArgs := []any{q.BeforeID, q.BeforeID}
	if q.Sort == "pain" {
		cursor = `AND (? <= 0 OR (count < ? OR (count = ? AND id < ?)))`
		order = `ORDER BY count DESC, id DESC`
		cursorArgs = []any{q.BeforeCount, q.BeforeCount, q.BeforeCount, q.BeforeID}
	}
	args := []any{
		q.Project, q.Project,
		q.Type, q.Type,
		q.Level, q.Level,
		q.Env, q.Env,
		q.Channel, q.Channel,
		q.Q, q.Q,
		q.Status,
	}
	args = append(args, cursorArgs...)
	args = append(args, limit)
	rows, err := s.rdb.Query(`SELECT id, project_key, fingerprint, type, title, exception_class, level, environment, channel, count, first_seen_at, last_seen_at, resolved
		FROM issues
		WHERE (? = '' OR project_key = ?)
		  AND (? = '' OR type = ?)
		  AND (? = '' OR level = ?)
		  AND (? = '' OR environment = ?)
		  AND (? = '' OR channel = ?)
		  AND (? = '' OR title LIKE '%' || ? || '%')
		  AND (CASE ? WHEN 'open' THEN resolved = 0 WHEN 'resolved' THEN resolved = 1 ELSE 1 END)
	`+cursor+`
		`+order+` LIMIT ?`,
		args...)
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
	rows, err := s.rdb.Query(`SELECT DISTINCT project_key FROM (
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
	rows, err := s.rdb.Query(`SELECT DISTINCT environment FROM issues
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
	err := s.rdb.QueryRow(`SELECT id, project_key, fingerprint, type, title, exception_class, level, environment, channel, count, first_seen_at, last_seen_at, resolved
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
	rows, err := s.rdb.Query(`SELECT payload, seen_at FROM occurrences WHERE issue_id = ? ORDER BY seen_at DESC, id DESC LIMIT ?`, issueID, limit)
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

// SetResolvedAt resolves an issue with an explicit resolution stamp —
// the regression rule compares last_seen_at > resolved_at, and tooling
// (plus tests) needs deterministic control over that boundary.
func (s *Store) SetResolvedAt(id int64, at int64) error {
	_, err := s.wdb.Exec(`UPDATE issues SET resolved = 1, resolved_at = ? WHERE id = ?`, at, id)
	return err
}

func (s *Store) SetResolved(id int64, resolved bool) error {
	stamp := 0
	if resolved {
		stamp = 1
	}
	_, err := s.wdb.Exec(`UPDATE issues SET resolved = ?, resolved_at = CASE WHEN ? = 1 THEN CAST(strftime('%s','now') AS INTEGER) ELSE 0 END WHERE id = ?`, resolved, stamp, id)
	return err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// StoreStats is the dashboard health strip for the store itself.
type StoreStats struct {
	Issues             int64
	Occurrences        int64
	Profiles           int64
	DBBytes            int64
	DroppedOccurrences int64
}

// Stats reads table counts and the physical DB size. Counts run on the
// reader pool; a growing occurrences COUNT is itself the signal to surface.
func (s *Store) Stats() (StoreStats, error) {
	var out StoreStats
	out.DroppedOccurrences = s.dropped.Load()
	if err := s.rdb.QueryRow(`SELECT COUNT(*) FROM issues`).Scan(&out.Issues); err != nil {
		return out, err
	}
	if err := s.rdb.QueryRow(`SELECT COUNT(*) FROM occurrences`).Scan(&out.Occurrences); err != nil {
		return out, err
	}
	if err := s.rdb.QueryRow(`SELECT COUNT(*) FROM profiles`).Scan(&out.Profiles); err != nil {
		return out, err
	}
	var pages, pageSize int64
	if err := s.rdb.QueryRow(`PRAGMA page_count`).Scan(&pages); err == nil {
		if err2 := s.rdb.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err2 == nil {
			out.DBBytes = pages * pageSize
		}
	}
	return out, nil
}
