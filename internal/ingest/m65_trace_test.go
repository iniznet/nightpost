package ingest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseTraceparent(t *testing.T) {
	const valid = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{valid, "4bf92f3577b34da6a3ce929d0e0e4736", true},
		{"  " + valid + "  ", "4bf92f3577b34da6a3ce929d0e0e4736", true},
		{"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra", "4bf92f3577b34da6a3ce929d0e0e4736", true},
		{"", "", false},
		{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7", "", false},
		{"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "", false},
		{"00-00000000000000000000000000000000-00f067aa0ba902b7-01", "", false},
		{"00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01", "", false},
		{"00-4bf92f3577b34da6a3ce929d0e0e473-00f067aa0ba902b7-01", "", false},
		{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b-01", "", false},
		{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-zz", "", false},
		{"garbage", "", false},
		{"----", "", false},
	}
	for _, c := range cases {
		got, ok := ParseTraceparent(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseTraceparent(%q) = %q,%v want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestTraceIDFromRequestPrecedence(t *testing.T) {
	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	mk := func(legacy string, withTP bool) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/ingest/p/profile", nil)
		if legacy != "" {
			req.Header.Set(HeaderTraceID, legacy)
		}
		if withTP {
			req.Header.Set(HeaderTraceparent, tp)
		}
		return req
	}
	if got := TraceIDFromRequest(mk("from-legacy", true), "from-payload"); got != "from-payload" {
		t.Errorf("the payload value must win, got %q", got)
	}
	if got := TraceIDFromRequest(mk("from-legacy", true), ""); got != "from-legacy" {
		t.Errorf("the legacy header must beat traceparent, got %q", got)
	}
	if got := TraceIDFromRequest(mk("", true), ""); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("traceparent fallback, got %q", got)
	}
	if got := TraceIDFromRequest(mk("", false), ""); got != "" {
		t.Errorf("a request with no context must stay empty, got %q", got)
	}
}

// TestProfileHandlerJoinsTraceFromTraceparent is the point of the item: a
// service with no Nightpost client joins the trace by header propagation alone.
func TestProfileHandlerJoinsTraceFromTraceparent(t *testing.T) {
	st := newBenchStore(t)
	req := httptest.NewRequest(http.MethodPost, "/ingest/backend/profile",
		strings.NewReader(`{"name":"GET /chapters/1","duration_ms":120}`))
	req.SetPathValue("project", "backend")
	req.Header.Set(HeaderTraceparent, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	rec := httptest.NewRecorder()
	ProfileHandler(st)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	legs, err := st.ProfilesByTrace("4bf92f3577b34da6a3ce929d0e0e4736", 20)
	if err != nil {
		t.Fatalf("byTrace: %v", err)
	}
	if len(legs) != 1 {
		t.Fatalf("traceparent did not join the trace: %d legs", len(legs))
	}
}

func TestXHProfHandlerTraceparentFallback(t *testing.T) {
	st := newBenchStore(t)
	body := `{"profile":{"main()":{"ct":1,"wt":500000}},"tags":{"env":"production"},"app_name":"x","hostname":"n","date":1728300000}`
	req := httptest.NewRequest(http.MethodPost, "/api/profiler/store", strings.NewReader(body))
	req.Header.Set(HeaderTraceparent, "00-11111111111111111111111111111111-00f067aa0ba902b7-01")
	rec := httptest.NewRecorder()
	XHProfHandler(st)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	legs, err := st.ProfilesByTrace("11111111111111111111111111111111", 20)
	if err != nil {
		t.Fatalf("byTrace: %v", err)
	}
	if len(legs) != 1 {
		t.Fatalf("callgraph traceparent fallback missing: %d legs", len(legs))
	}
}
