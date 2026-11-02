package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/iniznet/nightpost/internal/store"
)

// This file guards the triage-first redesign (iniznet/fenrirealm-tracker#525).
// Each test names the acceptance item it pins, so a later refactor cannot
// quietly restore the shell it replaced.

// seedTwoProjects gives the landing page two projects to rank across.
func seedTwoProjects(t *testing.T, st *store.Store) {
	t.Helper()
	seedUI(t, st)
	fp := store.ForException("TypeError", "cannot read cover of undefined", "SeriesCard:42")
	if _, err := st.RecordError("front", "TypeError", "cannot read cover of undefined", fp, "production", "error", map[string]any{
		"level": "error",
		"exception": map[string]any{"values": []any{map[string]any{
			"type": "TypeError", "value": "cannot read cover of undefined",
		}}},
	}); err != nil {
		t.Fatalf("seed front error: %v", err)
	}
	if err := st.RecordRelease("backend", "v9.9.9", "production"); err != nil {
		t.Fatalf("seed release: %v", err)
	}
	if _, err := st.RecordQuery("backend", "select * from series where slug = ?", 12.5, "mysql", "production"); err != nil {
		t.Fatalf("seed query: %v", err)
	}
}

// Acceptance 1: the landing page names defects, with the project on every row,
// instead of rendering cards plus unattributable tables.
func TestNowRanksTopDefectsAcrossProjects(t *testing.T) {
	srv, st := newTestServer(t)
	seedTwoProjects(t, st)

	body := readAll(t, mustGet(t, srv.URL+"/"))

	for _, want := range []string{"class=\"rank\"", "Top defects right now", "cannot read cover of undefined", "user 42 not found"} {
		if !strings.Contains(body, want) {
			t.Errorf("all-projects dashboard missing %q", want)
		}
	}
	if !strings.Contains(body, "<td class=\"mono dim\">front</td>") || !strings.Contains(body, "<td class=\"mono dim\">backend</td>") {
		t.Error("ranked defects do not carry the project that produced them")
	}
	if !strings.Contains(body, "/issues/") {
		t.Error("ranked defects are not reachable from the dashboard")
	}
	// Acceptance 4: aggregates stay attributable too.
	if !strings.Contains(body, "<th scope=\"col\">Project</th>") {
		t.Error("aggregate tables lost their Project column")
	}
}

// Acceptance 2: a page filter carries the org dimension. Dropping it silently
// widened scope to every tenant while the header looked unchanged.
func TestFilterFormsKeepOrgScope(t *testing.T) {
	srv, st := newTestServer(t)
	seedUI(t, st)

	body := readAll(t, mustGet(t, srv.URL+"/errors?org=acme&q=timeout"))
	if !strings.Contains(body, "name=\"org\" value=\"acme\"") {
		t.Error("the issues filter form dropped the org scope")
	}
	if !strings.Contains(body, "name=\"q\" value=\"timeout\"") {
		t.Error("the issues filter form lost its own query")
	}
	if !strings.Contains(body, "acme") {
		t.Error("scope line missing from the page header")
	}
}

// Acceptance 6a: a page-level failure renders inside the shell, with a way out.
func TestPageErrorRendersInsideLayout(t *testing.T) {
	srv, _ := newTestServer(t)

	resp := mustGet(t, srv.URL+"/issues/424242")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body := readAll(t, resp)
	for _, want := range []string{"class=\"appbar\"", "Retry this page", "Back to Now"} {
		if !strings.Contains(body, want) {
			t.Errorf("error page missing %q", want)
		}
	}
	if strings.Contains(body, "404 page not found") {
		t.Error("error page fell back to the bare http.Error response")
	}
}

// Acceptance 5: one level = one treatment. Sections are the only nav in the
// bar, and the active one is announced, not only coloured.
func TestShellHasFourSectionsWithAriaCurrent(t *testing.T) {
	srv, st := newTestServer(t)
	seedUI(t, st)

	body := readAll(t, mustGet(t, srv.URL+"/errors"))
	for _, want := range []string{">Now</a>", ">Errors</a>", ">Performance</a>", ">Configure</a>"} {
		if !strings.Contains(body, want) {
			t.Errorf("section nav missing %q", want)
		}
	}
	if !strings.Contains(body, "aria-current=\"page\">Errors</a>") {
		t.Error("active section is not announced with aria-current")
	}
	if !strings.Contains(body, "aria-current=\"page\">Issues</a>") {
		t.Error("active tab is not announced with aria-current")
	}
}

// Acceptance 6b: the way back from a detail page lands on the list the reader
// left, filters included.
func TestDetailBackLinkKeepsArrivalURL(t *testing.T) {
	srv, st := newTestServer(t)
	seedUI(t, st)
	issues, err := st.Issues(store.IssueQuery{Project: "backend", Type: "error", Status: "open", Limit: 1})
	if err != nil || len(issues) == 0 {
		t.Fatalf("seed issue: %v", err)
	}
	id := strconv.FormatInt(issues[0].ID, 10)

	from := url.QueryEscape("/errors?q=timeout&range=7d")
	body := readAll(t, mustGet(t, srv.URL+"/issues/"+id+"?from="+from))
	if !strings.Contains(body, "href=\"/errors?q=timeout") {
		t.Error("detail page did not carry the arrival URL back to the list")
	}
	// A foreign or absolute target must not become an open redirect.
	foreign := readAll(t, mustGet(t, srv.URL+"/issues/"+id+"?from="+url.QueryEscape("https://evil.example/x")))
	if strings.Contains(foreign, "evil.example") {
		t.Error("detail page accepted an absolute back target")
	}
}

func mustGet(t *testing.T, u string) *http.Response {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	return resp
}
