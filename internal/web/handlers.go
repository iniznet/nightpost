package web

import (
	"embed"
	"html/template"
	"log"
	"net/http"
	"strconv"

	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

func Register(mux *http.ServeMux, st *store.Store) {
	indexTmpl := template.Must(template.ParseFS(templateFS, "templates/index.html"))
	detailTmpl := template.Must(template.ParseFS(templateFS, "templates/detail.html"))
	profilesTmpl := template.Must(template.ParseFS(templateFS, "templates/profiles.html"))
	profileTmpl := template.Must(template.ParseFS(templateFS, "templates/profile.html"))
	slowestTmpl := template.Must(template.ParseFS(templateFS, "templates/slowest.html"))

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		project := r.URL.Query().Get("project")
		issues, err := st.Issues(project, 100)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := indexTmpl.Execute(w, map[string]any{"Issues": issues, "Project": project}); err != nil {
			log.Printf("render index: %v", err)
		}
	})

	mux.HandleFunc("GET /issues/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		issue, err := st.Issue(id)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		occurrences, err := st.Occurrences(id, 20)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := detailTmpl.Execute(w, map[string]any{"Issue": issue, "Occurrences": occurrences}); err != nil {
			log.Printf("render detail: %v", err)
		}
	})

	mux.HandleFunc("POST /issues/{id}/resolve", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		if err := st.SetResolved(id, true); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	mux.HandleFunc("POST /issues/{id}/reopen", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		if err := st.SetResolved(id, false); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	mux.HandleFunc("GET /profiles", func(w http.ResponseWriter, r *http.Request) {
		project := r.URL.Query().Get("project")
		profiles, err := st.Profiles(project, 50)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := profilesTmpl.Execute(w, map[string]any{"Profiles": profiles, "Project": project}); err != nil {
			log.Printf("render profiles: %v", err)
		}
	})

	mux.HandleFunc("GET /profiles/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		profile, err := st.Profile(id)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err := profileTmpl.Execute(w, map[string]any{"Profile": profile}); err != nil {
			log.Printf("render profile: %v", err)
		}
	})

	mux.HandleFunc("GET /slowest", func(w http.ResponseWriter, r *http.Request) {
		project := r.URL.Query().Get("project")
		rows, err := st.SlowestCode(project, 500)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := slowestTmpl.Execute(w, map[string]any{"Rows": rows, "Project": project}); err != nil {
			log.Printf("render slowest: %v", err)
		}
	})
}