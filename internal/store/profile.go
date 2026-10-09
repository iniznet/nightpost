package store

import (
	"database/sql"
	"encoding/json"
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
	Name       string         `json:"name"`
	DurationMs float64        `json:"duration_ms"`
	// StartMs is the span's offset from ITS OWN profile's start (elapsed,
	// never absolute wall-clock) — cross-service waterfalls join legs through
	// the parent's measured window, so clock sync between containers is
	// never required. Zero means "report order", the legacy layout.
	StartMs    float64        `json:"start_ms,omitempty"`
	Tags       map[string]any `json:"tags,omitempty"`
}

// TreeNode is one node of a pruned call tree (#181): the execution stack
// with an inclusive wall-clock duration per step. Keys are deliberately
// short because the JSON ships on the ingest wire from the SvelteKit front.
type TreeNode struct {
	N string     `json:"n"`          // function name
	S float64    `json:"s"`          // self ms (inclusive minus expanded children)
	I float64    `json:"i"`          // inclusive ms
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
	res, err := s.wdb.Exec(`INSERT INTO profiles (project_key, type, name, duration_ms, environment, trace_id, spans, hotspots, tree, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.ProjectKey, in.Type, truncate(in.Name, 240), in.DurationMs, in.Environment, truncate(in.TraceID, 64),
		marshalJSON(in.Spans), marshalJSON(in.Hotspots), marshalJSON(in.Tree), time.Now().Unix())
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
func (s *Store) SlowestCode(projectKey string, sampleLimit int) ([]HotspotAgg, error) {
	if sampleLimit <= 0 || sampleLimit > 1000 {
		sampleLimit = 500
	}
	rows, err := s.rdb.Query(`SELECT id, name, hotspots, created_at FROM profiles
		WHERE type = ? AND (? = '' OR project_key = ?) AND hotspots <> '' AND hotspots IS NOT NULL
		ORDER BY created_at DESC, id DESC LIMIT ?`, ProfileTypeCallgraph, projectKey, projectKey, sampleLimit)
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
		ra.P95Ms = a.durs[min(int(a.n)-1, int(a.n*95/100))]
		out = append(out, ra)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TotalMs > out[j].TotalMs })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
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

// PruneProfiles keeps the profile table bounded: the newest keep rows survive.
func (s *Store) PruneProfiles(keep int64) error {
	if keep <= 0 {
		keep = 5000
	}
	_, err := s.wdb.Exec(`DELETE FROM profiles WHERE id NOT IN
		(SELECT id FROM profiles ORDER BY created_at DESC, id DESC LIMIT ?)`, keep)
	return err
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
