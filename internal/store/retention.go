package store

import (
	"fmt"
	"time"
)

// Bounded retention: the single-writer SQLite connection is shared with
// ingest, so every prune runs in chunked DELETEs (never one giant statement)
// and only from the background ticker — never from a request path.

// PruneOccurrencesBefore deletes occurrences older than cutoff in bounded
// chunks and returns the total removed.
func (s *Store) PruneOccurrencesBefore(cutoff time.Time, chunk int) (int64, error) {
	if chunk <= 0 {
		chunk = 500
	}
	var total int64
	for {
		res, err := s.wdb.Exec(`DELETE FROM occurrences WHERE id IN
			(SELECT id FROM occurrences WHERE seen_at < ? LIMIT ?)`, cutoff.Unix(), chunk)
		if err != nil {
			return total, fmt.Errorf("prune occurrences: %w", err)
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

// PruneIssueOccurrences caps each issue's stored occurrences at the newest
// `keep` rows, so a high-frequency fingerprint cannot flood the table.
func (s *Store) PruneIssueOccurrences(keep int) (int64, error) {
	if keep <= 0 {
		keep = 50
	}
	ids, err := s.rdb.Query(`SELECT id FROM issues`)
	if err != nil {
		return 0, err
	}
	var issueIDs []int64
	for ids.Next() {
		var id int64
		if err := ids.Scan(&id); err != nil {
			ids.Close()
			return 0, err
		}
		issueIDs = append(issueIDs, id)
	}
	if err := ids.Err(); err != nil {
		ids.Close()
		return 0, err
	}
	ids.Close()

	var total int64
	for _, id := range issueIDs {
		res, err := s.wdb.Exec(`DELETE FROM occurrences WHERE issue_id = ? AND id NOT IN
			(SELECT id FROM occurrences WHERE issue_id = ? ORDER BY seen_at DESC, id DESC LIMIT ?)`,
			id, id, keep)
		if err != nil {
			return total, fmt.Errorf("prune issue occurrences (issue %d): %w", id, err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// RunRetention executes one full retention cycle: age cutoff, per-issue cap
// and the profile keep-window. Issues are never deleted (the defect list is
// the history); only raw occurrences and profiles are bounded.
func (s *Store) RunRetention(days int, keepProfiles int) (string, error) {
	if days <= 0 {
		days = 30
	}
	if keepProfiles <= 0 {
		keepProfiles = 5000
	}
	n1, err := s.PruneOccurrencesBefore(time.Now().AddDate(0, 0, -days), 500)
	if err != nil {
		return "", err
	}
	n2, err := s.PruneIssueOccurrences(50)
	if err != nil {
		return "", err
	}
	if err := s.PruneProfiles(int64(keepProfiles)); err != nil {
		return "", err
	}
	n3, err := s.PruneAlertsBefore(time.Now().AddDate(0, 0, -days).Unix(), 500)
	if err != nil {
		return "", err
	}
	if rows, err := s.wdb.Query(`PRAGMA wal_checkpoint(TRUNCATE)`); err == nil {
		rows.Close()
	}
	return fmt.Sprintf("retention: pruned %d old + %d over-cap occurrences + %d alerts, profiles kept <= %d", n1, n2, n3, keepProfiles), nil
}
