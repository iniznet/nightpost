package ingest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iniznet/nightpost/internal/store"
)

// The two collector routes are frozen by golden payloads, so a field rename or a
// dropped level of the envelope fails a test here instead of failing a
// collector's retry loop in a deployment.
const (
	otlpTraceGolden = "trace-two-services.json"
	otlpLogGolden   = "logs-monolog-record.json"
	otlpGoldenTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
)

func otlpGolden(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "wire", "otlp", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return body
}

// postOTLP mounts the handler on its own mux, the way production mounts it,
// so the request carries the patterns and query this route really sees.
func postOTLP(t *testing.T, st *store.Store, route string, h http.HandlerFunc, project string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(route, h)
	target := route
	if project != "" {
		target += "?project=" + project
	}
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// serviceIDs reads the project's admitted producer identities by name.
func serviceIDs(t *testing.T, st *store.Store, project string) map[string]int64 {
	t.Helper()
	services, err := st.Services(project)
	if err != nil {
		t.Fatalf("services: %v", err)
	}
	out := make(map[string]int64, len(services))
	for _, svc := range services {
		out[svc.Name] = svc.ID
	}
	return out
}

// TestOTLPTraceGoldenTwoServices is the mapping contract for POST /v1/traces: two
// services in one envelope, four spans, one client span with no child, every
// start_ms relative to the trace and never a wall clock.
func TestOTLPTraceGoldenTwoServices(t *testing.T) {
	st, _ := newV2Store(t)
	rec := postOTLP(t, st, "/v1/traces", TracesHandler(st), "wire", otlpGolden(t, otlpTraceGolden))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var accepted struct {
		Spans    int      `json:"spans"`
		TraceIDs []string `json:"trace_ids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("response %q: %v", rec.Body.String(), err)
	}
	if accepted.Spans != 4 {
		t.Fatalf("stored %d spans, want 4", accepted.Spans)
	}
	if len(accepted.TraceIDs) != 1 || accepted.TraceIDs[0] != otlpGoldenTrace {
		t.Fatalf("trace ids = %v, want the payload's own id", accepted.TraceIDs)
	}

	spans, err := st.TraceSpans(otlpGoldenTrace)
	if err != nil {
		t.Fatalf("spans: %v", err)
	}
	if len(spans) != 4 {
		t.Fatalf("trace holds %d spans, want 4", len(spans))
	}
	ids := serviceIDs(t, st, "wire")
	// The clock never leaks: the first span starts at 0ms, and the rest are
	// offsets from it even though the payload carried nanoseconds since the
	// epoch.
	want := []struct {
		spanID  string
		startMs float64
		durMs   float64
		kind    string
		status  string
		service string
		parent  string
	}{
		{"aaaaaaaaaaaaaaaa", 0, 120, "server", "ok", "wire-front", ""},
		{"bbbbbbbbbbbbbbbb", 20, 40, "client", "error", "wire-front", "aaaaaaaaaaaaaaaa"},
		{"cccccccccccccccc", 25, 30, "server", "ok", "wire-backend", "bbbbbbbbbbbbbbbb"},
		{"eeeeeeeeeeeeeeee", 70, 10, "client", "ok", "wire-front", "aaaaaaaaaaaaaaaa"},
	}
	for i, w := range want {
		got := spans[i]
		if got.SpanID != w.spanID {
			t.Fatalf("position %d is span %s, want %s (ordered by relative start)", i, got.SpanID, w.spanID)
		}
		if got.StartMs != w.startMs {
			t.Errorf("span %s start_ms = %v, want %v", w.spanID, got.StartMs, w.startMs)
		}
		if got.DurationMs != w.durMs {
			t.Errorf("span %s duration_ms = %v, want %v", w.spanID, got.DurationMs, w.durMs)
		}
		if got.Kind != w.kind {
			t.Errorf("span %s kind = %q, want %q", w.spanID, got.Kind, w.kind)
		}
		if got.Status != w.status {
			t.Errorf("span %s status = %q, want %q", w.spanID, got.Status, w.status)
		}
		if want := ids[w.service]; got.ServiceID != want {
			t.Errorf("span %s service = %d, want %s (%d)", w.spanID, got.ServiceID, w.service, want)
		}
		if got.ParentSpanID != w.parent {
			t.Errorf("span %s parent = %q, want %q", w.spanID, got.ParentSpanID, w.parent)
		}
		if !strings.Contains(got.Attributes, `"trace_origin":"adopted"`) {
			t.Errorf("span %s attributes = %s, want the payload's trace adopted", w.spanID, got.Attributes)
		}
	}

	// The peer a client span names is admitted as an identity, so a gap row can
	// name the callee that never reported.
	if _, ok := ids["wire-ads"]; !ok {
		t.Errorf("peer service wire-ads was not admitted: %v", ids)
	}

	now := time.Now()
	if err := st.RecomputeJourneys(now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	journeys, err := st.Journeys("wire", "", 10)
	if err != nil || len(journeys) != 1 {
		t.Fatalf("journeys = %d, err = %v, want one materialised path", len(journeys), err)
	}
	j := journeys[0]
	if j.HopCount != 3 || j.Samples != 1 {
		t.Fatalf("journey hops = %d samples = %d, want 3 hops of one sample", j.HopCount, j.Samples)
	}
	hps, err := st.JourneyHops(j.ID)
	if err != nil {
		t.Fatalf("hops: %v", err)
	}
	wantStatus := []string{"ok", "ok", "unreported"}
	if len(hps) != len(wantStatus) {
		t.Fatalf("hop rows = %d, want %d", len(hps), len(wantStatus))
	}
	for i, want := range wantStatus {
		if hps[i].Status != want {
			t.Errorf("hop %d status = %q, want %q", i, hps[i].Status, want)
		}
		if hps[i].RouteID == 0 {
			t.Errorf("hop %d names no route", i)
		}
	}
	coverage, err := st.JourneyCoverage(j.ID)
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if coverage.String() != "2/3" {
		t.Errorf("coverage = %s, want 2/3 (the ads call never reported)", coverage.String())
	}
}

// TestOTLPLogGoldenMonologRecord holds POST /v1/logs to the contract that matters:
// a collector record lands where a Monolog line lands, with the same severity,
// body, channel and context.
func TestOTLPLogGoldenMonologRecord(t *testing.T) {
	st, _ := newV2Store(t)
	rec := postOTLP(t, st, "/v1/logs", LogsHandler(st), "wire", otlpGolden(t, otlpLogGolden))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}

	issues, err := st.Issues(store.IssueQuery{Project: "wire", Type: "log", Limit: 5})
	if err != nil || len(issues) != 1 {
		t.Fatalf("log rows = %d, err = %v, want one", len(issues), err)
	}
	issue := issues[0]
	if issue.Title != "wire: Turnstile verification rejected" {
		t.Errorf("title = %q, want the record body", issue.Title)
	}
	if issue.Level != "error" {
		t.Errorf("level = %q, want error (severityText ERROR, as a Monolog level_name)", issue.Level)
	}
	if issue.Channel != "security" {
		t.Errorf("channel = %q, want the log.channel attribute", issue.Channel)
	}
	if issue.Environment != "production" {
		t.Errorf("environment = %q, want the resource environment", issue.Environment)
	}

	occurrences, err := st.Occurrences(issue.ID, 5)
	if err != nil || len(occurrences) != 1 {
		t.Fatalf("occurrences = %d, err = %v", len(occurrences), err)
	}
	// The occurrence carries the record's payload, exactly where a Monolog line's
	// context lands.
	payload, _ := occurrences[0]["payload"].(map[string]any)
	context, _ := payload["context"].(map[string]any)
	if context["error-codes"] != "invalid-input-response" {
		t.Errorf("context = %v, want the record's attributes", context)
	}
	if _, leaked := context["log.channel"]; leaked {
		t.Errorf("context = %v, want log.channel lifted into its column", context)
	}

	// The producer identity is admitted even though a log row carries no service
	// column: the services table is the install's inventory of producers.
	if _, ok := serviceIDs(t, st, "wire")["wire-backend"]; !ok {
		t.Error("log envelope did not admit service.name as an identity")
	}
}

// TestOTLPMalformedTraceIDRefusesEnvelope is the whole-or-nothing rule: one
// unverifiable id refuses the envelope before any row is written, because a
// half-written trace draws a gap no reader can distinguish from a silent callee.
func TestOTLPMalformedTraceIDRefusesEnvelope(t *testing.T) {
	st, _ := newV2Store(t)
	body := strings.ReplaceAll(string(otlpGolden(t, otlpTraceGolden)), otlpGoldenTrace, otlpGoldenTrace[:31])

	rec := postOTLP(t, st, "/v1/traces", TracesHandler(st), "wire", []byte(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), ErrOTLPTraceID.Error()) {
		t.Fatalf("body = %q, want the named error %q", rec.Body.String(), ErrOTLPTraceID.Error())
	}
	if spans, err := st.TraceSpans(otlpGoldenTrace[:31]); err != nil || len(spans) != 0 {
		t.Fatalf("spans = %d, err = %v, want nothing stored", len(spans), err)
	}
	if services, err := st.Services("wire"); err != nil || len(services) != 0 {
		t.Fatalf("services = %d, err = %v, want no side effect from a refused envelope", len(services), err)
	}
}

// TestOTLPResentEnvelopeIsIdempotent is the retry path: a collector re-posts a
// batch it did not get a 2xx for, and the journey must not count the resent hops
// twice.
func TestOTLPResentEnvelopeIsIdempotent(t *testing.T) {
	st, _ := newV2Store(t)
	body := otlpGolden(t, otlpTraceGolden)
	first := postOTLP(t, st, "/v1/traces", TracesHandler(st), "wire", body)
	if first.Code != http.StatusOK {
		t.Fatalf("first post: %d: %s", first.Code, first.Body.String())
	}
	now := time.Now()
	if err := st.RecomputeJourneys(now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	before, err := st.Journeys("wire", "", 10)
	if err != nil || len(before) != 1 {
		t.Fatalf("journeys = %d, err = %v", len(before), err)
	}

	resent := postOTLP(t, st, "/v1/traces", TracesHandler(st), "wire", body)
	if resent.Code != http.StatusOK {
		t.Fatalf("resent post: %d: %s", resent.Code, resent.Body.String())
	}
	var accepted struct {
		Spans      int `json:"spans"`
		Duplicates int `json:"duplicates"`
	}
	if err := json.Unmarshal(resent.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("response %q: %v", resent.Body.String(), err)
	}
	if accepted.Spans != 0 || accepted.Duplicates != 4 {
		t.Fatalf("resent envelope stored %d spans with %d duplicates, want 0 stored and 4 refused", accepted.Spans, accepted.Duplicates)
	}
	if spans, err := st.TraceSpans(otlpGoldenTrace); err != nil || len(spans) != 4 {
		t.Fatalf("spans = %d, err = %v, want the original four", len(spans), err)
	}

	now = time.Now()
	if err := st.RecomputeJourneys(now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatalf("second recompute: %v", err)
	}
	after, err := st.Journeys("wire", "", 10)
	if err != nil || len(after) != 1 {
		t.Fatalf("journeys = %d, err = %v", len(after), err)
	}
	if after[0].PatternKey != before[0].PatternKey {
		t.Fatalf("pattern key moved from %s to %s: the resent span split the journey", before[0].PatternKey, after[0].PatternKey)
	}
	if after[0].HopCount != 3 || after[0].Samples != 1 {
		t.Fatalf("journey hops = %d samples = %d after the resend, want 3 and 1", after[0].HopCount, after[0].Samples)
	}
	if hps, err := st.JourneyHops(after[0].ID); err != nil || len(hps) != 3 {
		t.Fatalf("hop rows = %d, err = %v, want 3", len(hps), err)
	}
}

// TestOTLPSpanKindAndStatus pins the two numeric mappings, including the values
// a golden payload does not reach.
func TestOTLPSpanKindAndStatus(t *testing.T) {
	kinds := map[int]string{0: "internal", 1: "internal", 2: "server", 3: "client", 4: "producer", 5: "consumer"}
	for code, want := range kinds {
		if got := otlpSpanKind(code); got != want {
			t.Errorf("kind %d = %q, want %q", code, got, want)
		}
	}
	statuses := map[int]string{0: "ok", 1: "ok", 2: "error"}
	for code, want := range statuses {
		if got := otlpSpanStatus(code); got != want {
			t.Errorf("status %d = %q, want %q", code, got, want)
		}
	}
}
