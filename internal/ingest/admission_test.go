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
