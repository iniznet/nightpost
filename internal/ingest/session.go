package ingest

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/iniznet/nightpost/internal/store"
)

// sessionPayload is the v2 session-outcome beacon: one reader session, its
// release, and how it ended. It carries no identifier that can be traced back
// to a reader — session_id is the producer's own random key.
type sessionPayload struct {
	SessionID  string `json:"session_id"`
	Release    string `json:"release"`
	Status     string `json:"status"`
	Route      string `json:"route"`
	DurationMs int64  `json:"duration_ms"`
	Service    string `json:"service"`
	Env        string `json:"env"`
}

// Session outcome vocabulary. "ok" is the clean end, "crashed" the producer
// saw the process die, "abnormal" anything else that did not end by itself.
const (
	sessionStatusOK       = "ok"
	sessionStatusCrashed  = "crashed"
	sessionStatusAbnormal = "abnormal"

	maxSessionID    = 128
	maxSessionRel   = 128
	maxSessionRoute = 255
)

// SessionHandler records one session outcome per POST.
//
// Crash-free is NOT computed here: it is a ratio over every session of a
// release, and doing it on the write path would turn an ingest POST into a
// scan. The releases page reads it through store.CrashFree.
func SessionHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project := r.PathValue("project")
		if project == "" {
			project = "default"
		}
		r.Body = http.MaxBytesReader(w, r.Body, 2048)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "malformed json", http.StatusBadRequest)
			return
		}
		var p sessionPayload
		dec := json.NewDecoder(bytes.NewReader(body))
		if err := dec.Decode(&p); err != nil && err != io.EOF {
			captureRaw(st, project, "session", body, false, "body: malformed json")
			http.Error(w, "malformed json", http.StatusBadRequest)
			return
		}
		d := newDigest(st, project, "session")
		p.SessionID = strings.TrimSpace(p.SessionID)
		p.Status = strings.ToLower(strings.TrimSpace(p.Status))
		p.Release = strings.TrimSpace(p.Release)
		if p.SessionID == "" {
			d.issue("session_id", "session_id missing")
		}
		if len(p.SessionID) > maxSessionID {
			d.issue("session_id", "session_id oversized")
		}
		switch p.Status {
		case sessionStatusOK, sessionStatusCrashed, sessionStatusAbnormal:
		default:
			d.issue("status", "status must be ok, crashed or abnormal")
		}
		if len(p.Release) > maxSessionRel {
			d.issue("release", "release oversized")
		}
		if len(p.Route) > maxSessionRoute {
			d.issue("route", "route oversized")
		}
		if p.DurationMs < 0 {
			d.issue("duration_ms", "duration_ms must not be negative")
		}
		if d.rejected() {
			captureRaw(st, project, "session", body, false, d.reason())
			http.Error(w, d.reason(), http.StatusUnprocessableEntity)
			return
		}

		// sessions has no service column in v2; the identity is admitted so the
		// route key below always belongs to a real service row.
		svc := resolveService(st, project, p.Service, p.Env, "session")
		routeID := upsertRoute(st, svc.ID, p.Route, "session", d)
		if err := st.UpsertSession(store.Session{
			ProjectKey: project, Release: p.Release, SessionID: p.SessionID,
			Status: p.Status, RouteID: routeID, DurationMs: p.DurationMs,
		}); err != nil {
			log.Printf("record session: %v", err)
			captureRaw(st, project, "session", body, false, "store: "+err.Error())
			http.Error(w, "record session", http.StatusInternalServerError)
			return
		}
		captureRaw(st, project, "session", body, true, "")
		w.WriteHeader(http.StatusNoContent)
	}
}
