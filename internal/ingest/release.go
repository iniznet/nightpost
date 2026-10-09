package ingest

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

// releasePayload is the additive deploy-marker wire contract (#158).
type releasePayload struct {
	Version     string `json:"version"`
	Environment string `json:"environment"`
}

// ReleaseHandler records one deploy marker per POST. Producers: CI webhook,
// or a one-shot boot publisher guarded by a per-deploy lock.
func ReleaseHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project := r.PathValue("project")
		if project == "" {
			project = "default"
		}
		r.Body = http.MaxBytesReader(w, r.Body, 2048)
		var p releasePayload
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&p); err != nil && err != io.EOF {
			http.Error(w, "malformed json", http.StatusBadRequest)
			return
		}
		p.Version = strings.TrimSpace(p.Version)
		if p.Version == "" || len(p.Version) > 128 {
			http.Error(w, "version missing or oversized", http.StatusUnprocessableEntity)
			return
		}
		if len(p.Environment) > 64 {
			http.Error(w, "environment oversized", http.StatusUnprocessableEntity)
			return
		}
		if err := st.RecordRelease(project, p.Version, p.Environment); err != nil {
			http.Error(w, "record release: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
}
