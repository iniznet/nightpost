package web

import (
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"regexp"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

type Page string

type issueFilter struct {
	Type, Level, Env, Channel, Status, Q string
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

type SpanRow struct {
	Name       string
	Left       string
	Width      string
	DurationMs float64
}

const (
	PageDashboard Page = "dashboard"
	PageIssues    Page = "issues"
	PageLogs      Page = "logs"
	PageProfiles  Page = "profiles"
	PageSlowest   Page = "slowest"
	PageTrace     Page = "trace"
)

var funcMap = template.FuncMap{
	"rel":   relTime,
	"spark": sparkPoints,
	"ms":    fmtMs,
	"mb":    fmtMB,
	"fn":    prettifyFn,
	"pct":   func(v, max float64) string { return strconv.FormatFloat(pctOf(v, max), 'f', 1, 64) },
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

// baseData carries the fields every page needs for the shared layout.
func baseData(st *store.Store, project string, page Page) map[string]any {
	projects, err := st.Projects()
	if err != nil {
		log.Printf("projects: %v", err)
	}
	return map[string]any{"Projects": projects, "Project": project, "Page": page}
}

func parseID(s string) int64 {
	id, _ := strconv.ParseInt(s, 10, 64)
	return id
}

func Register(mux *http.ServeMux, st *store.Store) {
	dashboardTmpl := mustLayout("dashboard.html")
	issuesTmpl := mustLayout("index.html")
	detailTmpl := mustLayout("detail.html")
	logsTmpl := mustLayout("logs.html")
	profilesTmpl := mustLayout("profiles.html")
	profileTmpl := mustLayout("profile.html")
	slowestTmpl := mustLayout("slowest.html")
	traceTmpl := mustLayout("trace.html")

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		project := r.URL.Query().Get("project")
		data := baseData(st, project, PageDashboard)
		summaries, err := st.DashboardSummaries(24)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if project == "" {
			rows := make([]ProjectRow, 0, len(summaries))
			for _, s := range summaries {
				rows = append(rows, ProjectRow{Summary: s.Summary, Trend: s.Trend})
			}
			data["All"] = true
			data["Rows"] = rows
		} else {
			var sum store.ProjectSummary
			var trend []int64
			for _, s := range summaries {
				if s.Summary.Project == project {
					sum, trend = s.Summary, s.Trend
					break
				}
			}
			recent, err := st.Issues(store.IssueQuery{Project: project, Status: "open", Limit: 8, Sort: "pain"})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			spark, _ := st.IssueSparklines(project, 24)
			envs, _ := st.Environments(project)
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
			data["Envs"] = envs
		}
		if stats, serr := st.Stats(); serr == nil {
			data["StoreStats"] = stats
		}
		if err := dashboardTmpl.ExecuteTemplate(w, "layout", data); err != nil {
			log.Printf("render dashboard: %v", err)
		}
	})

	mux.HandleFunc("GET /issues", func(w http.ResponseWriter, r *http.Request) {
		project := r.URL.Query().Get("project")
		f := issueFilter{
			Type:   r.URL.Query().Get("type"),
			Level:  r.URL.Query().Get("level"),
			Env:    r.URL.Query().Get("env"),
			Status: r.URL.Query().Get("status"),
			Q:      r.URL.Query().Get("q"),
		}
		if f.Status == "" {
			f.Status = "open"
		}
		// Pain order (count DESC) is the default: a wall of recent one-offs
		// otherwise buries the recurring noise that actually hurts.
		f.Sort = r.URL.Query().Get("sort")
		if f.Sort != "recent" {
			f.Sort = "pain"
		}
		issues, err := st.Issues(store.IssueQuery{
			Project: project, Type: f.Type, Level: f.Level, Env: f.Env, Q: f.Q,
			Status:      f.Status,
			Sort:        f.Sort,
			BeforeID:    parseID(r.URL.Query().Get("before")),
			BeforeCount: parseID(r.URL.Query().Get("beforecount")),
			Limit:       50,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		spark, _ := st.IssueSparklines(project, 24)
		envs, _ := st.Environments(project)
		// The per-path 404 family collapses into one row unless the user is
		// already drilling into it via the title search.
		shown := issues
		if f.Q == "" {
			shown = collapseNotFound(issues)
		}
		data := baseData(st, project, PageIssues)
		data["Issues"] = shown
		data["Spark"] = spark
		data["Filter"] = f
		data["Envs"] = envs
		if len(issues) == 50 {
			v := url.Values{}
			v.Set("project", project)
			v.Set("type", f.Type)
			v.Set("level", f.Level)
			v.Set("env", f.Env)
			v.Set("status", f.Status)
			v.Set("q", f.Q)
			v.Set("sort", f.Sort)
			v.Set("before", strconv.FormatInt(issues[len(issues)-1].ID, 10))
			v.Set("beforecount", strconv.FormatInt(issues[len(issues)-1].Count, 10))
			data["NextURL"] = "/issues?" + v.Encode()
		}
		if err := issuesTmpl.ExecuteTemplate(w, "layout", data); err != nil {
			log.Printf("render issues: %v", err)
		}
	})

	mux.HandleFunc("GET /issues/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		issue, err := st.Issue(id)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		occurrences, err := st.Occurrences(id, 20)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data := baseData(st, issue.ProjectKey, PageIssues)
		data["Issue"] = issue
		data["Views"] = buildOccViews(occurrences)
		if err := detailTmpl.ExecuteTemplate(w, "layout", data); err != nil {
			log.Printf("render detail: %v", err)
		}
	})

	mux.HandleFunc("POST /issues/{id}/resolve", resolveHandler(st, true))
	mux.HandleFunc("POST /issues/{id}/reopen", resolveHandler(st, false))

	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		project := r.URL.Query().Get("project")
		f := issueFilter{
			Level:   r.URL.Query().Get("level"),
			Env:     r.URL.Query().Get("env"),
			Channel: r.URL.Query().Get("channel"),
			Q:       r.URL.Query().Get("q"),
			Status:  "all",
			Type:    "log",
		}
		logs, err := st.Issues(store.IssueQuery{
			Project: project, Type: "log", Level: f.Level, Env: f.Env, Channel: f.Channel, Q: f.Q,
			Status:   "all",
			BeforeID: parseID(r.URL.Query().Get("before")),
			Limit:    50,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		levels, _ := st.LogLevels(project)
		envs, _ := st.Environments(project)
		data := baseData(st, project, PageLogs)
		data["Issues"] = logs
		data["Filter"] = f
		data["Levels"] = levels
		data["Envs"] = envs
		if len(logs) == 50 {
			v := url.Values{}
			v.Set("project", project)
			v.Set("level", f.Level)
			v.Set("env", f.Env)
			v.Set("channel", f.Channel)
			v.Set("q", f.Q)
			v.Set("before", strconv.FormatInt(logs[len(logs)-1].ID, 10))
			data["NextURL"] = "/logs?" + v.Encode()
		}
		if err := logsTmpl.ExecuteTemplate(w, "layout", data); err != nil {
			log.Printf("render logs: %v", err)
		}
	})

	mux.HandleFunc("GET /profiles", func(w http.ResponseWriter, r *http.Request) {
		project := r.URL.Query().Get("project")
		typeFilter := r.URL.Query().Get("type")
		view := r.URL.Query().Get("view")
		q := r.URL.Query().Get("q")
		data := baseData(st, project, PageProfiles)
		data["TypeF"] = typeFilter
		data["View"] = view
		data["Q"] = q
		if view != "raw" && (typeFilter == store.ProfileTypeRequest || typeFilter == store.ProfileTypeCallgraph) {
			// Aggregated-by-route is the default view: a flat recency list
			// repeats the same hot route ten times and hides what actually
			// accumulates wall-clock time.
			routes, err := st.ProfileRouteStats(project, typeFilter, 24, 50)
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
			if err := profilesTmpl.ExecuteTemplate(w, "layout", data); err != nil {
				log.Printf("render profiles: %v", err)
			}
			return
		}
		profiles, err := st.ProfilesFiltered(project, typeFilter, q, 50)
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
		if err := profilesTmpl.ExecuteTemplate(w, "layout", data); err != nil {
			log.Printf("render profiles: %v", err)
		}
	})

	mux.HandleFunc("GET /profiles/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		profile, err := st.Profile(id)
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
		data := baseData(st, profile.ProjectKey, PageProfiles)
		data["Profile"] = profile
		data["SpanRows"] = buildSpanRows(profile)
		data["MaxSelf"] = maxSelf
		if err := profileTmpl.ExecuteTemplate(w, "layout", data); err != nil {
			log.Printf("render profile: %v", err)
		}
	})

	mux.HandleFunc("GET /traces/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		legs, err := st.ProfilesByTrace(id, 20)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(legs) == 0 {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		data := baseData(st, "", PageTrace)
		data["Trace"] = buildTraceView(legs)
		data["TraceID"] = id
		if err := traceTmpl.ExecuteTemplate(w, "layout", data); err != nil {
			log.Printf("render trace: %v", err)
		}
	})

	mux.HandleFunc("GET /slowest", func(w http.ResponseWriter, r *http.Request) {
		project := r.URL.Query().Get("project")
		rows, err := st.SlowestCode(project, 500)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var maxSelf float64
		for _, row := range rows {
			if row.TotalSelfMs > maxSelf {
				maxSelf = row.TotalSelfMs
			}
		}
		data := baseData(st, project, PageSlowest)
		data["Rows"] = rows
		data["MaxSelf"] = maxSelf
		if err := slowestTmpl.ExecuteTemplate(w, "layout", data); err != nil {
			log.Printf("render slowest: %v", err)
		}
	})
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

// TraceView is the assembled /traces/{id} page model.
type TraceView struct {
	Rows     []TraceRow
	TotalMs  float64
	Legs     []store.Profile
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
	return v
}

func f1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

// resolveHandler flips the resolved bit and redirects back to the issues
// list preserving the project filter.
func resolveHandler(st *store.Store, resolved bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		iss, err := st.Issue(id)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err := st.SetResolved(id, resolved); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/issues?project="+url.QueryEscape(iss.ProjectKey), http.StatusSeeOther)
	}
}
