package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/iniznet/nightpost/internal/store"
)

// Invariant: the walk reads in the order a request happens. A render step
// sorted after a downstream leg would tell the reader the opposite story.
func TestBuildStepsOrdersRenderSpansLegsThenGap(t *testing.T) {
	now := time.Now()
	front := store.Profile{
		ProjectKey: "front",
		Name:       "GET /main/(home)",
		DurationMs: 40,
		CreatedAt:  now,
		Spans:      []store.Span{{Name: "backend fetch", DurationMs: 25}},
	}
	steps := buildSteps([]store.Profile{front})
	kinds := make([]string, 0, len(steps))
	for _, s := range steps {
		kinds = append(kinds, s.Kind)
	}
	want := []string{stepRender, stepSpan, stepMissing}
	if len(kinds) != len(want) {
		t.Fatalf("step kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("step kinds = %v, want %v", kinds, want)
		}
	}
	if steps[2].Note == "" {
		t.Fatal("a missing leg must say why it is missing")
	}
}

// Invariant: a second service that reported is a leg, never a gap row.
func TestBuildStepsStitchesSecondServiceLeg(t *testing.T) {
	now := time.Now()
	steps := buildSteps([]store.Profile{
		{ProjectKey: "front", Name: "GET /main/(home)", DurationMs: 40, CreatedAt: now},
		{ProjectKey: "fenrirealm-backend-production", Name: "GET /api/new/home", DurationMs: 25, CreatedAt: now.Add(time.Second)},
	})
	if steps[len(steps)-1].Kind == stepMissing {
		t.Fatal("a trace with two legs must not render a gap row")
	}
	if steps[len(steps)-1].Kind != stepLeg {
		t.Fatalf("last step = %q, want %q", steps[len(steps)-1].Kind, stepLeg)
	}
}

// Invariant: the pre-merge paths keep the scope a bookmark carried.
func TestPerfRedirectKeepsScope(t *testing.T) {
	s := &server{}
	rec := httptest.NewRecorder()
	s.perfRedirect(rec, httptest.NewRequest(http.MethodGet, "/traces?range=7d&project=front", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != "/performance?range=7d&project=front" {
		t.Fatalf("location = %q", got)
	}
}
