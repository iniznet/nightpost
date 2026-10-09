package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Release health (#158): deploy markers arrive via POST /ingest/{project}/release
// (CI webhook or boot publisher). The table is HARD-CAPPED at 100 rows per
// project on every insert — newest kept — so a crash-loop that re-POSTs its
// release can never grow the table.

type ReleaseMark struct {
	Version     string
	Environment string
	At          time.Time
}

const releasesKeep = 100

// RecordRelease stores one deploy marker and trims to the newest 100.
func (s *Store) RecordRelease(projectKey, version, environment string) error {
	now := time.Now().Unix()
	if _, err := s.wdb.Exec(`INSERT INTO releases (project_key, version, environment, released_at) VALUES (?, ?, ?, ?)`,
		projectKey, version, environment, now); err != nil {
		return fmt.Errorf("record release: %w", err)
	}
	if _, err := s.wdb.Exec(`DELETE FROM releases WHERE project_key = ? AND id NOT IN (
		SELECT id FROM releases WHERE project_key = ? ORDER BY released_at DESC, id DESC LIMIT ?)`,
		projectKey, projectKey, releasesKeep); err != nil {
		return fmt.Errorf("cap releases: %w", err)
	}
	return nil
}

// LatestRelease is the anchor for "new since deploy" badges. env "" = any.
func (s *Store) LatestRelease(projectKey, env string) (*ReleaseMark, error) {
	var r ReleaseMark
	var at int64
	err := s.rdb.QueryRow(`SELECT version, COALESCE(environment, ''), released_at FROM releases
		WHERE (? = '' OR project_key = ?) AND (? = '' OR environment = ?)
		ORDER BY released_at DESC, id DESC LIMIT 1`,
		projectKey, projectKey, env, env).Scan(&r.Version, &r.Environment, &at)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest release: %w", err)
	}
	r.At = time.Unix(at, 0)
	return &r, nil
}

// ReleaseHealth is the per-release sampled view: full-volume error
// occurrences against SAMPLED request profiles. The monitor cannot know the
// producer's sample rate, so the ratio is a conservative upper bound on the
// error rate — callers must label it as a sampled estimate.
type ReleaseHealth struct {
	ReleaseMark
	Errors24h      int64
	SampledReqs24h int64
	CrashFree      string // conservative upper-bound estimate; see file header
}

func (s *Store) ReleasesWithHealth(project string, limit int) ([]ReleaseHealth, error) {
	if limit <= 0 || limit > 50 {
		limit = 5
	}
	rows, err := s.rdb.Query(`SELECT version, COALESCE(environment, ''), released_at FROM releases
		WHERE (? = '' OR project_key = ?)
		ORDER BY released_at DESC, id DESC LIMIT ?`, project, project, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReleaseHealth
	for rows.Next() {
		var h ReleaseHealth
		var at int64
		if err := rows.Scan(&h.Version, &h.Environment, &at); err != nil {
			return nil, err
		}
		h.At = time.Unix(at, 0)
		since := at
		now := time.Now().Unix()
		if now-since > 86400 {
			since = now - 86400 // health window: since-deploy, max 24h
		}
		err := s.rdb.QueryRow(`SELECT COUNT(*) FROM occurrences o JOIN issues i ON i.id = o.issue_id
			WHERE i.project_key = ? AND i.type = 'error' AND (? = '' OR i.environment = ?) AND o.seen_at >= ?`,
			project, h.Environment, h.Environment, since).Scan(&h.Errors24h)
		if err != nil {
			return nil, fmt.Errorf("release error count: %w", err)
		}
		err = s.rdb.QueryRow(`SELECT COUNT(*) FROM profiles
			WHERE type = 'request' AND project_key = ? AND (? = '' OR environment = ?) AND created_at >= ?`,
			project, h.Environment, h.Environment, since).Scan(&h.SampledReqs24h)
		if err != nil {
			return nil, fmt.Errorf("release sampled requests: %w", err)
		}
		switch {
		case h.SampledReqs24h == 0 && h.Errors24h == 0:
			h.CrashFree = "100%"
		case h.SampledReqs24h == 0:
			h.CrashFree = "\u2014"
		default:
			rate := float64(h.Errors24h) / float64(h.SampledReqs24h)
			if rate > 1 {
				rate = 1
			}
			h.CrashFree = fmt.Sprintf("%.1f%%", (1-rate)*100)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
