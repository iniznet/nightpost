package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"regexp"
	"strings"

	"github.com/iniznet/nightpost/internal/store"
)

// Server-side grouping.
//
// The producer used to decide the issue key. That made the key a producer
// detail: the day an SDK changed its scheme every open issue reopened as a new
// one, and two producers of the same defect never met. The server now owns the
// key, so the identity of a defect is a property of the defect.
//
// The key is deliberately narrow — exception type, the first two APP frames and
// the message with volatile tokens blanked — because every part of it is a
// promise: change one and history splits. Nothing about the producer's own
// fingerprint enters it except through a project rule that says so explicitly.

// groupFrame is one stack frame as the grouping pass sees it. Only the three
// fields that can identify a call site are kept; line numbers are not, because
// a moved statement is not a new defect.
type groupFrame struct {
	Module   string
	Function string
	File     string
}

// groupEvent is everything one exception contributes to the decision.
type groupEvent struct {
	Type                string
	Message             string
	Frames              []groupFrame
	ProducerFingerprint string
	// Fields is the view a fingerprint rule matches against. It is passed in
	// already shaped so the store never has to know a producer's wire format.
	Fields map[string]any
}

var (
	groupUUID   = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	groupQuoted = regexp.MustCompile("\"[^\"]*\"|'[^']*'")
	groupDigits = regexp.MustCompile(`[0-9]+`)
)

// FoldError is the grouping decision for one exception: the stored key and the
// path that produced it.
//
// A matching fingerprint rule wins outright — that is the operator's override
// and the only place a producer key may survive (through {{ default }} inside
// the rule's fingerprint). Without a rule the server buckets the app frames. A
// project that reports no frames at all has nothing to bucket, so the producer's
// key is the only identity available; the source records that so the read side
// can label such an issue instead of pretending it was computed here.
func FoldError(cfg store.GroupingConfig, ev groupEvent) (fingerprint, source string) {
	if key, ok := cfg.MatchFingerprintRule(ev.Fields, ev.ProducerFingerprint); ok {
		return key, store.GroupSourceRule
	}
	bucket := appFrameBucket(ev.Frames, cfg.StackTraceRules)
	if bucket == "" && strings.TrimSpace(ev.ProducerFingerprint) != "" {
		return strings.TrimSpace(ev.ProducerFingerprint), store.GroupSourceProducer
	}
	return exceptionKey(ev.Type, bucket, ev.Message), store.GroupSourceServer
}

// exceptionKey is hex(sha256(type|bucket|template))[0:32] — the documented
// shape. It is truncated to 32 characters because the key is written on every
// occurrence row's issue join and never read by a human.
func exceptionKey(class, bucket, message string) string {
	sum := sha256.Sum256([]byte(class + "|" + bucket + "|" + messageTemplate(message)))
	return hex.EncodeToString(sum[:])[:32]
}

// messageTemplate blanks the volatile tokens of a message: uuids first, then
// quoted values (a quoted value is often the variable part of a message), then
// bare digits. Order matters — a uuid starred late would already have lost its
// shape to the digit pass and two different ids would still look different.
func messageTemplate(message string) string {
	s := groupUUID.ReplaceAllString(message, "*")
	s = groupQuoted.ReplaceAllString(s, "*")
	s = groupDigits.ReplaceAllString(s, "*")
	return strings.TrimSpace(s)
}

// appFrameBucket renders the first two application frames as the stable part of
// the key. "app" excludes dependencies and the runtime: a framework upgrade
// changes their frames constantly, and grouping a defect under the framework
// version would make every deploy a fresh incident.
//
// group_by "all" is honoured for a project that wants every frame counted; any
// other value falls back to the documented default rather than inventing a
// policy.
func appFrameBucket(frames []groupFrame, rule store.StackTraceRule) string {
	allFrames := strings.EqualFold(strings.TrimSpace(rule.GroupBy), "all")
	parts := make([]string, 0, 2)
	for _, f := range frames {
		if !allFrames && frameIgnored(f, rule.Ignore) {
			continue
		}
		parts = append(parts, f.Module+":"+f.Function+":"+f.File)
		if len(parts) == 2 {
			break
		}
	}
	return strings.Join(parts, " -> ")
}

// frameIgnored reports whether a frame belongs to a dependency or the runtime
// rather than to the application. A pattern with a glob metacharacter is matched
// against the file and the module; anything else is a substring, which is how
// the documented defaults ("vendor/", "node_modules/") are written.
func frameIgnored(f groupFrame, ignore []string) bool {
	for _, raw := range ignore {
		pattern := strings.TrimSpace(raw)
		if pattern == "" {
			continue
		}
		if strings.ContainsAny(pattern, "*?[") {
			if ok, err := path.Match(pattern, f.File); err == nil && ok {
				return true
			}
			if ok, err := path.Match(pattern, f.Module); err == nil && ok {
				return true
			}
			continue
		}
		if strings.Contains(f.Module+" "+f.Function+" "+f.File, pattern) {
			return true
		}
	}
	return isRuntimeFrame(f)
}

// isRuntimeFrame catches the Go runtime by function package and by file, since
// a runtime frame's path depends on how the binary was built (the documented
// default ignore only covers the toolchain's own GOROOT).
func isRuntimeFrame(f groupFrame) bool {
	fn := strings.TrimSpace(f.Function)
	if strings.HasPrefix(fn, "runtime.") || strings.HasPrefix(fn, "runtime/") {
		return true
	}
	file := strings.ReplaceAll(f.File, "\\", "/")
	return strings.Contains(file, "/runtime/") || strings.HasPrefix(file, "runtime/")
}

// sentryFrames reads the frame list off an already-decoded Sentry exception.
// Frames stay in the producer's order: the plan's bucket is app_frames[0:2] as
// received, so reversing here would silently regroup every stored issue.
func sentryFrames(exc map[string]any) []groupFrame {
	trace, ok := exc["stacktrace"].(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := trace["frames"].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	frames := make([]groupFrame, 0, len(raw))
	for _, item := range raw {
		frame, ok := item.(map[string]any)
		if !ok {
			continue
		}
		file, _ := frame["filename"].(string)
		if file == "" {
			file, _ = frame["abs_path"].(string)
		}
		module, _ := frame["module"].(string)
		fn, _ := frame["function"].(string)
		if file == "" && module == "" && fn == "" {
			continue
		}
		frames = append(frames, groupFrame{Module: module, Function: fn, File: file})
	}
	return frames
}

// producerFingerprint reads the key the producing SDK would have used. Sentry
// sends it as an array and some hand-built payloads as a plain string.
func producerFingerprint(event map[string]any) string {
	switch v := event["fingerprint"].(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				parts = append(parts, strings.TrimSpace(s))
			}
		}
		return strings.Join(parts, ",")
	}
	return ""
}

// errorRuleFields is the view a fingerprint rule matches against: the fields
// every error carries, under dotted paths ("error.type", "error.message"), so a
// rule names a field rather than a producer's nesting.
func errorRuleFields(projectKey, class, message, producerFP, level, environment string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"type":        class,
			"message":     message,
			"fingerprint": producerFP,
		},
		"level":       level,
		"environment": environment,
		"project":     projectKey,
	}
}
