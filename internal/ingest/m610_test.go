package ingest

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/iniznet/nightpost/internal/store"
)

// TestGuardThrottlesDistinctNameFlood is the P0 guard: the per-project
// bucket is keyed on a caller-chosen name, so a flood that varies {project}
// must still meet an attacker-independent ceiling.
func TestGuardThrottlesDistinctNameFlood(t *testing.T) {
	g := NewIngressGuard(0.0001, 5, 0, 0) // global burst 5; per-address leg off
	served, rejected := 0, 0
	h := WithIngressGuard(g, func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusAccepted)
	})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingest/{project}/sentry", h)
	for i := 0; i < 200; i++ {
		req := httptest.NewRequest(http.MethodPost, "/ingest/flood-"+strconv.Itoa(i)+"/sentry", nil)
		req.RemoteAddr = "10.0.0.9:1234"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			rejected++
		}
	}
	if served > 5 {
		t.Errorf("the guard served %d requests past a burst of 5", served)
	}
	if rejected == 0 {
		t.Error("a distinct-name flood was not throttled at all")
	}
	if g.GlobalRejections() == 0 {
		t.Error("global rejections were not counted for the health strip")
	}
}

func TestGuardPerAddressLegIsolation(t *testing.T) {
	g := NewIngressGuard(1000, 1000, 0.0001, 1)
	check := func(ip string) int {
		h := WithIngressGuard(g, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		})
		req := httptest.NewRequest(http.MethodPost, "/ingest/x/sentry", nil)
		req.RemoteAddr = ip + ":5000"
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}
	if got := check("10.0.0.1"); got != http.StatusAccepted {
		t.Fatalf("first request from an address: %d", got)
	}
	if got := check("10.0.0.1"); got != http.StatusTooManyRequests {
		t.Errorf("same address past its burst: got %d, want 429", got)
	}
	if got := check("10.0.0.2"); got != http.StatusAccepted {
		t.Errorf("another address must have its own bucket: got %d", got)
	}
	if g.IPRejections() != 1 {
		t.Errorf("ip rejections = %d, want 1", g.IPRejections())
	}
}

// TestGuardIgnoresForwardedFor pins the anti-spoof property: honouring the
// caller-controlled header would hand an attacker a fresh per-address bucket
// per request — the exact bypass the guard exists to close.
func TestGuardIgnoresForwardedFor(t *testing.T) {
	g := NewIngressGuard(1000, 1000, 0.0001, 1)
	h := WithIngressGuard(g, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	call := func(xff string) int {
		req := httptest.NewRequest(http.MethodPost, "/ingest/x/sentry", nil)
		req.RemoteAddr = "10.0.0.7:5000"
		req.Header.Set("X-Forwarded-For", xff)
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}
	if got := call("1.1.1.1"); got != http.StatusAccepted {
		t.Fatalf("first request: %d", got)
	}
	if got := call("2.2.2.2"); got != http.StatusTooManyRequests {
		t.Errorf("X-Forwarded-For minted a fresh bucket: got %d, want 429", got)
	}
}

func TestClientIPStripsPort(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/ingest/x/sentry", nil)
	req.RemoteAddr = "192.0.2.10:54321"
	if got := ClientIP(req); got != "192.0.2.10" {
		t.Errorf("ClientIP = %q, want 192.0.2.10", got)
	}
}

// TestShaperBucketMapIsBounded covers the memory half of the same defect: the
// bucket map is keyed on a caller-chosen name and must not grow without limit.
func TestShaperBucketMapIsBounded(t *testing.T) {
	sh := NewShaper(0.0001, 1)
	for i := 0; i < maxShaperBuckets*3; i++ {
		sh.Allow("flood-" + strconv.Itoa(i))
	}
	if n := sh.BucketCount(); n > maxShaperBuckets {
		t.Errorf("bucket map grew to %d entries, want <= %d", n, maxShaperBuckets)
	}
}

// countingKeys is a ProjectKeyStore that also reports its unsigned-project
// count, so the auto-provision cap can be exercised.
type countingKeys struct {
	fakeKeys
	unsigned int
}

func (c *countingKeys) UnsignedProjectCount() (int, error) { return c.unsigned, nil }

func newCountingKeys(unsigned int) *countingKeys {
	return &countingKeys{
		fakeKeys: fakeKeys{keys: map[string]string{}, ensured: map[string]int{}},
		unsigned: unsigned,
	}
}

func TestKeyGateAutoProvisionCapRefusesNewNames(t *testing.T) {
	svc := newCountingKeys(2)
	g := NewKeyGate(svc, 0)
	g.ConfigureAutoProvision(2, false)

	if code := post(g, "flood-1", "", ""); code != http.StatusTooManyRequests {
		t.Errorf("past the cap: got %d, want 429", code)
	}
	if n := svc.ensureCount("flood-1"); n != 0 {
		t.Errorf("a refused name was auto-provisioned %d time(s)", n)
	}
	if g.Refusals() != 1 {
		t.Errorf("refusals = %d, want 1", g.Refusals())
	}

	// Under the cap the legacy (additive) behaviour is preserved.
	svc.unsigned = 1
	if code := post(g, "legit", "", ""); code != http.StatusAccepted {
		t.Errorf("under the cap: got %d, want 202", code)
	}
	if n := svc.ensureCount("legit"); n != 1 {
		t.Errorf("admitted name provisioned %d time(s), want 1", n)
	}
}

func TestKeyGateRequireKeysRefusesUnsignedButKeepsSignedIngest(t *testing.T) {
	svc := newCountingKeys(0)
	g := NewKeyGate(svc, 0)
	g.ConfigureAutoProvision(0, true)

	if code := post(g, "unsigned-proj", "", ""); code != http.StatusUnauthorized {
		t.Errorf("unsigned project: got %d, want 401", code)
	}
	if n := svc.ensureCount("unsigned-proj"); n != 0 {
		t.Error("no project may be provisioned when keys are required")
	}
	if g.Refusals() != 1 {
		t.Errorf("refusals = %d, want 1", g.Refusals())
	}

	// A KEYED producer keeps working — the hardening must not break signed
	// ingest, which is the whole point of the escape hatch.
	svc.set("signed", store.HashIngestKey("s3cret"))
	if code := post(g, "signed", "X-Monitor-Key", "s3cret"); code != http.StatusAccepted {
		t.Errorf("keyed request: got %d, want 202", code)
	}
}
