package store

import "testing"

// Invariant: a route's row carries the other service's leg when the trace ids
// match, and an empty cell when they do not. A blank that reads as "fast"
// would hide exactly the outage this column exists for.
func TestRouteIndexCountsBackendLeg(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.RecordProfile(ProfileInput{ProjectKey: "front", Type: ProfileTypeRequest, Name: "GET /home", DurationMs: 120, TraceID: "t1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordProfile(ProfileInput{ProjectKey: "backend", Type: ProfileTypeRequest, Name: "GET /api/home", DurationMs: 80, TraceID: "t1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordProfile(ProfileInput{ProjectKey: "front", Type: ProfileTypeRequest, Name: "GET /quiet", DurationMs: 10}); err != nil {
		t.Fatal(err)
	}

	rows, err := st.RouteIndex("", 24, 50)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]RouteIndexRow{}
	for _, row := range rows {
		byName[row.Project+"/"+row.Name] = row
	}

	traced, ok := byName["front/GET /home"]
	if !ok {
		t.Fatalf("front/GET /home missing from %v", byName)
	}
	if traced.Traces != 1 || traced.Traced != 1 {
		t.Errorf("traces/traced = %d/%d, want 1/1", traced.Traces, traced.Traced)
	}
	if traced.BackendN != 1 {
		t.Errorf("backend legs = %d, want 1", traced.BackendN)
	}
	if traced.BackendP95 != 80 {
		t.Errorf("backend p95 = %v, want 80", traced.BackendP95)
	}

	quiet, ok := byName["front/GET /quiet"]
	if !ok {
		t.Fatalf("front/GET /quiet missing from %v", byName)
	}
	if quiet.BackendN != 0 || quiet.BackendP95 != 0 {
		t.Errorf("a trace with no second leg must stay empty, got n=%d p95=%v", quiet.BackendN, quiet.BackendP95)
	}
}
