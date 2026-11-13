package ingest

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/iniznet/nightpost/internal/store"
)

// Operator knobs this file reads on every request. They live in settings, not
// in flags, because both answers can change while ingest serves: an operator
// chasing a producer bug turns envelope capture on, then turns it back off.
const (
	// SettingKeepEnvelopes is 0|N: keep the last N raw bodies (0 = none).
	SettingKeepEnvelopes = "ingest.keep_envelopes"
	// SettingValidate is strict|warn. Strict (the default) refuses an invalid
	// payload; warn logs the JSON path and stores the event anyway.
	SettingValidate = "ingest.validate"
	// ValidateWarn is the only non-default value SettingValidate accepts.
	ValidateWarn = "warn"
)

// maxRawEnvelope bounds one stored body. The ring is for "what did the
// producer send", not for keeping a second copy of a 10 MiB envelope.
const maxRawEnvelope = 64 << 10

// maxServiceName bounds a producer-supplied name before it reaches the services
// table, where UNIQUE(project_key, name) would otherwise be grown by a flood.
const maxServiceName = 128

// serviceRef is the producer identity resolved for one payload.
type serviceRef struct {
	ID      int64
	Name    string
	Trusted bool
}

// resolveService admits (or refreshes) the payload's producer identity.
//
// An ABSENT service is the v1 rollout path, not an error: the event is labelled
// with the project key, so every project has a service from its first event and
// the "upgrade your producer" banner can disappear per project. That label is
// TRUSTED because the pre-v2 server joined a traceparent from exactly this
// caller; trusting nothing on rollout would silently split traces a v1 producer
// already propagates.
//
// An existing service keeps the trust an operator granted it: the payload does
// not vote on its own trust, or every event would reset the flag.
func resolveService(st *store.Store, projectKey, name, env, leg string) serviceRef {
	name = strings.TrimSpace(name)
	v1 := name == ""
	if v1 {
		name = projectKey
	}
	name = truncate(name, maxServiceName)
	ref := serviceRef{Name: name, Trusted: v1}
	if st == nil {
		return ref
	}
	existing, err := st.ServiceByName(projectKey, name)
	if err != nil {
		log.Printf("resolve service %s/%s: %v", projectKey, name, err)
	}
	if existing != nil {
		ref.Trusted = existing.TrustedTraceparent
	}
	id, err := st.UpsertService(projectKey, name, "", truncate(env, 64), ref.Trusted)
	if err != nil {
		log.Printf("upsert service %s/%s (%s): %v", projectKey, name, leg, err)
		return ref
	}
	ref.ID = id
	return ref
}

// digest decides what one invalid field costs.
//
// Strict is the shipped behaviour and is unchanged byte for byte: the payload
// is refused exactly where it was refused before. Warn exists for a rollout,
// where refusing a whole event over one field loses the data an operator is
// watching for — the field's JSON path is logged and the event is stored.
type digest struct {
	project string
	route   string
	warn    bool
	fatal   int
	first   string
}

func newDigest(st *store.Store, projectKey, route string) *digest {
	mode := ""
	if st != nil {
		if v, err := st.Setting(SettingValidate); err == nil {
			mode = v
		}
	}
	return &digest{
		project: projectKey,
		route:   route,
		warn:    strings.EqualFold(strings.TrimSpace(mode), ValidateWarn),
	}
}

// issue records a field the payload cannot be accepted with in strict mode.
// msg is the operator-facing reason AND the response body a strict refusal has
// always sent, so enabling warn mode never changes the words on the wire.
func (d *digest) issue(path, msg string) {
	d.fatal++
	if d.first == "" {
		d.first = msg
	}
	if d.warn {
		log.Printf("ingest warn: %s %s %s: %s", d.project, d.route, path, msg)
	}
}

// note records a field this server can repair. It never refuses a payload in
// either mode: a bad v2 span field must not turn a payload a v1 producer sent
// into a rejection.
func (d *digest) note(path, format string, args ...any) {
	log.Printf("ingest warn: %s %s %s: %s", d.project, d.route, path, fmt.Sprintf(format, args...))
}

// rejected reports whether strict mode refuses this payload.
func (d *digest) rejected() bool { return d.fatal > 0 && !d.warn }

// reason is the first field the payload was refused for.
func (d *digest) reason() string { return d.first }

// captureRaw keeps the raw body when ingest.keep_envelopes > 0, so the
// /ingest-log page can show exactly what a producer sent beside the verdict.
// Accepted and refused bodies are both kept: a refusal is the interesting half.
func captureRaw(st *store.Store, project, route string, body []byte, accepted bool, reason string) {
	if st == nil {
		return
	}
	keep := st.SettingInt(SettingKeepEnvelopes, 0)
	if keep <= 0 {
		return
	}
	if len(body) > maxRawEnvelope {
		body = body[:maxRawEnvelope]
	}
	if err := st.InsertRawEnvelope(store.RawEnvelope{
		ProjectKey: project, Route: route, Accepted: accepted, Error: reason, Body: body,
	}, keep); err != nil {
		log.Printf("ingest envelope: %v", err)
	}
}

// profilePayload is the request/job/command profile contract, additive to v1:
// service, env, kind, route, span_id and the v2 span fields are all optional.
type profilePayload struct {
	Name        string           `json:"name"`
	DurationMs  float64          `json:"duration_ms"`
	Environment string           `json:"environment"`
	Env         string           `json:"env"`
	Service     string           `json:"service"`
	Type        string           `json:"type"`
	Kind        string           `json:"kind"`
	Route       string           `json:"route"`
	SpanID      string           `json:"span_id"`
	TraceID     string           `json:"trace_id"`
	Spans       []spanPayload    `json:"spans"`
	Hotspots    []store.Hotspot  `json:"hotspots"`
	Tree        []store.TreeNode `json:"tree"`
}

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

		var payload profilePayload
		if err := json.Unmarshal(body, &payload); err != nil {
			captureRaw(st, projectKey, "profile", body, false, "body: not a json object")
			http.Error(w, "invalid profile", http.StatusBadRequest)
			return
		}
		d := newDigest(st, projectKey, "profile")
		if strings.TrimSpace(payload.Name) == "" {
			d.issue("name", "is required")
		}
		if payload.DurationMs <= 0 {
			d.issue("duration_ms", "must be positive")
		}
		spans := clipSpans(payload.Spans, d)
		if d.rejected() {
			captureRaw(st, projectKey, "profile", body, false, d.reason())
			http.Error(w, "invalid profile", http.StatusBadRequest)
			return
		}
		switch payload.Type {
		case store.ProfileTypeCallgraph, store.ProfileTypeJob, store.ProfileTypeCommand:
		default:
			payload.Type = store.ProfileTypeRequest
		}

		legacy := make([]store.Span, 0, len(spans))
		for _, span := range spans {
			tags, _ := store.Clean(span.Tags).(map[string]any)
			legacy = append(legacy, store.Span{Name: span.Name, DurationMs: span.DurationMs, StartMs: span.StartMs, Tags: tags})
		}

		svc := resolveService(st, projectKey, payload.Service, payload.Env, "profile")
		traceID := TraceIDFromRequest(r, payload.TraceID)
		origin := ""
		if len(spans) > 0 {
			traceID, origin = traceIdentity(r, payload.TraceID, svc.Trusted)
		}
		kind := truncate(strings.TrimSpace(payload.Kind), 32)
		if kind == "" {
			kind = store.ProfileTypeRequest
		}

		rootID := ""
		if payload.SpanID != "" || len(spans) > 0 {
			rootID = spanIDOrMint(payload.SpanID)
		}
		var routeID int64
		if len(spans) > 0 {
			// One extra token covers the payload's own root span.
			if admitSpans(r, len(spans)+1) {
				batch := spanBatch{
					Project: projectKey, TraceID: traceID, Origin: origin, Service: svc,
					Root: spanPayload{
						Span:   store.Span{Name: payload.Name, DurationMs: payload.DurationMs},
						SpanID: rootID,
						Kind:   kind,
						Status: "ok",
						Route:  payload.Route,
					},
					Children: spans,
				}
				id, err := batch.write(st, d)
				if err != nil {
					log.Printf("record profile spans: %v", err)
				}
				routeID = id
			}
		}

		id, err := st.RecordProfile(store.ProfileInput{
			ProjectKey: projectKey, Type: payload.Type, Name: payload.Name,
			DurationMs: payload.DurationMs, Environment: payload.Environment,
			TraceID: traceID,
			Spans:   legacy, Hotspots: sanitizeHotspots(payload.Hotspots),
			Tree:      SanitizeTree(payload.Tree, payload.DurationMs),
			ServiceID: svc.ID, RouteID: routeID, SpanID: rootID, Kind: kind,
		})
		if err != nil {
			log.Printf("record profile: %v", err)
			captureRaw(st, projectKey, "profile", body, false, "store: "+err.Error())
			http.Error(w, "store", http.StatusInternalServerError)
			return
		}
		captureRaw(st, projectKey, "profile", body, true, "")

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "trace_id": traceID})
	}
}

// errorPayload is the first-party error report the SvelteKit hook speaks.
type errorPayload struct {
	Message     string        `json:"message"`
	Stack       string        `json:"stack"`
	URL         string        `json:"url"`
	Route       string        `json:"route"`
	Environment string        `json:"environment"`
	Env         string        `json:"env"`
	Service     string        `json:"service"`
	Release     string        `json:"release"`
	SpanID      string        `json:"span_id"`
	TraceID     string        `json:"trace_id"`
	Spans       []spanPayload `json:"spans"`
	UserID      any           `json:"user_id"`
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

		var payload errorPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			captureRaw(st, projectKey, "error", body, false, "body: not a json object")
			http.Error(w, "invalid error report", http.StatusBadRequest)
			return
		}
		d := newDigest(st, projectKey, "error")
		if payload.Message == "" {
			d.issue("message", "is required")
		}
		spans := clipSpans(payload.Spans, d)
		if d.rejected() {
			captureRaw(st, projectKey, "error", body, false, d.reason())
			http.Error(w, "invalid error report", http.StatusBadRequest)
			return
		}

		fp := store.ForException("JSError", payload.Message, topStackLine(payload.Stack))
		ctx := map[string]any{
			"url":     payload.URL,
			"route":   payload.Route,
			"stack":   truncate(payload.Stack, 16<<10),
			"release": payload.Release,
			"user_id": payload.UserID,
		}
		if _, err := st.RecordError(projectKey, "JSError", payload.Message, fp, payload.Environment, "error", ctx); err != nil {
			log.Printf("record js error: %v", err)
			captureRaw(st, projectKey, "error", body, false, "store: "+err.Error())
			http.Error(w, "store", http.StatusInternalServerError)
			return
		}

		if len(spans) > 0 {
			svc := resolveService(st, projectKey, payload.Service, payload.Env, "error")
			traceID, origin := traceIdentity(r, payload.TraceID, svc.Trusted)
			if admitSpans(r, len(spans)+1) {
				rootName := strings.TrimSpace(payload.Route)
				if rootName == "" {
					rootName = "error"
				}
				batch := spanBatch{
					Project: projectKey, TraceID: traceID, Origin: origin, Service: svc,
					Root: spanPayload{
						Span:   store.Span{Name: rootName},
						SpanID: spanIDOrMint(payload.SpanID),
						Kind:   "browser",
						Status: "error",
						Route:  payload.Route,
					},
					Children: spans,
				}
				if _, err := batch.write(st, d); err != nil {
					log.Printf("record error spans: %v", err)
				}
			}
		}
		captureRaw(st, projectKey, "error", body, true, "")

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
