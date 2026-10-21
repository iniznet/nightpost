package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// HashIngestKey renders the at-rest form of an ingest secret. Only the
// SHA-256 is stored (keys are 192-bit random, so unsalted hashing is the
// right trade): a leaked database cannot mint signed ingest requests.
func HashIngestKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Project carries the ingest-key state for one producer project. Projects
// are configuration, not events: the retention loop must never prune this
// table .
type Project struct {
	Key       string
	IngestKey string
	CreatedAt time.Time
	RotatedAt time.Time
	// CORSOrigin is the project's browser-origin allowlist (comma-separated).
	// Empty means "fall back to the install's -cors-origin default".
	CORSOrigin string
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

// ProjectIngestKey returns the stored key HASH for a project; "" means the
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
	rows, err := s.rdb.Query(`SELECT key, ingest_key, created_at, rotated_at, cors_origin FROM projects ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		var created, rotated int64
		if err := rows.Scan(&p.Key, &p.IngestKey, &created, &rotated, &p.CORSOrigin); err != nil {
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
	if _, err := s.wdb.Exec(`UPDATE projects SET ingest_key = ?, rotated_at = CAST(strftime('%s','now') AS INTEGER) WHERE key = ?`, HashIngestKey(secret), key); err != nil {
		return "", err
	}
	return secret, nil
}

// RevokeIngestKey drops a project's ingest key, returning it to unsigned
// (legacy) admission — the escape hatch when a producer cannot present the
// key (rotated too early, lost secret, un-upgradable client). Callers must
// invalidate any cached gate decision, exactly like rotation.
func (s *Store) RevokeIngestKey(key string) error {
	if _, err := s.wdb.Exec(`UPDATE projects SET ingest_key = '', rotated_at = 0 WHERE key = ?`, key); err != nil {
		return fmt.Errorf("revoke ingest key: %w", err)
	}
	return nil
}

// ProjectExists reports whether a project row exists. The
// registration-required mode asks before it would auto-provision a name, so a
// caller-chosen project cannot be created by an ingest POST.
func (s *Store) ProjectExists(key string) (bool, error) {
	var n int
	err := s.rdb.QueryRow(`SELECT COUNT(*) FROM projects WHERE key = ?`, key).Scan(&n)
	return n > 0, err
}

// ProjectCORSOrigin returns the project's browser-origin allowlist; "" means
// the install default applies and an unknown project is reported as "" too.
func (s *Store) ProjectCORSOrigin(key string) (string, error) {
	var origin string
	err := s.rdb.QueryRow(`SELECT cors_origin FROM projects WHERE key = ?`, key).Scan(&origin)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return origin, err
}

// SetProjectCORSOrigin stores the normalized allowlist for a project. The
// caller validates first (NormalizeCORSOrigins); an empty value clears it back
// to the install default.
func (s *Store) SetProjectCORSOrigin(key, origin string) error {
	if err := s.EnsureProject(key); err != nil {
		return err
	}
	if _, err := s.wdb.Exec(`UPDATE projects SET cors_origin = ? WHERE key = ?`, strings.TrimSpace(origin), key); err != nil {
		return fmt.Errorf("set project cors origin: %w", err)
	}
	return nil
}

// UnsignedProjectCount reports projects still accepting legacy ingest;
// boot uses it for the deprecation warning .
func (s *Store) UnsignedProjectCount() (int, error) {
	var n int
	err := s.rdb.QueryRow(`SELECT COUNT(*) FROM projects WHERE ingest_key = ''`).Scan(&n)
	return n, err
}

// NormalizeCORSOrigins validates and normalizes a comma-separated
// browser-origin allowlist for one project.
//
// Each entry must be an explicit scheme://host[:port] with no path, query or
// fragment — a wildcard is refused outright, because these routes accept
// UNAUTHENTICATED browser posts and "*" would let any page on the internet
// write into the project's telemetry. An empty return means "inherit the
// install default".
func NormalizeCORSOrigins(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		if entry == "*" {
			return "", fmt.Errorf("wildcard origin is not allowed; name each origin explicitly")
		}
		u, err := url.Parse(entry)
		if err != nil {
			return "", fmt.Errorf("origin %q: %w", entry, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return "", fmt.Errorf("origin %q: scheme must be http or https", entry)
		}
		if u.Host == "" {
			return "", fmt.Errorf("origin %q: missing host", entry)
		}
		if u.Path != "" && u.Path != "/" {
			return "", fmt.Errorf("origin %q: must not contain a path", entry)
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return "", fmt.Errorf("origin %q: must not contain a query or fragment", entry)
		}
		out = append(out, u.Scheme+"://"+u.Host)
	}
	return strings.Join(out, ","), nil
}

func newIngestSecret() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("ingest key entropy: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
