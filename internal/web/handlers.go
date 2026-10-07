package web

import (
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

type Page string

const (
	PageDashboard Page = "dashboard"
	PageIssues    Page = "issues"
	PageLogs      Page = "logs"
	PageProfiles  Page = "profiles"
	PageSlowest   Page = "slowest"
)

type issueFilter struct {
	Type, Level, Env, Channel, Status, Q string
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

var funcMap = template.FuncMap{
	"rel":   relTime,
	"spark": sparkPoints,
	"ms":    fmtMs,
	"mb":    fmtMB,
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

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		project := r.URL.Query().Get("project")
		data := baseData(st, project, PageDashboard)
		if project == "" {
			rows := make([]ProjectRow, 0, len(data["Projects"].([]string)))
			for _, p := range data["Projects"].([]string) {
				sum, err := st.ProjectSummary(p)
				if err != nil {
					log.Printf("summary %s: %v", p, err)
					continue
				}
				trend, err := st.HourBuckets(p, 24, "error")
				if err != nil {
					log.Printf("trend %s: %v", p, err)
					continue
				}
				rows = append(rows, ProjectRow{Summary: sum, Trend: trend})
			}
			data["All"] = true
			data["Rows"] = rows
		} else {
			sum, err := st.ProjectSummary(project)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			trend, err := st.HourBuckets(project, 24, "error")
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			recent, err := st.Issues(store.IssueQuery{Project: project, Status: "open", Limit: 8})
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
		issues, err := st.Issues(store.IssueQuery{
			Project: project, Type: f.Type, Level: f.Level, Env: f.Env, Q: f.Q,
			Status:   f.Status,
			BeforeID: parseID(r.URL.Query().Get("before")),
			Limit:    50,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		spark, _ := st.IssueSparklines(project, 24)
		envs, _ := st.Environments(project)
		data := baseData(st, project, PageIssues)
		data["Issues"] = issues
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
			v.Set("before", strconv.FormatInt(issues[len(issues)-1].ID, 10))
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
		var profiles []store.Profile
		var err error
		if typeFilter == "request" || typeFilter == "callgraph" {
			profiles, err = st.ProfilesByType(project, typeFilter, 50)
		} else {
			profiles, err = st.Profiles(project, 50)
		}
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
		data := baseData(st, project, PageProfiles)
		data["Profiles"] = profiles
		data["MaxDur"] = maxDur
		data["TypeF"] = typeFilter
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

// buildSpanRows lays out a request-profile span waterfall: spans are stored
// in report order without start offsets, so each bar starts where the
// previous one ends (clamped to the reported total duration).
func buildSpanRows(p *store.Profile) []SpanRow {
	total := p.DurationMs
	var cum float64
	for _, s := range p.Spans {
		cum += s.DurationMs
	}
	if cum > total {
		total = cum
	}
	rows := make([]SpanRow, 0, len(p.Spans))
	var start float64
	for _, s := range p.Spans {
		left, width := 0.0, 0.0
		if total > 0 {
			left = start / total * 100
			width = s.DurationMs / total * 100
		}
		rows = append(rows, SpanRow{Name: s.Name, Left: f1(left), Width: f1(width), DurationMs: s.DurationMs})
		start += s.DurationMs
	}
	return rows
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
