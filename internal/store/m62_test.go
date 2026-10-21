package store

import "testing"

// TestPruneProfilesKeepsPerProjectWindow is the acceptance guard: the
// profile keep-window is PER PROJECT, so a noisy sibling can no longer evict
// another org's profiles and traces (a single global window of 5 would have
// left "quiet" with zero rows).
func TestPruneProfilesKeepsPerProjectWindow(t *testing.T) {
	st := newTestStore(t)
	seed := func(project string, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if _, err := st.RecordProfile(ProfileInput{
				ProjectKey: project, Type: ProfileTypeRequest, Name: "GET /x", DurationMs: 10,
			}); err != nil {
				t.Fatalf("seed %s: %v", project, err)
			}
		}
	}
	seed("loud", 700) // > the 500-row chunk, so the delete loop must iterate
	seed("quiet", 4)

	if err := st.PruneProfiles(5); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for _, c := range []struct {
		project string
		want    int64
	}{{"loud", 5}, {"quiet", 4}} {
		var n int64
		if err := st.rdb.QueryRow(`SELECT COUNT(*) FROM profiles WHERE project_key = ?`, c.project).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", c.project, err)
		}
		if n != c.want {
			t.Errorf("%s kept %d profiles, want %d (per-project window)", c.project, n, c.want)
		}
	}
}

// TestDefaultOrgAbsorbsProjectsAndAccounts covers the additive hierarchy: a
// pre-volume lands in the default org, and the M4 single admin becomes
// that org's owner.
func TestDefaultOrgAbsorbsProjectsAndAccounts(t *testing.T) {
	st := newTestStore(t)
	if err := st.EnsureProject("legacy"); err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	if orgID, err := st.ProjectOrg("legacy"); err != nil || orgID != DefaultOrgID {
		t.Errorf("ProjectOrg(legacy) = %d (%v), want the default org", orgID, err)
	}
	// An unknown producer must not be orphaned either.
	if orgID, err := st.ProjectOrg("never-seen"); err != nil || orgID != DefaultOrgID {
		t.Errorf("ProjectOrg(unknown) = %d (%v), want the default org", orgID, err)
	}

	orgs, err := st.ListOrgs()
	if err != nil || len(orgs) != 1 {
		t.Fatalf("orgs = %+v (%v), want exactly the default org", orgs, err)
	}
	if orgs[0].Slug != "default" || orgs[0].Projects != 1 {
		t.Errorf("default org = %+v", orgs[0])
	}

	pw, err := HashPassword("correct-horse")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if ok, err := st.CreateUserIfNone("ops@example.com", pw); err != nil || !ok {
		t.Fatalf("create user: ok=%v err=%v", ok, err)
	}
	members, err := st.OrgMembers(DefaultOrgID)
	if err != nil || len(members) != 1 {
		t.Fatalf("members = %+v (%v)", members, err)
	}
	if members[0].Email != "ops@example.com" || members[0].Role != RoleOwner {
		t.Errorf("seeded member = %+v, want the admin as owner", members[0])
	}
	if role, err := st.UserRoleInOrg(members[0].UserID, DefaultOrgID); err != nil || role != RoleOwner {
		t.Errorf("UserRoleInOrg = %q (%v), want owner", role, err)
	}
}

// TestOrgScopedProjectListing pins the tenancy projection: projects are
// assigned to an org without touching their keys (convention 3).
func TestOrgScopedProjectListing(t *testing.T) {
	st := newTestStore(t)
	for _, k := range []string{"alpha", "beta", "gamma"} {
		if err := st.EnsureProject(k); err != nil {
			t.Fatalf("ensure %s: %v", k, err)
		}
	}
	acme, err := st.CreateOrg("Acme Corp", "")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	o, err := st.OrgBySlug("acme-corp")
	if err != nil || o.ID != acme {
		t.Fatalf("OrgBySlug = %+v (%v)", o, err)
	}
	for _, k := range []string{"alpha", "beta"} {
		if err := st.AssignProjectToOrg(k, acme); err != nil {
			t.Fatalf("assign %s: %v", k, err)
		}
	}
	got, err := st.ProjectsInOrg(acme)
	if err != nil || len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("ProjectsInOrg(acme) = %v (%v)", got, err)
	}
	if got, _ := st.ProjectsInOrg(DefaultOrgID); len(got) != 1 || got[0] != "gamma" {
		t.Errorf("ProjectsInOrg(default) = %v, want [gamma]", got)
	}
	if got, _ := st.ProjectsInOrg(0); len(got) != 3 {
		t.Errorf("ProjectsInOrg(0) = %v, want every project", got)
	}
	// Keys are untouched by tenancy.
	if key, err := st.ProjectIngestKey("alpha"); err != nil || key != "" {
		t.Errorf("assigning an org changed the ingest key state: %q (%v)", key, err)
	}
}

func TestAddOrgMemberRejectsUnknownRole(t *testing.T) {
	st := newTestStore(t)
	if err := st.AddOrgMember(DefaultOrgID, 1, "superuser"); err == nil {
		t.Error("an unknown org role must be rejected, not silently stored")
	}
	if err := st.AddOrgMember(DefaultOrgID, 1, RoleViewer); err != nil {
		t.Fatalf("valid role rejected: %v", err)
	}
	if role, err := st.UserRoleInOrg(1, DefaultOrgID); err != nil || role != RoleViewer {
		t.Errorf("role = %q (%v), want viewer", role, err)
	}
	// Upsert, not duplicate rows.
	if err := st.AddOrgMember(DefaultOrgID, 1, RoleAdmin); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if role, _ := st.UserRoleInOrg(1, DefaultOrgID); role != RoleAdmin {
		t.Errorf("role = %q after upsert, want admin", role)
	}
}
