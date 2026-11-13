package ingest

import "testing"

// TestItemForPathRoutesOTLPTracesToSpans pins the admission dimension each
// route spends. The OTLP exports were added after the per-item budgets, so an
// unmapped path silently fell through to the logs dimension: a trace-heavy
// collector then exhausted the log budget and never touched the span one.
func TestItemForPathRoutesOTLPTracesToSpans(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/v1/traces", ItemSpans},
		{"/v1/logs", ItemLogs},
		{"/ingest/demo/profile", ItemProfiles},
		{"/ingest/demo/sentry", ItemErrors},
	}
	for _, c := range cases {
		if got := itemForPath(c.path); got != c.want {
			t.Fatalf("itemForPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}
