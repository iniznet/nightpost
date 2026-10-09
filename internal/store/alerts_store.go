package store

import (
	"fmt"
	"strings"
)

// Alert support queries (#157): the alerts_log is the cooldown/cap ledger;
// the evaluation queries are read-only aggregates except Regressions,
// which reopens matched issues in the same statement window.

type IssueEvent struct {
	ID          int64
	ProjectKey  string
	Fingerprint string
	Title       string
	FirstSeenAt int64
}

type CountBreach struct {
	ProjectKey  string
	Fingerprint string
	Title       string
	N           int64
}

func projectClause(projects []string) (string, []any) {
	if len(projects) == 0 {
		return "", nil
	}
	ph := strings.Join(strings.Split(strings.Repeat("?", len(projects)), ""), ",")
	args := make([]any, len(projects))
	for i, p := range projects {
		args[i] = p
	}
	return " AND project_key IN (" + ph + ")", args
}

// RecordAlert appends one successful send to the cooldown ledger.
func (s *Store) RecordAlert(channel, rule, targetKey string, sentAt int64) error {
	_, err := s.wdb.Exec(`INSERT INTO alerts_log (channel, rule, target_key, sent_at) VALUES (?, ?, ?, ?)`,
		channel, rule, targetKey, sentAt)
	return err
}

// LastAlertSent returns the newest send for (rule, target), 0 if never.
func (s *Store) LastAlertSent(rule, targetKey string) (int64, error) {
	var n int64
	err := s.rdb.QueryRow(`SELECT COALESCE(MAX(sent_at), 0) FROM alerts_log WHERE rule = ? AND target_key = ?`,
		rule, targetKey).Scan(&n)
	return n, err
}

// ChannelAlertCount counts sends on one channel since the cutoff.
func (s *Store) ChannelAlertCount(channel string, since int64) (int64, error) {
	var n int64
	err := s.rdb.QueryRow(`SELECT COUNT(*) FROM alerts_log WHERE channel = ? AND sent_at >= ?`, channel, since).Scan(&n)
	return n, err
}

// PruneAlertsBefore chunk-deletes the alert ledger like occurrences.
func (s *Store) PruneAlertsBefore(cutoff int64, chunk int) (int64, error) {
	if chunk <= 0 {
		chunk = 500
	}
	var total int64
	for {
		res, err := s.wdb.Exec(`DELETE FROM alerts_log WHERE id IN
			(SELECT id FROM alerts_log WHERE sent_at < ? LIMIT ?)`, cutoff, chunk)
		if err != nil {
			return total, fmt.Errorf("prune alerts: %w", err)
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

// NewIssueEvents lists error issues first seen after the cutoff.
func (s *Store) NewIssueEvents(since int64, projects []string) ([]IssueEvent, error) {
	clause, args := projectClause(projects)
	q := append([]any{since}, args...)
	rows, err := s.rdb.Query(`SELECT id, project_key, fingerprint, title, first_seen_at FROM issues
		WHERE type = 'error' AND resolved = 0 AND first_seen_at >= ?`+clause+`
		ORDER BY first_seen_at DESC LIMIT 10`, q...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanIssueEvents(rows)
}

// Regressions finds resolved issues whose last_seen_at moved past their
// resolution stamp and reopens them atomically, so each regression alerts
// exactly once (the resolved=1 predicate is consumed by this pass).
func (s *Store) Regressions(projects []string) ([]IssueEvent, error) {
	clause, args := projectClause(projects)
	q := append([]any{}, args...)
	rows, err := s.rdb.Query(`SELECT id, project_key, fingerprint, title, last_seen_at FROM issues
		WHERE resolved = 1 AND resolved_at > 0 AND last_seen_at > resolved_at`+clause+`
		ORDER BY last_seen_at DESC LIMIT 10`, q...)
	if err != nil {
		return nil, err
	}
	events, err := scanIssueEvents(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, ev := range events {
		if _, err := s.wdb.Exec(`UPDATE issues SET resolved = 0, resolved_at = 0 WHERE id = ? AND resolved = 1`, ev.ID); err != nil {
			return events, err
		}
	}
	return events, nil
}

func scanIssueEvents(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]IssueEvent, error) {
	var out []IssueEvent
	for rows.Next() {
		var ev IssueEvent
		if err := rows.Scan(&ev.ID, &ev.ProjectKey, &ev.Fingerprint, &ev.Title, &ev.FirstSeenAt); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// CountBreaches finds issues with at least threshold occurrences inside
// the window, walking the (issue_id, seen_at) index.
func (s *Store) CountBreaches(since, threshold int64, projects []string) ([]CountBreach, error) {
	clause, args := projectClause(projects)
	q := append([]any{since}, args...)
	q = append(q, threshold)
	rows, err := s.rdb.Query(`SELECT i.project_key, i.fingerprint, i.title, COUNT(*) n
		FROM occurrences o JOIN issues i ON i.id = o.issue_id
		WHERE o.seen_at >= ? AND i.resolved = 0`+clause+`
		GROUP BY i.id, i.project_key, i.fingerprint, i.title
		HAVING n >= ? ORDER BY n DESC LIMIT 10`, q...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CountBreach
	for rows.Next() {
		var cb CountBreach
		if err := rows.Scan(&cb.ProjectKey, &cb.Fingerprint, &cb.Title, &cb.N); err != nil {
			return nil, err
		}
		out = append(out, cb)
	}
	return out, rows.Err()
}
