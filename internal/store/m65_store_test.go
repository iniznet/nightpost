package store

import (
	"path/filepath"
	"testing"
)

func TestNormalizeCORSOrigins(t *testing.T) {
	cases := []struct {
		in   string
		want string
		bad  bool
	}{
		{"", "", false},
		{"https://a.example.com/", "https://a.example.com", false},
		{" https://a.example.com , https://b.example.com ", "https://a.example.com,https://b.example.com", false},
		{"http://localhost:5173", "http://localhost:5173", false},
		{"*", "", true},
		{"https://a.example.com/path", "", true},
		{"ftp://a.example.com", "", true},
		{"a.example.com", "", true},
		{"https://a.example.com?x=1", "", true},
	}
	for _, c := range cases {
		got, err := NormalizeCORSOrigins(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("%q: expected an error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q => %q, want %q", c.in, got, c.want)
		}
	}
}

func TestProjectCORSOriginRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	if err := st.SetProjectCORSOrigin("alpha", "https://alpha.example.com"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, _ := st.ProjectCORSOrigin("alpha"); got != "https://alpha.example.com" {
		t.Errorf("origin = %q", got)
	}

	projects, err := st.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, p := range projects {
		if p.Key == "alpha" {
			found = true
			if p.CORSOrigin != "https://alpha.example.com" {
				t.Errorf("ListProjects dropped the origin: %q", p.CORSOrigin)
			}
		}
	}
	if !found {
		t.Error("alpha missing from ListProjects")
	}

	// Empty clears back to the install default.
	if err := st.SetProjectCORSOrigin("alpha", ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, _ := st.ProjectCORSOrigin("alpha"); got != "" {
		t.Errorf("after clear = %q, want empty", got)
	}
	// An unknown project inherits the default too.
	if got, _ := st.ProjectCORSOrigin("nobody"); got != "" {
		t.Errorf("unknown project = %q, want empty", got)
	}
}
