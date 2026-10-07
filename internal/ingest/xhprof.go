package ingest

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

// XHProfHandler accepts the wire format spiral-packages/profiler's WebStorage
// POSTs (via maantje/xhprof-buggregator-laravel, pointed at PROFILER_ENDPOINT):
//
//	{"profile": {"main()": {"ct":1,"wt":1000000,...}, "main()==>Fn::bar": {...}},
//	 "tags": {"env": "production", "host": "..."}, "app_name": "fenrirealm-backend",
//	 "hostname": "...", "date": 1728300000}
//
// Trace keys are classic xhprof "parent==>child" edges ("main()" root entries
// carry the root's inclusive cost). Costs are microseconds. The handler folds
// the edge list into per-function SELF time and stores the top functions as a
// callgraph profile.
func XHProfHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectKey := xhprofProject(r)
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}

		var payload struct {
			Profile  map[string]xhprofEntry `json:"profile"`
			Tags     map[string]string      `json:"tags"`
			AppName  string                 `json:"app_name"`
			Hostname string                 `json:"hostname"`
			Date     int64                  `json:"date"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || len(payload.Profile) == 0 {
			http.Error(w, "invalid xhprof payload", http.StatusBadRequest)
			return
		}

		hotspots, totalMs := foldTrace(payload.Profile)
		if len(hotspots) == 0 {
			http.Error(w, "empty trace", http.StatusBadRequest)
			return
		}

		env := payload.Tags["env"]
		name := payload.AppName
		if name == "" {
			name = "xhprof"
		}
		if payload.Hostname != "" {
			name += " @ " + payload.Hostname
		}

		id, err := st.RecordProfile(store.ProfileInput{
			ProjectKey: projectKey, Type: store.ProfileTypeCallgraph, Name: name,
			DurationMs: totalMs, Environment: env, Hotspots: hotspots,
		})
		if err != nil {
			log.Printf("record xhprof profile: %v", err)
			http.Error(w, "store", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
	}
}

type xhprofEntry struct {
	Ct  int64   `json:"ct"`
	Wt  float64 `json:"wt"`
	Cpu float64 `json:"cpu"`
	Mu  float64 `json:"mu"`
	Pmu float64 `json:"pmu"`
}

// xhprofProject resolves the project key: explicit path segment first, then
// HTTP Basic username (PROFILER_ENDPOINT="http://backend@host/..." ->
// "backend"), then the X-Monitor-Project header, else "backend".
func xhprofProject(r *http.Request) string {
	if p := r.PathValue("project"); p != "" {
		return p
	}
	if _, pass, ok := r.BasicAuth(); ok && pass == "" {
		if user, _, ok2 := r.BasicAuth(); ok2 && user != "" {
			return user
		}
	}
	if h := r.Header.Get("X-Monitor-Project"); h != "" {
		return h
	}
	return "backend"
}

// foldTrace converts the xhprof edge list into per-function self time (ms),
// sorted descending and capped at the store's hotspot limit.
func foldTrace(edges map[string]xhprofEntry) ([]store.Hotspot, float64) {
	inclusive := map[string]float64{}
	childWt := map[string]float64{}
	calls := map[string]int64{}
	var rootMs float64

	for key, e := range edges {
		if parent, child, ok := strings.Cut(key, "==>"); ok {
			inclusive[child] += e.Wt
			calls[child] += e.Ct
			// Self-recursive edges (fn==>fn) burn time inside fn itself.
			childWt[parent] += e.Wt
		} else {
			// Root entry ("main()"): its wt is the whole-request inclusive.
			inclusive[key] += e.Wt
			calls[key] += e.Ct
			rootMs += e.Wt / 1000
		}
	}

	type selfRow struct {
		fn     string
		selfMs float64
	}
	rows := make([]selfRow, 0, len(inclusive))
	for fn, inc := range inclusive {
		self := inc - childWt[fn]
		if self < 0 {
			self = 0
		}
		rows = append(rows, selfRow{fn: fn, selfMs: self / 1000})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].selfMs > rows[j].selfMs })

	if len(rows) > 50 {
		rows = rows[:50]
	}
	hotspots := make([]store.Hotspot, 0, len(rows))
	for _, row := range rows {
		hotspots = append(hotspots, store.Hotspot{
			Function: row.fn,
			SelfMs:   row.selfMs,
			Calls:    calls[row.fn],
		})
	}
	return hotspots, rootMs
}