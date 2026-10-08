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
	if err != nil || got != key1 {
		t.Fatalf("lookup mismatch: %q vs %q (err %v)", got, key1, err)
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
