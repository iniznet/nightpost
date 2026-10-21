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
		for _, want := range []string{"fenrirealm", "class=\"toolbar\"", "<main class=\"wrap\">"} {
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
		{"/errors?tab=nope", "Issues"},
		{"/errors?tab=", "Issues"},
		{"/performance?tab=nope", "Endpoints"},
		{"/performance?tab=", "Endpoints"},
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
		{"/profiles?project=backend&type=request", "/performance?project=backend&type=request", "GET /chapters/123"},
		{"/slowest?project=backend", "/performance?tab=hotspots&project=backend", "Hotspots"},
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
	resp, _ := http.Get(srv.URL + "/traces?project=front")
	body := readAll(t, resp)
	if !strings.Contains(body, "tree") {
		t.Errorf("traces index missing has-tree badge: %.300s", body)
	}
	if !strings.Contains(body, "depth 12") {
		t.Error("traces index missing the deep-cap note")
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
