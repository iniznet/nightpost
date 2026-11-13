package web

import (
	"log"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/iniznet/nightpost/internal/store"
)

// JobRunRow is one sampled run of a job: when it ran, what it cost, and the
// trace to open when it carried spans.
type JobRunRow struct {
	At       time.Time
	Ms       float64
	TraceID  string
	SpanN    int
	TopSpan  string
	TopMs    float64
	Spanless bool
}

// FrameRow is one hop aggregated across the sampled runs of a job. The
// waterfall of a single run answers "what happened once"; this table answers
// "what always happens", which is the question a slow mechanism raises.
type FrameRow struct {
	Name    string
	Count   int
	TotalMs float64
	AvgMs   float64
	Share   float64
}

// JobHotspotRow is one app frame aggregated across the sampled runs.
type JobHotspotRow struct {
	Function string
	File     string
	SelfMs   float64
	Calls    int64
}

// buildJobRuns flattens the sampled profiles into the rows the page renders.
func buildJobRuns(detail *store.JobRuns) []JobRunRow {
	out := make([]JobRunRow, 0, len(detail.Runs))
	for _, p := range detail.Runs {
		row := JobRunRow{At: p.CreatedAt, Ms: p.DurationMs, TraceID: p.TraceID, SpanN: len(p.Spans)}
		if len(p.Spans) == 0 {
			row.Spanless = true
		}
		for _, s := range p.Spans {
			if s.DurationMs > row.TopMs {
				row.TopMs = s.DurationMs
				row.TopSpan = s.Name
			}
		}
		out = append(out, row)
	}
	return out
}

// buildJobFrames aggregates every span name across the sampled runs. The
// share is of the sampled total, so a job whose database work is 82% of its
// time says 82% here and the reader never has to add columns.
func buildJobFrames(detail *store.JobRuns) []FrameRow {
	byName := map[string]*FrameRow{}
	var total float64
	for _, p := range detail.Runs {
		for _, s := range p.Spans {
			f := byName[s.Name]
			if f == nil {
				f = &FrameRow{Name: s.Name}
				byName[s.Name] = f
			}
			f.Count++
			f.TotalMs += s.DurationMs
			total += s.DurationMs
		}
	}
	out := make([]FrameRow, 0, len(byName))
	for _, f := range byName {
		f.AvgMs = f.TotalMs / float64(f.Count)
		if total > 0 {
			f.Share = f.TotalMs / total * 100
		}
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TotalMs > out[j].TotalMs })
	return out
}

// buildJobHotspots aggregates the app frames the sampler attributed across
// the sampled runs. Only frames a reader can act on survive: a framework's own
// hot function is not a defect in their code.
func buildJobHotspots(detail *store.JobRuns) []JobHotspotRow {
	byFn := map[string]*JobHotspotRow{}
	for _, p := range detail.Runs {
		for _, h := range p.Hotspots {
			row := byFn[h.Function]
			if row == nil {
				row = &JobHotspotRow{Function: h.Function, File: h.File}
				byFn[h.Function] = row
			}
			row.SelfMs += h.SelfMs
			row.Calls += h.Calls
		}
	}
	out := make([]JobHotspotRow, 0, len(byFn))
	for _, r := range byFn {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SelfMs > out[j].SelfMs })
	if len(out) > 15 {
		out = out[:15]
	}
	return out
}

// handleJobDetail is /jobs/{kind}/{name}: one job's sampled runs, the frames
// that carry its cost, and the trace to open for any run that reported spans.
func (s *server) handleJobDetail(w http.ResponseWriter, r *http.Request) {
	kind, name := r.PathValue("kind"), r.PathValue("name")
	f := parsePerfFilter(r.URL.Query())
	detail, err := s.st.JobRuns(f.Project, kind, name, f.Hours(), 25)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data := s.perfData(r, f, PageJobs)
	data["Path"] = "/jobs/" + kind + "/" + url.PathEscape(name)
	data["Kind"] = kind
	data["Name"] = name
	data["Detail"] = detail
	data["Runs"] = buildJobRuns(detail)
	data["Frames"] = buildJobFrames(detail)
	data["Hotspots"] = buildJobHotspots(detail)
	if err := s.jobDetail.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render job detail: %v", err)
	}
}
