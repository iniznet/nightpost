package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestSchemaV2TablesAndUniqueness pins the v2 table shape and the uniqueness
// contracts the journeys model depends on. One temp database per test keeps
// the single shared-writer store out of other tests' rows; the round-trip
// goes through the public helpers so a schema drift cannot pass silently.
func TestSchemaV2TablesAndUniqueness(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "v2.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	for _, table := range []string{"services", "routes", "spans", "journeys", "journey_hops", "sessions", "raw_envelopes"} {
		var n int
		if err := st.rdb.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if n != 1 {
			t.Fatalf("table %s missing after Open", table)
		}
	}

	svcID, err := st.UpsertService("p", "api", "php", "prod", false)
	if err != nil {
		t.Fatalf("upsert service: %v", err)
	}
	if _, err := st.wdb.Exec(`INSERT INTO services (project_key, name) VALUES ('p', 'api')`); err == nil {
		t.Fatal("expected duplicate service (project_key, name) to be rejected")
	}

	routeID, err := st.UpsertRoute(svcID, "GET", "/x", "http")
	if err != nil {
		t.Fatalf("upsert route: %v", err)
	}
	if _, err := st.wdb.Exec(`INSERT INTO routes (service_id, method, pattern) VALUES (?, 'GET', '/x')`, svcID); err == nil {
		t.Fatal("expected duplicate route (service_id, method, pattern) to be rejected")
	}

	now := time.Now()
	jid, err := st.UpsertJourney(Journey{
		PatternKey:   "k",
		EntryKind:    "api",
		EntryRouteID: routeID,
		HopCount:     2,
		Samples:      5,
		P50Ms:        10,
		P95Ms:        20,
		FirstSeenAt:  now,
		LastSeenAt:   now,
	}, []JourneyHop{
		{Position: 0, RouteID: routeID, Status: "ok", P95Ms: 20, Share: 1},
		{Position: 1, RouteID: routeID, Status: "unreported", P95Ms: 0, Share: 0},
	})
	if err != nil {
		t.Fatalf("upsert journey: %v", err)
	}
	if _, err := st.wdb.Exec(`INSERT INTO journeys (pattern_key, entry_kind) VALUES ('k', 'api')`); err == nil {
		t.Fatal("expected duplicate journey pattern_key to be rejected")
	}

	got, err := st.Journeys("p", "api", 10)
	if err != nil {
		t.Fatalf("journeys: %v", err)
	}
	var found *Journey
	for i := range got {
		if got[i].ID == jid {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("journey %d not returned by Journeys", jid)
	}
	if found.PatternKey != "k" || found.EntryKind != "api" || found.HopCount != 2 || found.Samples != 5 {
		t.Fatalf("journey round-trip mismatch: %+v", found)
	}

	hops, err := st.JourneyHops(jid)
	if err != nil {
		t.Fatalf("journey hops: %v", err)
	}
	if len(hops) != 2 {
		t.Fatalf("expected 2 hops, got %d", len(hops))
	}
	if hops[0].Position != 0 || hops[0].Status != "ok" || hops[1].Position != 1 || hops[1].Status != "unreported" {
		t.Fatalf("hop round-trip mismatch: %+v", hops)
	}
}
