package ingest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/iniznet/nightpost/internal/store"
)

// releasePayload is the additive deploy-marker wire contract .
type releasePayload struct {
	Version     string `json:"version"`
	Environment string `json:"environment"`
	// Service and Env are the v2 producer identity; both optional, so a v1 CI
	// webhook keeps posting the same body.
	Service string `json:"service"`
	Env     string `json:"env"`
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
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "malformed json", http.StatusBadRequest)
			return
		}
		var p releasePayload
		dec := json.NewDecoder(bytes.NewReader(body))
		if err := dec.Decode(&p); err != nil && err != io.EOF {
			captureRaw(st, project, "release", body, false, "body: malformed json")
			http.Error(w, "malformed json", http.StatusBadRequest)
			return
		}
		d := newDigest(st, project, "release")
		p.Version = strings.TrimSpace(p.Version)
		if p.Version == "" || len(p.Version) > 128 {
			d.issue("version", "version missing or oversized")
		}
		if len(p.Environment) > 64 {
			d.issue("environment", "environment oversized")
		}
		if d.rejected() {
			captureRaw(st, project, "release", body, false, d.reason())
			http.Error(w, d.reason(), http.StatusUnprocessableEntity)
			return
		}
		// releases carries no service column in v2: the identity is admitted so a
		// deploy records which producer shipped, without a schema column this
		// increment does not own.
		resolveService(st, project, p.Service, p.Env, "release")
		if err := st.RecordRelease(project, p.Version, p.Environment); err != nil {
			captureRaw(st, project, "release", body, false, "store: "+err.Error())
			http.Error(w, "record release: "+err.Error(), http.StatusInternalServerError)
			return
		}
		captureRaw(st, project, "release", body, true, "")
		w.WriteHeader(http.StatusAccepted)
	}
}
