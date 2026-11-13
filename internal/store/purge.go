package store

import (
	"errors"
	"fmt"
	"time"
)

// ErrPurgeActive means the same project already has a purge in flight. A second
// request is refused rather than stacked, so "did my click work" has one answer.
var ErrPurgeActive = errors.New("a purge is already queued for this project")

// purgeChunk bounds ONE statement. The value matches the retention loop's: the
// single WAL writer is shared with ingest, so an unbounded DELETE would stall
// the writers that feed the dashboard.
const purgeChunk = 500

// purgeSteps is the FK-safe order. Each statement is bounded by its subquery's
// LIMIT, so one tick removes at most purgeChunk rows from one table.
//
// Journeys move BEFORE routes: a journey points at route ids, and deleting the
// routes first would leave rows pointing at nothing.
var purgeSteps = []struct{ name, stmt string }{
	{"occurrences", `DELETE FROM occurrences WHERE id IN (
		SELECT o.id FROM occurrences o JOIN issues i ON i.id = o.issue_id
		WHERE i.project_key = ? LIMIT ?)`},
	{"profiles", `DELETE FROM profiles WHERE rowid IN (
		SELECT rowid FROM profiles WHERE project_key = ? LIMIT ?)`},
	{"spans", `DELETE FROM spans WHERE id IN (
		SELECT sp.id FROM spans sp JOIN services sv ON sv.id = sp.service_id
		WHERE sv.project_key = ? LIMIT ?)`},
	{"journeys", `DELETE FROM journeys WHERE id IN (
		SELECT j.id FROM journeys j
		WHERE j.entry_route_id IN (SELECT r.id FROM routes r JOIN services sv ON sv.id = r.service_id WHERE sv.project_key = ?)
		   OR EXISTS (SELECT 1 FROM journey_hops h JOIN routes r ON r.id = h.route_id
		              JOIN services sv ON sv.id = r.service_id
		              WHERE h.journey_id = j.id AND sv.project_key = ?)
		LIMIT ?)`},
	{"journey_hops", `DELETE FROM journey_hops WHERE rowid IN (
		SELECT rowid FROM journey_hops WHERE journey_id NOT IN (SELECT id FROM journeys) LIMIT ?)`},
	{"issues", `DELETE FROM issues WHERE id IN (SELECT id FROM issues WHERE project_key = ? LIMIT ?)`},
	{"query_stats", `DELETE FROM query_stats WHERE rowid IN (SELECT rowid FROM query_stats WHERE project_key = ? LIMIT ?)`},
	{"job_stats", `DELETE FROM job_stats WHERE rowid IN (SELECT rowid FROM job_stats WHERE project_key = ? LIMIT ?)`},
	{"releases", `DELETE FROM releases WHERE id IN (SELECT id FROM releases WHERE project_key = ? LIMIT ?)`},
	{"sessions", `DELETE FROM sessions WHERE id IN (SELECT id FROM sessions WHERE project_key = ? LIMIT ?)`},
	{"raw_envelopes", `DELETE FROM raw_envelopes WHERE id IN (SELECT id FROM raw_envelopes WHERE project_key = ? LIMIT ?)`},
	{"routes", `DELETE FROM routes WHERE id IN (
		SELECT r.id FROM routes r JOIN services sv ON sv.id = r.service_id
		WHERE sv.project_key = ? LIMIT ?)`},
	{"services", `DELETE FROM services WHERE id IN (SELECT id FROM services WHERE project_key = ? LIMIT ?)`},
}

// PurgeState is what /orgs renders beside a project: what was asked for, how
// far the ticker got, and how many rows it has removed so far.
type PurgeState struct {
	Project     string
	Action      string
	State       string
	Step        int
	Steps       int
	Removed     int64
	RequestedAt time.Time
	RequestedBy string
}

// Done reports whether the ticker finished this purge.
func (p PurgeState) Done() bool { return p.State == "done" }

// RequestPurge queues a purge for one project. action is "clear" (keep the
// project and its key) or "delete" (remove the project row afterwards).
func (s *Store) RequestPurge(project, action, actor string) error {
	if action != "clear" && action != "delete" {
		return fmt.Errorf("purge action %q", action)
	}
	if project == "" {
		return errors.New("purge needs a project")
	}
	var state string
	err := s.rdb.QueryRow(`SELECT state FROM purges WHERE project_key = ?`, project).Scan(&state)
	if err == nil && state != "done" {
		return ErrPurgeActive
	}
	now := time.Now().Unix()
	if _, err := s.wdb.Exec(`INSERT INTO purges (project_key, action, state, step, removed, requested_at, requested_by)
		VALUES (?, ?, 'queued', 0, 0, ?, ?)
		ON CONFLICT(project_key) DO UPDATE SET action = excluded.action, state = 'queued',
		  step = 0, removed = 0, requested_at = excluded.requested_at, requested_by = excluded.requested_by`,
		project, action, now, actor); err != nil {
		return fmt.Errorf("queue purge: %w", err)
	}
	return s.RecordAdminAction(actor, "purge."+action, project, "queued")
}

// DrainPurgeStep runs ONE bounded statement for the oldest active purge and
// reports whether that purge finished. The ticker calls it once per cycle.
func (s *Store) DrainPurgeStep() (string, bool, error) {
	var project, action, state string
	var step int64
	err := s.rdb.QueryRow(`SELECT project_key, action, state, step FROM purges
		WHERE state IN ('queued','purging') ORDER BY requested_at LIMIT 1`).Scan(&project, &action, &state, &step)
	if err != nil {
		return "", false, nil // nothing queued is not an error
	}
	if state == "queued" {
		if _, err := s.wdb.Exec(`UPDATE purges SET state = 'purging' WHERE project_key = ?`, project); err != nil {
			return project, false, err
		}
	}
	if int(step) >= len(purgeSteps) {
		return project, true, s.finishPurge(project, action)
	}

	st := purgeSteps[step]
	// Every ? before the last one binds the project; the last one is the chunk
	// bound. The statements are written so that this holds for all of them.
	placeholders := countPlaceholders(st.stmt)
	args := make([]any, 0, placeholders)
	for i := 0; i < placeholders-1; i++ {
		args = append(args, project)
	}
	args = append(args, purgeChunk)
	res, err := s.wdb.Exec(st.stmt, args...)
	if err != nil {
		return project, false, fmt.Errorf("purge %s: %w", st.name, err)
	}
	n, _ := res.RowsAffected()
	if _, err := s.wdb.Exec(`UPDATE purges SET step = step + 1, removed = removed + ? WHERE project_key = ?`, n, project); err != nil {
		return project, false, err
	}
	return project, false, nil
}

// countPlaceholders counts the ? tokens in one statement.
func countPlaceholders(stmt string) int {
	n := 0
	for _, r := range stmt {
		if r == '?' {
			n++
		}
	}
	return n
}

// finishPurge closes the state machine: a "delete" removes the project row
// last, so a half-purged project always still exists and still renders.
func (s *Store) finishPurge(project, action string) error {
	var removed int64
	var by string
	_ = s.rdb.QueryRow(`SELECT removed, requested_by FROM purges WHERE project_key = ?`, project).Scan(&removed, &by)
	if action == "delete" {
		if _, err := s.wdb.Exec(`DELETE FROM projects WHERE key = ?`, project); err != nil {
			return fmt.Errorf("delete project: %w", err)
		}
	}
	if _, err := s.wdb.Exec(`UPDATE purges SET state = 'done' WHERE project_key = ?`, project); err != nil {
		return err
	}
	return s.RecordAdminAction(by, "purge."+action, project, fmt.Sprintf("completed, %d rows removed", removed))
}

// PurgeStates is the UI's view: one row per project that has a purge recorded.
func (s *Store) PurgeStates() (map[string]PurgeState, error) {
	rows, err := s.rdb.Query(`SELECT project_key, action, state, step, removed, requested_at, requested_by FROM purges`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]PurgeState{}
	for rows.Next() {
		var p PurgeState
		var at int64
		if err := rows.Scan(&p.Project, &p.Action, &p.State, &p.Step, &p.Removed, &at, &p.RequestedBy); err != nil {
			return nil, err
		}
		p.RequestedAt = time.Unix(at, 0)
		p.Steps = len(purgeSteps)
		out[p.Project] = p
	}
	return out, rows.Err()
}

// AdminAction is one line of the audit trail a destructive request leaves.
type AdminAction struct {
	ID        int64
	Actor     string
	Action    string
	Target    string
	Detail    string
	CreatedAt time.Time
}

// RecordAdminAction appends one audit line. A purge whose trail cannot be
// written fails visibly instead of running unrecorded.
func (s *Store) RecordAdminAction(actor, action, target, detail string) error {
	_, err := s.wdb.Exec(`INSERT INTO admin_actions (actor, action, target, detail, created_at) VALUES (?, ?, ?, ?, ?)`,
		actor, action, target, detail, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("admin action: %w", err)
	}
	return nil
}

// AdminActions returns the newest audit lines, newest first.
func (s *Store) AdminActions(limit int) ([]AdminAction, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, err := s.rdb.Query(`SELECT id, actor, action, target, detail, created_at FROM admin_actions ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminAction
	for rows.Next() {
		var a AdminAction
		var at int64
		if err := rows.Scan(&a.ID, &a.Actor, &a.Action, &a.Target, &a.Detail, &at); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(at, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}
