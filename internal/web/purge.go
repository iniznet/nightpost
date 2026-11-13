package web

import (
	"net/http"
	"strings"
)

// actor is the signed-in operator, or a marker when auth is off. The audit line
// names a person, never an IP: a shared office IP identifies nobody.
func (s *server) actor(r *http.Request) string {
	if s.auth != nil {
		if email, ok := s.auth.sessionUser(r); ok {
			return email
		}
	}
	return "unauthenticated"
}

// handleProjectPurge clears every event of one project and keeps the project,
// its key and its org membership, so the producer keeps reporting into an empty
// project instead of a 401.
func (s *server) handleProjectPurge(w http.ResponseWriter, r *http.Request) {
	s.requestPurge(w, r, "clear")
}

// handleProjectDelete clears the data first, then removes the project row: the
// key stops authenticating and the project leaves every picker.
func (s *server) handleProjectDelete(w http.ResponseWriter, r *http.Request) {
	s.requestPurge(w, r, "delete")
}

// requestPurge validates the typed confirmation before anything is queued. The
// operator types the project key back; a mismatch deletes nothing and leaves a
// line saying so, because a refused destructive request is part of the record.
func (s *server) requestPurge(w http.ResponseWriter, r *http.Request, action string) {
	project := strings.TrimSpace(r.PostFormValue("project"))
	confirm := strings.TrimSpace(r.PostFormValue("confirm"))
	actor := s.actor(r)
	if project == "" || confirm != project {
		_ = s.st.RecordAdminAction(actor, "purge."+action, project, "refused: the typed confirmation did not match")
		s.renderOrgs(w, r, map[string]any{"Error": "The typed text did not match the project key. Nothing was deleted."})
		return
	}
	if err := s.st.RequestPurge(project, action, actor); err != nil {
		s.renderOrgs(w, r, map[string]any{"Error": err.Error()})
		return
	}
	s.renderOrgs(w, r, map[string]any{"Notice": "Queued. The retention ticker drains it in bounded chunks; this page shows the progress."})
}

// handleOrgDelete refuses while the org still holds projects. Deleting those
// first IS the cascade — done explicitly, one project at a time, so a single
// click can never remove an org and everything under it.
func (s *server) handleOrgDelete(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PostFormValue("org"))
	confirm := strings.TrimSpace(r.PostFormValue("confirm"))
	actor := s.actor(r)
	if slug == "" || confirm != slug {
		_ = s.st.RecordAdminAction(actor, "org.delete", slug, "refused: the typed confirmation did not match")
		s.renderOrgs(w, r, map[string]any{"Error": "The typed text did not match the org slug. Nothing was deleted."})
		return
	}
	org, err := s.st.OrgBySlug(slug)
	if err != nil {
		s.renderOrgs(w, r, map[string]any{"Error": "No org with that slug."})
		return
	}
	projects, err := s.st.ProjectsInOrg(org.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(projects) > 0 {
		_ = s.st.RecordAdminAction(actor, "org.delete", slug, "refused: the org still holds projects")
		s.renderOrgs(w, r, map[string]any{"Error": "That org still holds projects. Delete or clear each project first; there is no one-click cascade on purpose."})
		return
	}
	if err := s.st.DeleteOrg(org.ID); err != nil {
		s.renderOrgs(w, r, map[string]any{"Error": err.Error()})
		return
	}
	_ = s.st.RecordAdminAction(actor, "org.delete", slug, "completed")
	s.renderOrgs(w, r, map[string]any{"Notice": "Org deleted."})
}
