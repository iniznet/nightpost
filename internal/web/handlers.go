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

	"github.com/iniznet/fenrirealm-monitor/internal/ingest"
	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

// Page identifies the rendered page. The M6.1 IA consolidates pages into two
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
	case PageKeys:
		return "keys"
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
}

// IngestHealthSnapshot is the rendered form of IngestHealth.
type IngestHealthSnapshot struct {
	Shaper429     int64
	KeyGate401    int64
	UnknownAdmits int64
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

// Option configures the page server.
type Option func(*server)

// WithIngestHealth attaches the live ingest-rejection counters the footer
// strip renders. Without it the strip shows store figures only.
func WithIngestHealth(h IngestHealth) Option {
	return func(s *server) { s.health = h }
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
	// paths keep serving the same handlers (Expand/Contract, M6.1 invariant).
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
	page.HandleFunc("GET /keys", s.handleKeys)
	page.HandleFunc("POST /keys/rotate", s.handleKeyRotate)
	page.HandleFunc("POST /keys/revoke", s.handleKeyRevoke)
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
	projects, err := s.st.Projects()
	if err != nil {
		log.Printf("projects: %v", err)
	}
	envs, err := s.st.Environments(t.Project)
	if err != nil {
		log.Printf("environments: %v", err)
	}
	data := map[string]any{
		"Projects": projects,
		"Project":  t.Project,
		"Page":     page,
		"Nav":      page.nav(),
		"Toolbar":  t,
		"Ranges":   rangeSpecs,
		"Envs":     envs,
		"Path":     r.URL.Path,
		"Tab":      r.URL.Query().Get("tab"),
	}
	if s.health.ShaperRejections != nil || s.health.KeyGateRejections != nil || s.health.UnknownAdmits != nil {
		data["Health"] = s.health.Snapshot()
	}
	if stats := s.storeStats(); stats != nil {
		data["StoreStats"] = stats
	}
	return data
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
// an unknown tab resolves to it, never a 404 (M6.1 invariant I2).
func (s *server) handleErrors(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("tab") == "logs" {
		s.handleLogs(w, r)
		return
	}
	s.handleIssues(w, r)
}

// handleIssues renders the error stream. M6.1 folds the log-type
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	issue, err := s.st.Issue(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	occurrences, err := s.st.Occurrences(id, 20)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	t := parseToolbar(r.URL.Query())
	// The detail link targets the section the row came from (Errors); the
	// legacy path is preserved so bookmarks keep working.
	backPath := "/errors"
	if r.URL.Query().Get("from") == "logs" {
		backPath = "/errors"
	}
	data := s.baseData(r, t, PageErrors)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
			http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	profile, err := s.st.Profile(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(legs) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data["Rows"] = aggs
	if err := s.jobs.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render jobs: %v", err)
	}
}

func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	t := parseToolbar(r.URL.Query())
	hours := t.Hours()
	data := s.baseData(r, t, PageDashboard)
	summaries, err := s.st.DashboardSummaries(hours)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if t.Project == "" {
		rows := make([]ProjectRow, 0, len(summaries))
		cards := make([]ProjectCard, 0, len(summaries))
		for _, sm := range summaries {
			rows = append(rows, ProjectRow{Summary: sm.Summary, Trend: sm.Trend})
			cards = append(cards, s.projectCard(sm, hours, t.Env))
		}
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.gate != nil {
		s.gate.Invalidate(project)
	}
	log.Printf("ingest key revoked for project %q", project)
	w.Header().Set("Cache-Control", "no-store")
	s.renderKeys(w, r, map[string]any{"Revoked": project})
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

// resolveHandler flips the resolved bit and redirects back to the issues
// list preserving the project filter.
func (s *server) resolveHandler(resolved bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		if _, err := s.st.Issue(id); err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err := s.st.SetResolved(id, resolved); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		t := parseToolbar(r.URL.Query())
		http.Redirect(w, r, t.Link("/errors", nil), http.StatusSeeOther)
	}
}
