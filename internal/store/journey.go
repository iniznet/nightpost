package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Journey is the materialised cross-service path: one row per distinct
// (entry_kind, ordered route pattern list). It is recomputed from spans, never
// written by ingest directly, so a producer's reporting order cannot split one
// path into duplicate journeys.
type Journey struct {
	ID           int64
	PatternKey   string
	EntryKind    string
	EntryRouteID int64
	HopCount     int64
	Samples      int64
	P50Ms        float64
	P95Ms        float64
	FirstSeenAt  time.Time
	LastSeenAt   time.Time
}

// JourneyHop is one step of a journey: the route at a position plus its
// latency share. An outbound span with no matching child row materialises as
// status "unreported" here, so a gap is a named hop, never a blank.
type JourneyHop struct {
	JourneyID int64
	Position  int
	RouteID   int64
	Status    string
	P95Ms     float64
	Share     float64
}

// UpsertJourney replaces one journey row and its hops atomically. pattern_key
// is the natural key: a recompute that lands the same path again updates the
// aggregates instead of minting a duplicate, and the hop list is rewritten
// wholesale so positions never drift out of order between recomputes.
func (s *Store) UpsertJourney(j Journey, hops []JourneyHop) (int64, error) {
	now := time.Now().Unix()
	first := j.FirstSeenAt
	if first.IsZero() {
		first = time.Unix(now, 0)
	}
	last := j.LastSeenAt
	if last.IsZero() {
		last = time.Unix(now, 0)
	}
	tx, err := s.wdb.Begin()
	if err != nil {
		return 0, fmt.Errorf("upsert journey: begin: %w", err)
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRow(`INSERT INTO journeys
		(pattern_key, entry_kind, entry_route_id, hop_count, samples, p50_ms, p95_ms, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(pattern_key) DO UPDATE SET
			entry_kind = excluded.entry_kind,
			entry_route_id = excluded.entry_route_id,
			hop_count = excluded.hop_count,
			samples = excluded.samples,
			p50_ms = excluded.p50_ms,
			p95_ms = excluded.p95_ms,
			last_seen_at = excluded.last_seen_at
		RETURNING id`,
		j.PatternKey, j.EntryKind, nullableID(j.EntryRouteID), j.HopCount, j.Samples,
		j.P50Ms, j.P95Ms, first.Unix(), last.Unix()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert journey: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM journey_hops WHERE journey_id = ?`, id); err != nil {
		return 0, fmt.Errorf("upsert journey: clear hops: %w", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO journey_hops (journey_id, position, route_id, status, p95_ms, share)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("upsert journey: prepare hops: %w", err)
	}
	defer stmt.Close()
	for _, h := range hops {
		status := h.Status
		if status == "" {
			status = "ok"
		}
		if _, err := stmt.Exec(id, h.Position, nullableID(h.RouteID), status, h.P95Ms, h.Share); err != nil {
			return 0, fmt.Errorf("upsert journey: insert hop %d: %w", h.Position, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("upsert journey: commit: %w", err)
	}
	return id, nil
}

// Journeys lists a project's materialised paths, most painful first, by
// joining the entry route back to its owning service. entryKind "" means
// every kind.
func (s *Store) Journeys(projectKey, entryKind string, limit int) ([]Journey, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.rdb.Query(`SELECT j.id, j.pattern_key, j.entry_kind, j.entry_route_id,
			j.hop_count, j.samples, j.p50_ms, j.p95_ms, j.first_seen_at, j.last_seen_at
		FROM journeys j
		JOIN routes r ON r.id = j.entry_route_id
		JOIN services s ON s.id = r.service_id
		WHERE (? = '' OR s.project_key = ?) AND (? = '' OR j.entry_kind = ?)
		ORDER BY j.p95_ms DESC
		LIMIT ?`,
		projectKey, projectKey, entryKind, entryKind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Journey
	for rows.Next() {
		var j Journey
		var entryRouteID, first, last sql.NullInt64
		if err := rows.Scan(&j.ID, &j.PatternKey, &j.EntryKind, &entryRouteID, &j.HopCount, &j.Samples,
			&j.P50Ms, &j.P95Ms, &first, &last); err != nil {
			return nil, err
		}
		j.EntryRouteID = entryRouteID.Int64
		if first.Valid {
			j.FirstSeenAt = time.Unix(first.Int64, 0)
		}
		if last.Valid {
			j.LastSeenAt = time.Unix(last.Int64, 0)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// JourneyHops returns one journey's hops in position order.
func (s *Store) JourneyHops(journeyID int64) ([]JourneyHop, error) {
	rows, err := s.rdb.Query(`SELECT journey_id, position, route_id, status, p95_ms, share
		FROM journey_hops WHERE journey_id = ? ORDER BY position`, journeyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JourneyHop
	for rows.Next() {
		var h JourneyHop
		var routeID sql.NullInt64
		var status sql.NullString
		if err := rows.Scan(&h.JourneyID, &h.Position, &routeID, &status, &h.P95Ms, &h.Share); err != nil {
			return nil, err
		}
		h.RouteID = routeID.Int64
		h.Status = status.String
		out = append(out, h)
	}
	return out, rows.Err()
}
