package store

import (
	"strings"
	"testing"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	hashed, err := HashPassword("correcthorsebattery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hashed, "pbkdf2-sha256$") {
		t.Fatalf("unexpected format %q", hashed)
	}
	if !VerifyPassword(hashed, "correcthorsebattery") {
		t.Fatal("correct password must verify")
	}
	if VerifyPassword(hashed, "wrong") {
		t.Fatal("wrong password must fail")
	}
	if VerifyPassword("garbage", "x") {
		t.Fatal("malformed hash must fail closed")
	}
	if VerifyPassword(timingDummyHash, "anything") {
		t.Fatal("timing dummy must never verify")
	}
}

func TestCreateFirstAdminWinsLock(t *testing.T) {
	st := newTestStore(t)
	ok, err := st.CreateUserIfNone("admin@example.com", timingDummyHash)
	if err != nil || !ok {
		t.Fatalf("first create must win: %v %v", ok, err)
	}
	ok, err = st.CreateUserIfNone("second@example.com", timingDummyHash)
	if err != nil || ok {
		t.Fatalf("second create must lose: %v %v", ok, err)
	}
	if n, _ := st.UserCount(); n != 1 {
		t.Fatalf("count %d", n)
	}
}

func TestVerifyUserPaths(t *testing.T) {
	st := newTestStore(t)
	hashed, err := HashPassword("s3cret-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUserIfNone("admin@example.com", hashed); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.VerifyUser("admin@example.com", "s3cret-passphrase"); err != nil || !ok {
		t.Fatalf("correct login must verify: %v %v", ok, err)
	}
	if ok, err := st.VerifyUser("admin@example.com", "nope"); err != nil || ok {
		t.Fatalf("wrong password must fail: %v %v", ok, err)
	}
	if ok, err := st.VerifyUser("ghost@example.com", "x"); err != nil || ok {
		t.Fatalf("unknown email must fail without error: %v %v", ok, err)
	}
}

func TestResetAdminPassword(t *testing.T) {
	st := newTestStore(t)
	pw, err := st.ResetAdminPassword("ops@example.com")
	if err != nil || len(pw) != 20 {
		t.Fatalf("reset must mint 20 chars: %q %v", pw, err)
	}
	if ok, _ := st.VerifyUser("ops@example.com", pw); !ok {
		t.Fatal("reset password must verify")
	}
	pw2, err := st.ResetAdminPassword("ops@example.com")
	if err != nil || pw2 == pw {
		t.Fatalf("second reset must mint a new password: %q vs %q (%v)", pw, pw2, err)
	}
	if ok, _ := st.VerifyUser("ops@example.com", pw); ok {
		t.Fatal("old password must stop verifying after reset")
	}
	if ok, _ := st.VerifyUser("ops@example.com", pw2); !ok {
		t.Fatal("new password must verify")
	}
	if n, _ := st.UserCount(); n != 1 {
		t.Fatalf("reset must upsert, not duplicate: %d", n)
	}
}
