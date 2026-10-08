package ingest

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

// ProfileHandler accepts a sampled request profile from the first-party
// clients (Laravel MonitorClient, SvelteKit reporter):
//
//	{"name": "GET /chapters/123", "duration_ms": 412.5, "environment": "production",
//	 "spans": [{"name": "request", "duration_ms": 412.5, "tags": {"status": 200}}],
//	 "hotspots": [{"function": "render", "file": "+page.server.ts", "self_ms": 40.1}]}
//
// Wire format is ours (first-party tool); spans and hotspots are both optional.
func ProfileHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectKey := r.PathValue("project")
		if projectKey == "" {
			http.Error(w, "missing project", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}

		var payload struct {
			Name        string           `json:"name"`
			DurationMs  float64          `json:"duration_ms"`
			Environment string           `json:"environment"`
			Type        string           `json:"type"`
			TraceID     string           `json:"trace_id"`
			Spans       []store.Span     `json:"spans"`
			Hotspots    []store.Hotspot  `json:"hotspots"`
			Tree        []store.TreeNode `json:"tree"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || payload.Name == "" || payload.DurationMs <= 0 {
			http.Error(w, "invalid profile", http.StatusBadRequest)
			return
		}
		if payload.Type != store.ProfileTypeCallgraph {
			payload.Type = store.ProfileTypeRequest
		}

		spans := make([]store.Span, 0, len(payload.Spans))
		for _, span := range payload.Spans {
			tags, _ := store.Clean(span.Tags).(map[string]any)
			spans = append(spans, store.Span{Name: span.Name, DurationMs: span.DurationMs, StartMs: span.StartMs, Tags: tags})
		}
		hotspots := sanitizeHotspots(payload.Hotspots)

		id, err := st.RecordProfile(store.ProfileInput{
			ProjectKey: projectKey, Type: payload.Type, Name: payload.Name,
			DurationMs: payload.DurationMs, Environment: payload.Environment,
			TraceID: payload.TraceID,
			Spans:   spans, Hotspots: hotspots,
			Tree: SanitizeTree(payload.Tree, payload.DurationMs),
		})
		if err != nil {
			log.Printf("record profile: %v", err)
			http.Error(w, "store", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
	}
}

// ErrorHandler accepts a first-party error report (the SvelteKit hook speaks
// this): {"message", "stack", "url", "route", "environment", "release"}.
func ErrorHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectKey := r.PathValue("project")
		if projectKey == "" {
			http.Error(w, "missing project", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}

		var payload struct {
			Message     string `json:"message"`
			Stack       string `json:"stack"`
			URL         string `json:"url"`
			Route       string `json:"route"`
			Environment string `json:"environment"`
			Release     string `json:"release"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || payload.Message == "" {
			http.Error(w, "invalid error report", http.StatusBadRequest)
			return
		}

		fp := store.ForException("JSError", payload.Message, topStackLine(payload.Stack))
		ctx := map[string]any{
			"url":     payload.URL,
			"route":   payload.Route,
			"stack":   truncate(payload.Stack, 16<<10),
			"release": payload.Release,
		}
		if _, err := st.RecordError(projectKey, "JSError", payload.Message, fp, payload.Environment, "error", ctx); err != nil {
			log.Printf("record js error: %v", err)
			http.Error(w, "store", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func sanitizeHotspots(in []store.Hotspot) []store.Hotspot {
	if len(in) == 0 {
		return nil
	}
	out := make([]store.Hotspot, 0, len(in))
	for _, h := range in {
		if h.Function == "" || h.SelfMs < 0 {
			continue
		}
		out = append(out, store.Hotspot{Function: truncate(h.Function, 300), File: truncate(h.File, 300), SelfMs: h.SelfMs, Calls: h.Calls})
	}
	return out
}

func topStackLine(stack string) string {
	for _, line := range strings.Split(stack, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "Error") {
			return line
		}
	}
	return ""
}
