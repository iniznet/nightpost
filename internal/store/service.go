package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Service is one reporting producer identity: a (project, name) pair that
// first appears when an ingest payload carries a "service" field. v1 payloads
// are labelled with the project key as the service name, so every project has
// at least one row and the "upgrade your producer" banner can disappear
// per-project instead of per-install.
type Service struct {
	ID                 int64
	ProjectKey         string
	Name               string
	Runtime            string
	Env                string
	TrustedTraceparent bool
	FirstSeenAt        time.Time
	LastSeenAt         time.Time
}

// UpsertService admits (or refreshes) one service identity and returns its
// row id. runtime/env/trusted take the newest claim because a producer
// upgrade is the only thing that changes them, while first_seen_at is
// preserved so a re-labelled producer keeps its original discovery stamp.
func (s *Store) UpsertService(projectKey, name, runtime, env string, trusted bool) (int64, error) {
	now := time.Now().Unix()
	trust := 0
	if trusted {
		trust = 1
	}
	var id int64
	err := s.wdb.QueryRow(`INSERT INTO services
		(project_key, name, runtime, env, trusted_traceparent, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_key, name) DO UPDATE SET
			runtime = excluded.runtime,
			env = excluded.env,
			trusted_traceparent = excluded.trusted_traceparent,
			last_seen_at = excluded.last_seen_at
		RETURNING id`,
		projectKey, name, runtime, env, trust, now, now).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert service: %w", err)
	}
	return id, nil
}

// Services lists a project's service identities by name. An empty projectKey
// means every project, matching the filter convention used across the store.
func (s *Store) Services(projectKey string) ([]Service, error) {
	rows, err := s.rdb.Query(`SELECT id, project_key, name, runtime, env, trusted_traceparent, first_seen_at, last_seen_at
		FROM services WHERE (? = '' OR project_key = ?) ORDER BY name`, projectKey, projectKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Service
	for rows.Next() {
		var svc Service
		if err := scanService(rows, &svc); err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

// ServiceByName resolves one service identity; nil means the project has not
// reported that service yet. Callers use the nil to decide the v1-labelling
// path without a second lookup.
func (s *Store) ServiceByName(projectKey, name string) (*Service, error) {
	var svc Service
	err := scanService(s.rdb.QueryRow(`SELECT id, project_key, name, runtime, env, trusted_traceparent, first_seen_at, last_seen_at
		FROM services WHERE project_key = ? AND name = ?`, projectKey, name), &svc)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &svc, nil
}

type serviceScanner interface{ Scan(dest ...any) error }

func scanService(row serviceScanner, svc *Service) error {
	var trust int
	var first, last sql.NullInt64
	if err := row.Scan(&svc.ID, &svc.ProjectKey, &svc.Name, &svc.Runtime, &svc.Env, &trust, &first, &last); err != nil {
		return err
	}
	svc.TrustedTraceparent = trust == 1
	if first.Valid {
		svc.FirstSeenAt = time.Unix(first.Int64, 0)
	}
	if last.Valid {
		svc.LastSeenAt = time.Unix(last.Int64, 0)
	}
	return nil
}
