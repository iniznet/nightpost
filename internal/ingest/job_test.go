package ingest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iniznet/nightpost/internal/store"
)

func doJob(t *testing.T, st *store.Store, body string) int {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingest/{project}/job", JobHandler(st))
	req := httptest.NewRequest("POST", "/ingest/backend/job", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

func TestJobHandlerIngestsAndGroups(t *testing.T) {
	st := openQueryStore(t)
	if code := doJob(t, st, `{"name":"quests:reset-stale-progress","kind":"command","seconds":3.2,"ok":true}`); code != http.StatusAccepted {
		t.Fatalf("accepted expected, got %d", code)
	}
	if code := doJob(t, st, `{"name":"quests:reset-stale-progress","kind":"command","seconds":1,"ok":false}`); code != http.StatusAccepted {
		t.Fatalf("got %d", code)
	}
	rows, err := st.JobLeaderboard("backend", 24, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Runs != 2 || rows[0].LastStatus != "failed" {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestJobHandlerInfersKindAndDefaults(t *testing.T) {
	st := openQueryStore(t)
	if code := doJob(t, st, `{"name":"AppJobs\\SendMail","queue":"high","seconds":2}`); code != http.StatusAccepted {
		t.Fatalf("got %d", code)
	}
	if code := doJob(t, st, `{"name":"series:resync-counters","seconds":9}`); code != http.StatusAccepted {
		t.Fatalf("got %d", code)
	}
	rows, _ := st.JobLeaderboard("backend", 24, 50)
	if len(rows) != 2 {
		t.Fatalf("want 2 aggregates, got %d", len(rows))
	}
	kinds := map[string]string{}
	for _, r := range rows {
		kinds[r.Name] = r.Kind
	}
	if kinds["AppJobs\\SendMail"] != "queue" || kinds["series:resync-counters"] != "command" {
		t.Fatalf("kind inference wrong: %+v", kinds)
	}
}

func TestJobHandlerRejectsGarbage(t *testing.T) {
	st := openQueryStore(t)
	if code := doJob(t, st, `{"name":""}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("empty name must 422, got %d", code)
	}
	if code := doJob(t, st, `{"name":"ok","kind":"weird"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("bad kind must 422, got %d", code)
	}
	if code := doJob(t, st, `{"name":"ok","kind":"command","queue":"`+strings.Repeat("q", 65)+`"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized queue must 422, got %d", code)
	}
	if code := doJob(t, st, `{"name"`); code != http.StatusBadRequest {
		t.Fatalf("malformed must 400, got %d", code)
	}
	rows, _ := st.JobLeaderboard("", 24, 50)
	if len(rows) != 0 {
		t.Fatal("rejected payloads must not create rows")
	}
}

func TestJobHandlerStoresLastTrace(t *testing.T) {
	st := openQueryStore(t)
	if code := doJob(t, st, `{"name":"schedule:run","kind":"command","seconds":9.5,"ok":true,"count":1,"trace":"deadbeefcafe0123"}`); code != http.StatusAccepted {
		t.Fatalf("accepted expected, got %d", code)
	}
	if code := doJob(t, st, `{"name":"schedule:run","kind":"command","seconds":1.5,"ok":true,"count":1}`); code != http.StatusAccepted {
		t.Fatalf("accepted expected, got %d", code)
	}
	rows, err := st.JobLeaderboard("", 24*7, 50)
	if err != nil || len(rows) != 1 || rows[0].LastTrace != "deadbeefcafe0123" {
		t.Fatalf("trace round-trip failed: %+v err=%v", rows, err)
	}
}
