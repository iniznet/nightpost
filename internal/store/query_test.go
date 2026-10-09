package store

import (
	"strings"
	"testing"
	"time"
)

func TestRecordQueryGroupsShapes(t *testing.T) {
	st := newTestStore(t)
	v1 := "select * from chapters where id = 42 and slug = 'abc'"
	v2 := "select * from chapters where id = 43 and slug = 'zzz'"
	if _, err := st.RecordQuery("backend", v1, 120, "mysql", "production"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordQuery("backend", v2, 30, "mysql", "production"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordQuery("backend", "select 1", 5, "redis", "production"); err != nil {
		t.Fatal(err)
	}
	rows, err := st.QueryLeaderboard("backend", 24, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("parameterized variants must collapse, got %d rows: %+v", len(rows), rows)
	}
	top := rows[0]
	if top.Count != 2 || top.TotalMs != 150 || top.MaxMs != 120 {
		t.Fatalf("aggregates wrong: %+v", top)
	}
	if !strings.Contains(top.SQL, "<n>") || !strings.Contains(top.SQL, "<s>") {
		t.Fatalf("sql must be normalized, got %q", top.SQL)
	}
	if !strings.Contains(top.SQL, "chapters") {
		t.Fatal("table names must survive normalization")
	}
	if top.Connection != "mysql" {
		t.Fatalf("connection must persist, got %q", top.Connection)
	}
	// Placeholder SQL from the Laravel producer must survive normalize()
	// unchanged — one grouping authority across the wire.
	if _, err := st.RecordQuery("backend", "select * from users where id = ?", 9, "mysql", "production"); err != nil {
		t.Fatal(err)
	}
	ph := ForQuery("select * from users where id = ?")
	found := false
	for _, r := range rows {
		if r.Fingerprint == ph {
			found = true
		}
	}
	if !found {
		if fresh, ferr := st.QueryLeaderboard("backend", 24, 10); ferr != nil {
			t.Fatal(ferr)
		} else {
			for _, r := range fresh {
				if r.Fingerprint == ph {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("placeholder-shaped sql must fingerprint stably")
	}
	// Paired issue rows exist with type=query.
	issues, err := st.Issues(IssueQuery{Project: "backend", Type: "query", Status: "all", Limit: 10})
	if err != nil || len(issues) != 3 {
		t.Fatalf("query issues: %d %v", len(issues), err)
	}
	want := ForQuery(v1)
	var grouped *Issue
	for i := range issues {
		if issues[i].Fingerprint == want {
			grouped = &issues[i]
		}
	}
	if grouped == nil || grouped.Count != 2 {
		t.Fatalf("issue count must track events, got %+v", grouped)
	}
}

func TestPruneQueryStats(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.RecordQuery("backend", "select 1", 5, "mysql", "production"); err != nil {
		t.Fatal(err)
	}
	n, err := st.PruneQueryStats(time.Now().Add(time.Hour).Unix(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("stale row must prune, removed %d", n)
	}
	rows, _ := st.QueryLeaderboard("backend", 24, 10)
	if len(rows) != 0 {
		t.Fatal("pruned")
	}
}

func TestForQueryStable(t *testing.T) {
	a := ForQuery("where id = 1")
	b := ForQuery("where id = 22")
	c := ForQuery("where id = 1")
	if a != b || a != c {
		t.Fatal("shape stability broken")
	}
	if ForQuery("select 1") == ForQuery("update t set x=1") {
		t.Fatal("distinct statements must not collide")
	}
	if ForQuery("select * from orders_2024") == ForQuery("select * from orders_2025") {
		t.Fatal("year-suffixed tables are distinct statements; numRe must not glue them")
	}
	if ForQuery("where id = 5") != ForQuery("where id = 6") {
		t.Fatal("standalone numeric literals must collapse")
	}
	if ForQuery("where id = ?") != ForQuery("where id = ?") {
		t.Fatal("placeholder sql must be stable")
	}
}
