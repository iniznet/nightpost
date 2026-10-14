package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// M6.2 multi-tenancy. The hierarchy is ADDITIVE: projects gain an `org_id`
// while ingest keys stay project-grain (program convention 3), so no live
// ingest surface is re-keyed to fix tenancy. Roles are the whole M6.2
// authorization vocabulary; M6.3's Settings UI is where they get edited.

const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

// DefaultOrgID is the org every pre-M6.2 project and dashboard account is
// absorbed into, so an upgraded volume is never orphaned.
const DefaultOrgID = 1

// Org is one tenant: a named group of projects plus the accounts that may
// see them.
type Org struct {
	ID        int64
	Name      string
	Slug      string
	CreatedAt time.Time
	Projects  int64
	Members   int64
}

// ValidOrgRole reports whether role is in the M6.2 vocabulary.
func ValidOrgRole(role string) bool {
	return role == RoleOwner || role == RoleAdmin || role == RoleViewer
}

// EnsureDefaultOrg creates the default org if absent (idempotent).
func (s *Store) EnsureDefaultOrg() error {
	_, err := s.wdb.Exec(`INSERT OR IGNORE INTO orgs (id, name, slug, created_at)
		VALUES (?, 'Default', 'default', CAST(strftime('%s','now') AS INTEGER))`, DefaultOrgID)
	return err
}

// SeedMemberships makes every existing dashboard account an owner of the
// default org. Idempotent; runs at boot so upgraded volumes keep access.
func (s *Store) SeedMemberships() error {
	_, err := s.wdb.Exec(`INSERT OR IGNORE INTO org_members (org_id, user_id, role, created_at)
		SELECT ?, id, ?, CAST(strftime('%s','now') AS INTEGER) FROM users`, DefaultOrgID, RoleOwner)
	return err
}

// ListOrgs returns every org with its project and member counts.
func (s *Store) ListOrgs() ([]Org, error) {
	rows, err := s.rdb.Query(`SELECT o.id, o.name, o.slug, o.created_at,
			(SELECT COUNT(*) FROM projects p WHERE p.org_id = o.id),
			(SELECT COUNT(*) FROM org_members m WHERE m.org_id = o.id)
		FROM orgs o ORDER BY o.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Org
	for rows.Next() {
		var o Org
		var created int64
		if err := rows.Scan(&o.ID, &o.Name, &o.Slug, &created, &o.Projects, &o.Members); err != nil {
			return nil, err
		}
		if created > 0 {
			o.CreatedAt = time.Unix(created, 0)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OrgBySlug resolves an org by slug; an empty slug resolves the default org.
func (s *Store) OrgBySlug(slug string) (Org, error) {
	slug = strings.TrimSpace(strings.ToLower(slug))
	if slug == "" {
		slug = "default"
	}
	var o Org
	var created int64
	err := s.rdb.QueryRow(`SELECT id, name, slug, created_at FROM orgs WHERE slug = ?`, slug).
		Scan(&o.ID, &o.Name, &o.Slug, &created)
	if err == sql.ErrNoRows {
		return Org{}, fmt.Errorf("unknown org %q", slug)
	}
	if err != nil {
		return o, err
	}
	if created > 0 {
		o.CreatedAt = time.Unix(created, 0)
	}
	return o, nil
}

// CreateOrg adds an org; the slug is derived from the name when empty.
func (s *Store) CreateOrg(name, slug string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, fmt.Errorf("org name required")
	}
	if slug = slugify(slug); slug == "" {
		slug = slugify(name)
	}
	if slug == "" {
		return 0, fmt.Errorf("org slug required")
	}
	res, err := s.wdb.Exec(`INSERT INTO orgs (name, slug, created_at)
		VALUES (?, ?, CAST(strftime('%s','now') AS INTEGER))`, name, slug)
	if err != nil {
		return 0, fmt.Errorf("create org: %w", err)
	}
	return res.LastInsertId()
}

// AssignProjectToOrg moves a project under an org. Additive by construction:
// the project key and its ingest key are untouched, so a producer never has
// to be reconfigured to be tenanted.
func (s *Store) AssignProjectToOrg(projectKey string, orgID int64) error {
	if err := s.EnsureProject(projectKey); err != nil {
		return err
	}
	if _, err := s.wdb.Exec(`UPDATE projects SET org_id = ? WHERE key = ?`, orgID, projectKey); err != nil {
		return fmt.Errorf("assign project: %w", err)
	}
	return nil
}

// ProjectOrg returns the org owning a project, falling back to the default
// org for an unknown key so a brand-new producer is never orphaned.
func (s *Store) ProjectOrg(projectKey string) (int64, error) {
	var orgID int64
	err := s.rdb.QueryRow(`SELECT org_id FROM projects WHERE key = ?`, projectKey).Scan(&orgID)
	if err == sql.ErrNoRows {
		return DefaultOrgID, nil
	}
	return orgID, err
}

// ProjectsInOrg lists project keys for one org; orgID 0 means every org.
//
// The candidate set is every producer EVER SEEN — the projects table plus any
// event-only key — because the TCP Monolog leg and direct store writes bypass
// the ingest gate, so a log-only producer can legitimately have no projects
// row. A key with no row belongs to the default org rather than vanishing.
func (s *Store) ProjectsInOrg(orgID int64) ([]string, error) {
	rows, err := s.rdb.Query(`SELECT x.k FROM (
			SELECT key AS k FROM projects
			UNION SELECT project_key FROM issues
			UNION SELECT project_key FROM profiles
		) x
		LEFT JOIN projects p ON p.key = x.k
		WHERE (? <= 0 OR COALESCE(p.org_id, ?) = ?)
		ORDER BY x.k`, orgID, DefaultOrgID, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ProjectOrgMap returns every project row's org id, so the assignment UI
// renders in one query instead of one lookup per project.
func (s *Store) ProjectOrgMap() (map[string]int64, error) {
	rows, err := s.rdb.Query(`SELECT key, org_id FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var orgID int64
		if err := rows.Scan(&k, &orgID); err != nil {
			return nil, err
		}
		out[k] = orgID
	}
	return out, rows.Err()
}

// AddOrgMember upserts a membership; an unknown role is rejected rather than
// silently stored, because a typo would be an unrecoverable lockout.
func (s *Store) AddOrgMember(orgID, userID int64, role string) error {
	if !ValidOrgRole(role) {
		return fmt.Errorf("invalid org role %q", role)
	}
	_, err := s.wdb.Exec(`INSERT INTO org_members (org_id, user_id, role, created_at)
		VALUES (?, ?, ?, CAST(strftime('%s','now') AS INTEGER))
		ON CONFLICT(org_id, user_id) DO UPDATE SET role = excluded.role`, orgID, userID, role)
	return err
}

// UserRoleInOrg returns the role a user holds in an org ("" when none).
func (s *Store) UserRoleInOrg(userID, orgID int64) (string, error) {
	var role string
	err := s.rdb.QueryRow(`SELECT role FROM org_members WHERE user_id = ? AND org_id = ?`, userID, orgID).Scan(&role)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return role, err
}

// OrgMember is one row of an org's membership list.
type OrgMember struct {
	UserID int64
	Email  string
	Role   string
}

// OrgMembers lists an org's members with their emails.
func (s *Store) OrgMembers(orgID int64) ([]OrgMember, error) {
	rows, err := s.rdb.Query(`SELECT m.user_id, u.email, m.role
		FROM org_members m JOIN users u ON u.id = m.user_id
		WHERE m.org_id = ? ORDER BY u.email`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrgMember
	for rows.Next() {
		var m OrgMember
		if err := rows.Scan(&m.UserID, &m.Email, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// slugify lowercases and hyphenates a name into a URL-stable org slug.
func slugify(s string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
