package ingest

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/iniznet/nightpost/internal/store"
)

// queryPayload is the additive slow-query wire contract (#157). Producers
// send only what DB::listen yields with bindings already redacted.
type queryPayload struct {
	SQL         string  `json:"sql"`
	TimeMs      float64 `json:"time_ms"`
	Environment string  `json:"environment"`
	Connection  string  `json:"connection"`
}

const maxQuerySQL = 8192

// QueryHandler records one slow-query event per POST. It is a NEW route:
// the compat-locked ingest surface keeps its shape (#145).
func QueryHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project := r.PathValue("project")
		if project == "" {
			project = "default"
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
		var p queryPayload
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&p); err != nil && err != io.EOF {
			http.Error(w, "malformed json", http.StatusBadRequest)
			return
		}
		p.SQL = strings.TrimSpace(p.SQL)
		if p.SQL == "" || len(p.SQL) > maxQuerySQL {
			http.Error(w, "sql missing or oversized", http.StatusUnprocessableEntity)
			return
		}
		if p.TimeMs <= 0 || p.TimeMs > 86_400_000 {
			http.Error(w, "time_ms out of range", http.StatusUnprocessableEntity)
			return
		}
		conn := p.Connection
		if len(conn) > 64 {
			http.Error(w, "connection oversized", http.StatusUnprocessableEntity)
			return
		}
		if _, err := st.RecordQuery(project, p.SQL, p.TimeMs, conn, p.Environment); err != nil {
			http.Error(w, "store failure", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
}
