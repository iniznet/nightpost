package store

import (
	"encoding/json"
)

// GroupingConfig is the server-side fingerprinting policy for one project,
// stored as JSON. A producer-supplied fingerprint becomes a rule INPUT
// ({{ default }}), never the stored key, so a producer that changes its own
// scheme cannot split issue history.
type GroupingConfig struct {
	FingerprintRules []FingerprintRule `json:"fingerprint_rules"`
	StackTraceRules  StackTraceRule    `json:"stack_trace_rules"`
}

type FingerprintRule struct {
	Match       string `json:"match"`
	Pattern     string `json:"pattern"`
	Fingerprint string `json:"fingerprint"`
}

type StackTraceRule struct {
	GroupBy string   `json:"group_by"`
	Ignore  []string `json:"ignore"`
}

// groupingSettingsKey is the per-project prefix; the full settings key is
// "<prefix>:<projectKey>" so two projects never share fingerprint rules.
const groupingSettingsKey = "grouping"

// defaultStackIgnore keeps vendored and runtime frames out of the app-frame
// bucket. Without it, one dependency version bump would re-fingerprint every
// issue as a brand-new defect.
var defaultStackIgnore = []string{"vendor/", "node_modules/", "/usr/local/go/src"}

// GroupingConfig returns a project's fingerprinting policy, or the documented
// default (group by app frames, ignore vendored/runtime frames) when none has
// been stored. A malformed blob falls back to defaults rather than taking
// ingest down.
func (s *Store) GroupingConfig(projectKey string) (GroupingConfig, error) {
	cfg := GroupingConfig{
		StackTraceRules: StackTraceRule{
			GroupBy: "app_frames",
			Ignore:  append([]string(nil), defaultStackIgnore...),
		},
	}
	raw, err := s.Setting(groupingSettingsKey + ":" + projectKey)
	if err != nil {
		return cfg, err
	}
	if raw == "" {
		return cfg, nil
	}
	var stored GroupingConfig
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return cfg, nil
	}
	if stored.StackTraceRules.GroupBy != "" {
		cfg.StackTraceRules.GroupBy = stored.StackTraceRules.GroupBy
	}
	if stored.StackTraceRules.Ignore != nil {
		cfg.StackTraceRules.Ignore = stored.StackTraceRules.Ignore
	}
	cfg.FingerprintRules = stored.FingerprintRules
	return cfg, nil
}
