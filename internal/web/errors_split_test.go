package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/iniznet/nightpost/internal/store"
)

// This file guards the exceptions/logs split (iniznet/fenrirealm-tracker#526).
// A log group is an event stream: it has no resolve state, its detail page must
// not offer one, and the level facet must be built from data rather than from a
// hardcoded four.

// seedSplit adds the rows the old screen could not handle: a notice-level log
// record and an exception at critical, a level the hardcoded select omitted.
func seedSplit(t *testing.T, st *store.Store) (errID, logID int64) {
	t.Helper()
	seedUI(t, st)
	if _, err := st.RecordLog("backend", "security", "notice", "captcha verification rejected", map[string]any{"source": "rate_limit_recover"}, "production"); err != nil {
		t.Fatalf("seed notice log: %v", err)
	}
	fp := store.ForException("QueryException", "deadlock on series write", "App\\\\Repo:save")
	if _, err := st.RecordError("backend", "QueryException", "deadlock on series write", fp, "production", "critical", map[string]any{
		"level": "critical",
		"exception": map[string]any{"values": []any{map[string]any{
			"type": "QueryException", "value": "deadlock on series write",
		}}},
	}); err != nil {
		t.Fatalf("seed critical error: %v", err)
	}
	errs, err := st.Issues(store.IssueQuery{Project: "backend", Type: "error", Level: "critical", Limit: 1})
	if err != nil || len(errs) == 0 {
		t.Fatalf("find critical error: %v (%d rows)", err, len(errs))
	}
	logs, err := st.Issues(store.IssueQuery{Project: "backend", Type: "log", Level: "notice", Limit: 1})
	if err != nil || len(logs) == 0 {
		t.Fatalf("find notice log: %v (%d rows)", err, len(logs))
	}
	return errs[0].ID, logs[0].ID
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func postForm(t *testing.T, u string, form url.Values) *http.Response {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(u, form)
	if err != nil {
		t.Fatalf("POST %s: %v", u, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// Acceptance 1: the resolve endpoint refuses a log row, and the flag it would
// have written stays untouched.
func TestResolveRefusesLogRows(t *testing.T) {
	srv, st := newTestServer(t)
	errID, logID := seedSplit(t, st)

	resp := postForm(t, srv.URL+"/issues/"+itoa(logID)+"/resolve", url.Values{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("resolve on a log row: status %d, want 400", resp.StatusCode)
	}
	logRow, err := st.Issue(logID)
	if err != nil {
		t.Fatalf("reload log row: %v", err)
	}
	if logRow.Resolved {
		t.Error("a refused resolve still flipped the resolved bit")
	}

	resp = postForm(t, srv.URL+"/issues/"+itoa(errID)+"/resolve", url.Values{})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("resolve on an exception: status %d, want 303", resp.StatusCode)
	}
	errRow, err := st.Issue(errID)
	if err != nil {
		t.Fatalf("reload error row: %v", err)
	}
	if !errRow.Resolved {
		t.Error("resolving an exception did not stick")
	}
}

// Acceptance 1b: the resolve redirect returns to the list the reader came from,
// instead of always dumping them on the exceptions tab.
func TestResolveReturnsToOriginList(t *testing.T) {
	srv, st := newTestServer(t)
	errID, _ := seedSplit(t, st)

	resp := postForm(t, srv.URL+"/issues/"+itoa(errID)+"/reopen", url.Values{"from": {"/errors?project=backend&level=critical"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("reopen: status %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/errors?project=backend&level=critical" {
		t.Errorf("reopen redirect = %q, want the origin list", got)
	}
}

// Acceptance 2: the detail page offers no resolve control for a log row, and
// still offers one for an exception.
func TestLogDetailHasNoResolveControl(t *testing.T) {
	srv, st := newTestServer(t)
	errID, logID := seedSplit(t, st)

	body := readAll(t, mustGet(t, srv.URL+"/issues/"+itoa(logID)))
	if strings.Contains(body, "/resolve") || strings.Contains(body, "<button>Resolve</button>") {
		t.Error("log detail still renders a Resolve control")
	}
	if !strings.Contains(body, "Read-only") {
		t.Error("log detail does not say why it is read-only")
	}

	body = readAll(t, mustGet(t, srv.URL+"/issues/"+itoa(errID)))
	if !strings.Contains(body, "<button>Resolve</button>") {
		t.Error("exception detail lost its Resolve control")
	}
}

// Acceptance 3: the tab strip follows the row, not the route.
func TestLogDetailKeepsLogsTab(t *testing.T) {
	srv, st := newTestServer(t)
	errID, logID := seedSplit(t, st)

	body := readAll(t, mustGet(t, srv.URL+"/issues/"+itoa(logID)+"?from=%2Ferrors%3Ftab%3Dlogs"))
	if !strings.Contains(body, "aria-current=\"page\">Logs</a>") {
		t.Error("log detail does not underline the Logs tab")
	}
	if strings.Contains(body, "aria-current=\"page\">Exceptions</a>") {
		t.Error("log detail underlines Exceptions")
	}

	body = readAll(t, mustGet(t, srv.URL+"/issues/"+itoa(errID)))
	if !strings.Contains(body, "aria-current=\"page\">Exceptions</a>") {
		t.Error("exception detail does not underline the Exceptions tab")
	}
}

// Acceptance 4: the level facet is built from data, so a level outside the old
// hardcoded four is selectable and a stale deep link keeps its selection.
func TestLevelFacetCoversDataLevels(t *testing.T) {
	srv, st := newTestServer(t)
	seedSplit(t, st)

	body := readAll(t, mustGet(t, srv.URL+"/errors?project=backend"))
	if !strings.Contains(body, `<option value="critical"`) {
		t.Error("exceptions facet omits an exception level that exists (critical)")
	}
	if strings.Contains(body, `<option value="notice"`) {
		t.Error("exceptions facet offers a log-only level (notice)")
	}

	body = readAll(t, mustGet(t, srv.URL+"/errors?project=backend&level=critical"))
	if !strings.Contains(body, `<option value="critical" selected>`) {
		t.Error("the selected level is not marked in the facet")
	}
	if !strings.Contains(body, "deadlock on series write") {
		t.Error("filtering by a data level returns no rows")
	}

	body = readAll(t, mustGet(t, srv.URL+"/errors?project=backend&level=deprecated"))
	if !strings.Contains(body, `<option value="deprecated" selected>`) {
		t.Error("a deep link to an absent level leaves the facet with nothing selected")
	}
}

// Acceptance 5: the exceptions list carries no type mixer, while an old
// ?type=log link still resolves and says what it is showing.
func TestIssuesFilterHasNoTypeMixer(t *testing.T) {
	srv, st := newTestServer(t)
	seedSplit(t, st)

	body := readAll(t, mustGet(t, srv.URL+"/errors?project=backend"))
	if strings.Contains(body, `name="type"`) {
		t.Error("exceptions filter still offers a type mixer")
	}
	if strings.Contains(body, "captcha verification rejected") {
		t.Error("log rows leak into the exceptions list")
	}

	body = readAll(t, mustGet(t, srv.URL+"/errors?project=backend&type=log"))
	if !strings.Contains(body, "captcha verification rejected") {
		t.Error("a legacy ?type=log link no longer resolves")
	}
	if !strings.Contains(body, "legacy link") {
		t.Error("a legacy ?type=log page does not say what it is showing")
	}
}
