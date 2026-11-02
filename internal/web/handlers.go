package web

import (
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iniznet/nightpost/internal/ingest"
	"github.com/iniznet/nightpost/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

// Page identifies the rendered page. The IA consolidates pages into two
// sections (Errors, Performance); a page still names itself so the nav can
// highlight the section and the tab row can mark the active tab.
type Page string

const (
	PageDashboard   Page = "dashboard"
	PageErrors      Page = "errors"
	PageIssues      Page = "issues"
	PageLogs        Page = "logs"
	PagePerformance Page = "performance"
	PageProfiles    Page = "profiles"
	PageSlowest     Page = "slowest"
	PageTraces      Page = "traces"
	PageTrace       Page = "trace"
	PageKeys        Page = "keys"
	PageQueries     Page = "queries"
	PageJobs        Page = "jobs"
	PageOrgs        Page = "orgs"
	PageSettings    Page = "settings"
	PageLogin       Page = "login"
	PageSetup       Page = "setup"
)

// nav derives the top-level nav item from the page so the active class is
// never re-typed per template (Errors and Performance are one nav item each).
func (p Page) nav() string {
	switch p {
	case PageErrors, PageIssues, PageLogs:
		return "errors"
	case PagePerformance, PageProfiles, PageSlowest, PageTraces, PageTrace:
		return "performance"
	case PageDashboard:
		return "dashboard"
	case PageQueries:
		return "queries"
	case PageJobs:
		return "jobs"
	case PageOrgs:
		return "orgs"
	case PageSettings:
		return "settings"
	case PageKeys:
		return "keys"
	}
	return ""
}

// section is the one top-level destination a page belongs to. Four sections,
// not eight peers: data surfaces and config surfaces stop competing for the
// same row, and Queries/Jobs read as Performance detail instead of siblings.
func (p Page) section() string {
	switch p {
	case PageDashboard:
		return "now"
	case PageErrors, PageIssues, PageLogs:
		return "errors"
	case PagePerformance, PageProfiles, PageSlowest, PageTraces, PageTrace, PageQueries, PageJobs:
		return "performance"
	case PageKeys, PageOrgs, PageSettings:
		return "configure"
	}
	return ""
}

// pageForPath resolves the page identity from a request path, so a failure can
// render inside the shell it interrupted.
func pageForPath(p string) Page {
	switch {
	case p == "/":
		return PageDashboard
	case strings.HasPrefix(p, "/errors"):
		return PageErrors
	case strings.HasPrefix(p, "/issues"):
		return PageIssues
	case strings.HasPrefix(p, "/logs"):
		return PageLogs
	case strings.HasPrefix(p, "/performance"):
		return PagePerformance
	case strings.HasPrefix(p, "/profiles"):
		return PageProfiles
	case strings.HasPrefix(p, "/slowest"):
		return PageSlowest
	case strings.HasPrefix(p, "/traces"):
		return PageTraces
	case strings.HasPrefix(p, "/queries"):
		return PageQueries
	case strings.HasPrefix(p, "/jobs"):
		return PageJobs
	case strings.HasPrefix(p, "/keys"):
		return PageKeys
	case strings.HasPrefix(p, "/orgs"):
		return PageOrgs
	case strings.HasPrefix(p, "/settings"):
		return PageSettings
	}
	return ""
}

type issueFilter struct {
	Type, Level, Env, Channel, Status, Q string
	NewSince                             string
	Sort                                 string
}

// notFoundPrefix marks the per-path 404 family that floods the issues list.
const notFoundPrefix = "Not found: "

// collapseNotFound folds the per-path "Not found: <path>" issues on one page
// into a single synthetic row (ID 0, rendered as a link to the filtered
// drill-down) so 404 noise cannot bury real errors beneath it.
func collapseNotFound(issues []store.Issue) []store.Issue {
	first := -1
	var paths, total int64
	for i, is := range issues {
		if strings.HasPrefix(is.Title, notFoundPrefix) {
			if first < 0 {
				first = i
			}
			paths++
			total += is.Count
		}
	}
	if first < 0 {
		return issues
	}
	group := store.Issue{
		Title:      fmt.Sprintf("Not found — %d paths on this page", paths),
		Type:       "error",
		Level:      "error",
		Count:      total,
		LastSeenAt: issues[first].LastSeenAt,
	}
	out := make([]store.Issue, 0, len(issues)-int(paths)+1)
	for i, is := range issues {
		if i == first {
			out = append(out, group)
		}
		if !strings.HasPrefix(is.Title, notFoundPrefix) {
			out = append(out, is)
		}
	}
	return out
}

// nsRe strips PHP namespace prefixes ("Illuminate\\Database\\") from
// prettified function labels.
var nsRe = regexp.MustCompile(`[A-Za-z0-9_]+\\`)

// innermostClosure finds the innermost "{closure:...}" span whose content
// holds no nested braces; xhprof names nest closures up to a few levels deep.
func innermostClosure(s string) (start, end int, ok bool) {
	for i := 0; i+9 <= len(s); i++ {
		if s[i:i+9] != "{closure:" {
			continue
		}
		depth := 0
		for j := i; j < len(s); j++ {
			switch s[j] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					if !strings.ContainsAny(s[i+9:j], "{}") {
						return i, j + 1, true
					}
				}
			}
		}
	}
	return 0, 0, false
}

// prettifyFn collapses xhprof closure noise —
// "Pkg\Class::{closure:Pkg\Class::method():398}" — to the method that
// defines the closure, with the class name shortened to its last segment.
func prettifyFn(name string) string {
	s := name
	closures := 0
	for {
		i, end, ok := innermostClosure(s)
		if !ok {
			break
		}
		s = s[:i] + s[i+9:end-1] + s[end:]
		closures++
	}
	s = nsRe.ReplaceAllString(s, "")
	// A closure unwrap chain stacks several "Class::" prefixes; only the
	// innermost "Class::method" pair is the interesting frame.
	parts := strings.Split(s, "::")
	if len(parts) > 2 {
		s = parts[len(parts)-2] + "::" + parts[len(parts)-1]
	}
	// A pipeline frame carries a "@<depth>" suffix; park it while tidying.
	var tail string
	if k := strings.LastIndex(s, "@"); k >= 0 && isDigits(s[k+1:]) {
		tail = s[k:]
		s = s[:k]
	}
	// Each unwrapped closure level leaves its own trailing ":<line>";
	// strip all but the defining method's line number.
	for i := 0; i < closures-1; i++ {
		if k := strings.LastIndex(s, ":"); k >= 0 && isDigits(s[k+1:]) {
			s = s[:k]
		}
	}
	switch {
	case closures == 1:
		s += " (closure)"
	case closures > 1:
		s += fmt.Sprintf(" (closure ×%d)", closures)
	}
	return s + tail
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

type ProjectRow struct {
	Summary store.ProjectSummary
	Trend   []int64
}

// ProjectCard is one dashboard triage card: the figures that answer "is this
// healthy right now?" for a single project, each deep-linking into the Errors
// or Performance section.
type ProjectCard struct {
	Project         string
	OpenIssues      int64
	Errors          int64
	NewSinceRelease int64
	ReleaseVersion  string
	P95Ms           float64
	DeltaPct        float64
	Trend           []int64
}

type SpanRow struct {
	Name       string
	Left       string
	Width      string
	DurationMs float64
}

// server holds the dashboard's dependencies and its parsed templates. Handlers
// are methods so the section dispatchers (Errors, Performance) can delegate to
// their tab handlers without duplicating logic.
type server struct {
	st     *store.Store
	gate   *ingest.KeyGate
	health IngestHealth
	cors   originInvalidator

	dashboard *template.Template
	issues    *template.Template
	detail    *template.Template
	logs      *template.Template
	profiles  *template.Template
	profile   *template.Template
	slowest   *template.Template
	traces    *template.Template
	trace     *template.Template
	keys      *template.Template
	queries   *template.Template
	jobs      *template.Template
	orgs      *template.Template
	settings  *template.Template

	settingDefaults store.SettingDefaults

	mu      sync.Mutex
	stats   *store.StoreStats
	statsAt time.Time
}

// IngestHealth is the live ingest-rejection picture the footer strip renders.
// Each field is a live reader (never a cached number): a counter that lags is
// worse than none when the reader is asking "why is my data missing?".
type IngestHealth struct {
	ShaperRejections  func() int64
	KeyGateRejections func() int64
	UnknownAdmits     func() int64
	GateRefusals      func() int64
	GuardGlobal       func() int64
	GuardIP           func() int64
}

// configured reports whether any counter is attached, so the footer can omit
// the strip entirely instead of rendering a row of zeros nobody wired.
func (h IngestHealth) configured() bool {
	return h.ShaperRejections != nil || h.KeyGateRejections != nil || h.UnknownAdmits != nil ||
		h.GateRefusals != nil || h.GuardGlobal != nil || h.GuardIP != nil
}

// IngestHealthSnapshot is the rendered form of IngestHealth.
type IngestHealthSnapshot struct {
	Shaper429     int64
	KeyGate401    int64
	UnknownAdmits int64
	Refusals      int64
	Global429     int64
	IP429         int64
}

func (h IngestHealth) Snapshot() IngestHealthSnapshot {
	var s IngestHealthSnapshot
	if h.ShaperRejections != nil {
		s.Shaper429 = h.ShaperRejections()
	}
	if h.KeyGateRejections != nil {
		s.KeyGate401 = h.KeyGateRejections()
	}
	if h.UnknownAdmits != nil {
		s.UnknownAdmits = h.UnknownAdmits()
	}
	if h.GateRefusals != nil {
		s.Refusals = h.GateRefusals()
	}
	if h.GuardGlobal != nil {
		s.Global429 = h.GuardGlobal()
	}
	if h.GuardIP != nil {
		s.IP429 = h.GuardIP()
	}
	return s
}

var funcMap = template.FuncMap{
	"rel":   relTime,
	"spark": sparkPoints,
	"ms":    fmtMs,
	"mb":    fmtMB,
	"fn":    prettifyFn,
	"tl":    toolbarLink,
	"delta": func(v float64) string { return fmt.Sprintf("%+.1f%%", v) },
	"pct":   func(v, max float64) string { return strconv.FormatFloat(pctOf(v, max), 'f', 1, 64) },
	"trunc": func(s string, n int) string {
		r := []rune(s)
		if len(r) <= n {
			return s
		}
		return string(r[:n]) + "…"
	},
	"pcti": func(v, max int64) string {
		if max <= 0 {
			return "0.0"
		}
		return strconv.FormatFloat(float64(v)/float64(max)*100, 'f', 1, 64)
	},
}

func pctOf(v, max float64) float64 {
	if max <= 0 {
		return 0
	}
	return v / max * 100
}

func fmtMs(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.2f s", v/1000)
	}
	return fmt.Sprintf("%.1f ms", v)
}

func fmtMB(b int64) string {
	if b < 0 {
		b = 0
	}
	return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
}

func relTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// sparkPoints renders an SVG polyline points list, normalized to a 96x24 box.
func sparkPoints(points []int64) string {
	if len(points) < 2 {
		return ""
	}
	const w, h = 96.0, 24.0
	var max int64
	for _, p := range points {
		if p > max {
			max = p
		}
	}
	if max == 0 {
		max = 1
	}
	pts := make([]string, 0, len(points))
	for i, p := range points {
		x := float64(i) * w / float64(len(points)-1)
		y := h - 2 - float64(p)/float64(max)*(h-4)
		pts = append(pts, fmt.Sprintf("%.1f,%.1f", x, y))
	}
	return strings.Join(pts, " ")
}

// mustLayout parses the shared layout with one page's content block.
func mustLayout(page string) *template.Template {
	return template.Must(template.New("layout").Funcs(funcMap).ParseFS(templateFS, "templates/base.html", "templates/"+page))
}

// errorLayout renders a page-level failure inside the shell: the reader who
// hits a broken page still needs the nav, the scope bar and a way back.
var errorLayout = mustLayout("error.html")

// fail answers a page failure inside the layout, keeping the status code.
func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.failStatus(w, r, http.StatusInternalServerError, err.Error())
}

func (s *server) failStatus(w http.ResponseWriter, r *http.Request, code int, msg string) {
	t := parseToolbar(r.URL.Query())
	page := pageForPath(r.URL.Path)
	data := s.baseData(r, t, page)
	data["Status"] = code
	data["Message"] = msg
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := errorLayout.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render error page: %v", err)
	}
}

// scopeLine is the one place the current scope is spelled out in words, so a
// reader never has to reconstruct it from four controls.
func scopeLine(t toolbar, orgs []store.Org, section string) string {
	parts := make([]string, 0, 4)
	if section == "configure" {
		return ""
	}
	if t.Org != "" {
		name := t.Org
		for _, o := range orgs {
			if o.Slug == t.Org {
				name = o.Name
				break
			}
		}
		parts = append(parts, name)
	}
	if t.Project == "" {
		parts = append(parts, "all projects")
	} else {
		parts = append(parts, t.Project)
	}
	parts = append(parts, t.Range)
	if t.Env == "" {
		parts = append(parts, "all envs")
	} else {
		parts = append(parts, t.Env)
	}
	return strings.Join(parts, " · ")
}

// safeBack accepts only a root-relative path, never an absolute URL.
func safeBack(v string) string {
	if v == "" || !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") {
		return ""
	}
	if strings.IndexByte(v, 92) >= 0 || strings.IndexByte(v, 13) >= 0 || strings.IndexByte(v, 10) >= 0 {
		return ""
	}
	return v
}

// topDefects is the landing page's focal block: the defects that hurt most
// right now, across every project in scope. The all-projects dashboard used to
// render cards and aggregate tables only, so the default page could not name a
// single defect.
func (s *server) topDefects(t toolbar, scope map[string]bool, hours, limit int) ([]store.Issue, map[int64][]int64, error) {
	q := store.IssueQuery{Project: t.Project, Type: "error", Status: "open", Sort: "pain", Limit: limit}
	if scope != nil {
		q.Limit = limit * 10
	}
	issues, err := s.st.Issues(q)
	if err != nil {
		return nil, nil, err
	}
	if scope != nil {
		kept := make([]store.Issue, 0, limit)
		for _, is := range issues {
			if scope[is.ProjectKey] {
				kept = append(kept, is)
			}
			if len(kept) >= limit {
				break
			}
		}
		issues = kept
	}
	spark, _ := s.st.IssueSparklines(t.Project, hours)
	return issues, spark, nil
}

// Option configures the page server.
type Option func(*server)

// WithIngestHealth attaches the live ingest-rejection counters the footer
// strip renders. Without it the strip shows store figures only.
func WithIngestHealth(h IngestHealth) Option {
	return func(s *server) { s.health = h }
}

// originInvalidator drops a cached CORS decision, so an allowlist edit takes
// effect on the next request instead of waiting out the resolver's TTL.
type originInvalidator interface{ Invalidate(project string) }

// WithCORSInvalidator attaches the ingest CORS resolver.
func WithCORSInvalidator(v originInvalidator) Option {
	return func(s *server) { s.cors = v }
}

// WithSettingDefaults supplies the import-once fallbacks (the process flags)
// the settings page shows for a key that has never been saved. Absent or
// nonsensical values fall back to the shipped defaults, so the page is always
// renderable and always saveable.
func WithSettingDefaults(d store.SettingDefaults) Option {
	return func(s *server) { s.settingDefaults = normalizeSettingDefaults(d) }
}

func normalizeSettingDefaults(d store.SettingDefaults) store.SettingDefaults {
	if d.RetentionDays <= 0 {
		d.RetentionDays = 30
	}
	if d.PruneIntervalMinutes <= 0 {
		d.PruneIntervalMinutes = 10
	}
	if d.OccurrenceCap < 0 {
		d.OccurrenceCap = 30
	}
	if d.ProfilesPerProject <= 0 {
		d.ProfilesPerProject = 5000
	}
	if d.IngestRate <= 0 {
		d.IngestRate = 50
	}
	if d.IngestBurst <= 0 {
		d.IngestBurst = 100
	}
	return d
}

// SettingRow is one editable setting with its applied-ness made explicit.
type SettingRow struct {
	Key, Label, Help, Value string
	Live                    bool
}

// settingMeta is the display order and copy for every writable setting.
var settingMeta = []struct {
	Key, Label, Help string
	Live             bool
}{
	{store.SettingRetentionDays, "Retention (days)", "Occurrences and alert history older than this are deleted. Issues are never deleted — the defect list is the history.", true},
	{store.SettingPruneMinutes, "Prune interval (minutes)", "How often the retention cycle runs. Re-read every cycle, so a change applies without a restart.", true},
	{store.SettingOccurrenceCap, "Occurrence cap per fingerprint / minute", "0 = unlimited. Caps stored occurrence rows while issue counts stay accurate.", true},
	{store.SettingProfilesPerProject, "Profiles kept per project", "PER PROJECT: one noisy producer can no longer evict another org's profile and trace window.", true},
	{store.SettingIngestRate, "Ingest rate (events/s per project)", "Token-bucket refill rate; requests past the budget get a cheap 429 before any body read. Re-read from this table every few seconds.", true},
	{store.SettingIngestBurst, "Ingest burst ceiling (per project)", "Token-bucket burst ceiling, re-read from this table every few seconds.", true},
}

// handleSettings is the settings surface: every operator-tunable value
// lives here instead of in a process flag.
func (s *server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, nil)
}

func (s *server) renderSettings(w http.ResponseWriter, r *http.Request, extra map[string]any) {
	t := parseToolbar(r.URL.Query())
	data := s.baseData(r, t, PageSettings)
	rows := make([]SettingRow, 0, len(settingMeta))
	for _, m := range settingMeta {
		v, err := s.st.Setting(m.Key)
		if err != nil {
			log.Printf("setting %s: %v", m.Key, err)
		}
		if v == "" {
			v = s.defaultSetting(m.Key)
		}
		rows = append(rows, SettingRow{Key: m.Key, Label: m.Label, Help: m.Help, Live: m.Live, Value: v})
	}
	data["Rows"] = rows
	for k, v := range extra {
		data[k] = v
	}
	if err := s.settings.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render settings: %v", err)
	}
}

// defaultSetting renders the import-once fallback for one key.
func (s *server) defaultSetting(key string) string {
	d := normalizeSettingDefaults(s.settingDefaults)
	switch key {
	case store.SettingRetentionDays:
		return strconv.Itoa(d.RetentionDays)
	case store.SettingPruneMinutes:
		return strconv.Itoa(d.PruneIntervalMinutes)
	case store.SettingOccurrenceCap:
		return strconv.Itoa(d.OccurrenceCap)
	case store.SettingProfilesPerProject:
		return strconv.Itoa(d.ProfilesPerProject)
	case store.SettingIngestRate:
		return strconv.FormatFloat(d.IngestRate, 'f', -1, 64)
	case store.SettingIngestBurst:
		return strconv.FormatFloat(d.IngestBurst, 'f', -1, 64)
	}
	return ""
}

func (s *server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin save rejected", http.StatusForbidden)
		return
	}
	// Validate EVERY submitted value before storing any, so a half-applied
	// save can never leave the retention loop with a broken knob.
	pending := make(map[string]string, len(settingMeta))
	for _, m := range settingMeta {
		v := strings.TrimSpace(r.FormValue(m.Key))
		if v == "" {
			continue
		}
		if err := store.ValidateSetting(m.Key, v); err != nil {
			s.renderSettings(w, r, map[string]any{"Error": err.Error()})
			return
		}
		pending[m.Key] = v
	}
	for k, v := range pending {
		if err := s.st.SetSetting(k, v); err != nil {
			s.renderSettings(w, r, map[string]any{"Error": err.Error()})
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	s.renderSettings(w, r, map[string]any{"Flash": fmt.Sprintf("Saved %d setting(s)", len(pending))})
}

// Register mounts the dashboard on the inner page mux (the auth gate wraps
// every route by construction). auth == nil keeps the dashboard open — the
// mode tests and pre-M4 dev runs use that.
func Register(mux *http.ServeMux, st *store.Store, gate *ingest.KeyGate, auth *Auth, opts ...Option) {
	s := &server{
		st:        st,
		gate:      gate,
		dashboard: mustLayout("dashboard.html"),
		issues:    mustLayout("index.html"),
		detail:    mustLayout("detail.html"),
		logs:      mustLayout("logs.html"),
		profiles:  mustLayout("profiles.html"),
		profile:   mustLayout("profile.html"),
		slowest:   mustLayout("slowest.html"),
		traces:    mustLayout("traces.html"),
		trace:     mustLayout("trace.html"),
		keys:      mustLayout("keys.html"),
		queries:   mustLayout("queries.html"),
		jobs:      mustLayout("jobs.html"),
		orgs:      mustLayout("orgs.html"),
		settings:  mustLayout("settings.html"),
	}
	for _, o := range opts {
		o(s)
	}

	page := http.NewServeMux()
	if auth != nil {
		auth.CompileTemplates()
		mux.HandleFunc("GET /login", auth.HandleLoginGET)
		mux.HandleFunc("POST /login", auth.HandleLoginPOST)
		mux.HandleFunc("GET /setup", auth.HandleSetupGET)
		mux.HandleFunc("POST /setup", auth.HandleSetupPOST)
		mux.HandleFunc("GET /logout", auth.HandleLogout)
		mux.Handle("/", auth.Gate(page))
	} else {
		mux.Handle("/", page)
	}

	page.HandleFunc("GET /{$}", s.handleDashboard)

	// Errors section: one nav item, Issues and Logs cross-linked. The legacy
	// paths keep serving the same handlers (Expand/Contract, invariant).
	page.HandleFunc("GET /errors", s.handleErrors)
	page.HandleFunc("GET /issues", s.handleIssues)
	page.HandleFunc("GET /issues/{id}", s.handleIssueDetail)
	page.HandleFunc("POST /issues/{id}/resolve", s.resolveHandler(true))
	page.HandleFunc("POST /issues/{id}/reopen", s.resolveHandler(false))
	page.HandleFunc("GET /logs", s.handleLogs)

	// Performance section: one nav item, Endpoints / Traces / Hotspots tabs.
	page.HandleFunc("GET /performance", s.handlePerformance)
	page.HandleFunc("GET /profiles", s.handleProfiles)
	page.HandleFunc("GET /profiles/{id}", s.handleProfileDetail)
	page.HandleFunc("GET /traces", s.handleTraces)
	page.HandleFunc("GET /traces/{id}", s.handleTraceDetail)
	page.HandleFunc("GET /slowest", s.handleSlowest)

	page.HandleFunc("GET /queries", s.handleQueries)
	page.HandleFunc("GET /jobs", s.handleJobs)
	page.HandleFunc("GET /orgs", s.handleOrgs)
	page.HandleFunc("POST /orgs/create", s.handleOrgCreate)
	page.HandleFunc("POST /orgs/assign", s.handleOrgAssign)
	page.HandleFunc("GET /settings", s.handleSettings)
	page.HandleFunc("POST /settings/save", s.handleSettingsSave)
	page.HandleFunc("GET /keys", s.handleKeys)
	page.HandleFunc("POST /keys/rotate", s.handleKeyRotate)
	page.HandleFunc("POST /keys/revoke", s.handleKeyRevoke)
	page.HandleFunc("POST /keys/origin", s.handleKeyOrigin)
}

// baseData builds the shared layout context for the pre-auth pages
// (login/setup): they carry no request-scoped toolbar to preserve, but the
// layout still needs the nav state and the range dialect.
func baseData(st *store.Store, project string, page Page) map[string]any {
	projects, err := st.Projects()
	if err != nil {
		log.Printf("projects: %v", err)
	}
	data := map[string]any{
		"Projects": projects,
		"Project":  project,
		"Page":     page,
		"Nav":      page.nav(),
		"Toolbar":  toolbar{Project: project, Range: defaultRange},
		"Ranges":   rangeSpecs,
		"Envs":     nil,
		"Path":     "/login",
		"Tab":      "",
	}
	if stats, serr := st.Stats(); serr == nil {
		data["StoreStats"] = stats
	}
	return data
}

// baseData carries the fields every page needs: the toolbar context, the nav
// state, the ingest-health strip and the (short-cached) store figures.
func (s *server) baseData(r *http.Request, t toolbar, page Page) map[string]any {
	projects, err := s.st.ProjectsInOrg(s.orgID(t))
	if err != nil {
		log.Printf("projects: %v", err)
	}
	envs, err := s.st.Environments(t.Project)
	if err != nil {
		log.Printf("environments: %v", err)
	}
	orgs, err := s.st.ListOrgs()
	if err != nil {
		log.Printf("orgs: %v", err)
	}
	section := page.section()
	data := map[string]any{
		"Projects":  projects,
		"Orgs":      orgs,
		"Project":   t.Project,
		"Page":      page,
		"Nav":       page.nav(),
		"Section":   section,
		"Toolbar":   t,
		"Ranges":    rangeSpecs,
		"Envs":      envs,
		"Path":      r.URL.Path,
		"Tab":       r.URL.Query().Get("tab"),
		"Self":      r.URL.RequestURI(),
		"Scope":     section != "" && section != "configure",
		"ScopeBar":  section != "",
		"ScopeLine": scopeLine(t, orgs, section),
	}
	if s.health.configured() {
		data["Health"] = s.health.Snapshot()
	}
	if stats := s.storeStats(); stats != nil {
		data["StoreStats"] = stats
	}
	return data
}

// orgID resolves the toolbar's org slug to an org id; 0 means every org.
func (s *server) orgID(t toolbar) int64 {
	if t.Org == "" {
		return 0
	}
	o, err := s.st.OrgBySlug(t.Org)
	if err != nil {
		log.Printf("org %q: %v", t.Org, err)
		return 0
	}
	return o.ID
}

// projectScope is the set of project keys the toolbar's org may see, or nil
// when no org is selected (every project). Tenancy is enforced here, at the
// projection the reader sees, never by re-keying the ingest surface.
func (s *server) projectScope(t toolbar) map[string]bool {
	orgID := s.orgID(t)
	if orgID == 0 {
		return nil
	}
	keys, err := s.st.ProjectsInOrg(orgID)
	if err != nil {
		log.Printf("org scope: %v", err)
		return nil
	}
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[k] = true
	}
	return set
}

// storeStats caches the table counts for a short interval: the footer strip
// renders on every page, and a per-request COUNT(*) over occurrences would be
// the wedge class this repo already refuses elsewhere.
func (s *server) storeStats() *store.StoreStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stats != nil && time.Since(s.statsAt) < 30*time.Second {
		return s.stats
	}
	st, err := s.st.Stats()
	if err != nil {
		return s.stats
	}
	s.stats, s.statsAt = &st, time.Now()
	return s.stats
}

func parseID(s string) int64 {
	id, _ := strconv.ParseInt(s, 10, 64)
	return id
}

// handleErrors is the Errors-section dispatcher: Issues is the default tab and
// an unknown tab resolves to it, never a 404 (invariant I2).
func (s *server) handleErrors(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("tab") == "logs" {
		s.handleLogs(w, r)
		return
	}
	s.handleIssues(w, r)
}

// handleIssues renders the error stream. folds the log-type
// pseudo-issues out of the default stream (they live on the Logs tab); an
// explicit `?type=` request restores them.
func (s *server) handleIssues(w http.ResponseWriter, r *http.Request) {
	t := parseToolbar(r.URL.Query())
	q := r.URL.Query()
	f := issueFilter{
		Type:     q.Get("type"),
		Level:    q.Get("level"),
		Env:      t.Env,
		Status:   q.Get("status"),
		Q:        q.Get("q"),
		NewSince: q.Get("new_since"),
	}
	if !q.Has("type") {
		f.Type = "error"
	}
	if f.Status == "" {
		f.Status = "open"
	}
	// Pain order (count DESC) is the default: a wall of recent one-offs
	// otherwise buries the recurring noise that actually hurts.
	f.Sort = q.Get("sort")
	if f.Sort != "recent" {
		f.Sort = "pain"
	}
	var newSince *store.ReleaseMark
	if f.NewSince == "release" {
		newSince, _ = s.st.LatestRelease(t.Project, t.Env)
	}
	query := store.IssueQuery{
		Project: t.Project, Type: f.Type, Level: f.Level, Env: t.Env, Q: f.Q,
		Status:      f.Status,
		Sort:        f.Sort,
		BeforeID:    parseID(q.Get("before")),
		BeforeCount: parseID(q.Get("beforecount")),
		Limit:       50,
	}
	if newSince != nil {
		query.SinceFirstSeen = newSince.At.Unix()
	}
	issues, err := s.st.Issues(query)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	spark, _ := s.st.IssueSparklines(t.Project, t.Hours())
	shown := issues
	if f.Q == "" {
		shown = collapseNotFound(issues)
	}
	page := PageErrors
	// The legacy path keeps its page identity so the tab still marks Issues.
	if r.URL.Path == "/issues" {
		page = PageIssues
	}
	data := s.baseData(r, t, page)
	data["Issues"] = shown
	data["Spark"] = spark
	data["Filter"] = f
	data["NewSince"] = newSince
	// The level facet is built from what the project actually emits, not from a
	// hardcoded four. A notice, critical or deprecated level reaches this table
	// through the exception path too, and a level the form does not offer cannot
	// be filtered (iniznet/fenrirealm-tracker#526).
	levels, _ := s.st.Levels(t.Project, f.Type)
	data["Levels"] = levelOptions(levels, f.Level)
	ids := make([]int64, 0, len(shown))
	for _, is := range shown {
		ids = append(ids, is.ID)
	}
	userCounts, _ := s.st.UserCounts(ids)
	data["UserCounts"] = userCounts
	if len(issues) == 50 {
		v := url.Values{}
		v.Set("type", f.Type)
		v.Set("level", f.Level)
		v.Set("status", f.Status)
		v.Set("q", f.Q)
		v.Set("sort", f.Sort)
		v.Set("new_since", f.NewSince)
		v.Set("before", strconv.FormatInt(issues[len(issues)-1].ID, 10))
		v.Set("beforecount", strconv.FormatInt(issues[len(issues)-1].Count, 10))
		data["NextURL"] = t.Link(r.URL.Path, v)
	}
	if err := s.issues.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render issues: %v", err)
	}
}

func (s *server) handleIssueDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.failStatus(w, r, http.StatusBadRequest, "bad id")
		return
	}
	issue, err := s.st.Issue(id)
	if err != nil {
		s.failStatus(w, r, http.StatusNotFound, "not found")
		return
	}
	occurrences, err := s.st.Occurrences(id, 20)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	t := parseToolbar(r.URL.Query())
	// The detail link carries the list it came from (filters and all), so the
	// way back lands where the reader was. A malformed or foreign value falls
	// back to the section root rather than becoming an open redirect.
	backPath := "/errors"
	if r.URL.Query().Get("from") == "logs" {
		backPath = "/errors"
	}
	if from := safeBack(r.URL.Query().Get("from")); from != "" {
		backPath = from
	}
	// The tab strip follows the row, not the route: a log detail opened from the
	// Logs list must underline Logs (iniznet/fenrirealm-tracker#526).
	page := PageErrors
	if issue.Type == "log" {
		page = PageLogs
	}
	data := s.baseData(r, t, page)
	data["Issue"] = issue
	data["BackPath"] = backPath
	if mark, merr := s.st.LatestRelease(issue.ProjectKey, issue.Environment); merr == nil && mark != nil && issue.FirstSeenAt.After(mark.At) {
		data["NewSince"] = mark
	}
	uc, _ := s.st.UserCounts([]int64{issue.ID})
	data["ImpactedUsers"] = uc[issue.ID]
	data["Views"] = buildOccViews(occurrences)
	if err := s.detail.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render detail: %v", err)
	}
}

func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	t := parseToolbar(r.URL.Query())
	q := r.URL.Query()
	f := issueFilter{
		Level:   q.Get("level"),
		Env:     t.Env,
		Channel: q.Get("channel"),
		Q:       q.Get("q"),
		Status:  "all",
		Type:    "log",
	}
	logs, err := s.st.Issues(store.IssueQuery{
		Project: t.Project, Type: "log", Level: f.Level, Env: t.Env, Channel: f.Channel, Q: f.Q,
		Status:   "all",
		BeforeID: parseID(q.Get("before")),
		Limit:    50,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	levels, _ := s.st.LogLevels(t.Project)
	page := PageErrors
	if r.URL.Path == "/logs" || r.URL.Query().Get("tab") == "logs" {
		page = PageLogs
	}
	data := s.baseData(r, t, page)
	data["Issues"] = logs
	data["Filter"] = f
	data["Levels"] = levels
	if len(logs) == 50 {
		v := url.Values{}
		v.Set("level", f.Level)
		v.Set("channel", f.Channel)
		v.Set("q", f.Q)
		v.Set("before", strconv.FormatInt(logs[len(logs)-1].ID, 10))
		data["NextURL"] = t.Link(r.URL.Path, v)
	}
	if err := s.logs.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render logs: %v", err)
	}
}

// handlePerformance is the Performance-section dispatcher: Endpoints is the
// default tab and an unknown tab resolves to it, never a 404.
func (s *server) handlePerformance(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("tab") {
	case "traces":
		s.handleTraces(w, r)
	case "hotspots":
		s.handleSlowest(w, r)
	default:
		s.handleProfiles(w, r)
	}
}

func (s *server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	t := parseToolbar(r.URL.Query())
	q := r.URL.Query()
	typeFilter := q.Get("type")
	view := q.Get("view")
	search := q.Get("q")
	page := PagePerformance
	if r.URL.Path == "/profiles" {
		page = PageProfiles
	}
	data := s.baseData(r, t, page)
	data["TypeF"] = typeFilter
	data["View"] = view
	data["Q"] = search
	if view != "raw" && (typeFilter == store.ProfileTypeRequest || typeFilter == store.ProfileTypeCallgraph) {
		// Aggregated-by-route is the default view: a flat recency list
		// repeats the same hot route ten times and hides what actually
		// accumulates wall-clock time.
		routes, err := s.st.ProfileRouteStats(t.Project, typeFilter, t.Hours(), 50)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		data["Routes"] = routes
		var maxTotal float64
		for _, ra := range routes {
			if ra.TotalMs > maxTotal {
				maxTotal = ra.TotalMs
			}
		}
		data["MaxTotal"] = maxTotal
		data["Profiles"] = nil
		if err := s.profiles.ExecuteTemplate(w, "layout", data); err != nil {
			log.Printf("render profiles: %v", err)
		}
		return
	}
	profiles, err := s.st.ProfilesFiltered(t.Project, typeFilter, search, 50)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var maxDur float64
	for _, p := range profiles {
		if p.DurationMs > maxDur {
			maxDur = p.DurationMs
		}
	}
	data["Profiles"] = profiles
	data["MaxDur"] = maxDur
	if err := s.profiles.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render profiles: %v", err)
	}
}

func (s *server) handleProfileDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.failStatus(w, r, http.StatusBadRequest, "bad id")
		return
	}
	profile, err := s.st.Profile(id)
	if err != nil {
		s.failStatus(w, r, http.StatusNotFound, "not found")
		return
	}
	var maxSelf float64
	for _, h := range profile.Hotspots {
		if h.SelfMs > maxSelf {
			maxSelf = h.SelfMs
		}
	}
	t := parseToolbar(r.URL.Query())
	data := s.baseData(r, t, PagePerformance)
	data["Profile"] = profile
	data["SpanRows"] = buildSpanRows(profile)
	data["Tree"] = buildTreeViews(profile.Tree, profile.DurationMs, 0)
	data["MaxSelf"] = maxSelf
	if err := s.profile.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render profile: %v", err)
	}
}

// handleTraces renders the Traces index: recent cross-service requests with
// their leg count, total window and a has-tree badge for the drill-down.
func (s *server) handleTraces(w http.ResponseWriter, r *http.Request) {
	t := parseToolbar(r.URL.Query())
	rows, err := s.st.Traces(t.Project, t.Hours(), 50)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data := s.baseData(r, t, PageTraces)
	data["Traces"] = rows
	data["DeepCap"] = 12
	if err := s.traces.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render traces: %v", err)
	}
}

func (s *server) handleTraceDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	legs, err := s.st.ProfilesByTrace(id, 20)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(legs) == 0 {
		s.failStatus(w, r, http.StatusNotFound, "not found")
		return
	}
	t := parseToolbar(r.URL.Query())
	data := s.baseData(r, t, PageTrace)
	data["Trace"] = buildTraceView(legs)
	data["TraceID"] = id
	if err := s.trace.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render trace: %v", err)
	}
}

// handleSlowest renders the Hotspots tab: proportional self-time bars with an
// app-vs-framework scope filter and pagination over the bounded sample set.
func (s *server) handleSlowest(w http.ResponseWriter, r *http.Request) {
	t := parseToolbar(r.URL.Query())
	q := r.URL.Query()
	scope := q.Get("scope")
	if scope != "app" && scope != "framework" {
		scope = "all"
	}
	pageNum := int(parseID(q.Get("page")))
	if pageNum < 1 {
		pageNum = 1
	}
	all, err := s.st.SlowestCode(t.Project, 500)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	filtered := make([]store.HotspotAgg, 0, len(all))
	for _, row := range all {
		app := isAppFrame(row.Function, row.File)
		if scope == "app" && !app {
			continue
		}
		if scope == "framework" && app {
			continue
		}
		filtered = append(filtered, row)
	}
	const perPage = 50
	total := len(filtered)
	start := (pageNum - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	shown := filtered[start:end]
	var maxSelf float64
	for _, row := range shown {
		if row.TotalSelfMs > maxSelf {
			maxSelf = row.TotalSelfMs
		}
	}
	data := s.baseData(r, t, PageSlowest)
	data["Rows"] = shown
	data["MaxSelf"] = maxSelf
	data["Scope"] = scope
	data["Total"] = total
	data["PageNum"] = pageNum
	if start > 0 {
		data["PrevURL"] = t.Link(r.URL.Path, url.Values{"scope": {scope}, "page": {strconv.Itoa(pageNum - 1)}})
	}
	if end < total {
		data["NextURL"] = t.Link(r.URL.Path, url.Values{"scope": {scope}, "page": {strconv.Itoa(pageNum + 1)}})
	}
	if err := s.slowest.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render slowest: %v", err)
	}
}

// isAppFrame separates first-party frames from vendor/framework frames: a
// path under vendor/ or node_modules/ is framework, as is a bare framework
// namespace when the producer sent no file.
func isAppFrame(fn, file string) bool {
	f := strings.ToLower(file)
	if strings.Contains(f, "/vendor/") || strings.Contains(f, "\\vendor\\") ||
		strings.Contains(f, "/node_modules/") || strings.Contains(f, "\\node_modules\\") {
		return false
	}
	for _, p := range []string{"illuminate\\", "symfony\\", "laravel\\", "monolog\\", "guzzlehttp\\", "predis\\", "doctrine\\", "psr\\", "sentry\\", "composer\\"} {
		if strings.HasPrefix(strings.ToLower(fn), p) {
			return false
		}
	}
	return true
}

func (s *server) handleQueries(w http.ResponseWriter, r *http.Request) {
	t := parseToolbar(r.URL.Query())
	data := s.baseData(r, t, PageQueries)
	aggs, err := s.st.QueryLeaderboard(t.Project, t.Hours(), 50)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	type QueryRow struct {
		SQL, Fingerprint string
		Connection       string
		Count            int64
		TotalMs, AvgMs   float64
		MaxMs            float64
		LastSeen         time.Time
	}
	rows := make([]QueryRow, 0, len(aggs))
	var maxTotal float64
	for _, a := range aggs {
		if a.TotalMs > maxTotal {
			maxTotal = a.TotalMs
		}
		avg := 0.0
		if a.Count > 0 {
			avg = a.TotalMs / float64(a.Count)
		}
		rows = append(rows, QueryRow{SQL: a.SQL, Fingerprint: a.Fingerprint, Connection: a.Connection, Count: a.Count, TotalMs: a.TotalMs, AvgMs: avg, MaxMs: a.MaxMs, LastSeen: a.LastSeen})
	}
	data["Rows"] = rows
	data["MaxTotal"] = maxTotal
	if err := s.queries.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render queries: %v", err)
	}
}

func (s *server) handleJobs(w http.ResponseWriter, r *http.Request) {
	t := parseToolbar(r.URL.Query())
	data := s.baseData(r, t, PageJobs)
	aggs, err := s.st.JobLeaderboard(t.Project, t.Hours(), 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data["Rows"] = aggs
	if err := s.jobs.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render jobs: %v", err)
	}
}

// handleOrgs is the tenancy surface: create an org and move projects
// under it. Assignment is additive — the project key and its ingest key are
// never touched, so no producer is reconfigured to become tenanted.
func (s *server) handleOrgs(w http.ResponseWriter, r *http.Request) {
	s.renderOrgs(w, r, nil)
}

func (s *server) renderOrgs(w http.ResponseWriter, r *http.Request, extra map[string]any) {
	t := parseToolbar(r.URL.Query())
	data := s.baseData(r, t, PageOrgs)
	orgs, err := s.st.ListOrgs()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	projects, err := s.st.ListProjects()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	assign, err := s.st.ProjectOrgMap()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data["Orgs"] = orgs
	data["ProjectRows"] = projects
	data["ProjectOrg"] = assign
	data["DefaultOrgID"] = store.DefaultOrgID
	for k, v := range extra {
		data[k] = v
	}
	if err := s.orgs.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render orgs: %v", err)
	}
}

func (s *server) handleOrgCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin create rejected", http.StatusForbidden)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	slug := strings.TrimSpace(r.FormValue("slug"))
	if name == "" {
		http.Error(w, "org name required", http.StatusBadRequest)
		return
	}
	if _, err := s.st.CreateOrg(name, slug); err != nil {
		s.renderOrgs(w, r, map[string]any{"Error": err.Error()})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.renderOrgs(w, r, map[string]any{"Flash": "Created org " + name})
}

func (s *server) handleOrgAssign(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin assign rejected", http.StatusForbidden)
		return
	}
	project := strings.TrimSpace(r.FormValue("project"))
	orgRef := strings.TrimSpace(r.FormValue("org"))
	if project == "" || orgRef == "" {
		http.Error(w, "project and org required", http.StatusBadRequest)
		return
	}
	o, err := s.st.OrgBySlug(orgRef)
	if err != nil {
		s.renderOrgs(w, r, map[string]any{"Error": err.Error()})
		return
	}
	if err := s.st.AssignProjectToOrg(project, o.ID); err != nil {
		s.renderOrgs(w, r, map[string]any{"Error": err.Error()})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.renderOrgs(w, r, map[string]any{"Flash": "Moved " + project + " into " + o.Name})
}

func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	t := parseToolbar(r.URL.Query())
	hours := t.Hours()
	data := s.baseData(r, t, PageDashboard)
	summaries, err := s.st.DashboardSummaries(hours)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	scope := s.projectScope(t)
	if t.Project == "" {
		rows := make([]ProjectRow, 0, len(summaries))
		cards := make([]ProjectCard, 0, len(summaries))
		for _, sm := range summaries {
			if scope != nil && !scope[sm.Summary.Project] {
				continue
			}
			rows = append(rows, ProjectRow{Summary: sm.Summary, Trend: sm.Trend})
			cards = append(cards, s.projectCard(sm, hours, t.Env))
		}
		recent, spark, derr := s.topDefects(t, scope, hours, 6)
		if derr != nil {
			s.fail(w, r, derr)
			return
		}
		data["Recent"] = recent
		data["Spark"] = spark
		data["All"] = true
		data["Rows"] = rows
		data["Cards"] = cards
	} else {
		var sum store.ProjectSummary
		var trend []int64
		for _, sm := range summaries {
			if sm.Summary.Project == t.Project {
				sum, trend = sm.Summary, sm.Trend
				break
			}
		}
		recent, ierr := s.st.Issues(store.IssueQuery{Project: t.Project, Type: "error", Status: "open", Limit: 8, Sort: "pain"})
		if ierr != nil {
			http.Error(w, ierr.Error(), http.StatusInternalServerError)
			return
		}
		spark, _ := s.st.IssueSparklines(t.Project, hours)
		var maxLevel int64
		for _, lc := range sum.LogLevels {
			if lc.Count > maxLevel {
				maxLevel = lc.Count
			}
		}
		data["All"] = false
		data["Summary"] = sum
		data["MaxLevel"] = maxLevel
		data["Trend"] = trend
		data["Recent"] = recent
		data["Spark"] = spark
		data["Card"] = s.projectCard(store.DashboardRow{Summary: sum, Trend: trend}, hours, t.Env)
	}
	if top, qerr := s.st.QueryLeaderboard(t.Project, hours, 5); qerr == nil && len(top) > 0 {
		data["TopQueries"] = top
	}
	if jobs, jerr := s.st.JobLeaderboard(t.Project, hours, 5); jerr == nil {
		data["Jobs"] = jobs
	}
	if rels, rerr := s.st.ReleasesWithHealth(t.Project, 5); rerr == nil {
		data["Releases"] = rels
	}
	if err := s.dashboard.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render dashboard: %v", err)
	}
}

// projectCard composes one triage card: the release delta, the p95 delta
// beside the previous equal-length window, and the error sparkline.
func (s *server) projectCard(row store.DashboardRow, hours int, env string) ProjectCard {
	card := ProjectCard{
		Project:    row.Summary.Project,
		OpenIssues: row.Summary.OpenIssues,
		Errors:     row.Summary.Errors24,
		P95Ms:      row.Summary.P95Ms,
		Trend:      row.Trend,
	}
	if mark, err := s.st.LatestRelease(card.Project, env); err == nil && mark != nil {
		card.ReleaseVersion = mark.Version
		if n, err := s.st.OpenIssuesSince(card.Project, mark.At.Unix()); err == nil {
			card.NewSinceRelease = n
		}
	}
	if d, err := s.st.RequestP95Delta(card.Project, hours); err == nil {
		card.DeltaPct = d.DeltaPct()
	}
	return card
}

func (s *server) handleKeys(w http.ResponseWriter, r *http.Request) {
	s.renderKeys(w, r, nil)
}

func (s *server) renderKeys(w http.ResponseWriter, r *http.Request, extra map[string]any) {
	t := parseToolbar(r.URL.Query())
	data := s.baseData(r, t, PageKeys)
	projects, err := s.st.ListProjects()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if scope := s.projectScope(t); scope != nil {
		scoped := make([]store.Project, 0, len(projects))
		for _, p := range projects {
			if scope[p.Key] {
				scoped = append(scoped, p)
			}
		}
		projects = scoped
	}
	data["ProjectRows"] = projects
	for k, v := range extra {
		data[k] = v
	}
	if err := s.keys.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render keys: %v", err)
	}
}

func (s *server) handleKeyRotate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin rotate rejected", http.StatusForbidden)
		return
	}
	project := strings.TrimSpace(r.FormValue("project"))
	if project == "" {
		http.Error(w, "project required", http.StatusBadRequest)
		return
	}
	secret, err := s.st.RotateIngestKey(project)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.gate != nil {
		s.gate.Invalidate(project)
	}
	log.Printf("ingest key rotated for project %q", project)
	w.Header().Set("Cache-Control", "no-store")
	s.renderKeys(w, r, map[string]any{"NewKey": secret, "NewKeyProject": project})
}

func (s *server) handleKeyRevoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin revoke rejected", http.StatusForbidden)
		return
	}
	project := strings.TrimSpace(r.FormValue("project"))
	if project == "" {
		http.Error(w, "project required", http.StatusBadRequest)
		return
	}
	if err := s.st.RevokeIngestKey(project); err != nil {
		s.fail(w, r, err)
		return
	}
	if s.gate != nil {
		s.gate.Invalidate(project)
	}
	log.Printf("ingest key revoked for project %q", project)
	w.Header().Set("Cache-Control", "no-store")
	s.renderKeys(w, r, map[string]any{"Revoked": project})
}

// handleKeyOrigin saves a project's browser-origin allowlist. The
// allowlist was one global -cors-origin string applied to every browser route,
// which a multi-tenant install cannot express: each tenant's front end is a
// different origin. Empty clears it back to the install default.
func (s *server) handleKeyOrigin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin origin edit rejected", http.StatusForbidden)
		return
	}
	project := strings.TrimSpace(r.FormValue("project"))
	if project == "" {
		http.Error(w, "project required", http.StatusBadRequest)
		return
	}
	normalized, err := store.NormalizeCORSOrigins(r.FormValue("cors_origin"))
	if err != nil {
		s.failStatus(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.SetProjectCORSOrigin(project, normalized); err != nil {
		s.fail(w, r, err)
		return
	}
	if s.cors != nil {
		s.cors.Invalidate(project)
	}
	log.Printf("cors origin set for project %q", project)
	w.Header().Set("Cache-Control", "no-store")
	s.renderKeys(w, r, map[string]any{"OriginSaved": project, "OriginValue": normalized})
}

// sameOrigin rejects a cross-origin form POST (CSRF hardening for the key
// actions); an absent Origin header is accepted for same-origin form posts.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// buildSpanRows lays out a request-profile span waterfall. Spans that carry
// start_ms offsets are placed at them (the offset form is authoritative and
// may overlap or gap); legacy spans without offsets keep the report-order
// layout where each bar starts where the previous one ends (clamped to the
// reported total duration).
func buildSpanRows(p *store.Profile) []SpanRow {
	total := p.DurationMs
	var cum, maxEnd float64
	offsets := false
	for _, s := range p.Spans {
		cum += s.DurationMs
		if s.StartMs > 0 {
			offsets = true
		}
		if end := s.StartMs + s.DurationMs; end > maxEnd {
			maxEnd = end
		}
	}
	if cum > total {
		total = cum
	}
	if maxEnd > total {
		total = maxEnd
	}
	rows := make([]SpanRow, 0, len(p.Spans))
	var start float64
	for _, s := range p.Spans {
		left, width := 0.0, 0.0
		if total > 0 {
			if offsets {
				left = s.StartMs / total * 100
			} else {
				left = start / total * 100
			}
			width = s.DurationMs / total * 100
		}
		rows = append(rows, SpanRow{Name: s.Name, Left: f1(left), Width: f1(width), DurationMs: s.DurationMs})
		start += s.DurationMs
	}
	return rows
}

// TraceRow is one bar of the cross-service trace waterfall: a span from any
// leg, positioned in trace-relative elapsed milliseconds.
type TraceRow struct {
	Project    string
	ProfileID  int64
	Span       string
	Left       string
	Width      string
	DurationMs float64
	StartMs    float64
}

// HotRow is one self-time hotspot rendered under a trace leg.
type HotRow struct {
	Function string
	Left     string
	Width    string
	SelfMs   float64
	Calls    int64
}

// LegHotspots is the code-execution detail of one trace leg: its pruned
// call tree when the producer sent one, else its top self-time functions
// bar-scaled against the leg's worst offender.
type LegHotspots struct {
	ProfileID int64
	Project   string
	Name      string
	Rows      []HotRow
	Trees     []TreeView
}

// TraceView is the assembled /traces/{id} page model.
type TraceView struct {
	Rows      []TraceRow
	HotGroups []LegHotspots
	TotalMs   float64
	Legs      []store.Profile
	FirstSeen time.Time
	LastSeen  time.Time
}

// buildTraceView stitches the legs of one trace into a single waterfall.
// Every leg's spans are elapsed offsets from that leg's own start, so legs
// are joined through the PARENT's measured window: profile N's timeline is
// anchored at the parent span tagged leg=backend (start + duration), or —
// when the parent reports no such span — after the parent's last span ends.
// No cross-container clock sync is ever involved.
func buildTraceView(legs []store.Profile) *TraceView {
	v := &TraceView{Legs: legs}
	if len(legs) == 0 {
		return v
	}
	v.FirstSeen, v.LastSeen = legs[0].CreatedAt, legs[len(legs)-1].CreatedAt
	type placed struct {
		abs, end float64
	}
	ends := make([]placed, len(legs))
	for i, leg := range legs {
		var anchor float64
		if i > 0 {
			// Anchor this leg inside the parent's measured window.
			anchor = ends[i-1].end
			for _, s := range legs[i-1].Spans {
				if t, ok := s.Tags["leg"].(string); ok && t == "backend" {
					// Anchor at the fetch span's START: the backend leg's
					// bars draw inside the parent's fetch window, not after it.
					anchor = ends[i-1].abs + s.StartMs
					break
				}
			}
		}
		// Legacy legs (no offsets) lay spans out sequentially.
		offsets := false
		for _, s := range leg.Spans {
			if s.StartMs > 0 {
				offsets = true
				break
			}
		}
		var seq float64
		for _, s := range leg.Spans {
			start := seq
			if offsets {
				start = s.StartMs
			}
			abs := anchor + start
			end := abs + s.DurationMs
			v.Rows = append(v.Rows, TraceRow{
				Project: leg.ProjectKey, ProfileID: leg.ID, Span: s.Name,
				Left: f1(abs), DurationMs: s.DurationMs, StartMs: abs,
			})
			if end > v.TotalMs {
				v.TotalMs = end
			}
			seq = start + s.DurationMs
			if end > ends[i].end {
				ends[i].end = end
			}
			ends[i].abs = anchor
		}
		if len(leg.Spans) == 0 {
			ends[i].abs = anchor
			ends[i].end = anchor + leg.DurationMs
			if ends[i].end > v.TotalMs {
				v.TotalMs = ends[i].end
			}
		}
	}
	// Percent bars against the trace total.
	for i := range v.Rows {
		var w float64
		if v.TotalMs > 0 {
			w = v.Rows[i].DurationMs / v.TotalMs * 100
		}
		v.Rows[i].Width = f1(w)
	}
	// Code-execution detail: each leg's top self-time functions, bars scaled
	// against that leg's worst offender. Hotspots arrive sorted descending
	// (foldTrace / the front reporter), so the first ten are the answer to
	// "where did the time go" without a second sort.
	for _, leg := range legs {
		if len(leg.Hotspots) == 0 && len(leg.Tree) == 0 {
			continue
		}
		var maxSelf float64
		for _, h := range leg.Hotspots {
			if h.SelfMs > maxSelf {
				maxSelf = h.SelfMs
			}
		}
		top := leg.Hotspots
		if len(top) > 10 {
			top = top[:10]
		}
		group := LegHotspots{ProfileID: leg.ID, Project: leg.ProjectKey, Name: leg.Name}
		if len(leg.Tree) > 0 {
			total := leg.DurationMs
			for _, n := range leg.Tree {
				if n.I > total {
					total = n.I
				}
			}
			group.Trees = buildTreeViews(leg.Tree, total, 0)
		}
		for _, h := range top {
			var w float64
			if maxSelf > 0 {
				w = h.SelfMs / maxSelf * 100
			}
			group.Rows = append(group.Rows, HotRow{
				Function: h.Function, Left: "0", Width: f1(w), SelfMs: h.SelfMs, Calls: h.Calls,
			})
		}
		v.HotGroups = append(v.HotGroups, group)
	}
	return v
}

func f1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

// TreeView is a render-ready call-tree node: inclusive time as a bar width
// against the profile total, plus the depth used to auto-open top levels.
type TreeView struct {
	Name     string
	Width    string
	SelfMs   float64
	InclMs   float64
	Calls    int64
	Depth    int
	Children []TreeView
}

// buildTreeViews converts stored TreeNode rows into render-ready views.
// The ingest caps tree depth, so this recursion is bounded.
func buildTreeViews(nodes []store.TreeNode, totalMs float64, depth int) []TreeView {
	out := make([]TreeView, 0, len(nodes))
	for _, n := range nodes {
		w := 0.0
		if totalMs > 0 {
			w = n.I / totalMs * 100
		}
		if w > 100 {
			w = 100
		}
		tv := TreeView{Name: n.N, Width: f1(w), SelfMs: n.S, InclMs: n.I, Calls: n.C, Depth: depth}
		tv.Children = buildTreeViews(n.K, totalMs, depth+1)
		out = append(out, tv)
	}
	return out
}

// resolveHandler flips the resolved bit on an exception and returns to the list
// the reader came from.
//
// A log group is an event stream, not a defect: there is no resolve state a
// screen reads, so the request is refused instead of quietly writing a flag
// nobody sees. The guard lives here, not only in the template, because a POST
// is reachable without the button (iniznet/fenrirealm-tracker#526).
func (s *server) resolveHandler(resolved bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			s.failStatus(w, r, http.StatusBadRequest, "bad id")
			return
		}
		issue, err := s.st.Issue(id)
		if err != nil {
			s.failStatus(w, r, http.StatusNotFound, "not found")
			return
		}
		if issue.Type != "error" {
			s.failStatus(w, r, http.StatusBadRequest, "only exceptions have a resolve state")
			return
		}
		if err := s.st.SetResolved(id, resolved); err != nil {
			s.fail(w, r, err)
			return
		}
		if back := safeBack(r.FormValue("from")); back != "" {
			http.Redirect(w, r, back, http.StatusSeeOther)
			return
		}
		t := parseToolbar(r.URL.Query())
		http.Redirect(w, r, t.Link("/errors", nil), http.StatusSeeOther)
	}
}

// levelOptions lists the levels the project emits, keeping the reader's chosen
// level first when the window no longer contains it, so a deep link never lands
// on a form with nothing selected.
func levelOptions(stats []store.LevelCount, selected string) []store.LevelCount {
	if selected == "" {
		return stats
	}
	for _, lc := range stats {
		if lc.Level == selected {
			return stats
		}
	}
	return append([]store.LevelCount{{Level: selected}}, stats...)
}
