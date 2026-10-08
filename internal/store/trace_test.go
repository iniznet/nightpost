package store

import (
	"testing"
	"time"
)

func TestProfilesByTraceOrdersLegsChronologically(t *testing.T) {
	st := newTestStore(t)
	for _, tr := range []struct{ name, trace string }{
		{"GET /page", "t-abc"},
		{"GET v2.chapters.showChapter", "t-abc"},
		{"GET /other", "t-other"},
	} {
		if _, err := st.RecordProfile(ProfileInput{
			ProjectKey: "front", Type: ProfileTypeRequest, Name: tr.name, DurationMs: 100, TraceID: tr.trace,
			Spans: []Span{{Name: "request", DurationMs: 100}},
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
		// Ensure created_at second-resolution ordering is not the only tiebreak.
		time.Sleep(time.Millisecond)
	}
	legs, err := st.ProfilesByTrace("t-abc", 20)
	if err != nil {
		t.Fatalf("byTrace: %v", err)
	}
	if len(legs) != 2 {
		t.Fatalf("expected 2 legs, got %d", len(legs))
	}
	if legs[0].Name != "GET /page" || legs[1].Name != "GET v2.chapters.showChapter" {
		t.Errorf("legs out of order: %s then %s", legs[0].Name, legs[1].Name)
	}
	if legs[0].TraceID != "t-abc" {
		t.Errorf("trace id lost: %q", legs[0].TraceID)
	}
}

func TestSpanStartMsSurvivesRoundTrip(t *testing.T) {
	st := newTestStore(t)
	id, err := st.RecordProfile(ProfileInput{
		ProjectKey: "front", Type: ProfileTypeRequest, Name: "GET /page", DurationMs: 500,
		Spans: []Span{
			{Name: "ssr", DurationMs: 120, StartMs: 0},
			{Name: "backend fetch", DurationMs: 300, StartMs: 120},
			{Name: "render", DurationMs: 80, StartMs: 420},
		},
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	p, err := st.Profile(id)
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	if len(p.Spans) != 3 || p.Spans[1].StartMs != 120 || p.Spans[2].StartMs != 420 {
		t.Errorf("start_ms lost: %+v", p.Spans)
	}
}
