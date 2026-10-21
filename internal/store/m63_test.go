package store

import (
	"testing"
	"time"
)

// TestSettingsSeedIsImportOnce is the contract: the process flags seed a
// key ONCE, and a value an operator saved afterwards survives a re-seed (i.e.
// a restart or redeploy with the old flags).
func TestSettingsSeedIsImportOnce(t *testing.T) {
	st := newTestStore(t)
	defaults := SettingDefaults{
		RetentionDays: 30, PruneIntervalMinutes: 10, OccurrenceCap: 30,
		ProfilesPerProject: 5000, IngestRate: 50, IngestBurst: 100,
	}
	if err := st.SeedSettings(defaults); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := st.SettingInt(SettingRetentionDays, -1); got != 30 {
		t.Errorf("retention_days = %d, want the seeded 30", got)
	}
	if err := st.SetSetting(SettingRetentionDays, "7"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := st.SeedSettings(defaults); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if got := st.SettingInt(SettingRetentionDays, -1); got != 7 {
		t.Errorf("re-seeding clobbered the operator's value: got %d, want 7", got)
	}
	// A key added later (absent) is still seeded by the same pass.
	if got := st.SettingInt(SettingProfilesPerProject, -1); got != 5000 {
		t.Errorf("profiles_per_project = %d, want the seeded 5000", got)
	}
}

func TestSettingReadersFallBack(t *testing.T) {
	st := newTestStore(t)
	if got := st.SettingInt("never-written", 42); got != 42 {
		t.Errorf("SettingInt fallback = %d, want 42", got)
	}
	if got := st.SettingFloat("never-written", 1.5); got != 1.5 {
		t.Errorf("SettingFloat fallback = %v, want 1.5", got)
	}
	// A corrupted row must degrade to the fallback, never take a loop down.
	if _, err := st.wdb.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('retention_days', 'not-a-number', 0)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got := st.SettingInt(SettingRetentionDays, 30); got != 30 {
		t.Errorf("corrupted setting = %d, want the fallback 30", got)
	}
}

// TestValidateSettingRejectsBadInput keeps a typo in the UI from minting a
// setting nothing reads, or a value the retention loop cannot survive.
func TestValidateSettingRejectsBadInput(t *testing.T) {
	bad := []struct{ key, value string }{
		{"no_such_key", "1"},
		{SettingRetentionDays, "abc"},
		{SettingRetentionDays, "0"},
		{SettingRetentionDays, "3660"},
		{SettingRetentionDays, "2.5"},
		{SettingIngestRate, "0"},
		{SettingProfilesPerProject, "-3"},
	}
	for _, c := range bad {
		if err := ValidateSetting(c.key, c.value); err == nil {
			t.Errorf("ValidateSetting(%q, %q) accepted bad input", c.key, c.value)
		}
	}
	good := []struct{ key, value string }{
		{SettingRetentionDays, "7"},
		{SettingOccurrenceCap, "0"},
		{SettingIngestRate, "12.5"},
		{SettingPruneMinutes, "1"},
	}
	for _, c := range good {
		if err := ValidateSetting(c.key, c.value); err != nil {
			t.Errorf("ValidateSetting(%q, %q) = %v, want nil", c.key, c.value, err)
		}
	}
	if _, err := st_settingsAll(t); err != nil {
		t.Fatal(err)
	}
}

// TestRetentionHonoursStoredSetting is the effect proof at the store
// level: the days value the retention loop reads back from settings is the
// one that prunes. The loop itself is a three-line reader of exactly this.
func TestRetentionHonoursStoredSetting(t *testing.T) {
	st := newTestStore(t)
	oldID, err := st.RecordError("backend", "E", "old", "fp-old", "production", "error", nil)
	if err != nil {
		t.Fatalf("record old: %v", err)
	}
	if _, err := st.wdb.Exec(`UPDATE occurrences SET seen_at = ? WHERE issue_id = ?`,
		time.Now().AddDate(0, 0, -2).Unix(), oldID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if _, err := st.RecordError("backend", "E", "new", "fp-new", "production", "error", nil); err != nil {
		t.Fatalf("record new: %v", err)
	}

	stored := 30
	if err := st.SeedSettings(SettingDefaults{RetentionDays: stored, PruneIntervalMinutes: 10, ProfilesPerProject: 5000}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.SetSetting(SettingRetentionDays, "1"); err != nil {
		t.Fatalf("set: %v", err)
	}

	days := st.SettingInt(SettingRetentionDays, stored)
	keep := st.SettingInt(SettingProfilesPerProject, 5000)
	if _, err := st.RunRetention(days, keep); err != nil {
		t.Fatalf("retention: %v", err)
	}
	var n int64
	if err := st.rdb.QueryRow(`SELECT COUNT(*) FROM occurrences`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("occurrences after a 1-day retention = %d, want 1 (the 2-day-old row pruned)", n)
	}
}

func st_settingsAll(t *testing.T) (map[string]string, error) {
	t.Helper()
	st := newTestStore(t)
	return st.AllSettings()
}
