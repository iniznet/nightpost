package store

import "testing"

func TestProjectProvisionAndRotation(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.RecordError("alpha", "Ex", "boom", "fp-a", "production", "error", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordProfile(ProfileInput{ProjectKey: "beta", Name: "GET /x", DurationMs: 5, Environment: "production"}); err != nil {
		t.Fatal(err)
	}
	if err := st.ProvisionProjects(); err != nil {
		t.Fatal(err)
	}
	if err := st.ProvisionProjects(); err != nil {
		t.Fatalf("second pass must be idempotent: %v", err)
	}
	projects, err := st.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 || projects[0].Key != "alpha" || projects[1].Key != "beta" {
		t.Fatalf("expected alpha+beta backfilled, got %+v", projects)
	}
	for _, p := range projects {
		if p.HasKey() {
			t.Fatalf("backfilled projects must start unsigned, %q has key", p.Key)
		}
	}

	key1, err := st.RotateIngestKey("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(key1) != 48 {
		t.Fatalf("expected 48 hex chars, got %d", len(key1))
	}
	got, err := st.ProjectIngestKey("alpha")
	if err != nil || got != HashIngestKey(key1) {
		t.Fatalf("store must hold the sha256 of the minted secret: %q (err %v)", got, err)
	}
	if got == key1 || len(got) != 64 {
		t.Fatalf("stored form must be a 64-hex hash, not the secret (%d)", len(got))
	}
	key2, err := st.RotateIngestKey("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if key2 == key1 {
		t.Fatal("rotation must produce a new secret")
	}
	if n, err := st.UnsignedProjectCount(); err != nil || n != 1 {
		t.Fatalf("expected 1 unsigned (beta), got %d (err %v)", n, err)
	}
	if err := st.EnsureProject("gamma"); err != nil {
		t.Fatal(err)
	}
	if k, err := st.ProjectIngestKey("gamma"); err != nil || k != "" {
		t.Fatalf("EnsureProject must admit unsigned, got %q (err %v)", k, err)
	}
	if k, err := st.ProjectIngestKey("never-seen"); err != nil || k != "" {
		t.Fatalf("unknown project must report unsigned without error, got %q (err %v)", k, err)
	}
	projects, _ = st.ListProjects()
	if projects[0].RotatedAt.IsZero() {
		t.Fatal("rotation must stamp rotated_at")
	}
}

func TestRevokeIngestKeyFallsBackToUnsigned(t *testing.T) {
	st := newTestStore(t)
	if err := st.EnsureProject("alpha"); err != nil {
		t.Fatal(err)
	}
	secret, err := st.RotateIngestKey("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := st.ProjectIngestKey("alpha"); k != HashIngestKey(secret) {
		t.Fatal("rotation must store the hash")
	}
	if err := st.RevokeIngestKey("alpha"); err != nil {
		t.Fatal(err)
	}
	if k, _ := st.ProjectIngestKey("alpha"); k != "" {
		t.Fatalf("revoke must clear the key, got %q", k)
	}
	rows, err := st.ListProjects()
	if err != nil || len(rows) != 1 || rows[0].HasKey() {
		t.Fatalf("revoked project must render unsigned: %+v err=%v", rows, err)
	}
	if err := st.RevokeIngestKey("never-seen"); err != nil {
		t.Fatalf("revoke of an unknown project must be a no-op, got %v", err)
	}
}
