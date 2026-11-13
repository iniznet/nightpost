package store

import (
	"database/sql"
	"fmt"
	"time"
)

// TraceSpan is one row of the v2 spans table: a single OTel-style span that
// joins into a journey through its trace_id. It is named TraceSpan rather than
// Span because profile.go already owns Span for the legacy JSON span column;
// the two must not collide, and the flat fields here are the subset the
// journey materialiser needs.
type TraceSpan struct {
	TraceID       string
	SpanID        string
	ParentSpanID  string
	ServiceID     int64
	RouteID       int64
	Kind          string
	Name          string
	StartMs       float64
	DurationMs    float64
	Status        string
	PeerServiceID int64
	Attributes    string
	CreatedAt     time.Time
}

// InsertSpans stores a batch of spans in one transaction on the single
// writer, so a trace's hops land atomically instead of appearing half-written
// to a concurrent journey recompute.
func (s *Store) InsertSpans(spans []TraceSpan) error {
	if len(spans) == 0 {
		return nil
	}
	tx, err := s.wdb.Begin()
	if err != nil {
		return fmt.Errorf("insert spans: begin: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO spans
		(trace_id, span_id, parent_span_id, service_id, route_id, kind, name,
		 start_ms, duration_ms, status, peer_service_id, attributes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("insert spans: prepare: %w", err)
	}
	defer stmt.Close()
	for _, sp := range spans {
		created := sp.CreatedAt
		if created.IsZero() {
			created = time.Now()
		}
		if _, err := stmt.Exec(sp.TraceID, sp.SpanID, sp.ParentSpanID,
			nullableID(sp.ServiceID), nullableID(sp.RouteID), sp.Kind, sp.Name,
			sp.StartMs, sp.DurationMs, sp.Status, nullableID(sp.PeerServiceID),
			sp.Attributes, created.Unix()); err != nil {
			return fmt.Errorf("insert spans: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("insert spans: commit: %w", err)
	}
	return nil
}

// TraceSpans returns every span of one trace ordered by its position on the
// waterfall (start_ms, then span_id as the stable tie-break), so the request
// page draws hops in the order they happened rather than insert order.
func (s *Store) TraceSpans(traceID string) ([]TraceSpan, error) {
	rows, err := s.rdb.Query(`SELECT trace_id, span_id, parent_span_id, service_id, route_id, kind, name,
			start_ms, duration_ms, status, peer_service_id, attributes, created_at
		FROM spans WHERE trace_id = ?
		ORDER BY start_ms ASC, span_id ASC`, traceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TraceSpan
	for rows.Next() {
		var sp TraceSpan
		var serviceID, routeID, peerID sql.NullInt64
		var created int64
		if err := rows.Scan(&sp.TraceID, &sp.SpanID, &sp.ParentSpanID, &serviceID, &routeID, &sp.Kind, &sp.Name,
			&sp.StartMs, &sp.DurationMs, &sp.Status, &peerID, &sp.Attributes, &created); err != nil {
			return nil, err
		}
		sp.ServiceID = serviceID.Int64
		sp.RouteID = routeID.Int64
		sp.PeerServiceID = peerID.Int64
		sp.CreatedAt = time.Unix(created, 0)
		out = append(out, sp)
	}
	return out, rows.Err()
}

// nullableID maps a zero id to SQL NULL: the v2 tables use NULL, not 0, as
// the "no service/route/peer" sentinel so a join never matches a phantom row 0.
func nullableID(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
