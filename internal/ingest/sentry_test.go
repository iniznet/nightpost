package ingest

import (
    "net/http"
    "net/http/httptest"
    "path/filepath"
    "strings"
    "testing"

    "github.com/iniznet/nightpost/internal/store"
)

func TestSentryHandlerRecordsException(t *testing.T) {
    st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
    if err != nil {
        t.Fatalf("open: %v", err)
    }
    defer st.Close()

    envelope := strings.Join([]string{
        `{"event_id":"e1","environment":"production"}`,
        `{"type":"event"}`,
        `{"level":"error","exception":{"values":[{"type":"QueryException","value":"user 42 not found","stacktrace":{"frames":[{"module":"App\\\\Http\\\\Controllers","function":"index"}]}}]}}`,
    }, "\n")

    req := httptest.NewRequest(http.MethodPost, "/ingest/backend/sentry", strings.NewReader(envelope))
    req.SetPathValue("project", "backend")
    rec := httptest.NewRecorder()
    SentryHandler(st)(rec, req)

    if rec.Code != http.StatusOK {
        t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
    }
    if !strings.Contains(rec.Body.String(), `"recorded":1`) {
        t.Fatalf("expected recorded=1, got %s", rec.Body.String())
    }

    issues, err := st.Issues(store.IssueQuery{Project: "backend", Limit: 10})
    if err != nil {
        t.Fatalf("issues: %v", err)
    }
    if len(issues) != 1 {
        t.Fatalf("expected 1 issue, got %d", len(issues))
    }
    if issues[0].ExceptionClass != "QueryException" {
        t.Errorf("wrong class: %s", issues[0].ExceptionClass)
    }
}

func TestSentryHandlerRejectsGarbage(t *testing.T) {
    st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
    if err != nil {
        t.Fatalf("open: %v", err)
    }
    defer st.Close()

    req := httptest.NewRequest(http.MethodPost, "/ingest/backend/sentry", strings.NewReader("not json"))
    req.SetPathValue("project", "backend")
    rec := httptest.NewRecorder()
    SentryHandler(st)(rec, req)

    if rec.Code != http.StatusBadRequest {
        t.Fatalf("expected 400, got %d", rec.Code)
    }
}