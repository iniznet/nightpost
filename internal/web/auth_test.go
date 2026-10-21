package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iniznet/nightpost/internal/store"
)

func newAuthServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st := openKeysStore(t)
	auth := NewAuth(st, []byte("test-secret-test-secret-0000000000"))
	mux := http.NewServeMux()
	Register(mux, st, nil, auth)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, st
}

// noRedirect keeps the client from following 302s so tests can assert
// Location and Set-Cookie on the redirect itself.
func noRedirect() *http.Client {
	c := &http.Client{}
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return c
}

func setupAdmin(t *testing.T, cl *http.Client, url string) string {
	t.Helper()
	resp, err := cl.Post(url+"/setup", "application/x-www-form-urlencoded",
		strings.NewReader("email=Admin@Example.com&password=correcthorsebattery"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup must 303, got %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			return c.Value
		}
	}
	t.Fatal("setup must issue a session cookie")
	return ""
}

func TestAuthSetupFlowLocksAndServe(t *testing.T) {
	srv, st := newAuthServer(t)
	cl := noRedirect()

	resp, err := cl.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/setup" {
		t.Fatalf("zero users must bounce / to /setup, got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp.Body.Close()

	cookie := setupAdmin(t, cl, srv.URL)
	if n, _ := st.UserCount(); n != 1 {
		t.Fatalf("setup must create exactly one user, got %d", n)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Header.Set("Cookie", sessionCookieName+"="+cookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid session must reach dashboard, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err = cl.Post(srv.URL+"/setup", "application/x-www-form-urlencoded",
		strings.NewReader("email=second@example.com&password=anotherpassword"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second setup POST must 404, got %d", resp.StatusCode)
	}
	if n, _ := st.UserCount(); n != 1 {
		t.Fatalf("setup-after-lock must not add users, count %d", n)
	}
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/issues", nil)
	req.Header.Set("Cookie", sessionCookieName+"="+cookie[:len(cookie)-3]+"AAA")
	resp, err = cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login" {
		t.Fatalf("tampered cookie must bounce to /login, got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/logout", nil)
	req.Header.Set("Cookie", sessionCookieName+"="+cookie)
	resp, err = cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login" {
		t.Fatalf("logout must redirect to /login, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Header.Set("Cookie", sessionCookieName+"="+cookie)
	resp, err = cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("logged-out session must be gated again, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAuthLoginAndThrottle(t *testing.T) {
	srv, _ := newAuthServer(t)
	cl := noRedirect()
	setupAdmin(t, cl, srv.URL)

	for i := 0; i < 5; i++ {
		resp, err := cl.Post(srv.URL+"/login", "application/x-www-form-urlencoded",
			strings.NewReader("email=ADMIN@example.com&password=wrongpassword"))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: wrong login must 401, got %d", i+1, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp, err := cl.Post(srv.URL+"/login", "application/x-www-form-urlencoded",
		strings.NewReader("email=admin@example.com&password=correcthorsebattery"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("6th attempt must be throttled even with the right password, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAuthLoginSuccessIssuesCookie(t *testing.T) {
	srv, _ := newAuthServer(t)
	cl := noRedirect()
	setupAdmin(t, cl, srv.URL)
	resp, err := cl.Post(srv.URL+"/login", "application/x-www-form-urlencoded",
		strings.NewReader("email=admin@example.com&password=correcthorsebattery&next=%2Fslowest"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/slowest" {
		t.Fatalf("good login must 303 to next, got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	found := false
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName && c.HttpOnly {
			found = true
		}
	}
	if !found {
		t.Fatal("login must set an HttpOnly session cookie")
	}
	resp.Body.Close()
}

func TestSafeNextConfinesRedirects(t *testing.T) {
	cases := map[string]string{
		"":           "/",
		"/issues":    "/issues",
		"//evil.com": "/",
		"http://x":   "/",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Fatalf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidEmail(t *testing.T) {
	good := []string{"a@b.co", "admin@example.com"}
	bad := []string{"", "@x.com", "a@", "a b@c.com", "a@@b.com"}
	for _, e := range good {
		if !validEmail(e) {
			t.Fatalf("%q must be valid", e)
		}
	}
	for _, e := range bad {
		if validEmail(e) {
			t.Fatalf("%q must be invalid", e)
		}
	}
}
