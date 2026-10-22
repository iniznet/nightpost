package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/iniznet/nightpost/internal/ingest"
	"github.com/iniznet/nightpost/internal/store"
	"github.com/iniznet/nightpost/internal/web"
)

// TestReadyHandlerReportsOK pins the readiness contract's shape.
// BuildRevision must always be present so "which build is deployed?" is
// answerable from the endpoint.
func TestReadyHandlerReportsOK(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// the machine-readable leg also carries the ingest-rejection
	// counters, so a monitor can alert on them without scraping the UI.
	health := web.IngestHealth{
		ShaperRejections: func() int64 { return 7 },
		GateRefusals:     func() int64 { return 3 },
		UnknownAdmits:    func() int64 { return 2 },
	}
	rec := httptest.NewRecorder()
	readyHandler(st, health)(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	if body["status"] != "ok" || body["db"] != "ok" {
		t.Errorf("payload = %v", body)
	}
	if _, ok := body["revision"]; !ok {
		t.Error("readiness must report the build revision")
	}
	ingest, ok := body["ingest"].(map[string]any)
	if !ok {
		t.Fatalf("no ingest counters in the readiness payload: %v", body)
	}
	for key, want := range map[string]float64{"shaper_rejections": 7, "gate_refusals": 3, "unknown_admits": 2} {
		if got, ok := ingest[key].(float64); !ok || got != want {
			t.Errorf("ingest.%s = %v, want %v", key, ingest[key], want)
		}
	}

	// Unauthenticated: it must never carry tenant figures.
	for _, banned := range []string{"projects", "unsigned_projects", "users"} {
		if _, ok := body[banned]; ok {
			t.Errorf("readiness leaks %q to an unauthenticated caller", banned)
		}
	}
}

// TestApplyBudgetReadsTheSettingsTable is the defect's own guard: before
// iniznet/fenrirealm-tracker#363 a saved ingest_rate/ingest_burst was stored,
// rendered, and read by nothing.
func TestApplyBudgetReadsTheSettingsTable(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.SeedSettings(store.SettingDefaults{
		RetentionDays:        30,
		PruneIntervalMinutes: 10,
		OccurrenceCap:        30,
		ProfilesPerProject:   5000,
		IngestRate:           50,
		IngestBurst:          100,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	sh := ingest.NewShaper(50, 100)
	if err := st.SetSetting(store.SettingIngestRate, "12.5"); err != nil {
		t.Fatalf("set rate: %v", err)
	}
	if err := st.SetSetting(store.SettingIngestBurst, "7"); err != nil {
		t.Fatalf("set burst: %v", err)
	}
	applyBudget(st, sh, 50, 100)
	if rate, burst := sh.Budget(); rate != 12.5 || burst != 7 {
		t.Fatalf("shaper budget = %v/%v, want the SAVED 12.5/7 (a flag must not win)", rate, burst)
	}

	// An unseeded key falls back to the flag, never to zero — a fresh install
	// must not start with admission switched off.
	bare, err := store.Open(filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatalf("open bare: %v", err)
	}
	defer bare.Close()
	bareShaper := ingest.NewShaper(50, 100)
	applyBudget(bare, bareShaper, 20, 40)
	if rate, burst := bareShaper.Budget(); rate != 20 || burst != 40 {
		t.Fatalf("unseeded budget = %v/%v, want the flag fallback 20/40", rate, burst)
	}
}

// TestReadyHandlerReportsStoreFailure is the half that matters: a readiness
// probe reporting 200 while the store is gone is worse than none at all.
func TestReadyHandlerReportsStoreFailure(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rec := httptest.NewRecorder()
	readyHandler(st, web.IngestHealth{})(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed store: got %d, want 503", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	if body["status"] != "unavailable" {
		t.Errorf("status = %v, want unavailable", body["status"])
	}
}

// TestResolveSessionSecretPersistsAcrossRestarts is the installer
// contract: the old random-per-boot default silently logged every admin out on
// each deploy.
func TestResolveSessionSecretPersistsAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "session.key")

	first, err := resolveSessionSecret("", path)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("generated an empty secret")
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("secret file not written: %v", err)
	}
	// Windows reports 0666 for every ordinary file (Go emulates permissions
	// there), so the mode assertion only means something on POSIX.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("secret file mode = %o, want 600", perm)
		}
	}

	second, err := resolveSessionSecret("", path)
	if err != nil {
		t.Fatalf("second boot: %v", err)
	}
	if string(second) != string(first) {
		t.Error("a restart must reuse the persisted secret, not mint a new one")
	}
}

func TestResolveSessionSecretFlagWinsThenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.key")
	if err := os.WriteFile(path, []byte("fromfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveSessionSecret("fromflag", path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fromflag" {
		t.Errorf("got %q, want the flag value to win", got)
	}

	got, err = resolveSessionSecret("", path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fromfile" {
		t.Errorf("got %q, want the file value (whitespace trimmed)", got)
	}
}

func TestResolveSessionSecretWithoutFileStaysPerBoot(t *testing.T) {
	a, err := resolveSessionSecret("", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := resolveSessionSecret("", "")
	if err != nil {
		t.Fatal(err)
	}
	if string(a) == string(b) {
		t.Error("with no file configured the key must stay random per boot")
	}
}
