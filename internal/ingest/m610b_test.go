package ingest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// gateFor builds a gate in front of a terminal handler that just records
// admission, so each test asserts the GATE's decision rather than a route's.
func gateFor(t *testing.T, g *KeyGate) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingest/{project}/profile", WithKeyGate(g, PathProject,
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	return mux
}

func postProject(t *testing.T, mux *http.ServeMux, project string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ingest/"+project+"/profile", nil))
	return rec.Code
}

// TestKeyGateRequireRegisteredRefusesUnknownProject is the M6.10 P0: a POST
// must not be able to MINT a project by naming it.
func TestKeyGateRequireRegisteredRefusesUnknownProject(t *testing.T) {
	st := newBenchStore(t)
	g := NewKeyGate(st, 0)
	g.RequireRegistered(true)
	mux := gateFor(t, g)

	if code := postProject(t, mux, "nobody-registered-this"); code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", code)
	}
	exists, err := st.ProjectExists("nobody-registered-this")
	if err != nil {
		t.Fatalf("exists: %v", err)
	}
	if exists {
		t.Error("registration-required mode created a project row")
	}
	if g.Refusals() != 1 {
		t.Errorf("refusals = %d, want 1 (the strip must count this)", g.Refusals())
	}
	if g.UnknownAdmits() != 0 {
		t.Errorf("unknown admits = %d, want 0", g.UnknownAdmits())
	}
}

// TestKeyGateRequireRegisteredAdmitsKnownUnsignedProject is the Expand/Contract
// half: flipping the flag must never break a producer that already exists.
func TestKeyGateRequireRegisteredAdmitsKnownUnsignedProject(t *testing.T) {
	st := newBenchStore(t)
	if err := st.EnsureProject("already-here"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	g := NewKeyGate(st, 0)
	g.RequireRegistered(true)
	mux := gateFor(t, g)

	if code := postProject(t, mux, "already-here"); code != http.StatusOK {
		t.Fatalf("a known unsigned project must stay admitted, got %d", code)
	}
	// And it is admitted without being re-admitted as "unknown".
	if g.UnknownAdmits() != 0 {
		t.Errorf("unknown admits = %d, want 0 for a registered project", g.UnknownAdmits())
	}
}

// TestKeyGateUnknownProjectsStillAdmittedWithoutTheFlag pins the legacy mode.
func TestKeyGateUnknownProjectsStillAdmittedWithoutTheFlag(t *testing.T) {
	st := newBenchStore(t)
	g := NewKeyGate(st, 0)
	if code := postProject(t, gateFor(t, g), "legacy-client"); code != http.StatusOK {
		t.Fatalf("legacy permissive mode must still admit, got %d", code)
	}
	if exists, _ := st.ProjectExists("legacy-client"); !exists {
		t.Error("legacy mode must auto-provision the project")
	}
}

// TestKnownSetIsBounded is the other M6.10 P0 half: the project name is chosen
// by the caller, so the set that remembers "already decided" must not grow with
// it. An evicted name costs one extra decision, never a wrong admission.
func TestKnownSetIsBounded(t *testing.T) {
	s := newKnownSet(64)
	for i := 0; i < 5000; i++ {
		s.add(fmt.Sprintf("project-%d", i))
	}
	if got := s.size(); got > 64 {
		t.Errorf("knownSet grew to %d entries after 5000 distinct names, want <= 64", got)
	}
	// Re-adding an existing name must not duplicate or double-count.
	s.add("project-1")
	if got := s.size(); got > 64 {
		t.Errorf("re-add grew the set to %d", got)
	}
	// A zero/negative cap must still produce a usable bound, not an unbounded map.
	if d := newKnownSet(0); d.max != 4096 {
		t.Errorf("default cap = %d, want 4096", d.max)
	}
}
