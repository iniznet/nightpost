package store

import (
	"fmt"
	"os"
	"sort"
	"testing"
	"time"
)

// TestScaleLatency is the measurement. The phase says "start only when
// dashboard slowness is measured — record the numbers first", so this IS the
// precondition: a production-shaped dataset, every dashboard query timed, the
// retention cycle timed, and the single-writer ingest ceiling timed.
//
// Env-gated so CI never pays for it:
//
//	MONITOR_SCALE=1 go test ./internal/store/ -run TestScaleLatency -v
//
// Seeding goes through raw SQL in ONE transaction (the public recorders commit
// per row, which would make the harness slower than the thing it measures).
func TestScaleLatency(t *testing.T) {
	if os.Getenv("MONITOR_SCALE") == "" {
		t.Skip("set MONITOR_SCALE=1 to run the scale measurement")
	}
	var st *Store
	if dbPath := os.Getenv("MONITOR_SCALE_DB"); dbPath != "" {
		// A real file, so the binary can be pointed at the result and the
		// dashboard checked against the same data the numbers came from.
		var err error
		st, err = Open(dbPath)
		if err != nil {
			t.Fatalf("open %s: %v", dbPath, err)
		}
		defer st.Close()
	} else {
		st = newTestStore(t)
	}

	const (
		errorIssues = 120   // distinct error fingerprints
		logIssues   = 20000 // a chatty app: EVERY distinct log message is an issue row
		perIssue    = 800
		logPerIssue = 5
		profiles    = 60000
		staleIssues = 500 // last seen a year ago: the archive policy's target
	)
	start := time.Now()
	seedScale(t, st, errorIssues, perIssue, logIssues, logPerIssue, profiles, staleIssues)
	t.Logf("seeded in %v: %d error issues x %d occurrences, %d log issues x %d, %d profiles",
		time.Since(start).Round(time.Millisecond), errorIssues, perIssue, logIssues, logPerIssue, profiles)

	var issues, occ, prof int64
	_ = st.rdb.QueryRow(`SELECT COUNT(*) FROM issues`).Scan(&issues)
	_ = st.rdb.QueryRow(`SELECT COUNT(*) FROM occurrences`).Scan(&occ)
	_ = st.rdb.QueryRow(`SELECT COUNT(*) FROM profiles`).Scan(&prof)
	t.Logf("stored: issues=%d occurrences=%d profiles=%d", issues, occ, prof)

	for _, q := range []struct {
		name string
		run  func() error
	}{
		{"Issues(errors,50)", func() error { _, err := st.Issues(IssueQuery{Project: "app", Type: "error", Limit: 50}); return err }},
		{"Issues(logs,50)", func() error { _, err := st.Issues(IssueQuery{Project: "app", Type: "log", Limit: 50}); return err }},
		{"IssueTotal", func() error { _, err := st.IssueTotal("app"); return err }},
		{"LogLevels", func() error { _, err := st.LogLevels("app"); return err }},
		{"HourBuckets(24h,error)", func() error { _, err := st.HourBuckets("app", 24, "error"); return err }},
		{"Profiles(50)", func() error { _, err := st.Profiles("app", 50); return err }},
		{"Traces(24h,50)", func() error { _, err := st.Traces("app", 24, 50); return err }},
		{"QueryLeaderboard", func() error { _, err := st.QueryLeaderboard("app", 24, 50); return err }},
		{"JobLeaderboard", func() error { _, err := st.JobLeaderboard("app", 24, 50); return err }},
	} {
		p50, worst := timeQuery(t, q.run, 5)
		if worst > 250*time.Millisecond {
			t.Logf("SLOW  %-22s p50=%-10v max=%-10v", q.name, p50, worst)
		} else {
			t.Logf("ok    %-22s p50=%-10v max=%-10v", q.name, p50, worst)
		}
	}

	// The retention cycle, which shares the single writer with ingest. The
	// per-issue cap is the pass rewrote.
	start = time.Now()
	n1, err := st.PruneIssueOccurrences(50)
	if err != nil {
		t.Fatalf("prune per-issue cap: %v", err)
	}
	t.Logf("PruneIssueOccurrences(50): %v, %d rows (issues=%d, chunk=%d statements)",
		time.Since(start).Round(time.Millisecond), n1, issues, (issues+pruneChunk-1)/pruneChunk)

	start = time.Now()
	n2, err := st.PruneOccurrencesBefore(time.Now().AddDate(0, 0, -30), 500)
	if err != nil {
		t.Fatalf("prune by age: %v", err)
	}
	t.Logf("PruneOccurrencesBefore(30d): %v, %d rows", time.Since(start).Round(time.Millisecond), n2)

	// The archive is what bounds the issues table: the staleIssues rows
	// are a year old with a year-old occurrence, so exactly they must archive.
	// MONITOR_SCALE_NO_ARCHIVE leaves the stale tail un-archived so the BINARY's
	// own retention tick can be watched doing it (it logs the archive count).
	if os.Getenv("MONITOR_SCALE_NO_ARCHIVE") != "" {
		t.Log("MONITOR_SCALE_NO_ARCHIVE set: leaving stale issues for the server's retention tick")
	} else {
		before, err := st.IssueTotal("app")
		if err != nil {
			t.Fatalf("issue total: %v", err)
		}
		start = time.Now()
		n3, err := st.ArchiveStaleIssues(30)
		if err != nil {
			t.Fatalf("archive: %v", err)
		}
		after, err := st.IssueTotal("app")
		if err != nil {
			t.Fatalf("issue total: %v", err)
		}
		t.Logf("ArchiveStaleIssues(30): %v, archived %d (live issue total %d -> %d, seeded stale=%d)",
			time.Since(start).Round(time.Millisecond), n3, before, after, staleIssues)
	}

	// RunRetention ARCHIVES too, so the NO_ARCHIVE mode has to skip this as well —
	// otherwise the harness consumes the stale tail before the server's own tick
	// can be observed doing it.
	if os.Getenv("MONITOR_SCALE_NO_ARCHIVE") == "" {
		start = time.Now()
		summary, err := st.RunRetention(30, 5000)
		if err != nil {
			t.Fatalf("retention: %v", err)
		}
		t.Logf("RunRetention(full cycle): %v — %s", time.Since(start).Round(time.Millisecond), summary)
	}

	// The write ceiling: every ingest write serializes through one WAL writer.
	const n = 2000
	start = time.Now()
	for i := 0; i < n; i++ {
		if _, err := st.RecordProfile(ProfileInput{
			ProjectKey: "app", Type: ProfileTypeRequest,
			Name: "GET /scale", DurationMs: 12.5,
		}); err != nil {
			t.Fatalf("record profile: %v", err)
		}
	}
	elapsed := time.Since(start)
	t.Logf("ingest write ceiling: %d profile writes in %v = %.0f events/s (single writer)",
		n, elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds())
}

func timeQuery(t *testing.T, run func() error, reps int) (time.Duration, time.Duration) {
	t.Helper()
	took := make([]time.Duration, 0, reps)
	for i := 0; i < reps; i++ {
		s := time.Now()
		if err := run(); err != nil {
			t.Fatalf("query: %v", err)
		}
		took = append(took, time.Since(s))
	}
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	return took[len(took)/2], took[len(took)-1]
}

func seedScale(t *testing.T, st *Store, errorIssues, perIssue, logIssues, logPerIssue, profiles, staleIssues int) {
	t.Helper()
	tx, err := st.wdb.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	insIssue, err := tx.Prepare(`INSERT INTO issues
		(project_key, fingerprint, type, title, exception_class, level, environment, channel, count, first_seen_at, last_seen_at)
		VALUES ('app', ?, ?, ?, ?, 'error', 'production', '', 1, ?, ?)`)
	if err != nil {
		t.Fatalf("prepare issues: %v", err)
	}
	insOcc, err := tx.Prepare(`INSERT INTO occurrences (issue_id, type, payload, seen_at, user_id)
		VALUES (?, 'error', NULL, ?, '')`)
	if err != nil {
		t.Fatalf("prepare occurrences: %v", err)
	}
	insProf, err := tx.Prepare(`INSERT INTO profiles
		(project_key, type, name, duration_ms, environment, trace_id, spans, hotspots, tree, created_at)
		VALUES ('app', 'request', ?, ?, 'production', '', '[]', '[]', NULL, ?)`)
	if err != nil {
		t.Fatalf("prepare profiles: %v", err)
	}

	now := time.Now().Unix()
	for i := 0; i < errorIssues; i++ {
		res, err := insIssue.Exec(fmt.Sprintf("err-%d", i), "error", fmt.Sprintf("QueryException: scale issue %d", i), "QueryException", now-86400, now)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		id, _ := res.LastInsertId()
		for j := 0; j < perIssue; j++ {
			if _, err := insOcc.Exec(id, now-int64(j)); err != nil {
				t.Fatalf("occurrence: %v", err)
			}
		}
	}
	// The chatty-log half: logs are stored as issues with type='log', so raw log
	// volume is what makes this table (never pruned before the archive policy)
	// grow without bound.
	for i := 0; i < logIssues; i++ {
		res, err := insIssue.Exec(fmt.Sprintf("log-%d", i), "log", fmt.Sprintf("scale log line %d", i), "", now-3600, now)
		if err != nil {
			t.Fatalf("log issue: %v", err)
		}
		id, _ := res.LastInsertId()
		for j := 0; j < logPerIssue; j++ {
			if _, err := insOcc.Exec(id, now-int64(j)); err != nil {
				t.Fatalf("log occurrence: %v", err)
			}
		}
	}
	// A year-old tail of log issues: nothing about them is recent, which is
	// precisely what the archive policy is for.
	yearAgo := now - 365*86400
	for i := 0; i < staleIssues; i++ {
		res, err := insIssue.Exec(fmt.Sprintf("stale-log-%d", i), "log", fmt.Sprintf("a log line nobody will see again %d", i), "", yearAgo, yearAgo)
		if err != nil {
			t.Fatalf("stale issue: %v", err)
		}
		id, _ := res.LastInsertId()
		if _, err := insOcc.Exec(id, yearAgo); err != nil {
			t.Fatalf("stale occurrence: %v", err)
		}
	}
	for i := 0; i < profiles; i++ {
		if _, err := insProf.Exec(fmt.Sprintf("GET /scale/%d", i%50), float64(i%400)+1.5, now-int64(i%86400)); err != nil {
			t.Fatalf("profile: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
