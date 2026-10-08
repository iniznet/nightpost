package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

// Project carries the ingest-key state for one producer project. Projects
// are configuration, not events: the retention loop must never prune this
// table (#157).
type Project struct {
	Key       string
	IngestKey string
	CreatedAt time.Time
	RotatedAt time.Time
}

// HasKey reports whether the project requires a signed ingest request.
func (p Project) HasKey() bool { return p.IngestKey != "" }

// ProvisionProjects copies every DISTINCT project_key already present in
// the event tables into the projects table (idempotent), so upgraded
// volumes keep their producers as unsigned legacy projects until a key is
// issued.
func (s *Store) ProvisionProjects() error {
	_, err := s.wdb.Exec(`INSERT OR IGNORE INTO projects (key, ingest_key, created_at)
		SELECT project_key, '', CAST(strftime('%s','now') AS INTEGER) FROM (
			SELECT DISTINCT project_key FROM issues
			UNION SELECT DISTINCT project_key FROM profiles)`)
	return err
}

// EnsureProject admits an unknown producer key as an unsigned legacy
// project; called once per project by the ingest gate, never per event.
func (s *Store) EnsureProject(key string) error {
	_, err := s.wdb.Exec(`INSERT OR IGNORE INTO projects (key, ingest_key, created_at)
		VALUES (?, '', CAST(strftime('%s','now') AS INTEGER))`, key)
	return err
}

// ProjectIngestKey returns the stored key for a project; "" means the
// project accepts unsigned legacy ingest. Unknown projects report "" too —
// they are provisioned by the gate on first sight.
func (s *Store) ProjectIngestKey(key string) (string, error) {
	var k string
	err := s.rdb.QueryRow(`SELECT ingest_key FROM projects WHERE key = ?`, key).Scan(&k)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return k, err
}

// ListProjects returns every project ordered by key.
func (s *Store) ListProjects() ([]Project, error) {
	rows, err := s.rdb.Query(`SELECT key, ingest_key, created_at, rotated_at FROM projects ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		var created, rotated int64
		if err := rows.Scan(&p.Key, &p.IngestKey, &created, &rotated); err != nil {
			return nil, err
		}
		if created > 0 {
			p.CreatedAt = time.Unix(created, 0)
		}
		if rotated > 0 {
			p.RotatedAt = time.Unix(rotated, 0)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RotateIngestKey replaces (or first issues) the project's ingest key and
// returns the new secret. Callers must invalidate any cached decision.
func (s *Store) RotateIngestKey(key string) (string, error) {
	secret, err := newIngestSecret()
	if err != nil {
		return "", err
	}
	if err := s.EnsureProject(key); err != nil {
		return "", err
	}
	if _, err := s.wdb.Exec(`UPDATE projects SET ingest_key = ?, rotated_at = CAST(strftime('%s','now') AS INTEGER) WHERE key = ?`, secret, key); err != nil {
		return "", err
	}
	return secret, nil
}

// UnsignedProjectCount reports projects still accepting legacy ingest;
// boot uses it for the deprecation warning (#157).
func (s *Store) UnsignedProjectCount() (int, error) {
	var n int
	err := s.rdb.QueryRow(`SELECT COUNT(*) FROM projects WHERE ingest_key = ''`).Scan(&n)
	return n, err
}

func newIngestSecret() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("ingest key entropy: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
