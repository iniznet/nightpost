package web

import (
	"net/url"
	"strings"
)

// One dialect, one reader. The pre-mortem guard is explicit: every page
// parses the persistent context (project, time range, environment) through
// parseToolbar and nothing else, because per-page re-typing is how a filter
// silently resets when a link drops one field.

// toolbar is the persistent page context: project, time range and
// environment. It renders in the shared toolbar on every page and rides every
// generated link, so switching sections or tabs never loses the reader's
// place.
type toolbar struct {
	Project string
	Range   string
	Env     string
	Org     string
}

// rangeSpec is one time-range option: the URL key, its short label and the
// window width in hours.
type rangeSpec struct {
	Key   string
	Label string
	Hours int
}

// rangeSpecs is the single range dialect; the first entry is the default that
// an absent or unknown range resolves to.
var rangeSpecs = []rangeSpec{
	{Key: "1h", Label: "1h", Hours: 1},
	{Key: "24h", Label: "24h", Hours: 24},
	{Key: "7d", Label: "7d", Hours: 168},
	{Key: "30d", Label: "30d", Hours: 720},
}

const defaultRange = "24h"

func validRange(key string) bool {
	for _, r := range rangeSpecs {
		if r.Key == key {
			return true
		}
	}
	return false
}

// parseToolbar is the ONLY reader of the project/range/env/org query params. An
// unknown or absent range resolves to the default and never errors, matching
// the invariant that every page renders with zero params.
func parseToolbar(q url.Values) toolbar {
	t := toolbar{
		Project: strings.TrimSpace(q.Get("project")),
		Env:     strings.TrimSpace(q.Get("env")),
		Range:   strings.TrimSpace(q.Get("range")),
		Org:     strings.TrimSpace(q.Get("org")),
	}
	if !validRange(t.Range) {
		t.Range = defaultRange
	}
	return t
}

// Hours is the range's window width in hours, always inside the store's
// bounded window (1..720).
func (t toolbar) Hours() int {
	for _, r := range rangeSpecs {
		if r.Key == t.Range {
			return r.Hours
		}
	}
	return 24
}

// Values returns the canonical toolbar params. Only non-empty values are
// emitted, so the zero-value toolbar encodes away and existing short links
// (?project=x) keep their shape.
func (t toolbar) Values() url.Values {
	v := url.Values{}
	if t.Project != "" {
		v.Set("project", t.Project)
	}
	if t.Range != "" && t.Range != defaultRange {
		v.Set("range", t.Range)
	}
	if t.Env != "" {
		v.Set("env", t.Env)
	}
	if t.Org != "" {
		v.Set("org", t.Org)
	}
	return v
}

// Link builds a root-relative URL carrying the toolbar plus extra params
// (given as alternating key/value pairs). Extra values override the toolbar.
func (t toolbar) Link(path string, extra url.Values) string {
	v := t.Values()
	for k, vals := range extra {
		v.Del(k)
		for _, val := range vals {
			v.Add(k, val)
		}
	}
	if len(v) == 0 {
		return path
	}
	return path + "?" + v.Encode()
}

// --- performance filter bar ------------------------------------------------
//
// The six performance pages share one filter bar, and it is one dimension
// wider than the site toolbar: a chain can be narrowed to a service (a hop
// owner) and to an entry kind (a page a browser rendered, or an API a server
// answered). Re-typed per page, the bar drifts and a link silently drops a
// dimension — the guard parseToolbar already exists for.

// perfFilter is the filter bar's state: the persistent toolbar plus the two
// dimensions only the performance surface offers.
type perfFilter struct {
	toolbar
	Service string
	Entry   string
}

// entrySpec is one entry-kind option. The empty key is "all", so the absent
// param is the unfiltered ask rather than a third kind.
type entrySpec struct {
	Key   string
	Label string
}

// entrySpecs is the entry dialect; store.EntryKind* names the two real kinds.
var entrySpecs = []entrySpec{
	{Key: "", Label: "all entries"},
	{Key: "page", Label: "page"},
	{Key: "api", Label: "api"},
}

// parsePerfFilter reads the filter bar. An unknown entry resolves to "all"
// rather than erroring, matching the range dialect: every page renders with
// zero params and with nonsense params alike.
func parsePerfFilter(q url.Values) perfFilter {
	f := perfFilter{toolbar: parseToolbar(q), Service: strings.TrimSpace(q.Get("service"))}
	switch entry := strings.TrimSpace(q.Get("entry")); entry {
	case "api", "page":
		f.Entry = entry
	}
	return f
}

// toolbarLink is the template-facing form of toolbar.Link: kv is a flat
// alternating key/value list so templates can build a link in one expression.
func toolbarLink(t toolbar, path string, kv ...string) string {
	extra := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			extra.Set(kv[i], kv[i+1])
		}
	}
	return t.Link(path, extra)
}
