package store

import (
	"database/sql"
	"fmt"
	"time"
)

// RawEnvelope is one ingest body kept for inspection: the /ingest-log page
// reads it to answer "what exactly did the producer send, and which field was
// refused". It is a debug ring, never a second copy of the data model — the
// body is the bytes on the wire, and nothing reads it back into the store.
type RawEnvelope struct {
	ID         int64
	ProjectKey string
	ReceivedAt time.Time
	Route      string
	Accepted   bool
	Error      string
	Body       []byte
}

// InsertRawEnvelope stores one raw body and drops every row older than the
// newest keep in the SAME transaction. Pruning on write rather than on a timer
// is what keeps the ring bounded: an operator who lowers the setting sees the
// table shrink on the next event instead of waiting a prune cycle, and a
// process that dies mid-write cannot leave the ring over its ceiling.
func (s *Store) InsertRawEnvelope(env RawEnvelope, keep int) error {
	if keep <= 0 {
		return nil
	}
	received := env.ReceivedAt
	if received.IsZero() {
		received = time.Now()
	}
	accepted := 0
	if env.Accepted {
		accepted = 1
	}
	tx, err := s.wdb.Begin()
	if err != nil {
		return fmt.Errorf("insert envelope: begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO raw_envelopes (project_key, received_at, route, accepted, error, body)
		VALUES (?, ?, ?, ?, ?, ?)`,
		env.ProjectKey, received.Unix(), env.Route, accepted, env.Error, env.Body); err != nil {
		return fmt.Errorf("insert envelope: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM raw_envelopes WHERE id NOT IN
		(SELECT id FROM raw_envelopes ORDER BY id DESC LIMIT ?)`, keep); err != nil {
		return fmt.Errorf("prune envelopes: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("insert envelope: commit: %w", err)
	}
	return nil
}

// RawEnvelopes lists the newest bodies, newest first, for the ingest log.
func (s *Store) RawEnvelopes(limit int) ([]RawEnvelope, error) {
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	rows, err := s.rdb.Query(`SELECT id, project_key, received_at, route, accepted, error, body
		FROM raw_envelopes ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RawEnvelope
	for rows.Next() {
		var env RawEnvelope
		var received sql.NullInt64
		var accepted int
		if err := rows.Scan(&env.ID, &env.ProjectKey, &received, &env.Route, &accepted, &env.Error, &env.Body); err != nil {
			return nil, err
		}
		if received.Valid {
			env.ReceivedAt = time.Unix(received.Int64, 0)
		}
		env.Accepted = accepted == 1
		out = append(out, env)
	}
	return out, rows.Err()
}
