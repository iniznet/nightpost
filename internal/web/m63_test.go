package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/iniznet/nightpost/internal/store"
)

// TestSettingsPageRendersAndSaves is the M6.3 done-when in miniature: a
// setting changed through the page is the value the store now reports.
func TestSettingsPageRendersAndSaves(t *testing.T) {
	srv, st := newTestServer(t)
	if err := st.SeedSettings(store.SettingDefaults{
		RetentionDays: 30, PruneIntervalMinutes: 10, OccurrenceCap: 30,
		ProfilesPerProject: 5000, IngestRate: 50, IngestBurst: 100,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Zero params must render the page with its defaults.
	resp, _ := http.Get(srv.URL + "/settings")
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings status %d", resp.StatusCode)
	}
	for _, want := range []string{`name="retention_days"`, `value="30"`, "import-once", "<main class=\"wrap\">"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}

	// Save a new value; it must persist and re-render.
	form := url.Values{"retention_days": {"7"}, "prune_interval_minutes": {"5"}}
	resp, err := http.PostForm(srv.URL+"/settings/save", form)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body = readAll(t, resp)
	if !strings.Contains(body, "Saved 2 setting(s)") {
		t.Errorf("save did not report: %.300s", body)
	}
	if got := st.SettingInt(store.SettingRetentionDays, -1); got != 7 {
		t.Errorf("retention_days after save = %d, want 7", got)
	}
	if got := st.SettingInt(store.SettingPruneMinutes, -1); got != 5 {
		t.Errorf("prune_interval_minutes after save = %d, want 5", got)
	}

	// A rejected value must store nothing at all (no half-applied save).
	bad := url.Values{"retention_days": {"7"}, "prune_interval_minutes": {"9999"}}
	resp, err = http.PostForm(srv.URL+"/settings/save", bad)
	if err != nil {
		t.Fatalf("post bad: %v", err)
	}
	body = readAll(t, resp)
	if !strings.Contains(body, "form-error") {
		t.Errorf("invalid value was not surfaced: %.300s", body)
	}
	if got := st.SettingInt(store.SettingPruneMinutes, -1); got != 5 {
		t.Errorf("a rejected save mutated the store: prune = %d, want 5", got)
	}
}
