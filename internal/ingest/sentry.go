package ingest

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/iniznet/nightpost/internal/store"
)

const maxEnvelopeBytes = 10 << 20 // 10 MiB

// sentryEnvelopeHeader is the event header line. v2 adds the optional producer
// identity, which the SDK sends once for the whole envelope.
type sentryEnvelopeHeader struct {
	EventID     string `json:"event_id"`
	Environment string `json:"environment"`
	Service     string `json:"service"`
	Env         string `json:"env"`
	Release     string `json:"release"`
}

// SentryHandler accepts a Sentry envelope POST — the wire protocol
// sentry/sentry-laravel speaks when its DSN points at this server. Body is
// newline-delimited JSON: one event header line, then repeating
// (item header, payload) line pairs.
//
// v2 adds three OPTIONAL things to the same envelope: the producer identity
// ("service", "env") in the header, "spans" inside an event, and a "session"
// item carrying one session outcome. A v1 envelope is unchanged.
func SentryHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectKey := r.PathValue("project")
		if projectKey == "" {
			http.Error(w, "missing project", http.StatusBadRequest)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxEnvelopeBytes))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		lines := strings.Split(strings.TrimSpace(string(body)), "\n")
		if len(lines) < 2 {
			captureRaw(st, projectKey, "sentry", body, false, "body: not an envelope")
			http.Error(w, "invalid envelope", http.StatusBadRequest)
			return
		}

		var header sentryEnvelopeHeader
		if err := json.Unmarshal([]byte(lines[0]), &header); err != nil || header.EventID == "" {
			captureRaw(st, projectKey, "sentry", body, false, "header: not a sentry event header")
			http.Error(w, "invalid envelope header", http.StatusBadRequest)
			return
		}

		recorded := 0
		sessions := 0
		for i := 1; i+1 < len(lines); i += 2 {
			var itemHeader struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(lines[i]), &itemHeader); err != nil {
				continue
			}
			switch itemHeader.Type {
			case "event":
				var event map[string]any
				if err := json.Unmarshal([]byte(lines[i+1]), &event); err != nil {
					continue
				}
				recorded += recordEvent(st, projectKey, header.Environment, event)
				recordSentrySpans(r, st, projectKey, header, event)
			case "session":
				if recordSentrySession(st, projectKey, header, lines[i+1]) {
					sessions++
				}
			}
		}
		captureRaw(st, projectKey, "sentry", body, true, "")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"recorded": recorded, "sessions": sessions})
	}
}

// sentryService resolves the producer identity for one event: the envelope
// header first (the SDK sends it once), then the event body (a hand-built
// payload names it there).
func sentryService(header sentryEnvelopeHeader, event map[string]any) (service, env string) {
	service, env = header.Service, header.Env
	if service == "" {
		service, _ = event["service"].(string)
	}
	if env == "" {
		env, _ = event["env"].(string)
	}
	if env == "" {
		env = header.Environment
	}
	return service, env
}

// eventSpans decodes the optional v2 span array off an already-decoded event.
// The round trip is deliberate: the span fields live on ONE struct that both
// this file and the profile handler use, so the two cannot drift apart.
func eventSpans(event map[string]any) []spanPayload {
	raw, ok := event["spans"].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	buf, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var spans []spanPayload
	if err := json.Unmarshal(buf, &spans); err != nil {
		return nil
	}
	return spans
}

// recordSentrySpans writes an event's span batch into its trace. A span batch
// that does not fit its budget is dropped: the issue in the same envelope is
// the primary record and must not be lost to a secondary ceiling.
func recordSentrySpans(r *http.Request, st *store.Store, projectKey string, header sentryEnvelopeHeader, event map[string]any) {
	spans := eventSpans(event)
	if len(spans) == 0 {
		return
	}
	d := newDigest(st, projectKey, "sentry")
	spans = clipSpans(spans, d)
	if len(spans) == 0 {
		return
	}
	service, env := sentryService(header, event)
	svc := resolveService(st, projectKey, service, env, "sentry")
	traceID, origin := traceIdentity(r, "", svc.Trusted)
	if !admitSpans(r, len(spans)+1) {
		return
	}
	rootName := "sentry event"
	if tx, ok := event["transaction"].(string); ok && strings.TrimSpace(tx) != "" {
		rootName = strings.TrimSpace(tx)
	}
	batch := spanBatch{
		Project: projectKey, TraceID: traceID, Origin: origin, Service: svc,
		Root: spanPayload{
			Span:   store.Span{Name: rootName},
			SpanID: mintSpanID(),
			Kind:   "server",
			Status: "ok",
		},
		Children: spans,
	}
	if _, err := batch.write(st, d); err != nil {
		log.Printf("record sentry spans: %v", err)
	}
}

// recordSentrySession stores one session item. Sentry says "exited" where this
// server says "ok"; the mapping lives here so a producer may send either word
// and the crash-free read stays one ratio. An unknown status is skipped rather
// than guessed at — miscounting it as abnormal would report phantom crashes.
func recordSentrySession(st *store.Store, projectKey string, header sentryEnvelopeHeader, line string) bool {
	var p struct {
		SessionID  string `json:"session_id"`
		SID        string `json:"sid"`
		Status     string `json:"status"`
		Release    string `json:"release"`
		Route      string `json:"route"`
		DurationMs int64  `json:"duration_ms"`
		Service    string `json:"service"`
		Env        string `json:"env"`
	}
	if err := json.Unmarshal([]byte(line), &p); err != nil {
		return false
	}
	id := strings.TrimSpace(p.SessionID)
	if id == "" {
		id = strings.TrimSpace(p.SID)
	}
	if id == "" || len(id) > maxSessionID {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(p.Status))
	switch status {
	case sessionStatusOK, sessionStatusCrashed, sessionStatusAbnormal:
	case "exited":
		status = sessionStatusOK
	case "errored":
		status = sessionStatusAbnormal
	default:
		log.Printf("sentry session: unknown status %q skipped", p.Status)
		return false
	}
	release := strings.TrimSpace(p.Release)
	if release == "" {
		release = strings.TrimSpace(header.Release)
	}
	service := p.Service
	if service == "" {
		service = header.Service
	}
	env := p.Env
	if env == "" {
		env = header.Environment
	}
	svc := resolveService(st, projectKey, service, env, "sentry session")
	d := newDigest(st, projectKey, "sentry session")
	routeID := upsertRoute(st, svc.ID, truncate(strings.TrimSpace(p.Route), maxSessionRoute), "session", d)
	if err := st.UpsertSession(store.Session{
		ProjectKey: projectKey, Release: truncate(release, maxSessionRel), SessionID: id,
		Status: status, RouteID: routeID, DurationMs: p.DurationMs,
	}); err != nil {
		log.Printf("record sentry session: %v", err)
		return false
	}
	return true
}

func recordEvent(st *store.Store, projectKey, environment string, event map[string]any) int {
	level, _ := event["level"].(string)
	if level == "" {
		level = "error"
	}

	recorded := 0
	if excValues, ok := event["exception"].(map[string]any); ok {
		if values, ok := excValues["values"].([]any); ok {
			for _, v := range values {
				exc, ok := v.(map[string]any)
				if !ok {
					continue
				}
				class, _ := exc["type"].(string)
				message, _ := exc["value"].(string)
				if class == "" && message == "" {
					continue
				}
				if class == "" {
					class = "Exception"
				}
				if message == "" {
					message = "unknown exception"
				}
				fp := store.ForException(class, message, topFrame(exc))
				if _, err := st.RecordError(projectKey, class, message, fp, environment, level, event); err != nil {
					log.Printf("record error: %v", err)
					continue
				}
				recorded++
			}
		}
	}

	if recorded == 0 {
		message, _ := event["message"].(string)
		if message == "" {
			message = "unknown event"
		}
		fp := store.ForException("Message", message, "")
		if _, err := st.RecordError(projectKey, "Message", message, fp, environment, level, event); err != nil {
			log.Printf("record message event: %v", err)
			return 0
		}
		recorded = 1
	}

	return recorded
}

// topFrame picks the deepest in-app frame for grouping; Sentry orders frames
// oldest-first, so the last in_app frame is the one that threw.
func topFrame(exc map[string]any) string {
	trace, ok := exc["stacktrace"].(map[string]any)
	if !ok {
		return ""
	}
	frames, ok := trace["frames"].([]any)
	if !ok || len(frames) == 0 {
		return ""
	}
	for i := len(frames) - 1; i >= 0; i-- {
		frame, ok := frames[i].(map[string]any)
		if !ok {
			continue
		}
		class, _ := frame["module"].(string)
		if class == "" {
			class, _ = frame["filename"].(string)
		}
		fn, _ := frame["function"].(string)
		if class != "" || fn != "" {
			return class + ":" + fn
		}
	}
	return ""
}
