package ingest

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestShaperBurstThenExhaust(t *testing.T) {
	sh := NewShaper(0.0001, 3)
	for i := 0; i < 3; i++ {
		if !sh.Allow("backend") {
			t.Fatalf("event %d: expected burst allowance", i)
		}
	}
	if sh.Allow("backend") {
		t.Fatal("expected exhaustion after burst")
	}
}

func TestShaperProjectsIsolated(t *testing.T) {
	sh := NewShaper(0.0001, 1)
	if !sh.Allow("backend") {
		t.Fatal("backend first event should pass")
	}
	if sh.Allow("backend") {
		t.Fatal("backend second event should be rejected")
	}
	if !sh.Allow("front") {
		t.Fatal("front must have its own budget")
	}
}

// TestShaperBudgetAppliesLive pins iniznet/fenrirealm-tracker#363: a budget
// change reaches the RUNNING shaper, so a saved ingest_rate/ingest_burst no
// longer needs a restart to apply.
func TestShaperBudgetAppliesLive(t *testing.T) {
	sh := NewShaper(0.0001, 1)
	if !sh.Allow("backend") {
		t.Fatal("precondition: burst 1 admits one event")
	}
	if sh.Allow("backend") {
		t.Fatal("precondition: burst 1 rejects the second event")
	}

	// Raise the ceiling: a fresh project sees the new burst immediately.
	if rate, burst := sh.SetBudget(0.0001, 5); rate != 0.0001 || burst != 5 {
		t.Fatalf("SetBudget reported %v/%v, want 0.0001/5", rate, burst)
	}
	if rate, burst := sh.Budget(); rate != 0.0001 || burst != 5 {
		t.Fatalf("Budget() = %v/%v, want 0.0001/5", rate, burst)
	}
	for i := 0; i < 5; i++ {
		if !sh.Allow("front") {
			t.Fatalf("raised burst must admit event %d", i+1)
		}
	}
	if sh.Allow("front") {
		t.Fatal("raised burst must still reject the sixth event")
	}

	// Lower it again: the same running shaper throttles at the new ceiling.
	sh.SetBudget(0.0001, 2)
	if !sh.Allow("admin") || !sh.Allow("admin") {
		t.Fatal("lowered burst must admit two events")
	}
	if sh.Allow("admin") {
		t.Fatal("lowered burst must reject the third event")
	}
}

// TestShaperBudgetFallsBackWhenUnusable is the fail-safe: a corrupted settings
// row must never stop admission entirely.
func TestShaperBudgetFallsBackWhenUnusable(t *testing.T) {
	sh := NewShaper(0.0001, 1)
	rate, burst := sh.SetBudget(0, -3)
	if rate != 50 || burst != 100 {
		t.Fatalf("garbage budget applied %v/%v, want the shipped 50/100", rate, burst)
	}
}

func TestWithShaperReturns429WithoutRunningHandler(t *testing.T) {
	sh := NewShaper(0.0001, 1)
	if !sh.Allow("backend") {
		t.Fatal("precondition: first event passes")
	}
	h := WithShaper(sh, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("wrapped handler must not run past the budget")
	})
	req := httptest.NewRequest(http.MethodPost, "/ingest/backend/error", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
}
