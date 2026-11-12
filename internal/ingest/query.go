package ingest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/iniznet/nightpost/internal/store"
)

// queryPayload is the additive slow-query wire contract . Producers
// send only what DB::listen yields with bindings already redacted.
type queryPayload struct {
	SQL         string  `json:"sql"`
	TimeMs      float64 `json:"time_ms"`
	Environment string  `json:"environment"`
	Connection  string  `json:"connection"`
	// Service and Env are the v2 producer identity; both optional.
	Service string `json:"service"`
	Env     string `json:"env"`
}

const maxQuerySQL = 8192

// QueryHandler records one slow-query event per POST. It is a NEW route:
// the compat-locked ingest surface keeps its shape .
func QueryHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project := r.PathValue("project")
		if project == "" {
			project = "default"
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "malformed json", http.StatusBadRequest)
			return
		}
		var p queryPayload
		dec := json.NewDecoder(bytes.NewReader(body))
		if err := dec.Decode(&p); err != nil && err != io.EOF {
			captureRaw(st, project, "query", body, false, "body: malformed json")
			http.Error(w, "malformed json", http.StatusBadRequest)
			return
		}
		d := newDigest(st, project, "query")
		p.SQL = strings.TrimSpace(p.SQL)
		if p.SQL == "" || len(p.SQL) > maxQuerySQL {
			d.issue("sql", "sql missing or oversized")
		}
		if p.TimeMs <= 0 || p.TimeMs > 86_400_000 {
			d.issue("time_ms", "time_ms out of range")
		}
		if len(p.Connection) > 64 {
			d.issue("connection", "connection oversized")
		}
		if d.rejected() {
			captureRaw(st, project, "query", body, false, d.reason())
			http.Error(w, d.reason(), http.StatusUnprocessableEntity)
			return
		}
		// query_stats has no service column in v2; the identity is admitted so the
		// producer is known, and the aggregate keeps its v1 shape.
		resolveService(st, project, p.Service, p.Env, "query")
		if _, err := st.RecordQuery(project, p.SQL, p.TimeMs, p.Connection, p.Environment); err != nil {
			captureRaw(st, project, "query", body, false, "store: "+err.Error())
			http.Error(w, "store failure", http.StatusInternalServerError)
			return
		}
		captureRaw(st, project, "query", body, true, "")
		w.WriteHeader(http.StatusAccepted)
	}
}
