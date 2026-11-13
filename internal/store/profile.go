package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Profile types: "request" (client-reported route timing), "callgraph"
// (code-level attribution from xhprof or the V8 sampling profiler), and the
// per-run job/command legs of #203 that make /jobs drill into /traces.
const (
	ProfileTypeRequest   = "request"
	ProfileTypeCallgraph = "callgraph"
	ProfileTypeJob       = "job"
	ProfileTypeCommand   = "command"
)

const (
	maxProfileSpans     = 200
	maxProfileHotspots  = 50
	defaultProfileLimit = 50
	maxSlowestRows      = 100
)

type Span struct {
	Name       string  `json:"name"`
	DurationMs float64 `json:"duration_ms"`
	// StartMs is the span's offset from ITS OWN profile's start (elapsed,
	// never absolute wall-clock) — cross-service waterfalls join legs through
	// the parent's measured window, so clock sync between containers is
	// never required. Zero means "report order", the legacy layout.
	StartMs float64        `json:"start_ms,omitempty"`
	Tags    map[string]any `json:"tags,omitempty"`
}

// TreeNode is one node of a pruned call tree : the execution stack
// with an inclusive wall-clock duration per step. Keys are deliberately
// short because the JSON ships on the ingest wire from the SvelteKit front.
type TreeNode struct {
	N string     `json:"n"`           // function name
	S float64    `json:"s"`           // self ms (inclusive minus expanded children)
	I float64    `json:"i"`           // inclusive ms
	C int64      `json:"c,omitempty"` // calls folded into this edge
	K []TreeNode `json:"k,omitempty"` // children
}

// Hotspot is one function's self-time inside a callgraph profile.
type Hotspot struct {
	Function string  `json:"function"`
	File     string  `json:"file,omitempty"`
	SelfMs   float64 `json:"self_ms"`
	Calls    int64   `json:"calls,omitempty"`
}

type Profile struct {
	ID          int64
	ProjectKey  string
	Type        string
	Name        string
	DurationMs  float64
	Environment string
	TraceID     string
	Spans       []Span
	Hotspots    []Hotspot
	Tree        []TreeNode
	CreatedAt   time.Time
}

// HotspotAgg is a /slowest leaderboard row: one function aggregated across
// the most recent callgraph profiles of a project.
type HotspotAgg struct {
	Function        string
	File            string
	TotalSelfMs     float64
	Samples         int
	LastSeen        time.Time
	SampleProfileID int64
	SampleProfile   string
}

// AvgSelfMs is exposed for template rendering.
func (h HotspotAgg) AvgSelfMs() float64 {
	if h.Samples == 0 {
		return 0
	}
	return h.TotalSelfMs / float64(h.Samples)
}

// RouteAgg is one route's request-profile aggregate for the profiles page:
// pain-sorted by total time across the trailing window.
type RouteAgg struct {
	Name     string
	Count    int64
	TotalMs  float64
	AvgMs    float64
	P50Ms    float64
	P95Ms    float64
	MaxMs    float64
	LastSeen time.Time
}

type ProfileInput struct {
	ProjectKey  string
	Type        string
	Name        string
	DurationMs  float64
	Environment string
	TraceID     string
	Spans       []Span
	Hotspots    []Hotspot
	Tree        []TreeNode
	// v2 identity columns: the service that reported the leg, the route it
	// named, the span that stands for the profile itself in the trace
	// waterfall, and the profile kind. Zero/empty means "not reported" and
	// lands as NULL('') rather than a phantom row 0.
	ServiceID int64
	RouteID   int64
	SpanID    string
	Kind      string
}

func (s *Store) RecordProfile(in ProfileInput) (int64, error) {
	if in.Type == "" {
		in.Type = ProfileTypeRequest
	}
	if len(in.Spans) > maxProfileSpans {
		in.Spans = in.Spans[:maxProfileSpans]
	}
	if len(in.Hotspots) > maxProfileHotspots {
		in.Hotspots = in.Hotspots[:maxProfileHotspots]
	}
	if in.Kind == "" {
		in.Kind = "request"
	}
	res, err := s.wdb.Exec(`INSERT INTO profiles
		(project_key, type, name, duration_ms, environment, trace_id, spans, hotspots, tree, created_at,
		 service_id, route_id, span_id, kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.ProjectKey, in.Type, truncate(in.Name, 240), in.DurationMs, in.Environment, truncate(in.TraceID, 64),
		marshalJSON(in.Spans), marshalJSON(in.Hotspots), marshalJSON(in.Tree), time.Now().Unix(),
		nullableID(in.ServiceID), nullableID(in.RouteID), truncate(in.SpanID, 32), in.Kind)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ProfilesByType lists profiles of one type ("request" or "callgraph").
func (s *Store) ProfilesByType(projectKey, typ string, limit int) ([]Profile, error) {
	if limit <= 0 || limit > 200 {
		limit = defaultProfileLimit
	}
	rows, err := s.rdb.Query(`SELECT id, project_key, type, name, duration_ms, environment, trace_id, spans, hotspots, tree, created_at
		FROM profiles WHERE type = ? AND (? = '' OR project_key = ?)
		ORDER BY created_at DESC, id DESC LIMIT ?`, typ, projectKey, projectKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Profiles(projectKey string, limit int) ([]Profile, error) {
	if limit <= 0 || limit > 200 {
		limit = defaultProfileLimit
	}
	rows, err := s.rdb.Query(`SELECT id, project_key, type, name, duration_ms, environment, trace_id, spans, hotspots, tree, created_at
		FROM profiles WHERE (? = '' OR project_key = ?)
		ORDER BY created_at DESC, id DESC LIMIT ?`, projectKey, projectKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Profile(id int64) (*Profile, error) {
	row := s.rdb.QueryRow(`SELECT id, project_key, type, name, duration_ms, environment, trace_id, spans, hotspots, tree, created_at
		FROM profiles WHERE id = ?`, id)
	p, err := scanProfile(row)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// SlowestCode aggregates hotspots across the most recent callgraph profiles
// of a project into a ranked leaderboard.
// SlowestCode is the project-wide hotspot leaderboard.
func (s *Store) SlowestCode(projectKey string, sampleLimit int) ([]HotspotAgg, error) {
	return s.slowestCode(projectKey, "", sampleLimit)
}

// SlowestCodeForRoute scopes the same leaderboard to one route's callgraph
// samples, so a page's drill-down can name the function it spends time in.
func (s *Store) SlowestCodeForRoute(projectKey, route string, sampleLimit int) ([]HotspotAgg, error) {
	if route == "" {
		return nil, nil
	}
	return s.slowestCode(projectKey, route, sampleLimit)
}

func (s *Store) slowestCode(projectKey, route string, sampleLimit int) ([]HotspotAgg, error) {
	if sampleLimit <= 0 || sampleLimit > 1000 {
		sampleLimit = 500
	}
	rows, err := s.rdb.Query(`SELECT id, name, hotspots, created_at FROM profiles
		WHERE type = ? AND (? = '' OR project_key = ?) AND (? = '' OR name = ?) AND hotspots <> '' AND hotspots IS NOT NULL
		ORDER BY created_at DESC, id DESC LIMIT ?`, ProfileTypeCallgraph, projectKey, projectKey, route, route, sampleLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type agg struct {
		fn, file, profile string
		total, best       float64
		samples           int
		pid               int64
		last              time.Time
	}
	byKey := map[string]*agg{}
	for rows.Next() {
		var id int64
		var name string
		var raw sql.NullString
		var created int64
		if err := rows.Scan(&id, &name, &raw, &created); err != nil {
			return nil, err
		}
		if !raw.Valid {
			continue
		}
		var hotspots []Hotspot
		if err := json.Unmarshal([]byte(raw.String), &hotspots); err != nil {
			continue
		}
		when := time.Unix(created, 0)
		for _, h := range hotspots {
			key := h.File + "\x00" + h.Function
			a := byKey[key]
			if a == nil {
				a = &agg{fn: h.Function, file: h.File}
				byKey[key] = a
			}
			a.total += h.SelfMs
			a.samples++
			if when.After(a.last) {
				a.last = when
			}
			if h.SelfMs > a.best {
				a.best = h.SelfMs
				a.pid = id
				a.profile = name
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]HotspotAgg, 0, len(byKey))
	for _, a := range byKey {
		out = append(out, HotspotAgg{Function: a.fn, File: a.file, TotalSelfMs: a.total, Samples: a.samples, LastSeen: a.last, SampleProfileID: a.pid, SampleProfile: a.profile})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TotalSelfMs > out[j].TotalSelfMs })
	if len(out) > maxSlowestRows {
		out = out[:maxSlowestRows]
	}
	return out, nil
}

// ProfileRouteStats aggregates profiles of one type by route name over the
// trailing window, pain-sorted by total time. Row collection is capped
// (rowCap) so a busy window cannot wedge the page; aggregates are computed
// over the most recent cap rows, walked through the (type, project_key,
// created_at) index.
func (s *Store) ProfileRouteStats(projectKey, typ string, hours, limit int) ([]RouteAgg, error) {
	if hours <= 0 || hours > 168 {
		hours = 24
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	const rowCap = 20000
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	rows, err := s.rdb.Query(`SELECT name, duration_ms, created_at FROM profiles
		WHERE type = ? AND (? = '' OR project_key = ?) AND created_at >= ?
		ORDER BY created_at DESC, id DESC LIMIT ?`, typ, projectKey, projectKey, cutoff, rowCap)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type acc struct {
		n        int64
		sum, max float64
		last     int64
		durs     []float64
	}
	byName := map[string]*acc{}
	for rows.Next() {
		var name string
		var ms float64
		var created int64
		if err := rows.Scan(&name, &ms, &created); err != nil {
			return nil, err
		}
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
		if created > a.last {
			a.last = created
		}
		a.durs = append(a.durs, ms)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]RouteAgg, 0, len(byName))
	for name, a := range byName {
		sort.Float64s(a.durs)
		ra := RouteAgg{Name: name, Count: a.n, TotalMs: a.sum, AvgMs: a.sum / float64(a.n), MaxMs: a.max, LastSeen: time.Unix(a.last, 0)}
		ra.P50Ms = a.durs[min(int(a.n)-1, int(a.n*50/100))]
		ra.P95Ms = a.durs[min(int(a.n)-1, int(a.n*95/100))]
		out = append(out, ra)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TotalMs > out[j].TotalMs })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// RouteIndexRow is one row of the merged Performance index: a route's cost in
// the window, plus the cross-service picture stitched through trace_id.
// Backend fields stay zero when no second leg arrived, and the page renders
// that absence as a dash instead of dropping the column.
type RouteIndexRow struct {
	Project    string
	Name       string
	Count      int64
	TotalMs    float64
	AvgMs      float64
	P50Ms      float64
	P95Ms      float64
	MaxMs      float64
	LastSeen   time.Time
	Traces     int64
	Traced     int64
	BackendN   int64
	BackendP95 float64
}

// TracedPct is the share of the route's traces that carried a second leg.
func (r RouteIndexRow) TracedPct() float64 {
	if r.Traces == 0 {
		return 0
	}
	return float64(r.Traced) / float64(r.Traces) * 100
}

// RouteIndex answers "which page is slow" for every project in scope, and
// "did the other service answer" in the same row. Three bounded reads over
// one rowCap window, no schema change: root request legs, per-route trace
// counts, and every cross-project leg that shares a trace id.
func (s *Store) RouteIndex(project string, hours, limit int) ([]RouteIndexRow, error) {
	if hours <= 0 || hours > 168 {
		hours = 24
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	const rowCap = 20000
	const backendCap = 2000
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()

	type acc struct {
		row    RouteIndexRow
		durs   []float64
		be     []float64
		traces map[string]struct{}
	}
	byKey := map[[2]string]*acc{}

	roots, err := s.rdb.Query(`SELECT project_key, name, duration_ms, created_at FROM profiles
		WHERE type = ? AND (? = '' OR project_key = ?) AND created_at >= ?
		ORDER BY created_at DESC, id DESC LIMIT ?`,
		ProfileTypeRequest, project, project, cutoff, rowCap)
	if err != nil {
		return nil, err
	}
	defer roots.Close()
	for roots.Next() {
		var projectKey, name string
		var ms float64
		var created int64
		if err := roots.Scan(&projectKey, &name, &ms, &created); err != nil {
			return nil, err
		}
		a := byKey[[2]string{projectKey, name}]
		if a == nil {
			a = &acc{traces: map[string]struct{}{}}
			a.row.Project, a.row.Name = projectKey, name
			byKey[[2]string{projectKey, name}] = a
		}
		a.row.Count++
		a.row.TotalMs += ms
		if ms > a.row.MaxMs {
			a.row.MaxMs = ms
		}
		if created > a.row.LastSeen.Unix() {
			a.row.LastSeen = time.Unix(created, 0)
		}
		a.durs = append(a.durs, ms)
	}
	if err := roots.Err(); err != nil {
		return nil, err
	}

	cov, err := s.rdb.Query(`SELECT project_key, name, COUNT(DISTINCT trace_id) FROM profiles
		WHERE type = ? AND trace_id <> '' AND (? = '' OR project_key = ?) AND created_at >= ?
		GROUP BY project_key, name`,
		ProfileTypeRequest, project, project, cutoff)
	if err != nil {
		return nil, err
	}
	defer cov.Close()
	for cov.Next() {
		var projectKey, name string
		var n int64
		if err := cov.Scan(&projectKey, &name, &n); err != nil {
			return nil, err
		}
		if a := byKey[[2]string{projectKey, name}]; a != nil {
			a.row.Traces = n
		}
	}
	if err := cov.Err(); err != nil {
		return nil, err
	}

	legs, err := s.rdb.Query(`SELECT r.project_key, r.name, r.trace_id, o.duration_ms
		FROM profiles r JOIN profiles o
		  ON o.trace_id = r.trace_id AND o.project_key <> r.project_key
		WHERE r.type = ? AND r.trace_id <> '' AND (? = '' OR r.project_key = ?) AND r.created_at >= ?
		ORDER BY r.created_at DESC LIMIT 40000`,
		ProfileTypeRequest, project, project, cutoff)
	if err != nil {
		return nil, err
	}
	defer legs.Close()
	for legs.Next() {
		var projectKey, name, trace string
		var ms float64
		if err := legs.Scan(&projectKey, &name, &trace, &ms); err != nil {
			return nil, err
		}
		a := byKey[[2]string{projectKey, name}]
		if a == nil {
			continue
		}
		a.traces[trace] = struct{}{}
		a.row.BackendN++
		if len(a.be) < backendCap {
			a.be = append(a.be, ms)
		}
	}
	if err := legs.Err(); err != nil {
		return nil, err
	}

	out := make([]RouteIndexRow, 0, len(byKey))
	for _, a := range byKey {
		sort.Float64s(a.durs)
		n := len(a.durs)
		if n == 0 {
			continue
		}
		a.row.AvgMs = a.row.TotalMs / float64(a.row.Count)
		a.row.P50Ms = a.durs[min(n-1, n*50/100)]
		a.row.P95Ms = a.durs[min(n-1, n*95/100)]
		a.row.Traced = int64(len(a.traces))
		if len(a.be) > 0 {
			sort.Float64s(a.be)
			a.row.BackendP95 = a.be[min(len(a.be)-1, len(a.be)*95/100)]
		}
		out = append(out, a.row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TotalMs > out[j].TotalMs })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// TraceIDsForRoute lists the traces a route appears in, newest first. The
// detail page stitches each id back into its full leg set.
func (s *Store) TraceIDsForRoute(project, route string, hours, limit int) ([]string, error) {
	if hours <= 0 || hours > 168 {
		hours = 24
	}
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	rows, err := s.rdb.Query(`SELECT trace_id, MAX(created_at) AS last FROM profiles
		WHERE trace_id <> '' AND name = ? AND (? = '' OR project_key = ?) AND created_at >= ?
		GROUP BY trace_id ORDER BY last DESC LIMIT ?`,
		route, project, project, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		var last int64
		if err := rows.Scan(&id, &last); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ProfilesFiltered lists profiles of one type whose name matches a substring
// (case-insensitive LIKE), newest first — the raw drill-down behind the
// aggregated profiles view.
func (s *Store) ProfilesFiltered(projectKey, typ, nameLike string, limit int) ([]Profile, error) {
	if limit <= 0 || limit > 200 {
		limit = defaultProfileLimit
	}
	rows, err := s.rdb.Query(`SELECT id, project_key, type, name, duration_ms, environment, trace_id, spans, hotspots, tree, created_at
		FROM profiles WHERE (? = '' OR type = ?) AND (? = '' OR project_key = ?)
		AND (? = '' OR name LIKE '%' || ? || '%')
		ORDER BY created_at DESC, id DESC LIMIT ?`, typ, typ, projectKey, projectKey, nameLike, nameLike, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PruneProfiles keeps the profile table bounded PER PROJECT: every project
// keeps its newest `keep` rows. A single global keep-window let one noisy
// producer evict every other org's profiles and traces — the acceptance
// add — so the partition is by project_key, never by table.
//
// The delete is chunked and ranked by a window function, so each statement
// stays bounded on the single writer instead of one whole-table pass.
func (s *Store) PruneProfiles(keep int64) error {
	if keep <= 0 {
		keep = 5000
	}
	const chunk = 500
	for {
		res, err := s.wdb.Exec(`DELETE FROM profiles WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (
					PARTITION BY project_key ORDER BY created_at DESC, id DESC) AS rn
				FROM profiles
			) WHERE rn > ? LIMIT ?)`, keep, chunk)
		if err != nil {
			return fmt.Errorf("prune profiles: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n < chunk {
			return nil
		}
	}
}

type profileScanner interface{ Scan(dest ...any) error }

// ProfilesByTrace returns every profile carrying a trace id, oldest first —
// the legs of one cross-service request in the order they happened.
func (s *Store) ProfilesByTrace(traceID string, limit int) ([]Profile, error) {
	if traceID == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.rdb.Query(`SELECT id, project_key, type, name, duration_ms, environment, trace_id, spans, hotspots, tree, created_at
		FROM profiles WHERE trace_id = ?
		ORDER BY created_at ASC, id ASC LIMIT ?`, traceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// TraceSummary is one row of the /traces index: a cross-service request's
// legs folded into the figures the index renders. HasTree marks a trace with
// at least one pruned call tree, so the index can show the drill-down affordance
// before the reader opens the waterfall.
type TraceSummary struct {
	TraceID   string
	Legs      int
	Projects  string
	RootName  string
	TotalMs   float64
	HasTree   bool
	FirstSeen time.Time
	LastSeen  time.Time
}

// Traces lists distinct trace ids of a project inside the window, newest
// first. The window carries a time bound and the row cap keeps the group
// aggregate bounded (the 70100-wedge class: no unbounded scan).
func (s *Store) Traces(projectKey string, hours, limit int) ([]TraceSummary, error) {
	if hours <= 0 || hours > 720 {
		hours = 24
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	rows, err := s.rdb.Query(`SELECT trace_id,
			COUNT(*),
			GROUP_CONCAT(DISTINCT project_key),
			SUM(duration_ms),
			MAX(CASE WHEN tree <> '' AND tree IS NOT NULL AND tree <> 'null' AND tree <> '[]' THEN 1 ELSE 0 END),
			MIN(created_at), MAX(created_at),
			(SELECT p2.name FROM profiles p2 WHERE p2.trace_id = p.trace_id ORDER BY p2.created_at ASC, p2.id ASC LIMIT 1)
		FROM profiles p
		WHERE trace_id <> '' AND (? = '' OR project_key = ?) AND created_at >= ?
		GROUP BY trace_id
		ORDER BY MAX(created_at) DESC
		LIMIT ?`, projectKey, projectKey, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TraceSummary
	for rows.Next() {
		var ts TraceSummary
		var projects sql.NullString
		var root sql.NullString
		var hasTree int64
		var first, last int64
		if err := rows.Scan(&ts.TraceID, &ts.Legs, &projects, &ts.TotalMs, &hasTree, &first, &last, &root); err != nil {
			return nil, err
		}
		ts.Projects = projects.String
		ts.RootName = root.String
		ts.HasTree = hasTree == 1
		ts.FirstSeen = time.Unix(first, 0)
		ts.LastSeen = time.Unix(last, 0)
		out = append(out, ts)
	}
	return out, rows.Err()
}

func scanProfile(row profileScanner) (Profile, error) {
	var p Profile
	var typ, env, trace, spans, hotspots, tree sql.NullString
	var created int64
	if err := row.Scan(&p.ID, &p.ProjectKey, &typ, &p.Name, &p.DurationMs, &env, &trace, &spans, &hotspots, &tree, &created); err != nil {
		return p, err
	}
	p.Type = typ.String
	p.Environment = env.String
	p.TraceID = trace.String
	p.CreatedAt = time.Unix(created, 0)
	p.Spans = unmarshalSpans(spans)
	p.Hotspots = unmarshalHotspots(hotspots)
	p.Tree = unmarshalTree(tree)
	return p, nil
}

func marshalJSON(v any) any {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func unmarshalSpans(raw sql.NullString) []Span {
	if !raw.Valid || raw.String == "" {
		return nil
	}
	var out []Span
	_ = json.Unmarshal([]byte(raw.String), &out)
	return out
}

func unmarshalTree(raw sql.NullString) []TreeNode {
	if !raw.Valid || raw.String == "" || raw.String == "null" {
		return nil
	}
	var out []TreeNode
	_ = json.Unmarshal([]byte(raw.String), &out)
	return out
}

func unmarshalHotspots(raw sql.NullString) []Hotspot {
	if !raw.Valid || raw.String == "" {
		return nil
	}
	var out []Hotspot
	_ = json.Unmarshal([]byte(raw.String), &out)
	return out
}
