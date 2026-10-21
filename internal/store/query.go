package store

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Query telemetry : slow queries aggregate per normalized SQL shape
// so parameterized variants share one row. Each event also upserts a
// type='query' issue, keeping the defect list the single history surface.

type QueryAgg struct {
	Fingerprint string
	SQL         string
	Connection  string
	Count       int64
	TotalMs     float64
	MaxMs       float64
	LastSeen    time.Time
}

// sqlNumRe blanks STANDALONE numeric literals only. The shared numRe
// ([0-9]+) would glue year-suffixed table names — orders_2024 and
// orders_2025 are distinct statements, while id = 5 and id = 6 must
// collapse — so SQL needs the boundary-aware variant. ( is RE2-supported;
// no PREG byte-offset mixing here: ReplaceAllString is character-safe.)
var sqlNumRe = regexp.MustCompile(`\b\d+\b`)

// normalizeSQL is the single grouping authority for query telemetry :
// the backend ships placeholder SQL verbatim, every other producer's raw
// SQL lands shaped the same way.
func normalizeSQL(s string) string {
	s = uuidRe.ReplaceAllString(s, "<uuid>")
	s = hexRe.ReplaceAllString(s, "<hex>")
	s = strRe.ReplaceAllString(s, "<s>")
	s = sqlNumRe.ReplaceAllString(s, "<n>")
	return strings.TrimSpace(s)
}

// ForQuery groups slow queries by statement shape. New key in the same
// unreleased PR as query_stats — no re-grouping of stored rows is possible
// (the fingerprint freeze applies to ForException/ForLog only).
func ForQuery(sql string) string {
	return hash("query|" + normalizeSQL(sql))
}

// RecordQuery upserts the aggregate row and the paired issue. The issues
// upsert mirrors RecordError's RETURNING pattern.
func (s *Store) RecordQuery(projectKey, sql string, timeMs float64, connection, environment string) (int64, error) {
	now := time.Now().Unix()
	norm := normalizeSQL(sql)
	if len(norm) > 4000 {
		norm = norm[:4000]
	}
	title := norm
	if len(title) > 200 {
		title = title[:200]
	}
	fp := ForQuery(sql)
	var issueID int64
	err := s.wdb.QueryRow(`INSERT INTO issues
		(project_key, fingerprint, type, title, exception_class, level, environment, count, first_seen_at, last_seen_at)
		VALUES (?, ?, 'query', ?, '', 'warning', ?, 1, ?, ?)
		ON CONFLICT(project_key, fingerprint) DO UPDATE SET
			count = count + 1, last_seen_at = excluded.last_seen_at
		RETURNING id`,
		projectKey, fp, title, environment, now, now).Scan(&issueID)
	if err != nil {
		return 0, fmt.Errorf("query issue upsert: %w", err)
	}
	if _, err := s.wdb.Exec(`INSERT INTO query_stats
		(project_key, fingerprint, sql, connection, count, total_ms, max_ms, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?)
		ON CONFLICT(project_key, fingerprint) DO UPDATE SET
			count = count + 1,
			total_ms = total_ms + excluded.total_ms,
			max_ms = CASE WHEN excluded.max_ms > max_ms THEN excluded.max_ms ELSE max_ms END,
			last_seen_at = excluded.last_seen_at`,
		projectKey, fp, norm, connection, timeMs, timeMs, now, now); err != nil {
		return issueID, fmt.Errorf("query stats upsert: %w", err)
	}
	return issueID, nil
}

// QueryLeaderboard ranks statement shapes by total time over the window.
func (s *Store) QueryLeaderboard(project string, hours, limit int) ([]QueryAgg, error) {
	if hours <= 0 {
		hours = 24
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	rows, err := s.rdb.Query(`SELECT fingerprint, sql, connection, count, total_ms, max_ms, last_seen_at
		FROM query_stats
		WHERE (? = '' OR project_key = ?) AND last_seen_at >= ?
		ORDER BY total_ms DESC LIMIT ?`, project, project, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueryAgg
	for rows.Next() {
		var q QueryAgg
		var last int64
		if err := rows.Scan(&q.Fingerprint, &q.SQL, &q.Connection, &q.Count, &q.TotalMs, &q.MaxMs, &last); err != nil {
			return nil, err
		}
		q.LastSeen = time.Unix(last, 0)
		out = append(out, q)
	}
	return out, rows.Err()
}

// PruneQueryStats drops aggregates silent past the retention window; the
// table is bounded by distinct statement shapes, so one chunked DELETE on
// the stale tail is enough.
func (s *Store) PruneQueryStats(cutoff int64, chunk int) (int64, error) {
	if chunk <= 0 {
		chunk = 500
	}
	var total int64
	for {
		res, err := s.wdb.Exec(`DELETE FROM query_stats WHERE project_key IN (
			SELECT project_key FROM query_stats WHERE last_seen_at < ?
			GROUP BY project_key HAVING COUNT(*) > 0 LIMIT ?) AND last_seen_at < ?`,
			cutoff, chunk, cutoff)
		if err != nil {
			return total, fmt.Errorf("prune query stats: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(chunk) {
			return total, nil
		}
	}
}
