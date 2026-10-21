package store

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// User impact : every occurrence records the opaque id of the acting
// user when the producer knew it. Raw id only — never email, name or any
// other PII (redaction rule from the ticket invariants). Counts answer
// "how many distinct readers felt this?" without storing identities beyond
// the id itself.

// extractUserID reads Sentry's event.user.id or a first-party top-level
// user_id from an occurrence payload. Anything non-scalar is ignored.
func extractUserID(payload any) string {
	m, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	if v := idScalar(m["user_id"]); v != "" {
		return v
	}
	if u, ok := m["user"].(map[string]any); ok {
		return idScalar(u["id"])
	}
	return ""
}

func idScalar(v any) string {
	switch x := v.(type) {
	case string:
		x = strings.TrimSpace(x)
		if x == "" || len(x) > 64 {
			return ""
		}
		return x
	case float64:
		if x > 0 && x < 1<<53 && x == math.Trunc(x) {
			return strconv.FormatInt(int64(x), 10)
		}
	case int64:
		if x > 0 {
			return strconv.FormatInt(x, 10)
		}
	case int:
		if x > 0 {
			return strconv.Itoa(x)
		}
	}
	return ""
}

// UserCounts returns distinct affected users per issue. Only ever called
// with bounded ID sets (page size), never with naked user lookups.
func (s *Store) UserCounts(issueIDs []int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	if len(issueIDs) == 0 {
		return out, nil
	}
	ph := make([]string, 0, len(issueIDs))
	args := make([]any, 0, len(issueIDs))
	for _, id := range issueIDs {
		ph = append(ph, "?")
		args = append(args, id)
	}
	rows, err := s.rdb.Query(`SELECT issue_id, COUNT(DISTINCT user_id) FROM occurrences
		WHERE issue_id IN (`+strings.Join(ph, ",")+`) AND user_id <> '' GROUP BY issue_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("user counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
