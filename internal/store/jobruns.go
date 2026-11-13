package store

import (
	"fmt"
	"sort"
	"time"
)

// JobRuns is the drill-down behind one row of /jobs: the sampled profiles of a
// single job or command, newest first, plus the aggregates a reader needs to
// decide whether to open one.
//
// Only a sampled run carries a profile (the producer sends its spans on a
// fraction of runs, nightpost#4), so Count is the number of *sampled* runs in
// the window, never the run count — the leaderboard row already carries that,
// and the two must not be confused in a heading.
type JobRuns struct {
	Project string
	Kind    string
	Name    string
	Runs    []Profile
	Count   int64
	P50Ms   float64
	P95Ms   float64
	P99Ms   float64
	MaxMs   float64
	// Spanless counts the sampled runs that arrived without spans[]: a run
	// from a producer on the 0.1/0.2 line, which cannot be waterfalled.
	Spanless int64
}

// JobRuns reads the sampled profiles of one (project, kind, name) over a
// window. The job name is matched exactly: a LIKE here would fold
// "chapters:unlock" into "chapters:unlock-all" and report one job's cost as
// another's.
func (s *Store) JobRuns(project, kind, name string, hours, limit int) (*JobRuns, error) {
	if hours <= 0 || hours > 168 {
		hours = 24
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	out := &JobRuns{Project: project, Kind: kind, Name: name}

	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	rows, err := s.rdb.Query(`SELECT id, project_key, type, name, duration_ms, environment, trace_id, spans, hotspots, tree, created_at
		FROM profiles
		WHERE type = ? AND name = ? AND (? = '' OR project_key = ?) AND created_at >= ?
		ORDER BY created_at DESC, id DESC LIMIT ?`,
		kind, name, project, project, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("job runs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out.Runs = append(out.Runs, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The aggregates read the SAME window without the row cap, so p95 is the
	// window's p95 and not the newest fifty runs' p95.
	var durs []float64
	agg, err := s.rdb.Query(`SELECT duration_ms FROM profiles
		WHERE type = ? AND name = ? AND (? = '' OR project_key = ?) AND created_at >= ?`,
		kind, name, project, project, cutoff)
	if err != nil {
		return nil, fmt.Errorf("job run durations: %w", err)
	}
	defer agg.Close()
	for agg.Next() {
		var ms float64
		if err := agg.Scan(&ms); err != nil {
			return nil, err
		}
		durs = append(durs, ms)
	}
	if err := agg.Err(); err != nil {
		return nil, err
	}
	out.Count = int64(len(durs))
	if len(durs) > 0 {
		sort.Float64s(durs)
		pick := func(pct int) float64 {
			i := int(float64(len(durs)) * float64(pct) / 100.0)
			if i >= len(durs) {
				i = len(durs) - 1
			}
			return durs[i]
		}
		out.P50Ms = pick(50)
		out.P95Ms = pick(95)
		out.P99Ms = pick(99)
		out.MaxMs = durs[len(durs)-1]
	}
	for _, p := range out.Runs {
		if len(p.Spans) == 0 {
			out.Spanless++
		}
	}
	return out, nil
}

// SpanlessProfiles counts the profiles in the window that arrived with no
// spans[]. It is the number that makes an empty /performance page honest: a
// producer on the old line sends events and no chain, so the page says how
// many arrived rather than implying the monitor is broken.
func (s *Store) SpanlessProfiles(project string, hours int) (int64, error) {
	if hours <= 0 || hours > 168 {
		hours = 24
	}
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	var n int64
	err := s.rdb.QueryRow(`SELECT COUNT(*) FROM profiles
		WHERE (? = '' OR project_key = ?) AND created_at >= ?
		AND (spans IS NULL OR spans = '' OR spans = '[]' OR spans = 'null')`, project, project, cutoff).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("spanless profiles: %w", err)
	}
	return n, nil
}
