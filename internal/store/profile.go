package store

import (
	"database/sql"
	"encoding/json"
	"sort"
	"time"
)

// Profile types: "request" (client-reported route timing) and "callgraph"
// (code-level attribution from xhprof or the V8 sampling profiler).
const (
	ProfileTypeRequest   = "request"
	ProfileTypeCallgraph = "callgraph"
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
	Tags       map[string]any `json:"tags,omitempty"`
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
	Spans       []Span
	Hotspots    []Hotspot
	CreatedAt   time.Time
}

// HotspotAgg is a /slowest leaderboard row: one function aggregated across
// the most recent callgraph profiles of a project.
type HotspotAgg struct {
	Function    string
	File        string
	TotalSelfMs float64
	Samples     int
	LastSeen    time.Time
}

// AvgSelfMs is exposed for template rendering.
func (h HotspotAgg) AvgSelfMs() float64 {
	if h.Samples == 0 {
		return 0
	}
	return h.TotalSelfMs / float64(h.Samples)
}

type ProfileInput struct {
	ProjectKey  string
	Type        string
	Name        string
	DurationMs  float64
	Environment string
	Spans       []Span
	Hotspots    []Hotspot
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
	res, err := s.wdb.Exec(`INSERT INTO profiles (project_key, type, name, duration_ms, environment, spans, hotspots, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		in.ProjectKey, in.Type, truncate(in.Name, 240), in.DurationMs, in.Environment,
		marshalJSON(in.Spans), marshalJSON(in.Hotspots), time.Now().Unix())
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
	rows, err := s.rdb.Query(`SELECT id, project_key, type, name, duration_ms, environment, spans, hotspots, created_at
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
	rows, err := s.rdb.Query(`SELECT id, project_key, type, name, duration_ms, environment, spans, hotspots, created_at
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
	row := s.rdb.QueryRow(`SELECT id, project_key, type, name, duration_ms, environment, spans, hotspots, created_at
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
	rows, err := s.rdb.Query(`SELECT hotspots, created_at FROM profiles
		WHERE type = ? AND (? = '' OR project_key = ?) AND hotspots <> '' AND hotspots IS NOT NULL
		ORDER BY created_at DESC, id DESC LIMIT ?`, ProfileTypeCallgraph, projectKey, projectKey, sampleLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type agg struct {
		fn, file string
		total    float64
		samples  int
		last     time.Time
	}
	byKey := map[string]*agg{}
	for rows.Next() {
		var raw sql.NullString
		var created int64
		if err := rows.Scan(&raw, &created); err != nil {
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
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]HotspotAgg, 0, len(byKey))
	for _, a := range byKey {
		out = append(out, HotspotAgg{Function: a.fn, File: a.file, TotalSelfMs: a.total, Samples: a.samples, LastSeen: a.last})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TotalSelfMs > out[j].TotalSelfMs })
	if len(out) > maxSlowestRows {
		out = out[:maxSlowestRows]
	}
	return out, nil
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

func scanProfile(row profileScanner) (Profile, error) {
	var p Profile
	var typ, env, spans, hotspots sql.NullString
	var created int64
	if err := row.Scan(&p.ID, &p.ProjectKey, &typ, &p.Name, &p.DurationMs, &env, &spans, &hotspots, &created); err != nil {
		return p, err
	}
	p.Type = typ.String
	p.Environment = env.String
	p.CreatedAt = time.Unix(created, 0)
	p.Spans = unmarshalSpans(spans)
	p.Hotspots = unmarshalHotspots(hotspots)
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

func unmarshalHotspots(raw sql.NullString) []Hotspot {
	if !raw.Valid || raw.String == "" {
		return nil
	}
	var out []Hotspot
	_ = json.Unmarshal([]byte(raw.String), &out)
	return out
}
