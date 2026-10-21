package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Job/command telemetry : one aggregate row per (project, kind, name),
// upserted on every ingest event. No per-run rows exist, so table size is
// bounded by the number of job/command NAMES the fleet can produce — a
// bounded set — not by run volume. Failing jobs are retried by the queue
// with backoff; a retry storm would never grow this table.

type JobAgg struct {
	ProjectKey  string
	Kind        string // queue | command
	Name        string
	Queue       string
	Runs        int64
	Failures    int64
	Failures24h int64
	AvgMs       float64
	LastMs      float64
	LastRunAt   time.Time
	LastOKAt    time.Time
	LastStatus  string // ok | failed
	LastTrace   string // run trace id of the last profiled leg (#203), '' until one lands
}

const jobWindowSeconds = 86400

// RecordJob upserts one job/command completion. Producers send only
// name/kind/queue/duration/ok — never job payloads or command arguments
// (redaction posture; payload bodies can carry reader data). seconds is
// clamped to one week so a clock skew cannot poison the average.
// The 24h failure counter uses a reset-at-next-failure window: the row
// stores failures_24h plus fail_window_start; a failure arriving 24h+
// after the stored window start reopens the window at 1, and readers
// recompute staleness so a one-off old failure never displays as recent.
func (s *Store) RecordJob(projectKey, kind, name, queue string, seconds float64, ok bool, count int64, lastTrace string) error {
	if seconds < 0 {
		seconds = 0
	}
	if seconds > 604800 {
		seconds = 604800
	}
	if count < 1 {
		count = 1
	}
	now := time.Now().Unix()
	status := "ok"
	fail := 0
	winStart := int64(0)
	okAt := now
	if !ok {
		status = "failed"
		fail = 1
		winStart = now
		okAt = 0
	}
	_, err := s.wdb.Exec(`INSERT INTO job_stats
		(project_key, kind, name, queue, runs, failures, fail_window_start, failures_24h,
		 seconds_sum, seconds_count, last_ok_at, last_run_at, last_seconds, last_status, last_trace)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_key, kind, name) DO UPDATE SET
			queue = COALESCE(NULLIF(excluded.queue, ''), job_stats.queue),
			runs = job_stats.runs + excluded.runs,
			failures = job_stats.failures + excluded.failures,
			failures_24h = CASE
				WHEN excluded.last_status <> 'failed' THEN job_stats.failures_24h
				WHEN job_stats.fail_window_start = 0 OR excluded.fail_window_start - job_stats.fail_window_start >= 86400 THEN excluded.failures
				ELSE job_stats.failures_24h + excluded.failures END,
			fail_window_start = CASE
				WHEN excluded.last_status = 'failed' AND (job_stats.fail_window_start = 0 OR excluded.fail_window_start - job_stats.fail_window_start >= 86400)
					THEN excluded.fail_window_start
				ELSE job_stats.fail_window_start END,
			seconds_sum = job_stats.seconds_sum + excluded.seconds_sum,
			seconds_count = job_stats.seconds_count + excluded.seconds_count,
			last_ok_at = CASE WHEN excluded.last_status = 'ok' THEN excluded.last_run_at ELSE job_stats.last_ok_at END,
			last_run_at = excluded.last_run_at,
			last_seconds = excluded.last_seconds,
			last_status = excluded.last_status,
			last_trace = COALESCE(NULLIF(excluded.last_trace, ''), job_stats.last_trace)`,
		projectKey, kind, name, queue, count, int64(fail)*count, winStart, int64(fail)*count, seconds*float64(count), count, okAt, now, seconds, status, truncate(lastTrace, 64))
	if err != nil {
		return fmt.Errorf("record job: %w", err)
	}
	return nil
}

// JobLeaderboard lists aggregates whose last run is inside the window,
// most recent first — a dead cron's row simply stops updating and ages
// out, while the heartbeat alert names it before that.
func (s *Store) JobLeaderboard(project string, hours, limit int) ([]JobAgg, error) {
	if hours <= 0 {
		hours = 24 * 7
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	now := time.Now().Unix()
	cutoff := now - int64(hours)*3600
	rows, err := s.rdb.Query(`SELECT project_key, kind, name, queue, runs, failures,
		CASE WHEN fail_window_start = 0 OR ? - fail_window_start >= 86400 THEN 0 ELSE failures_24h END,
		seconds_sum, seconds_count, last_ok_at, last_run_at, last_seconds, last_status, last_trace
		FROM job_stats
		WHERE (? = '' OR project_key = ?) AND last_run_at >= ?
		ORDER BY last_run_at DESC LIMIT ?`,
		now, project, project, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobAgg
	for rows.Next() {
		var j JobAgg
		var lastRun, lastOK int64
		var sum float64
		var count int64
		if err := rows.Scan(&j.ProjectKey, &j.Kind, &j.Name, &j.Queue, &j.Runs, &j.Failures,
			&j.Failures24h, &sum, &count, &lastOK, &lastRun, &j.LastMs, &j.LastStatus, &j.LastTrace); err != nil {
			return nil, err
		}
		j.LastMs *= 1000
		if count > 0 {
			j.AvgMs = sum / float64(count) * 1000
		}
		j.LastRunAt = time.Unix(lastRun, 0)
		if lastOK > 0 {
			j.LastOKAt = time.Unix(lastOK, 0)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// JobLastOKAt answers the heartbeat question: when did (project, kind, name)
// last COMPLETE successfully? 0 = never observed (startup grace applies).
func (s *Store) JobLastOKAt(project, kind, name string) (int64, error) {
	var t int64
	err := s.wdb.QueryRow(`SELECT last_ok_at FROM job_stats WHERE project_key = ? AND kind = ? AND name = ?`,
		project, kind, name).Scan(&t)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("job last ok lookup: %w", err)
	}
	return t, nil
}

// PruneJobStats drops aggregates idle past the cutoff (renamed/removed
// jobs aging out), chunked like every other prune.
func (s *Store) PruneJobStats(cutoff int64, chunk int) (int64, error) {
	if chunk <= 0 {
		chunk = 500
	}
	var total int64
	for {
		res, err := s.wdb.Exec(`DELETE FROM job_stats WHERE rowid IN
			(SELECT rowid FROM job_stats WHERE last_run_at < ? LIMIT ?)`, cutoff, chunk)
		if err != nil {
			return total, fmt.Errorf("prune job stats: %w", err)
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
