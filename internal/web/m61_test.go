package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// everyRoute is the page surface with ZERO query params. It is the I1
// guard: a page that needs a param to render is a broken page.
var everyRoute = []string{
	"/",
	"/errors", "/errors?tab=logs",
	"/issues", "/logs",
	"/performance", "/performance?tab=traces", "/performance?tab=hotspots",
	"/profiles", "/traces", "/slowest",
	"/queries", "/jobs", "/keys",
}

func TestEveryRouteRendersWithZeroParams(t *testing.T) {
	srv, st := newTestServer(t)
	seedUI(t, st)
	for _, path := range everyRoute {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body := readAll(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d", path, resp.StatusCode)
			continue
		}
		for _, want := range []string{"nightpost", "class=\"appbar\"", "<main class=\"wrap\" id=\"main\">"} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s: layout fragment %q missing", path, want)
			}
		}
	}
}

// TestUnknownTabNeverFails is the I2 guard: an unknown or empty tab resolves
// to the section default, never a 404 and never an empty page.
func TestUnknownTabNeverFails(t *testing.T) {
	srv, _ := newTestServer(t)
	cases := []struct{ path, want string }{
		// The exceptions tab was called "Issues" until #526.
		{"/errors?tab=nope", "Exceptions"},
		{"/errors?tab=", "Exceptions"},
		{"/performance?tab=nope", "Performance"},
		{"/performance?tab=", "Performance"},
	}
	for _, c := range cases {
		resp, err := http.Get(srv.URL + c.path)
		if err != nil {
			t.Fatalf("GET %s: %v", c.path, err)
		}
		body := readAll(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d (want 200)", c.path, resp.StatusCode)
			continue
		}
		if !strings.Contains(body, c.want) {
			t.Errorf("GET %s: default tab marker %q missing", c.path, c.want)
		}
	}
}

// TestLegacyRoutesServeConsolidatedContent is the I5 guard: Expand/Contract —
// the pre-paths keep answering with the same content as their
// consolidated section paths.
func TestLegacyRoutesServeConsolidatedContent(t *testing.T) {
	srv, st := newTestServer(t)
	seedUI(t, st)
	pairs := []struct{ legacy, consolidated, marker string }{
		{"/issues?project=backend", "/errors?project=backend", "user 42 not found"},
		{"/logs?project=backend", "/errors?tab=logs&project=backend", "login failed"},
	}
	for _, p := range pairs {
		for _, path := range []string{p.legacy, p.consolidated} {
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			body := readAll(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s: status %d", path, resp.StatusCode)
				continue
			}
			if !strings.Contains(body, p.marker) {
				t.Errorf("GET %s: marker %q missing", path, p.marker)
			}
		}
	}

	// The three route-performance paths merged into one page (#656). A bookmark
	// must land on the merged index with its scope, never on a 404. Queries and
	// Jobs are their own pages again (#658), so they are not in this list.
	for _, legacy := range []string{"/profiles?project=backend&type=request", "/traces?project=backend", "/slowest?project=backend"} {
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Get(srv.URL + legacy)
		if err != nil {
			t.Fatalf("GET %s: %v", legacy, err)
		}
		readAll(t, resp)
		if resp.StatusCode != http.StatusFound {
			t.Errorf("GET %s: status %d, want 302", legacy, resp.StatusCode)
			continue
		}
		if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/performance?") {
			t.Errorf("GET %s: location %q, want the merged index", legacy, loc)
		}
	}
}

// TestLogPseudoIssuesFoldOutOfIssueStream pins the Errors-section
// contract: the default issue stream carries error-class rows only, and the
// log row is reachable on the Logs tab.
func TestLogPseudoIssuesFoldOutOfIssueStream(t *testing.T) {
	srv, st := newTestServer(t)
	seedUI(t, st)

	resp, _ := http.Get(srv.URL + "/errors?project=backend")
	body := readAll(t, resp)
	if !strings.Contains(body, "user 42 not found") {
		t.Error("issue stream lost the error-class row")
	}
	if strings.Contains(body, "login failed") {
		t.Error("issue stream still carries the log-type pseudo-issue")
	}

	resp, _ = http.Get(srv.URL + "/errors?project=backend&tab=logs")
	body = readAll(t, resp)
	if !strings.Contains(body, "login failed") {
		t.Error("logs tab missing the seeded log")
	}
	if !strings.Contains(body, "auth") {
		t.Error("logs tab missing the channel")
	}
}

// TestToolbarSelectionIsReflected proves the persistent toolbar renders the
// parsed context back to the reader on every section.
func TestToolbarSelectionIsReflected(t *testing.T) {
	srv, st := newTestServer(t)
	seedUI(t, st)
	resp, _ := http.Get(srv.URL + "/errors?project=backend&range=7d&env=production")
	body := readAll(t, resp)
	for _, want := range []string{
		`value="backend" selected`,
		`value="7d" selected`,
		`value="production" selected`,
		"range=7d",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("toolbar missing %q", want)
		}
	}
}

// TestTracesIndexAndDeepLink covers the Traces tab plus the detail deep link.
func TestTracesIndexAndDeepLink(t *testing.T) {
	srv, st := newTestServer(t)
	if _, err := st.RecordProfile(store_ProfileInputFront()); err != nil {
		t.Fatalf("seed trace: %v", err)
	}
	// The traces index is one section of the merged Performance page (#656);
	// the path keeps answering with a redirect that carries the scope.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := client.Get(srv.URL + "/traces?project=front")
	readAll(t, resp)
	if resp.StatusCode != http.StatusFound {
		t.Errorf("traces index status %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/performance?project=front" {
		t.Errorf("traces index location %q", loc)
	}
	resp, _ = http.Get(srv.URL + "/traces/t1")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("trace detail status %d", resp.StatusCode)
	}
	readAll(t, resp)

	resp, _ = http.Get(srv.URL + fmt.Sprintf("/traces/missing-%d", 1))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown trace status %d, want 404", resp.StatusCode)
	}
	readAll(t, resp)
}


// Invariant: the Performance section is ONE merged route view plus two sibling
// pages (#658). Merging the route pages must not swallow the statement and
// queue surfaces, and a reader inside a route still sees which tab they are in.
func TestPerformanceTabsCarryThreeSiblings(t *testing.T) {
	srv, st := newTestServer(t)
	seedUI(t, st)
	cases := []struct{ path, current string }{
		{"/performance", "Overview"},
		{"/queries", "Queries"},
		{"/jobs", "Jobs"},
	}
	for _, c := range cases {
		resp, err := http.Get(srv.URL + c.path)
		if err != nil {
			t.Fatalf("GET %s: %v", c.path, err)
		}
		body := readAll(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d, want 200", c.path, resp.StatusCode)
		}
		for _, tab := range []string{"Overview", "Queries", "Jobs"} {
			if !strings.Contains(body, ">"+tab+"</a>") {
				t.Errorf("GET %s: tab %q missing from the strip", c.path, tab)
			}
		}
		if want := `aria-current="page">` + c.current; !strings.Contains(body, want) {
			t.Errorf("GET %s: %q is not marked current", c.path, c.current)
		}
	}
}

// The statement surface answers on its own page with its own heading.
func TestQueriesPageRendersLeaderboard(t *testing.T) {
	srv, st := newTestServer(t)
	seedUI(t, st)
	resp, err := http.Get(srv.URL + "/queries?project=backend")
	if err != nil {
		t.Fatalf("GET /queries: %v", err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "Slow queries") {
		t.Error("the queries page lost its heading")
	}
}

// The queue surface answers on its own page with its own heading.
func TestJobsPageRendersLeaderboard(t *testing.T) {
	srv, st := newTestServer(t)
	seedUI(t, st)
	resp, err := http.Get(srv.URL + "/jobs?project=backend")
	if err != nil {
		t.Fatalf("GET /jobs: %v", err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "Jobs &amp; commands") {
		t.Error("the jobs page lost its heading")
	}
}
