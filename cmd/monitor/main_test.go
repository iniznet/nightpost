package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestResolveSessionSecretPersistsAcrossRestarts is the M6.5 installer
// contract: the old random-per-boot default silently logged every admin out on
// each deploy.
func TestResolveSessionSecretPersistsAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "session.key")

	first, err := resolveSessionSecret("", path)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("generated an empty secret")
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("secret file not written: %v", err)
	}
	// Windows reports 0666 for every ordinary file (Go emulates permissions
	// there), so the mode assertion only means something on POSIX.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("secret file mode = %o, want 600", perm)
		}
	}

	second, err := resolveSessionSecret("", path)
	if err != nil {
		t.Fatalf("second boot: %v", err)
	}
	if string(second) != string(first) {
		t.Error("a restart must reuse the persisted secret, not mint a new one")
	}
}

func TestResolveSessionSecretFlagWinsThenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.key")
	if err := os.WriteFile(path, []byte("fromfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveSessionSecret("fromflag", path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fromflag" {
		t.Errorf("got %q, want the flag value to win", got)
	}

	got, err = resolveSessionSecret("", path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fromfile" {
		t.Errorf("got %q, want the file value (whitespace trimmed)", got)
	}
}

func TestResolveSessionSecretWithoutFileStaysPerBoot(t *testing.T) {
	a, err := resolveSessionSecret("", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := resolveSessionSecret("", "")
	if err != nil {
		t.Fatal(err)
	}
	if string(a) == string(b) {
		t.Error("with no file configured the key must stay random per boot")
	}
}
