package ingest

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iniznet/nightpost/internal/store"
)

func TestWithCORSDisabledByDefault(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()

	req := httptest.NewRequest(http.MethodPost, "/ingest/front/error", strings.NewReader(`{"message":"x","stack":""}`))
	req.SetPathValue("project", "front")
	rec := httptest.NewRecorder()
	WithCORS("", ErrorHandler(st))(rec, req)

	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("expected no CORS headers when origin flag is empty")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestWithCORSEnablesPreflightAndPost(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()

	pre := httptest.NewRequest(http.MethodOptions, "/ingest/front/error", nil)
	pr := httptest.NewRecorder()
	WithCORS("https://fenrirealm.com", ErrorHandler(st))(pr, pre)
	if pr.Code != http.StatusNoContent {
		t.Errorf("preflight expected 204, got %d", pr.Code)
	}
	if pr.Header().Get("Access-Control-Allow-Origin") != "https://fenrirealm.com" {
		t.Error("missing Allow-Origin on preflight")
	}

	req := httptest.NewRequest(http.MethodPost, "/ingest/front/error", strings.NewReader(`{"message":"x","stack":""}`))
	req.SetPathValue("project", "front")
	rec := httptest.NewRecorder()
	WithCORS("https://fenrirealm.com", ErrorHandler(st))(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("post expected 200, got %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://fenrirealm.com" {
		t.Error("missing Allow-Origin on POST")
	}
}

func TestPreflightHandlerRefusedWithoutOrigin(t *testing.T) {
	h := PreflightHandler("")
	req := httptest.NewRequest(http.MethodOptions, "/ingest/front/error", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 without origin, got %d", rec.Code)
	}
}
