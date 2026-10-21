package store

import (
	"database/sql"
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

// pruneChunk is how many issues one set-based prune statement covers. Large
// enough that the statement count is trivial, small enough that a statement's
// row set stays bounded on a single-writer SQLite connection.
const pruneChunk = 500

// PruneIssueOccurrences caps each issue's stored occurrences at the newest
// `keep` rows, so a high-frequency fingerprint cannot flood the table.
func (s *Store) PruneIssueOccurrences(keep int) (int64, error) {
	return s.pruneIssueOccurrences(keep, pruneChunk)
}

// pruneIssueOccurrences does the work in ONE set-based statement per issue-id
// RANGE.
//
// The previous form read EVERY issue id into memory and issued one DELETE per
// issue: O(issues) statements per tick against a single WAL writer, with an
// unbounded id slice to match. That is exactly the wedge class the retention
// loop must never enter (— the max_statement_time / 70100 pattern the
// backend rails against). Ranging keeps both bounded: the statement count is
// issues/chunk, and each statement's row set is one chunk of issues.
//
// Ranging (rather than LIMIT/OFFSET over occurrences) is deliberate: the cap is
// PER ISSUE, so a global row limit would starve later issues while over-pruning
// earlier ones.
func (s *Store) pruneIssueOccurrences(keep, chunk int) (int64, error) {
	if keep <= 0 {
		keep = 50
	}
	if chunk <= 0 {
		chunk = pruneChunk
	}

	var total int64
	lo := int64(0)
	for {
		// The last issue id of this range: the chunk-th id after lo, or the
		// table's maximum when the tail is shorter than one chunk.
		var hi sql.NullInt64
		err := s.rdb.QueryRow(`SELECT id FROM issues WHERE id > ? ORDER BY id LIMIT 1 OFFSET ?`,
			lo, chunk-1).Scan(&hi)
		if err == sql.ErrNoRows {
			if err := s.rdb.QueryRow(`SELECT MAX(id) FROM issues WHERE id > ?`, lo).Scan(&hi); err != nil {
				return total, err
			}
		} else if err != nil {
			return total, err
		}
		if !hi.Valid {
			return total, nil
		}

		res, err := s.wdb.Exec(`DELETE FROM occurrences
			WHERE issue_id BETWEEN ? AND ? AND id NOT IN (
				SELECT id FROM (
					SELECT id, ROW_NUMBER() OVER (
						PARTITION BY issue_id ORDER BY seen_at DESC, id DESC) AS rn
					FROM occurrences WHERE issue_id BETWEEN ? AND ?
				) WHERE rn <= ?)`,
			lo, hi.Int64, lo, hi.Int64, keep)
		if err != nil {
			return total, fmt.Errorf("prune issue occurrences (issues %d..%d): %w", lo, hi.Int64, err)
		}
		if n, aerr := res.RowsAffected(); aerr == nil {
			total += n
		}
		lo = hi.Int64
	}
}

// ArchiveStaleIssues marks issues as archived once retention has removed
// everything it will: not seen since the cutoff AND with no occurrence left
// inside the window. Archived rows leave the dashboard's live lists and counts
// but are KEPT, because count and first_seen_at are history; a recurrence
// UN-archives the row (both recorders clear archived_at on conflict).
//
// This bounds the `issues` table, which is otherwise never pruned — it IS the
// defect list — while every distinct log message adds a row to it. Chunked by
// issue-id range for the same reason the occurrence prune is: bounded statements
// on a single WAL writer.
func (s *Store) ArchiveStaleIssues(days int) (int64, error) {
	if days <= 0 {
		days = 30
	}
	now := time.Now().Unix()
	cutoff := time.Now().AddDate(0, 0, -days).Unix()

	var total int64
	lo := int64(0)
	for {
		var hi sql.NullInt64
		err := s.rdb.QueryRow(`SELECT id FROM issues WHERE id > ? ORDER BY id LIMIT 1 OFFSET ?`,
			lo, pruneChunk-1).Scan(&hi)
		if err == sql.ErrNoRows {
			if err := s.rdb.QueryRow(`SELECT MAX(id) FROM issues WHERE id > ?`, lo).Scan(&hi); err != nil {
				return total, err
			}
		} else if err != nil {
			return total, err
		}
		if !hi.Valid {
			return total, nil
		}
		// NOT EXISTS runs index-only through idx_occurrences_issue(issue_id,
		// seen_at DESC), which carries seen_at itself.
		res, err := s.wdb.Exec(`UPDATE issues SET archived_at = ?
			WHERE id BETWEEN ? AND ? AND archived_at = 0 AND last_seen_at < ?
			  AND NOT EXISTS (
				SELECT 1 FROM occurrences o
				WHERE o.issue_id = issues.id AND o.seen_at >= ?)`,
			now, lo, hi.Int64, cutoff, cutoff)
		if err != nil {
			return total, fmt.Errorf("archive stale issues (%d..%d): %w", lo, hi.Int64, err)
		}
		if n, aerr := res.RowsAffected(); aerr == nil {
			total += n
		}
		lo = hi.Int64
	}
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
	if _, err := s.PruneQueryStats(time.Now().AddDate(0, 0, -days).Unix(), 500); err != nil {
		return "", err
	}
	if _, err := s.PruneJobStats(time.Now().AddDate(0, 0, -days).Unix(), 500); err != nil {
		return "", err
	}
	// Archiving runs AFTER the prunes, so an issue whose occurrences this cycle
	// just removed becomes eligible in the same pass.
	n4, err := s.ArchiveStaleIssues(days)
	if err != nil {
		return "", err
	}
	if rows, err := s.wdb.Query(`PRAGMA wal_checkpoint(TRUNCATE)`); err == nil {
		rows.Close()
	}
	return fmt.Sprintf("retention: pruned %d old + %d over-cap occurrences + %d alerts, archived %d stale issues, profiles kept <= %d per project", n1, n2, n3, n4, keepProfiles), nil
}
