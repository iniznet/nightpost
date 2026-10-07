package store

import (
	"sort"
	"time"
)

// Bounded-window aggregates for the dashboard. Every query scans an index
// range (seen_at / created_at windows), never a whole table, because the
// single-writer SQLite connection shares itself with ingest.

type LevelCount struct {
	Level string
	Count int64
}

type RouteStat struct {
	Name  string
	Count int64
	AvgMs float64
	MaxMs float64
}

const maxStatsRows = 5000

type ProjectSummary struct {
	Project    string
	OpenIssues int64
	Errors24   int64
	Logs24     int64
	Requests24 int64
	P50Ms      float64
	P95Ms      float64
	TopRoutes  []RouteStat
	LogLevels  []LevelCount
}

// HourBuckets returns per-hour occurrence counts over the trailing window
// for one issue type ("error", "log" or "" for everything) of a project.
// The slice is aligned oldest-first and always has exactly `hours` entries.
func (s *Store) HourBuckets(project string, hours int, typ string) ([]int64, error) {
	if hours <= 0 || hours > 168 {
		hours = 24
	}
	now := time.Now().Unix()
	cutoff := now - int64(hours)*3600
	rows, err := s.rdb.Query(`SELECT (o.seen_at/3600)*3600 AS b, COUNT(*)
		FROM occurrences o JOIN issues i ON i.id = o.issue_id
		WHERE (? = '' OR i.project_key = ?)
		  AND (? = '' OR i.type = ?)
		  AND o.seen_at >= ?
		GROUP BY b`, project, project, typ, typ, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byBucket := map[int64]int64{}
	for rows.Next() {
		var b, c int64
		if err := rows.Scan(&b, &c); err != nil {
			return nil, err
		}
		byBucket[b] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]int64, hours)
	base := now/3600*3600 - int64(hours-1)*3600
	for i := range out {
		out[i] = byBucket[base+int64(i)*3600]
	}
	return out, nil
}

// OpenIssueCount counts unresolved issues for a project.
func (s *Store) OpenIssueCount(project string) (int64, error) {
	var n int64
	err := s.rdb.QueryRow(`SELECT COUNT(*) FROM issues
		WHERE resolved = 0 AND (? = '' OR project_key = ?)`, project, project).Scan(&n)
	return n, err
}

// IssueTotal counts all issues for a project.
func (s *Store) IssueTotal(project string) (int64, error) {
	var n int64
	err := s.rdb.QueryRow(`SELECT COUNT(*) FROM issues WHERE (? = '' OR project_key = ?)`, project, project).Scan(&n)
	return n, err
}

// LogLevels aggregates issue counts per level for the log stream filter.
func (s *Store) LogLevels(project string) ([]LevelCount, error) {
	rows, err := s.rdb.Query(`SELECT COALESCE(level, 'info'), COUNT(*) FROM issues
		WHERE type = 'log' AND (? = '' OR project_key = ?)
		GROUP BY level ORDER BY COUNT(*) DESC`, project, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LevelCount
	for rows.Next() {
		var lc LevelCount
		if err := rows.Scan(&lc.Level, &lc.Count); err != nil {
			return nil, err
		}
		out = append(out, lc)
	}
	return out, rows.Err()
}

// RequestStats summarizes sampled request profiles inside the window:
// overall latency percentiles plus the slowest routes by average duration.
// Row collection is capped (maxStatsRows) so a busy day cannot wedge the UI.
func (s *Store) RequestStats(project string, hours int) (count int64, p50, p95 float64, routes []RouteStat, err error) {
	if hours <= 0 || hours > 168 {
		hours = 24
	}
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	rows, err2 := s.rdb.Query(`SELECT name, duration_ms FROM profiles
		WHERE type = 'request' AND (? = '' OR project_key = ?) AND created_at >= ?
		ORDER BY created_at DESC LIMIT ?`, project, project, cutoff, maxStatsRows)
	if err2 != nil {
		return 0, 0, 0, nil, err2
	}
	defer rows.Close()

	var all []float64
	type acc struct {
		n   int64
		sum float64
		max float64
	}
	byName := map[string]*acc{}
	for rows.Next() {
		var name string
		var ms float64
		if err := rows.Scan(&name, &ms); err != nil {
			return 0, 0, 0, nil, err
		}
		all = append(all, ms)
		a := byName[name]
		if a == nil {
			a = &acc{}
			byName[name] = a
		}
		a.n++
		a.sum += ms
		if ms > a.max {
			a.max = ms
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, nil, err
	}
	count = int64(len(all))
	if count == 0 {
		return 0, 0, 0, nil, nil
	}
	sort.Float64s(all)
	p50 = all[len(all)*50/100]
	p95 = all[min(len(all)-1, len(all)*95/100)]
	for name, a := range byName {
		routes = append(routes, RouteStat{Name: name, Count: a.n, AvgMs: a.sum / float64(a.n), MaxMs: a.max})
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].AvgMs > routes[j].AvgMs })
	if len(routes) > 8 {
		routes = routes[:8]
	}
	return count, p50, p95, routes, nil
}

// IssueSparklines returns trailing-hour occurrence buckets per issue id for
// one batched query (used by the issues list trend column).
func (s *Store) IssueSparklines(project string, hours int) (map[int64][]int64, error) {
	if hours <= 0 || hours > 168 {
		hours = 24
	}
	now := time.Now().Unix()
	cutoff := now - int64(hours)*3600
	rows, err := s.rdb.Query(`SELECT o.issue_id, (o.seen_at/3600)*3600 AS b, COUNT(*)
		FROM occurrences o JOIN issues i ON i.id = o.issue_id
		WHERE (? = '' OR i.project_key = ?) AND o.seen_at >= ?
		GROUP BY o.issue_id, b`, project, project, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	buckets := map[int64]map[int64]int64{}
	for rows.Next() {
		var id, b, c int64
		if err := rows.Scan(&id, &b, &c); err != nil {
			return nil, err
		}
		m := buckets[id]
		if m == nil {
			m = map[int64]int64{}
			buckets[id] = m
		}
		m[b] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	base := now/3600*3600 - int64(hours-1)*3600
	out := make(map[int64][]int64, len(buckets))
	for id, m := range buckets {
		slice := make([]int64, hours)
		for i := range slice {
			slice[i] = m[base+int64(i)*3600]
		}
		out[id] = slice
	}
	return out, nil
}

// ProjectSummary composes the per-project dashboard figures.
func (s *Store) ProjectSummary(project string) (ProjectSummary, error) {
	sum := ProjectSummary{Project: project}
	var err error
	if sum.OpenIssues, err = s.OpenIssueCount(project); err != nil {
		return sum, err
	}
	errs24, err := s.HourBuckets(project, 24, "error")
	if err != nil {
		return sum, err
	}
	for _, v := range errs24 {
		sum.Errors24 += v
	}
	logs24, err := s.HourBuckets(project, 24, "log")
	if err != nil {
		return sum, err
	}
	for _, v := range logs24 {
		sum.Logs24 += v
	}
	n, p50, p95, routes, err := s.RequestStats(project, 24)
	if err != nil {
		return sum, err
	}
	sum.Requests24 = n
	sum.P50Ms = p50
	sum.P95Ms = p95
	sum.TopRoutes = routes
	sum.LogLevels, err = s.LogLevels(project)
	return sum, err
}
