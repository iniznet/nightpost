package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Route is one addressable endpoint of a service. A journey hop names a route
// by its normalised pattern rather than its URL: collapsing
// /series/{slug}/chapters/{n} into one pattern is what turns a million spans
// into one comparable journey row.
type Route struct {
	ID          int64
	ServiceID   int64
	Method      string
	Pattern     string
	Kind        string
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

// UpsertRoute admits (or refreshes) one route pattern and returns its row id.
// The (service_id, method, pattern) key is the identity, so a producer that
// restarts keeps its route ids and a journey pattern key stays comparable
// across deploys. kind defaults to "http" because every current producer
// reports an HTTP route and the column is never read empty.
func (s *Store) UpsertRoute(serviceID int64, method, pattern, kind string) (int64, error) {
	if kind == "" {
		kind = "http"
	}
	now := time.Now().Unix()
	var id int64
	err := s.wdb.QueryRow(`INSERT INTO routes (service_id, method, pattern, kind, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(service_id, method, pattern) DO UPDATE SET
			kind = excluded.kind,
			last_seen_at = excluded.last_seen_at
		RETURNING id`,
		serviceID, method, pattern, kind, now, now).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert route: %w", err)
	}
	return id, nil
}

// Routes lists one service's routes. serviceID 0 means every service, which is
// the unfiltered ask of the /performance/routes page.
func (s *Store) Routes(serviceID int64) ([]Route, error) {
	rows, err := s.rdb.Query(`SELECT id, service_id, method, pattern, kind, first_seen_at, last_seen_at
		FROM routes WHERE (? = 0 OR service_id = ?) ORDER BY pattern`, serviceID, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Route
	for rows.Next() {
		var r Route
		if err := scanRoute(rows, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RouteByID resolves one route for the route-detail page. sql.ErrNoRows is
// returned unchanged so the handler answers 404 without a second lookup.
func (s *Store) RouteByID(id int64) (*Route, error) {
	var r Route
	if err := scanRoute(s.rdb.QueryRow(`SELECT id, service_id, method, pattern, kind, first_seen_at, last_seen_at
		FROM routes WHERE id = ?`, id), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

type routeScanner interface{ Scan(dest ...any) error }

func scanRoute(row routeScanner, r *Route) error {
	var first, last sql.NullInt64
	if err := row.Scan(&r.ID, &r.ServiceID, &r.Method, &r.Pattern, &r.Kind, &first, &last); err != nil {
		return err
	}
	if first.Valid {
		r.FirstSeenAt = time.Unix(first.Int64, 0)
	}
	if last.Valid {
		r.LastSeenAt = time.Unix(last.Int64, 0)
	}
	return nil
}
