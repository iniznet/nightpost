package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/iniznet/nightpost/internal/store"
)

func purgePost(t *testing.T, srvURL, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	resp, err := http.PostForm(srvURL+path, form)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp, readAll(t, resp)
}

// drain runs the ticker step until the purge finishes, so the test asserts the
// end state rather than a step count.
func drain(t *testing.T, st *store.Store) {
	t.Helper()
	for i := 0; i < 200; i++ {
		project, done, err := st.DrainPurgeStep()
		if err != nil {
			t.Fatalf("drain purge: %v", err)
		}
		if project == "" || done {
			return
		}
	}
	t.Fatal("the purge never finished")
}

// A destructive request without the typed key must delete nothing. This is the
// guard the whole feature rests on: a stray click cannot wipe a project.
func TestPurgeRefusesAWrongConfirmation(t *testing.T) {
	srv, st := newTestServer(t)
	seedUI(t, st)
	before, err := st.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	_, body := purgePost(t, srv.URL, "/orgs/project/purge", url.Values{"project": {"backend"}, "confirm": {"backand"}})
	if !strings.Contains(body, "did not match") {
		t.Error("a refused purge must say why on the page")
	}
	after, _ := st.Stats()
	if before.Issues != after.Issues || before.Profiles != after.Profiles {
		t.Errorf("a refused purge changed the store: issues %d->%d, profiles %d->%d", before.Issues, after.Issues, before.Profiles, after.Profiles)
	}
	states, _ := st.PurgeStates()
	if _, queued := states["backend"]; queued {
		t.Error("a refused purge must not be queued")
	}
	audit, _ := st.AdminActions(5)
	if len(audit) == 0 || !strings.Contains(audit[0].Detail, "refused") {
		t.Error("a refused destructive request must leave an audit line")
	}
}

// Clear keeps the project and its ingest key, removes every event, and is the
// difference between "start over" and "this app never existed".
func TestClearKeepsTheProjectAndRemovesItsRows(t *testing.T) {
	srv, st := newTestServer(t)
	seedUI(t, st)
	// Ingest creates event rows for a project key without provisioning the
	// project itself; a purge test needs the row to exist so it can outlive it.
	if err := st.EnsureProject("backend"); err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	before, _ := st.Stats()
	if before.Issues == 0 || before.Profiles == 0 {
		t.Fatal("seed produced nothing to purge")
	}
	_, body := purgePost(t, srv.URL, "/orgs/project/purge", url.Values{"project": {"backend"}, "confirm": {"backend"}})
	if !strings.Contains(body, "Queued") {
		t.Errorf("the page must confirm the queue: %s", body[:min(len(body), 200)])
	}
	drain(t, st)
	after, _ := st.Stats()
	if after.Issues != 0 || after.Profiles != 0 || after.Occurrences != 0 {
		t.Errorf("clear left rows behind: %+v", after)
	}
	projects, err := st.ListProjects()
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	found := false
	for _, p := range projects {
		if p.Key == "backend" {
			found = true
		}
	}
	if !found {
		t.Error("clear must keep the project row")
	}
}

// Delete is clear plus the project row: the key stops identifying anything.
func TestDeleteRemovesTheProjectRowToo(t *testing.T) {
	srv, st := newTestServer(t)
	seedUI(t, st)
	if err := st.EnsureProject("backend"); err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	purgePost(t, srv.URL, "/orgs/project/delete", url.Values{"project": {"backend"}, "confirm": {"backend"}})
	drain(t, st)
	projects, _ := st.ListProjects()
	for _, p := range projects {
		if p.Key == "backend" {
			t.Error("delete must remove the project row")
		}
	}
}

// An org holding projects refuses: the explicit cascade is deleting each
// project, one confirmed click at a time.
func TestOrgDeleteRefusesWhileItHoldsProjects(t *testing.T) {
	srv, st := newTestServer(t)
	seedUI(t, st)
	orgs, err := st.ListOrgs()
	if err != nil || len(orgs) == 0 {
		t.Fatalf("list orgs: %v", err)
	}
	_, body := purgePost(t, srv.URL, "/orgs/org/delete", url.Values{"org": {orgs[0].Slug}, "confirm": {orgs[0].Slug}})
	if !strings.Contains(body, "still holds projects") {
		t.Error("an org with projects must refuse the delete and say why")
	}
}
