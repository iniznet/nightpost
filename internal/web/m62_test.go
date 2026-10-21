package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestToolbarOrgScope pins the org leg of the single param dialect.
func TestToolbarOrgScope(t *testing.T) {
	q, err := url.ParseQuery("org=acme&project=backend&range=7d")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := parseToolbar(q)
	if got.Org != "acme" || got.Project != "backend" || got.Range != "7d" {
		t.Fatalf("parseToolbar = %+v", got)
	}
	link, err := url.Parse(got.Link("/errors", nil))
	if err != nil {
		t.Fatalf("parse link: %v", err)
	}
	if link.Query().Get("org") != "acme" {
		t.Errorf("org did not round-trip through the toolbar link: %q", got.Link("/errors", nil))
	}
	if (toolbar{Project: "backend"}).Values().Has("org") {
		t.Error("an absent org must not be emitted")
	}
}

// TestOrgScopedProjectList proves tenancy is enforced at the projection the
// reader sees: the same page answers a different project set per org, and the
// default org keeps the projects nobody assigned.
func TestOrgScopedProjectList(t *testing.T) {
	srv, st := newTestServer(t)
	seedUI(t, st) // creates project "backend"
	acme, err := st.CreateOrg("Acme", "acme")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := st.AssignProjectToOrg("backend", acme); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := st.EnsureProject("other"); err != nil {
		t.Fatalf("ensure other: %v", err)
	}

	resp, _ := http.Get(srv.URL + "/keys?org=acme")
	body := readAll(t, resp)
	if !strings.Contains(body, "backend") || strings.Contains(body, "other") {
		t.Errorf("acme scope wrong: backend present=%v other present=%v",
			strings.Contains(body, "backend"), strings.Contains(body, "other"))
	}

	resp, _ = http.Get(srv.URL + "/keys")
	body = readAll(t, resp)
	if !strings.Contains(body, "backend") || !strings.Contains(body, "other") {
		t.Errorf("unscoped view must list every project")
	}

	// The org selector renders the org's name and marks it selected.
	resp, _ = http.Get(srv.URL + "/keys?org=acme")
	body = readAll(t, resp)
	if !strings.Contains(body, `value="acme" selected`) {
		t.Error("toolbar did not reflect the selected org")
	}
}
