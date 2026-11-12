package ingest

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/iniznet/nightpost/internal/store"
)

func groupingStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "grouping.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// postSentryEnvelope sends one event inside a Sentry envelope, the wire shape
// sentry-laravel speaks.
func postSentryEnvelope(t *testing.T, st *store.Store, project, event string) {
	t.Helper()
	envelope := strings.Join([]string{
		`{"event_id":"e1","environment":"production"}`,
		`{"type":"event"}`,
		event,
	}, "\n")
	req := httptest.NewRequest(http.MethodPost, "/ingest/"+project+"/sentry", strings.NewReader(envelope))
	req.SetPathValue("project", project)
	rec := httptest.NewRecorder()
	SentryHandler(st)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sentry: got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"recorded":1`) {
		t.Fatalf("sentry: event not recorded: %s", rec.Body.String())
	}
}

func listIssues(t *testing.T, st *store.Store, project string) []store.Issue {
	t.Helper()
	issues, err := st.Issues(store.IssueQuery{Project: project, Limit: 20})
	if err != nil {
		t.Fatalf("issues: %v", err)
	}
	return issues
}

func issueGroupSource(t *testing.T, st *store.Store, id int64) string {
	t.Helper()
	state, err := st.IssueGrouping(id)
	if err != nil {
		t.Fatalf("grouping state: %v", err)
	}
	return state.GroupSource
}

// sentryException builds an event whose exception carries one frame list.
// strconv.Quote is what keeps a message with a quote in it valid JSON.
func sentryException(class, message, producerFP, frames string) string {
	stack := ""
	if frames != "" {
		stack = `,"stacktrace":{"frames":` + frames + `}`
	}
	fingerprint := ""
	if producerFP != "" {
		fingerprint = `,"fingerprint":[` + strconv.Quote(producerFP) + `]`
	}
	return `{"level":"error"` + fingerprint + `,"exception":{"values":[{"type":` + strconv.Quote(class) +
		`,"value":` + strconv.Quote(message) + stack + `}]}}`
}

const appFrame = `[{"module":"AppHttp","function":"show","filename":"/srv/app/Http/SeriesController.php"}]`

// A producer that changes its own fingerprint scheme must not split issue
// history: the producer key is a rule input, never the stored key.
func TestGroupingKeepsOneIssueAcrossProducerFingerprintChange(t *testing.T) {
	st := groupingStore(t)
	postSentryEnvelope(t, st, "backend", sentryException("QueryException", "user 42 not found", "producer-v1", appFrame))
	postSentryEnvelope(t, st, "backend", sentryException("QueryException", "user 42 not found", "producer-v2", appFrame))

	issues := listIssues(t, st, "backend")
	if len(issues) != 1 {
		t.Fatalf("the same exception under two producer fingerprints made %d issues, want 1", len(issues))
	}
	if issues[0].Count != 2 {
		t.Errorf("issue count = %d, want 2", issues[0].Count)
	}
	if fp := issues[0].Fingerprint; len(fp) != 32 {
		t.Errorf("fingerprint = %q, want 32 hex characters", fp)
	}
	if src := issueGroupSource(t, st, issues[0].ID); src != store.GroupSourceServer {
		t.Errorf("group_source = %q, want %q", src, store.GroupSourceServer)
	}
}

// Dependency frames must not reach the bucket. Without the filter the two
// exceptions below differ in their framework frames and split in two, which is
// exactly what a version bump would do to every open issue.
func TestGroupingIgnoresDependencyFrames(t *testing.T) {
	st := groupingStore(t)
	// The envelope carries one item per LINE, so a payload stays on one line.
	vendorA := `[{"module":"IlluminateFoundation","function":"handle","filename":"/srv/vendor/laravel/framework/Kernel.php"},{"module":"IlluminateRouting","function":"dispatch","filename":"/srv/vendor/laravel/framework/Router.php"},{"module":"AppHttp","function":"show","filename":"/srv/app/Http/SeriesController.php"}]`
	vendorB := `[{"module":"IlluminateFoundation","function":"handle","filename":"/srv/vendor/laravel/framework/Kernel.php"},{"module":"IlluminateRouting","function":"send","filename":"/srv/vendor/laravel/framework/Response.php"},{"module":"AppHttp","function":"show","filename":"/srv/app/Http/SeriesController.php"}]`
	postSentryEnvelope(t, st, "backend", sentryException("QueryException", "user 42 not found", "k1", vendorA))
	postSentryEnvelope(t, st, "backend", sentryException("QueryException", "user 42 not found", "k2", vendorB))

	if issues := listIssues(t, st, "backend"); len(issues) != 1 {
		t.Fatalf("dependency frames split one defect into %d issues, want 1: %+v", len(issues), issues)
	}
}

// Two defects that throw from the same file are still two defects: the message
// template, not the file, is what separates them.
func TestGroupingSeparatesDefectsInOneFile(t *testing.T) {
	st := groupingStore(t)
	postSentryEnvelope(t, st, "backend", sentryException("QueryException", "chapter 41 not found", "k1", appFrame))
	postSentryEnvelope(t, st, "backend", sentryException("QueryException", "series 42 is locked", "k2", appFrame))

	if issues := listIssues(t, st, "backend"); len(issues) != 2 {
		t.Fatalf("two defects in one file made %d issues, want 2: %+v", len(issues), issues)
	}
}

// A fingerprint rule overrides the computed key, and the record says so.
func TestGroupingRuleOverridesComputedKey(t *testing.T) {
	st := groupingStore(t)
	if err := st.SetGroupingConfig("backend", store.GroupingConfig{
		FingerprintRules: []store.FingerprintRule{
			{Match: "error.type", Pattern: "ValidationException", Fingerprint: "validation"},
		},
		StackTraceRules: store.StackTraceRule{GroupBy: "app_frames", Ignore: []string{"vendor/"}},
	}); err != nil {
		t.Fatalf("set grouping config: %v", err)
	}

	// Different messages and different frames: only the rule can join them.
	postSentryEnvelope(t, st, "backend", sentryException("ValidationException", "title is required", "k1", appFrame))
	otherFrame := `[{"module":"AppHttp","function":"store","filename":"/srv/app/Http/SeriesController.php"}]`
	postSentryEnvelope(t, st, "backend", sentryException("ValidationException", "chapter number is required", "k2", otherFrame))

	issues := listIssues(t, st, "backend")
	if len(issues) != 1 {
		t.Fatalf("the rule did not join the events: %d issues", len(issues))
	}
	if issues[0].Fingerprint != "validation" {
		t.Errorf("fingerprint = %q, want the rule's key", issues[0].Fingerprint)
	}
	if src := issueGroupSource(t, st, issues[0].ID); src != store.GroupSourceRule {
		t.Errorf("group_source = %q, want %q", src, store.GroupSourceRule)
	}
}

// A project that reports no frames and has no rule has nothing the server can
// bucket: the producer key is the only identity, and the record labels it.
func TestGroupingFallsBackToProducerKeyWithoutFrames(t *testing.T) {
	st := groupingStore(t)
	postSentryEnvelope(t, st, "browser", sentryException("JSError", "boom", "sdk-key-1", ""))

	issues := listIssues(t, st, "browser")
	if len(issues) != 1 {
		t.Fatalf("issues = %d, want 1", len(issues))
	}
	if issues[0].Fingerprint != "sdk-key-1" {
		t.Errorf("fingerprint = %q, want the producer key", issues[0].Fingerprint)
	}
	if src := issueGroupSource(t, st, issues[0].ID); src != store.GroupSourceProducer {
		t.Errorf("group_source = %q, want %q", src, store.GroupSourceProducer)
	}
}
