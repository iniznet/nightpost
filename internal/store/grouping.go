package store

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
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

// Group-source values recorded on issues.group_source. The path is kept
// because it is what makes a key trustworthy: a "server" key can be recomputed
// after a policy change, a "rule" key names the rule that took over, and a
// "producer" key is the only identity a project that reports no frames has.
const (
	GroupSourceServer   = "server"
	GroupSourceRule     = "rule"
	GroupSourceProducer = "producer"
)

// ruleDefaultToken is the placeholder a rule may use to keep the producer's key
// inside its output. It is the ONLY way a producer fingerprint can reach the
// stored key, which is what stops a producer's scheme change from splitting
// issue history.
var ruleDefaultToken = regexp.MustCompile(`\{\{\s*default\s*\}\}`)

// rulePatterns caches compiled rule expressions. Ingest evaluates a project's
// rule list on every exception, so a compile per event would be a compile on
// the hot path. A pattern that fails to compile is cached as a nil entry: one
// malformed rule must not take ingest down, must not be retried forever, and
// must not shadow a later rule that still works.
var rulePatterns sync.Map

func compiledRule(pattern string) *regexp.Regexp {
	if cached, ok := rulePatterns.Load(pattern); ok {
		re, _ := cached.(*regexp.Regexp)
		return re
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		re = nil
	}
	rulePatterns.Store(pattern, re)
	return re
}

// MatchFingerprintRule evaluates a project's rules against one event, first
// match wins. match is a dotted field path ("error.type"); pattern is a regular
// expression over that field's value; fingerprint is the key every matching
// event groups under, with {{ default }} replaced by the producer's own key.
func (cfg GroupingConfig) MatchFingerprintRule(event map[string]any, producerFingerprint string) (string, bool) {
	for _, rule := range cfg.FingerprintRules {
		field := strings.TrimSpace(rule.Match)
		if field == "" || rule.Pattern == "" {
			continue
		}
		value, ok := eventField(event, field)
		if !ok {
			continue
		}
		re := compiledRule(rule.Pattern)
		if re == nil || !re.MatchString(value) {
			continue
		}
		key := strings.TrimSpace(ruleDefaultToken.ReplaceAllStringFunc(rule.Fingerprint, func(string) string {
			return producerFingerprint
		}))
		if key == "" {
			continue
		}
		return key, true
	}
	return "", false
}

// eventField resolves a dotted path against a nested event map. A missing or
// null segment reports "no value" rather than an empty string, so a rule never
// matches on the absence of a field.
func eventField(event map[string]any, path string) (string, bool) {
	var current any = event
	for _, segment := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		if current, ok = object[segment]; !ok {
			return "", false
		}
	}
	switch v := current.(type) {
	case nil:
		return "", false
	case string:
		return v, v != ""
	default:
		return fmt.Sprint(v), true
	}
}

// SetGroupingConfig stores a project's policy as JSON under the documented
// settings key. It refuses a rule that ingest would only skip at runtime — a
// rule that cannot fire is more expensive to debug in production than at the
// moment it is written.
func (s *Store) SetGroupingConfig(projectKey string, cfg GroupingConfig) error {
	if strings.TrimSpace(projectKey) == "" {
		return fmt.Errorf("grouping: project key is required")
	}
	for i, rule := range cfg.FingerprintRules {
		if strings.TrimSpace(rule.Match) == "" {
			return fmt.Errorf("grouping rule %d: match is required", i)
		}
		if strings.TrimSpace(rule.Fingerprint) == "" {
			return fmt.Errorf("grouping rule %d: fingerprint is required", i)
		}
		if _, err := regexp.Compile(rule.Pattern); err != nil {
			return fmt.Errorf("grouping rule %d: pattern: %w", i, err)
		}
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("grouping: encode: %w", err)
	}
	_, err = s.wdb.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		groupingSettingsKey+":"+projectKey, string(raw), time.Now().Unix())
	if err != nil {
		return fmt.Errorf("grouping: store: %w", err)
	}
	return nil
}

// GroupedError is one exception occurrence plus the path that produced its
// grouping key. RecordError keeps its historical signature for the non-error
// producers; the error path goes through here because group_source has to be
// written by whoever chose the key.
type GroupedError struct {
	ProjectKey     string
	ExceptionClass string
	Message        string
	Fingerprint    string
	GroupSource    string
	Environment    string
	Level          string
	Payload        any
}

// RecordGroupedError folds one occurrence into its issue and records the key's
// origin. The upsert mirrors RecordError's RETURNING pattern for the same
// reason: a conflict branch performs no insert, so LastInsertId would point at
// another table's row.
func (s *Store) RecordGroupedError(g GroupedError) (int64, error) {
	source := g.GroupSource
	if source == "" {
		source = GroupSourceServer
	}
	now := time.Now().Unix()
	var issueID, mergedInto int64
	err := s.wdb.QueryRow(`INSERT INTO issues
		(project_key, fingerprint, type, title, exception_class, level, environment, group_source, count, first_seen_at, last_seen_at)
		VALUES (?, ?, 'error', ?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(project_key, fingerprint) DO UPDATE SET
			count = count + 1, last_seen_at = excluded.last_seen_at,
			archived_at = 0, group_source = excluded.group_source
		RETURNING id, merged_into`,
		g.ProjectKey, g.Fingerprint, truncate(g.Message, 240), g.ExceptionClass,
		g.Level, g.Environment, source, now, now).Scan(&issueID, &mergedInto)
	if err != nil {
		return 0, fmt.Errorf("upsert issue: %w", err)
	}
	if mergedInto != 0 {
		// A merged duplicate keeps feeding its primary. Leaving the count on a
		// row the list collapses would accumulate history nobody can see.
		if _, err := s.wdb.Exec(`UPDATE issues SET count = MAX(count - 1, 0) WHERE id = ?`, issueID); err != nil {
			return issueID, fmt.Errorf("unfold merged duplicate: %w", err)
		}
		if _, err := s.wdb.Exec(`UPDATE issues SET count = count + 1, last_seen_at = ? WHERE id = ?`, now, mergedInto); err != nil {
			return issueID, fmt.Errorf("fold merged duplicate: %w", err)
		}
		issueID = mergedInto
	}
	if !s.allowOccurrence(g.Fingerprint) {
		s.dropped.Add(1)
		return issueID, nil
	}
	if err := s.insertOccurrence(issueID, "error", g.Payload, now); err != nil {
		return issueID, err
	}
	return issueID, nil
}

// MergeIssue folds duplicate into primary: the duplicate's occurrences move,
// the primary's window widens by the duplicate's, and the duplicate keeps only
// a pointer. Counts MOVE rather than copy, because every aggregate over
// occurrences would otherwise count the same events twice.
func (s *Store) MergeIssue(primaryID, duplicateID int64) error {
	if primaryID <= 0 || duplicateID <= 0 || primaryID == duplicateID {
		return fmt.Errorf("merge issues: need two distinct issue ids")
	}
	tx, err := s.wdb.Begin()
	if err != nil {
		return fmt.Errorf("merge issues: begin: %w", err)
	}
	defer tx.Rollback()

	var primaryProject string
	var primaryMerged int64
	if err := tx.QueryRow(`SELECT project_key, merged_into FROM issues WHERE id = ?`, primaryID).
		Scan(&primaryProject, &primaryMerged); err != nil {
		return fmt.Errorf("merge issues: primary: %w", err)
	}
	if primaryMerged != 0 {
		return fmt.Errorf("merge issues: primary %d is itself merged into %d", primaryID, primaryMerged)
	}
	var duplicateProject string
	var duplicateCount, duplicateFirst, duplicateLast int64
	if err := tx.QueryRow(`SELECT project_key, count, first_seen_at, last_seen_at FROM issues WHERE id = ?`, duplicateID).
		Scan(&duplicateProject, &duplicateCount, &duplicateFirst, &duplicateLast); err != nil {
		return fmt.Errorf("merge issues: duplicate: %w", err)
	}
	if duplicateProject != primaryProject {
		return fmt.Errorf("merge issues: %d and %d belong to different projects", primaryID, duplicateID)
	}

	for _, step := range []struct {
		what string
		stmt string
		args []any
	}{
		{"move occurrences", `UPDATE occurrences SET issue_id = ? WHERE issue_id = ?`, []any{primaryID, duplicateID}},
		{"widen window", `UPDATE issues SET count = count + ?, first_seen_at = MIN(first_seen_at, ?), last_seen_at = MAX(last_seen_at, ?) WHERE id = ?`,
			[]any{duplicateCount, duplicateFirst, duplicateLast, primaryID}},
		{"point duplicate", `UPDATE issues SET merged_into = ?, count = 0 WHERE id = ?`, []any{primaryID, duplicateID}},
	} {
		if _, err := tx.Exec(step.stmt, step.args...); err != nil {
			return fmt.Errorf("merge issues: %s: %w", step.what, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("merge issues: commit: %w", err)
	}
	return nil
}

// UnmergeIssue clears the pointer so the duplicate lists on its own again. The
// occurrences that moved stay with the primary: they cannot be told apart from
// the primary's own afterwards, and a wrong split would corrupt both windows.
// The reinstated row is a stub until its defect recurs.
func (s *Store) UnmergeIssue(duplicateID int64) error {
	res, err := s.wdb.Exec(`UPDATE issues SET merged_into = 0 WHERE id = ? AND merged_into <> 0`, duplicateID)
	if err != nil {
		return fmt.Errorf("unmerge issue: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("unmerge issue: %w", err)
	} else if n == 0 {
		return fmt.Errorf("unmerge issue %d: not a merged duplicate", duplicateID)
	}
	return nil
}

// SetIssueAssignee binds an issue to one user id; 0 unassigns.
func (s *Store) SetIssueAssignee(id, userID int64) error {
	if id <= 0 {
		return fmt.Errorf("assign issue: id is required")
	}
	if userID < 0 {
		return fmt.Errorf("assign issue: user id cannot be negative")
	}
	if _, err := s.wdb.Exec(`UPDATE issues SET assignee_user_id = ? WHERE id = ?`, userID, id); err != nil {
		return fmt.Errorf("assign issue: %w", err)
	}
	return nil
}

// SetIssueIgnoredUntil mutes an issue until a unix stamp; 0 (or a past stamp)
// unmutes it. The stamp is a READ filter only — ingest keeps counting, so a
// muted defect that keeps firing is still visible once the mute lapses.
func (s *Store) SetIssueIgnoredUntil(id, unix int64) error {
	if id <= 0 {
		return fmt.Errorf("ignore issue: id is required")
	}
	if unix < 0 {
		return fmt.Errorf("ignore issue: stamp cannot be negative")
	}
	if _, err := s.wdb.Exec(`UPDATE issues SET ignored_until = ? WHERE id = ?`, unix, id); err != nil {
		return fmt.Errorf("ignore issue: %w", err)
	}
	return nil
}

// IssueGrouping is the grouping state of one issue as the read side acts on it.
type IssueGrouping struct {
	GroupSource    string
	MergedInto     int64
	AssigneeUserID int64
	IgnoredUntil   int64
}

// IssueGrouping reads that state for one issue.
func (s *Store) IssueGrouping(id int64) (*IssueGrouping, error) {
	var g IssueGrouping
	err := s.rdb.QueryRow(`SELECT group_source, merged_into, assignee_user_id, ignored_until FROM issues WHERE id = ?`, id).
		Scan(&g.GroupSource, &g.MergedInto, &g.AssigneeUserID, &g.IgnoredUntil)
	if err != nil {
		return nil, fmt.Errorf("issue grouping: %w", err)
	}
	return &g, nil
}
