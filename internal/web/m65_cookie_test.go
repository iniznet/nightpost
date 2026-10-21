package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func cookieNamed(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// TestSessionCookieSecure covers the gap: the session cookie was HttpOnly
// and SameSite but never Secure, so behind a TLS-terminating proxy it could
// still travel over plain HTTP.
func TestSessionCookieSecure(t *testing.T) {
	st := openKeysStore(t)
	a := NewAuth(st, []byte("test-session-secret"))

	// Plain HTTP with no declaration: NOT Secure, or a local install over
	// http://localhost could not log in at all.
	rec := httptest.NewRecorder()
	a.setSession(rec, httptest.NewRequest(http.MethodPost, "http://box/login", nil), "a@example.com")
	if c := cookieNamed(rec.Result().Cookies(), sessionCookieName); c == nil || c.Secure {
		t.Errorf("plain HTTP: cookie = %+v, want present and not Secure", c)
	}

	// Direct TLS is detected without any flag.
	rec = httptest.NewRecorder()
	a.setSession(rec, httptest.NewRequest(http.MethodPost, "https://box/login", nil), "a@example.com")
	if c := cookieNamed(rec.Result().Cookies(), sessionCookieName); c == nil || !c.Secure {
		t.Errorf("direct TLS: cookie = %+v, want Secure", c)
	}

	// The proxy topology cannot be auto-detected, so the operator declares it.
	a.SetCookieSecure(true)
	rec = httptest.NewRecorder()
	a.setSession(rec, httptest.NewRequest(http.MethodPost, "http://box/login", nil), "a@example.com")
	c := cookieNamed(rec.Result().Cookies(), sessionCookieName)
	if c == nil || !c.Secure {
		t.Fatalf("declared TLS proxy: cookie = %+v, want Secure", c)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("hardening lost: %+v", c)
	}

	// Logout must carry the same attributes, or the browser keeps a second,
	// differently-scoped cookie beside the cleared one.
	rec = httptest.NewRecorder()
	a.clearSession(rec, httptest.NewRequest(http.MethodPost, "http://box/logout", nil))
	if c := cookieNamed(rec.Result().Cookies(), sessionCookieName); c == nil || !c.Secure {
		t.Errorf("clear cookie = %+v, want Secure", c)
	}
}
