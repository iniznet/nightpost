package store

import (
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// M6.3: operator-tunable settings live in the DATABASE, not in process flags.
// The flags survive as IMPORT-ONCE defaults — SeedSettings writes a default
// only when the key is absent, so a value an operator changed in the UI
// survives every restart and redeploy. That is the same shape M6.3 prescribes
// for -alerts-config, and it is why the seed must never overwrite.

const (
	SettingRetentionDays      = "retention_days"
	SettingPruneMinutes       = "prune_interval_minutes"
	SettingOccurrenceCap      = "occurrence_cap"
	SettingProfilesPerProject = "profiles_per_project"
	SettingIngestRate         = "ingest_rate"
	SettingIngestBurst        = "ingest_burst"
)

// settingBound is the accepted numeric range for one key. A bound of 0 for
// min means the key accepts zero (0 = unlimited for occurrence_cap).
type settingBound struct {
	min, max float64
	integer  bool
}

// settingBounds is the single validation table. A key absent from this map is
// NOT writable, so a typo in the UI cannot mint a setting nothing reads.
var settingBounds = map[string]settingBound{
	SettingRetentionDays:      {min: 1, max: 3650, integer: true},
	SettingPruneMinutes:       {min: 1, max: 1440, integer: true},
	SettingOccurrenceCap:      {min: 0, max: 100000, integer: true},
	SettingProfilesPerProject: {min: 1, max: 1000000, integer: true},
	SettingIngestRate:         {min: 0.1, max: 100000},
	SettingIngestBurst:        {min: 1, max: 1000000},
}

// SettingKeys lists every writable key in display order.
var SettingKeys = []string{
	SettingRetentionDays,
	SettingPruneMinutes,
	SettingOccurrenceCap,
	SettingProfilesPerProject,
	SettingIngestRate,
	SettingIngestBurst,
}

// SettingDefaults carries the boot-time defaults for every key — the values
// the process used to read straight from its flags.
type SettingDefaults struct {
	RetentionDays        int
	PruneIntervalMinutes int
	OccurrenceCap        int
	ProfilesPerProject   int
	IngestRate           float64
	IngestBurst          float64
}

// SeedSettings inserts any ABSENT key from the defaults; a stored value always
// wins, so the seed is idempotent and never clobbers an operator's choice.
func (s *Store) SeedSettings(d SettingDefaults) error {
	now := time.Now().Unix()
	seed := map[string]string{
		SettingRetentionDays:      strconv.Itoa(d.RetentionDays),
		SettingPruneMinutes:       strconv.Itoa(d.PruneIntervalMinutes),
		SettingOccurrenceCap:      strconv.Itoa(d.OccurrenceCap),
		SettingProfilesPerProject: strconv.Itoa(d.ProfilesPerProject),
		SettingIngestRate:         strconv.FormatFloat(d.IngestRate, 'f', -1, 64),
		SettingIngestBurst:        strconv.FormatFloat(d.IngestBurst, 'f', -1, 64),
	}
	for _, k := range SettingKeys {
		v := seed[k]
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			continue // an unusable default is skipped, never stored
		}
		if _, err := s.wdb.Exec(`INSERT OR IGNORE INTO settings (key, value, updated_at)
			VALUES (?, ?, ?)`, k, v, now); err != nil {
			return fmt.Errorf("seed setting %s: %w", k, err)
		}
	}
	return nil
}

// Setting returns a key's stored value ("" when unset).
func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.rdb.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SettingInt reads a key as an int, falling back when absent or unparseable —
// a corrupted row must never take the retention loop down.
func (s *Store) SettingInt(key string, fallback int) int {
	v, err := s.Setting(key)
	if err != nil {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// SettingFloat reads a key as a float, falling back when absent/unparseable.
func (s *Store) SettingFloat(key string, fallback float64) float64 {
	v, err := s.Setting(key)
	if err != nil {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return f
}

// AllSettings returns every stored key/value once.
func (s *Store) AllSettings() (map[string]string, error) {
	rows, err := s.rdb.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ValidateSetting checks one key/value against settingBounds.
func ValidateSetting(key, value string) error {
	b, ok := settingBounds[key]
	if !ok {
		return fmt.Errorf("unknown setting %q", key)
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fmt.Errorf("%s must be a number", key)
	}
	if b.integer && f != float64(int64(f)) {
		return fmt.Errorf("%s must be a whole number", key)
	}
	if f < b.min || f > b.max {
		return fmt.Errorf("%s must be between %g and %g", key, b.min, b.max)
	}
	return nil
}

// SetSetting validates and stores one key.
func (s *Store) SetSetting(key, value string) error {
	if err := ValidateSetting(key, value); err != nil {
		return err
	}
	_, err := s.wdb.Exec(`INSERT INTO settings (key, value, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("set setting %s: %w", key, err)
	}
	return nil
}
