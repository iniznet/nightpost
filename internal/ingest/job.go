package ingest

import (
	"bytes"
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
	// Service and Env are the v2 producer identity. They are optional: a v1
	// producer keeps working and its run is labelled with the project key.
	Service string `json:"service"`
	Env     string `json:"env"`
	// ScheduleCron and NextExpectedAt carry the promise a scheduled run makes.
	// The producer states the next expected fire time instead of the server
	// parsing cron: a parser is a dependency the single-binary rule cannot
	// pay for, and every supported producer already knows its own schedule.
	ScheduleCron   string `json:"schedule_cron"`
	NextExpectedAt int64  `json:"next_expected_at"`
	GraceSeconds   int64  `json:"grace_seconds"`
	// Trace is the per-run trace id of the last profiled leg ; it is
	// random hex minted by the producer, never user-derived, so it carries
	// no reader data across this boundary.
	Trace string `json:"trace"`
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
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "malformed json", http.StatusBadRequest)
			return
		}
		var p jobPayload
		dec := json.NewDecoder(bytes.NewReader(body))
		if err := dec.Decode(&p); err != nil && err != io.EOF {
			captureRaw(st, project, "job", body, false, "body: malformed json")
			http.Error(w, "malformed json", http.StatusBadRequest)
			return
		}
		d := newDigest(st, project, "job")
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			d.issue("name", "name missing")
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
			d.issue("kind", "kind must be queue or command")
		}
		if len(p.Queue) > 64 {
			d.issue("queue", "queue oversized")
		}
		if d.rejected() {
			captureRaw(st, project, "job", body, false, d.reason())
			http.Error(w, d.reason(), http.StatusUnprocessableEntity)
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
		// job_stats has no service column in v2, so the identity is admitted here
		// rather than attached to the row: the services list still learns the
		// producer, and the run can never disagree with it.
		resolveService(st, project, p.Service, p.Env, "job")
		if err := st.RecordJob(project, kind, p.Name, p.Queue, p.Seconds, ok, count, p.Trace); err != nil {
			captureRaw(st, project, "job", body, false, "store: "+err.Error())
			http.Error(w, "record job: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if p.NextExpectedAt > 0 {
			if err := st.SetJobSchedule(project, kind, p.Name, strings.TrimSpace(p.ScheduleCron), p.NextExpectedAt, p.GraceSeconds); err != nil {
				d.issue("next_expected_at", err.Error())
			}
		}
		captureRaw(st, project, "job", body, true, "")
		w.WriteHeader(http.StatusAccepted)
	}
}
