package ingest

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iniznet/nightpost/internal/store"
)

// newV2Store keeps the database path: the admission and envelope knobs are
// settings rows this increment READS but does not yet expose for writing, so a
// test writes them the way an operator's script will until the settings page
// learns the keys.
func newV2Store(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v2.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, path
}

func setRawSetting(t *testing.T, path, key, value string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open settings connection: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().Unix()); err != nil {
		t.Fatalf("set %s=%s: %v", key, value, err)
	}
}

func postProfile(t *testing.T, st *store.Store, project, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingest/{project}/profile", ProfileHandler(st))
	req := httptest.NewRequest(http.MethodPost, "/ingest/"+project+"/profile", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// lastProfile returns the newest profile of a project, which is the row the
// handler just wrote.
func lastProfile(t *testing.T, st *store.Store, project string) store.Profile {
	t.Helper()
	profiles, err := st.Profiles(project, 5)
	if err != nil || len(profiles) == 0 {
		t.Fatalf("profiles: %v / %d rows", err, len(profiles))
	}
	return profiles[0]
}

const v2Traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func TestTraceOriginMintedWhenNoTraceparent(t *testing.T) {
	st, _ := newV2Store(t)
	rec := postProfile(t, st, "backend",
		`{"name":"GET /x","duration_ms":10,"route":"GET /x","spans":[{"name":"db","duration_ms":3,"start_ms":1,"route":"GET /x"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	p := lastProfile(t, st, "backend")
	if len(p.TraceID) != 32 || !isLowerHex(p.TraceID) {
		t.Fatalf("minted trace id = %q, want 32 lowercase hex", p.TraceID)
	}
	spans, err := st.TraceSpans(p.TraceID)
	if err != nil {
		t.Fatalf("spans: %v", err)
	}
	if len(spans) != 2 {
		t.Fatalf("trace holds %d spans, want the root plus one child", len(spans))
	}
	for _, sp := range spans {
		if !strings.Contains(sp.Attributes, `"trace_origin":"minted"`) {
			t.Errorf("span %s attributes = %s, want trace_origin minted", sp.Name, sp.Attributes)
		}
	}
	if spans[0].ParentSpanID != "" {
		t.Errorf("the payload's own span must be the root, got parent %q", spans[0].ParentSpanID)
	}
	if spans[1].ParentSpanID != spans[0].SpanID {
		t.Errorf("a child without a parent must hang off the root, got %q", spans[1].ParentSpanID)
	}
}

func TestTraceOriginAdoptedForV1Producer(t *testing.T) {
	st, _ := newV2Store(t)
	rec := postProfile(t, st, "backend",
		`{"name":"GET /x","duration_ms":10,"spans":[{"name":"db","duration_ms":3}]}`,
		map[string]string{HeaderTraceparent: v2Traceparent})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	p := lastProfile(t, st, "backend")
	if p.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("v1 producer lost its trace: %q", p.TraceID)
	}
	spans, err := st.TraceSpans(p.TraceID)
	if err != nil || len(spans) != 2 {
		t.Fatalf("spans=%d err=%v", len(spans), err)
	}
	if !strings.Contains(spans[0].Attributes, `"trace_origin":"adopted"`) {
		t.Errorf("attributes = %s, want adopted", spans[0].Attributes)
	}
}

func TestTraceOriginReplacedForUntrustedService(t *testing.T) {
	st, _ := newV2Store(t)
	rec := postProfile(t, st, "backend",
		`{"name":"GET /x","duration_ms":10,"service":"backend-api","env":"production","spans":[{"name":"db","duration_ms":3}]}`,
		map[string]string{HeaderTraceparent: v2Traceparent})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	p := lastProfile(t, st, "backend")
	if p.TraceID == "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatal("an untrusted producer's traceparent was adopted")
	}
	spans, err := st.TraceSpans(p.TraceID)
	if err != nil || len(spans) == 0 {
		t.Fatalf("spans err=%v", err)
	}
	if !strings.Contains(spans[0].Attributes, `"trace_origin":"replaced"`) {
		t.Errorf("attributes = %s, want replaced", spans[0].Attributes)
	}
	services, err := st.Services("backend")
	if err != nil {
		t.Fatalf("services: %v", err)
	}
	found := false
	for _, svc := range services {
		if svc.Name == "backend-api" {
			found = true
			if svc.Env != "production" {
				t.Errorf("service env = %q, want production", svc.Env)
			}
		}
	}
	if !found {
		t.Errorf("service backend-api was not recorded: %+v", services)
	}
}

func TestTraceOriginAdoptedForTrustedService(t *testing.T) {
	st, _ := newV2Store(t)
	if _, err := st.UpsertService("backend", "backend-api", "go", "production", true); err != nil {
		t.Fatalf("seed service: %v", err)
	}
	postProfile(t, st, "backend",
		`{"name":"GET /x","duration_ms":10,"service":"backend-api","spans":[{"name":"db","duration_ms":3}]}`,
		map[string]string{HeaderTraceparent: v2Traceparent})
	p := lastProfile(t, st, "backend")
	if p.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("a trusted producer's traceparent was not adopted: %q", p.TraceID)
	}
	// The trust an operator granted must survive the producer's own payload,
	// which never votes on its own trust.
	svc, err := st.ServiceByName("backend", "backend-api")
	if err != nil || svc == nil || !svc.TrustedTraceparent {
		t.Fatalf("trust was reset by ingest: %+v err=%v", svc, err)
	}
}

func TestTraceOriginMalformedTraceparentIsMinted(t *testing.T) {
	st, _ := newV2Store(t)
	postProfile(t, st, "backend",
		`{"name":"GET /x","duration_ms":10,"spans":[{"name":"db","duration_ms":3}]}`,
		map[string]string{HeaderTraceparent: "garbage"})
	p := lastProfile(t, st, "backend")
	if len(p.TraceID) != 32 || !isLowerHex(p.TraceID) {
		t.Fatalf("trace id = %q, want a minted 32-hex id", p.TraceID)
	}
}

func TestStrictTraceparentRejectsWhatItCannotAssume(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{v2Traceparent, true},
		{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00", false},
		{"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", false},
		{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra", false},
		{"00-00000000000000000000000000000000-00f067aa0ba902b7-01", false},
		{"", false},
	}
	for _, c := range cases {
		if id, ok := strictTraceparent(c.in); ok != c.ok || (c.ok && id != "4bf92f3577b34da6a3ce929d0e0e4736") {
			t.Errorf("strictTraceparent(%q) = %q,%v want ok=%v", c.in, id, ok, c.ok)
		}
	}
}

func TestSpanRouteAndPeerServiceAreRecorded(t *testing.T) {
	st, _ := newV2Store(t)
	postProfile(t, st, "backend",
		`{"name":"GET /chapters/7","duration_ms":40,"service":"backend-api","route":"GET /chapters/{id}",
		  "spans":[{"name":"db","kind":"db","duration_ms":9,"route":"SELECT chapters","peer_service":"mysql-primary"}]}`,
		map[string]string{HeaderTraceparent: v2Traceparent})
	svc, err := st.ServiceByName("backend", "backend-api")
	if err != nil || svc == nil {
		t.Fatalf("service=%+v err=%v", svc, err)
	}
	routes, err := st.Routes(svc.ID)
	if err != nil || len(routes) != 2 {
		t.Fatalf("routes=%+v err=%v, want the root route and the span route", routes, err)
	}
	patterns := map[string]string{}
	for _, r := range routes {
		patterns[r.Pattern] = r.Kind
	}
	if patterns["GET /chapters/{id}"] != "request" {
		t.Errorf("root route = %+v, want kind from the profile", patterns)
	}
	if patterns["SELECT chapters"] != "db" {
		t.Errorf("span route = %+v, want the span's kind", patterns)
	}
	if _, err := st.ServiceByName("backend", "mysql-primary"); err != nil {
		t.Fatalf("peer service lookup: %v", err)
	}
}

func TestServiceLabellingForV1Payload(t *testing.T) {
	st, _ := newV2Store(t)
	postProfile(t, st, "backend", `{"name":"GET /x","duration_ms":5}`, nil)
	svc, err := st.ServiceByName("backend", "backend")
	if err != nil || svc == nil {
		t.Fatalf("a v1 payload must label the event with the project key: %+v err=%v", svc, err)
	}
	if !svc.TrustedTraceparent {
		t.Error("the v1 project label must keep the traceparent joining the pre-v2 server honoured")
	}
}

// fakeSettings is the live knob source used instead of the store, so a reload
// is provable without a database.
type fakeSettings map[string]string

func (f fakeSettings) Setting(key string) (string, error) { return f[key], nil }

func (f fakeSettings) SettingFloat(key string, fallback float64) float64 {
	v, ok := f[key]
	if !ok {
		return fallback
	}
	out, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return out
}

func TestAdmissionItemBudgetReloadsLive(t *testing.T) {
	src := fakeSettings{}
	sh := NewShaper(1000, 1000, WithSettings(src))
	if sh.ItemBudget(ItemErrors) != 0 {
		t.Fatal("an unset item budget must be off, not zero-capacity")
	}
	if !sh.AllowItem("backend", ItemErrors) {
		t.Fatal("an unconfigured item budget must admit")
	}

	src[SettingIngestRateErrors] = "0.0001"
	sh.SetBudget(1000, 1000)
	if got := sh.ItemBudget(ItemErrors); got != 0.0001 {
		t.Fatalf("item budget = %v, want the saved 0.0001", got)
	}
	if !sh.AllowItem("backend", ItemErrors) {
		t.Fatal("first event after the reload must fit")
	}
	if sh.AllowItem("backend", ItemErrors) {
		t.Fatal("the errors budget must refuse the second event")
	}
	// A different item type has its own bucket: the refusal above must not
	// spend the profiles allowance.
	if !sh.AllowItem("backend", ItemProfiles) {
		t.Fatal("item budgets must not share a bucket")
	}
}

func TestAdmissionHourlyCeilingReloadsLive(t *testing.T) {
	src := fakeSettings{}
	sh := NewShaper(1000, 1000, WithSettings(src))
	if !sh.Allow("backend") || !sh.Allow("backend") {
		t.Fatal("precondition: no hourly ceiling is configured")
	}

	src[SettingIngestHourly] = "2"
	sh.SetBudget(1000, 1000)
	if got := sh.HourlyCeiling(); got != 2 {
		t.Fatalf("hourly ceiling = %v, want 2", got)
	}
	// The window opens when the ceiling is first applied: events admitted
	// while it was off were never counted, so a freshly saved ceiling never
	// retroactively blocks a project mid-hour.
	if !sh.Allow("backend") || !sh.Allow("backend") {
		t.Fatal("the first two events of the hour must fit a ceiling of 2")
	}
	if sh.Allow("backend") {
		t.Fatal("the third event of the hour must be refused")
	}
	if !sh.Allow("other") {
		t.Fatal("the hourly ceiling is per project")
	}
}

func TestAdmission429CarriesRetryAfter(t *testing.T) {
	sh := NewShaper(0.0001, 1)
	if !sh.Allow("backend") {
		t.Fatal("precondition: first event fits the burst")
	}
	h := WithShaper(sh, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("the handler must not run past the budget")
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/ingest/backend/error", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429", rec.Code)
	}
	wait, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || wait < 1 {
		t.Fatalf("Retry-After = %q, want a positive number of seconds", rec.Header().Get("Retry-After"))
	}
}

func TestAdmissionItemBudgetAnswers429BeforeTheBody(t *testing.T) {
	src := fakeSettings{SettingIngestRateErrors: "0.0001"}
	sh := NewShaper(1000, 1000, WithSettings(src))
	served := 0
	h := WithShaper(sh, func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusAccepted)
	})
	call := func() int {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/ingest/backend/error", strings.NewReader(`{"message":"x"}`)))
		return rec.Code
	}
	if got := call(); got != http.StatusAccepted {
		t.Fatalf("first error event: %d", got)
	}
	if got := call(); got != http.StatusTooManyRequests {
		t.Fatalf("second error event past the errors budget: %d, want 429", got)
	}
	if served != 1 {
		t.Fatalf("the handler ran %d times, want 1 — a 429 must precede the body read", served)
	}
}

// TestSentryEnvelopeCarriesSpansAndSession covers the two v2 items a Sentry
// envelope gained: an event's span batch (rooted at its transaction) and a
// session item, whose "exited" is this server's "ok".
func TestSentryEnvelopeCarriesSpansAndSession(t *testing.T) {
	st, _ := newV2Store(t)
	envelope := strings.Join([]string{
		`{"event_id":"e1","environment":"production"}`,
		`{"type":"event"}`,
		`{"level":"error","transaction":"GET /orders/{id}","exception":{"values":[{"type":"QueryException","value":"boom"}]},"spans":[{"span_id":"cccccccccccccccc","name":"db","kind":"db","duration_ms":4,"route":"SELECT orders"}]}`,
		`{"type":"session"}`,
		`{"session_id":"sentry-s1","status":"exited","release":"r-9","duration_ms":100}`,
	}, "\n")
	req := httptest.NewRequest(http.MethodPost, "/ingest/backend/sentry", strings.NewReader(envelope))
	req.SetPathValue("project", "backend")
	req.Header.Set(HeaderTraceparent, v2Traceparent)
	rec := httptest.NewRecorder()
	SentryHandler(st)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"sessions":1`) {
		t.Errorf("envelope did not record the session item: %s", rec.Body.String())
	}
	issues, err := st.Issues(store.IssueQuery{Project: "backend", Limit: 10})
	if err != nil || len(issues) != 1 {
		t.Fatalf("issues=%d err=%v", len(issues), err)
	}
	spans, err := st.TraceSpans("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatalf("spans: %v", err)
	}
	if len(spans) != 2 {
		t.Fatalf("trace holds %d spans, want the transaction root plus one child", len(spans))
	}
	if spans[0].Name != "GET /orders/{id}" {
		t.Errorf("root span name = %q, want the transaction", spans[0].Name)
	}
	sessions, crashed, _, err := st.CrashFree("backend", "r-9")
	if err != nil || sessions != 1 || crashed != 0 {
		t.Fatalf("sessions=%d crashed=%d err=%v, want 1/0", sessions, crashed, err)
	}
}

func TestSessionHandlerRecordsOutcome(t *testing.T) {
	st, _ := newV2Store(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingest/{project}/session", SessionHandler(st))
	post := func(body string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ingest/backend/session", strings.NewReader(body)))
		return rec.Code
	}
	if code := post(`{"session_id":"s-1","release":"r-1","status":"crashed","route":"/chapters/[id]","duration_ms":8120,"service":"front","env":"production"}`); code != http.StatusNoContent {
		t.Fatalf("valid session: %d, want 204", code)
	}
	sessions, crashed, crashFree, err := st.CrashFree("backend", "r-1")
	if err != nil {
		t.Fatalf("crash free: %v", err)
	}
	if sessions != 1 || crashed != 1 || crashFree != 0 {
		t.Fatalf("sessions=%d crashed=%d crashFree=%v, want 1/1/0", sessions, crashed, crashFree)
	}
	// The beacon is idempotent: a retry must not count the session twice.
	if code := post(`{"session_id":"s-1","release":"r-1","status":"ok","duration_ms":10}`); code != http.StatusNoContent {
		t.Fatalf("retry: %d, want 204", code)
	}
	if sessions, _, _, _ = st.CrashFree("backend", "r-1"); sessions != 1 {
		t.Fatalf("a retried beacon counted %d sessions, want 1", sessions)
	}
	if code := post(`{"session_id":"s-2","status":"weird"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown status: %d, want 422", code)
	}
	if code := post(`not-json`); code != http.StatusBadRequest {
		t.Fatalf("malformed body: %d, want 400", code)
	}
}

func TestRawEnvelopeRingKeepsTheLastN(t *testing.T) {
	st, path := newV2Store(t)
	setRawSetting(t, path, SettingKeepEnvelopes, "2")
	for _, name := range []string{"one", "two", "three"} {
		postProfile(t, st, "backend", `{"name":"`+name+`","duration_ms":5}`, nil)
	}
	envelopes, err := st.RawEnvelopes(10)
	if err != nil {
		t.Fatalf("envelopes: %v", err)
	}
	if len(envelopes) != 2 {
		t.Fatalf("ring holds %d bodies, want the last 2", len(envelopes))
	}
	if !strings.Contains(string(envelopes[0].Body), `"three"`) {
		t.Errorf("newest body = %s, want the last payload", envelopes[0].Body)
	}
	if !envelopes[0].Accepted || envelopes[0].Route != "profile" {
		t.Errorf("envelope = %+v, want an accepted profile", envelopes[0])
	}
}

func TestRawEnvelopeRingOffByDefault(t *testing.T) {
	st, _ := newV2Store(t)
	postProfile(t, st, "backend", `{"name":"x","duration_ms":5}`, nil)
	envelopes, err := st.RawEnvelopes(10)
	if err != nil {
		t.Fatalf("envelopes: %v", err)
	}
	if len(envelopes) != 0 {
		t.Fatalf("default capture kept %d bodies, want none", len(envelopes))
	}
}

func TestDigestWarnModeStoresTheInvalidField(t *testing.T) {
	st, path := newV2Store(t)
	setRawSetting(t, path, SettingValidate, ValidateWarn)
	rec := postProfile(t, st, "backend", `{"name":"GET /warn","duration_ms":-5}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("warn mode refused the payload: %d %s", rec.Code, rec.Body.String())
	}
	p := lastProfile(t, st, "backend")
	if p.Name != "GET /warn" {
		t.Fatalf("warn mode dropped the event: %+v", p)
	}
}

func TestDigestStrictModeIsTheDefault(t *testing.T) {
	st, _ := newV2Store(t)
	rec := postProfile(t, st, "backend", `{"name":"GET /strict","duration_ms":-5}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("strict mode must still refuse: %d", rec.Code)
	}
	if rows, _ := st.Profiles("backend", 5); len(rows) != 0 {
		t.Fatalf("a refused payload stored %d profiles", len(rows))
	}
}
