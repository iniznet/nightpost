package store

import (
	"fmt"
	"time"
)

// Session is one producer-reported reader session outcome. The
// (project_key, session_id) pair is unique because a session beacon can
// arrive more than once (start, then outcome); the upsert keeps the first
// started_at and the newest outcome fields so a retried beacon cannot stretch
// a session's measured length.
type Session struct {
	ID         int64
	ProjectKey string
	Release    string
	SessionID  string
	Status     string
	RouteID    int64
	StartedAt  time.Time
	DurationMs int64
}

// UpsertSession stores a session outcome idempotently. On the conflict branch
// release/status/route take the newest non-empty claim while started_at stays
// at its first observation, which is the only stable anchor for duration.
func (s *Store) UpsertSession(sess Session) error {
	started := sess.StartedAt
	if started.IsZero() {
		started = time.Now()
	}
	_, err := s.wdb.Exec(`INSERT INTO sessions
		(project_key, release, session_id, status, route_id, started_at, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_key, session_id) DO UPDATE SET
			release = COALESCE(NULLIF(excluded.release, ''), sessions.release),
			status = COALESCE(NULLIF(excluded.status, ''), sessions.status),
			route_id = COALESCE(excluded.route_id, sessions.route_id),
			duration_ms = excluded.duration_ms`,
		sess.ProjectKey, sess.Release, sess.SessionID, sess.Status,
		nullableID(sess.RouteID), started.Unix(), sess.DurationMs)
	if err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}
	return nil
}

// CrashFree returns the session-outcome ratio for one release: sessions
// admitted, sessions crashed (status crashed or abnormal), and the crash-free
// fraction in [0,1]. No sessions is reported as fully crash-free (1.0) — the
// same "no data, no crashes" reading the release page already uses, so a
// brand-new release does not flash red before its first beacon.
func (s *Store) CrashFree(projectKey, release string) (sessions, crashed int64, crashFree float64, err error) {
	if err := s.rdb.QueryRow(`SELECT COUNT(*) FROM sessions WHERE project_key = ? AND (? = '' OR release = ?)`,
		projectKey, release, release).Scan(&sessions); err != nil {
		return 0, 0, 0, err
	}
	if err := s.rdb.QueryRow(`SELECT COUNT(*) FROM sessions
		WHERE project_key = ? AND (? = '' OR release = ?) AND status IN ('crashed', 'abnormal')`,
		projectKey, release, release).Scan(&crashed); err != nil {
		return 0, 0, 0, err
	}
	if sessions == 0 {
		return 0, 0, 1, nil
	}
	crashFree = float64(sessions-crashed) / float64(sessions)
	return sessions, crashed, crashFree, nil
}
