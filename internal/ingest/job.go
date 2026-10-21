package ingest

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/iniznet/nightpost/internal/store"
)

// jobPayload is the additive job/command telemetry wire contract .
// Name is the queue job class or the artisan command signature; nothing
// that can carry user data crosses this boundary.
type jobPayload struct {
	Name    string  `json:"name"`
	Kind    string  `json:"kind"`
	Queue   string  `json:"queue"`
	Seconds float64 `json:"seconds"`
	OK      *bool   `json:"ok"`
	Count   int64   `json:"count"`
	// Trace is the per-run trace id of the last profiled leg ; it is
	// random hex minted by the producer, never user-derived, so it carries
	// no reader data across this boundary.
	Trace   string  `json:"trace"`
}

const maxJobName = 191

// JobHandler records one job/command completion per POST. New route; the
// compat-locked ingest surface keeps its shape .
func JobHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project := r.PathValue("project")
		if project == "" {
			project = "default"
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		var p jobPayload
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&p); err != nil && err != io.EOF {
			http.Error(w, "malformed json", http.StatusBadRequest)
			return
		}
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			http.Error(w, "name missing", http.StatusUnprocessableEntity)
			return
		}
		if len(p.Name) > maxJobName {
			p.Name = p.Name[:maxJobName]
		}
		kind := strings.ToLower(strings.TrimSpace(p.Kind))
		if kind == "" {
			kind = "command"
			if strings.TrimSpace(p.Queue) != "" {
				kind = "queue"
			}
		}
		if kind != "queue" && kind != "command" {
			http.Error(w, "kind must be queue or command", http.StatusUnprocessableEntity)
			return
		}
		if len(p.Queue) > 64 {
			http.Error(w, "queue oversized", http.StatusUnprocessableEntity)
			return
		}
		ok := true
		if p.OK != nil {
			ok = *p.OK
		}
		count := p.Count
		if count < 1 {
			count = 1
		}
		if count > 100000 {
			count = 100000
		}
		if err := st.RecordJob(project, kind, p.Name, p.Queue, p.Seconds, ok, count, p.Trace); err != nil {
			http.Error(w, "record job: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
}
