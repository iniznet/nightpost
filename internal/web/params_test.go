package web

import (
	"net/url"
	"testing"
)

// TestParseToolbarDialect is the M6.1 "ONE shared param parser" guard: every
// page's project/range/env read resolves through these cases, so a new page
// cannot invent its own dialect (or its own default) without failing here.
func TestParseToolbarDialect(t *testing.T) {
	cases := []struct {
		name              string
		raw               string
		project, rng, env string
		hours             int
	}{
		{"zero params", "", "", "24h", "", 24},
		{"project only", "project=backend", "backend", "24h", "", 24},
		{"unknown range falls back", "range=9d", "", "24h", "", 24},
		{"range 1h", "range=1h", "", "1h", "", 1},
		{"range 7d", "range=7d", "", "7d", "", 168},
		{"range 30d", "range=30d", "", "30d", "", 720},
		{"env", "env=production", "", "24h", "production", 24},
		{"all three", "project=backend&range=7d&env=production", "backend", "7d", "production", 168},
		{"whitespace trimmed", "project=%20backend%20&env=%20prod%20", "backend", "24h", "prod", 24},
		{"empty range falls back", "range=", "", "24h", "", 24},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, err := url.ParseQuery(c.raw)
			if err != nil {
				t.Fatalf("parse %q: %v", c.raw, err)
			}
			got := parseToolbar(q)
			if got.Project != c.project || got.Range != c.rng || got.Env != c.env {
				t.Errorf("parseToolbar(%q) = %+v, want project=%q range=%q env=%q", c.raw, got, c.project, c.rng, c.env)
			}
			if got.Hours() != c.hours {
				t.Errorf("Hours() = %d, want %d", got.Hours(), c.hours)
			}
		})
	}
}

func TestToolbarLinkCarriesContext(t *testing.T) {
	tb := toolbar{Project: "backend", Range: "7d", Env: "production"}
	got := tb.Link("/performance", url.Values{"tab": {"traces"}})
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.Path != "/performance" {
		t.Errorf("path = %q", u.Path)
	}
	q := u.Query()
	for k, want := range map[string]string{"project": "backend", "range": "7d", "env": "production", "tab": "traces"} {
		if q.Get(k) != want {
			t.Errorf("link %q: %s = %q, want %q", got, k, q.Get(k), want)
		}
	}

	// The default range encodes away, so short legacy links stay short.
	if got := (toolbar{Project: "backend"}).Link("/errors", nil); got != "/errors?project=backend" {
		t.Errorf("default-range link = %q", got)
	}
	if got := (toolbar{}).Link("/errors", nil); got != "/errors" {
		t.Errorf("zero-toolbar link = %q", got)
	}
	// An explicit empty extra overrides the toolbar field: the caller chose to
	// clear it, so the parsed value is empty — and the other fields survive.
	cleared, err := url.Parse(tb.Link("/", url.Values{"project": {""}}))
	if err != nil {
		t.Fatalf("parse cleared link: %v", err)
	}
	if got := cleared.Query().Get("project"); got != "" {
		t.Errorf("explicit empty project should clear the param, got %q", got)
	}
	if cleared.Query().Get("range") != "7d" || cleared.Query().Get("env") != "production" {
		t.Error("clearing one field must not drop the rest of the toolbar")
	}
}
