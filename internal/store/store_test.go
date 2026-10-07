package store

import (
    "path/filepath"
    "testing"
)

func newTestStore(t *testing.T) *Store {
    t.Helper()
    st, err := Open(filepath.Join(t.TempDir(), "test.db"))
    if err != nil {
        t.Fatalf("open: %v", err)
    }
    t.Cleanup(func() { st.Close() })
    return st
}

func TestRecordErrorGroupsByFingerprint(t *testing.T) {
    st := newTestStore(t)

    fp1 := ForException("QueryException", "user 42 not found", "App\\\\Controller:index")
    fp2 := ForException("QueryException", "user 43 not found", "App\\\\Controller:index")
    if fp1 != fp2 {
        t.Fatalf("expected same fingerprint for volatile-token variants, got %s vs %s", fp1, fp2)
    }

    id1, err := st.RecordError("backend", "QueryException", "user 42 not found", fp1, "production", "error", map[string]any{"k": 1})
    if err != nil {
        t.Fatalf("record: %v", err)
    }
    id2, err := st.RecordError("backend", "QueryException", "user 43 not found", fp2, "production", "error", map[string]any{"k": 2})
    if err != nil {
        t.Fatalf("record: %v", err)
    }
    if id1 != id2 {
        t.Fatalf("expected occurrences to fold into issue %d, got new issue %d", id1, id2)
    }

    issue, err := st.Issue(id1)
    if err != nil {
        t.Fatalf("issue: %v", err)
    }
    if issue.Count != 2 {
        t.Errorf("expected count 2, got %d", issue.Count)
    }
}

func TestDistinctDefectsStayDistinct(t *testing.T) {
    st := newTestStore(t)

    fpA := ForException("QueryException", "column is_admin not found", "A:b")
    fpB := ForException("ValidationException", "column is_admin not found", "A:b")
    if fpA == fpB {
        t.Fatal("different exception classes must not share a fingerprint")
    }

    idA, _ := st.RecordError("backend", "QueryException", "column is_admin not found", fpA, "prod", "error", nil)
    idB, _ := st.RecordError("backend", "ValidationException", "column is_admin not found", fpB, "prod", "error", nil)
    if idA == idB {
        t.Fatal("distinct defects collapsed into one issue")
    }
}

func TestCleanRedactsSecrets(t *testing.T) {
    got := Clean(map[string]any{
        "password":   "hunter2",
        "api_key":    "abcd",
        "Authorization": "Bearer x",
        "nested": map[string]any{"token": "t", "safe": "ok"},
        "deep":       []any{map[string]any{"secret": "s", "n": float64(1)}},
    })

    m := got.(map[string]any)
    if m["password"] != redactedMarker || m["api_key"] != redactedMarker || m["Authorization"] != redactedMarker {
        t.Errorf("top-level secret not redacted: %v", m)
    }
    nested := m["nested"].(map[string]any)
    if nested["token"] != redactedMarker || nested["safe"] != "ok" {
        t.Errorf("nested redaction wrong: %v", nested)
    }
    deep := m["deep"].([]any)[0].(map[string]any)
    if deep["secret"] != redactedMarker {
        t.Errorf("deep secret not redacted: %v", deep)
    }
}