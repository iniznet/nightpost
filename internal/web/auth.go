package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

// Auth gates every dashboard page behind a single-admin session (#157).
// Sessions are stateless HMAC-signed cookies (HttpOnly, SameSite=Lax):
// no sessions table, so a restart cannot orphan logins, and revocation is
// bounded by the TTL or a changed -session-secret. /setup stays open only
// while the users table is empty — the first INSERT wins.
type Auth struct {
	st        *store.Store
	secret    []byte
	setupTmpl *template.Template
	loginTmpl *template.Template

	mu      sync.Mutex
	failers map[string]*failRecord
}

const (
	sessionCookieName = "frmon_session"
	sessionTTL        = 30 * 24 * time.Hour
	loginMaxFails     = 5
	loginFailWindow   = 15 * time.Minute
)

type failRecord struct {
	count int
	since time.Time
}

func NewAuth(st *store.Store, secret []byte) *Auth {
	return &Auth{st: st, secret: secret, failers: map[string]*failRecord{}}
}

// CompileTemplates wires the login/setup pages; called from Register.
func (a *Auth) CompileTemplates() {
	a.setupTmpl = mustLayout("setup.html")
	a.loginTmpl = mustLayout("login.html")
}

func (a *Auth) sign(payload string) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// sessionValue packs email|issued|expiry; the issued stamp lets
// SessionEpoch revoke stateless cookies on logout or password reset.
func (a *Auth) sessionValue(email string) string {
	now := time.Now().Unix()
	payload := email + "|" + strconv.FormatInt(now, 10) + "|" + strconv.FormatInt(now+int64(sessionTTL.Seconds()), 10)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + a.sign(payload)
}

// sessionUser verifies signature, expiry and revocation epoch; ok=false
// covers every failure mode identically (no oracle on which one tripped).
func (a *Auth) sessionUser(r *http.Request) (email string, ok bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	i := strings.LastIndex(c.Value, ".")
	if i <= 0 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value[:i])
	if err != nil || !hmac.Equal([]byte(c.Value[i+1:]), []byte(a.sign(string(raw)))) {
		return "", false
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 || parts[0] == "" {
		return "", false
	}
	issued, err1 := strconv.ParseInt(parts[1], 10, 64)
	exp, err2 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || time.Now().Unix() > exp {
		return "", false
	}
	epoch, err := a.st.SessionEpoch(parts[0])
	if err != nil || issued <= epoch {
		return "", false
	}
	return parts[0], true
}

// Gate wraps the page mux: valid session passes; everything else is
// redirected to /setup (first run) or /login.
func (a *Auth) Gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.sessionUser(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		target := "/login"
		if n, err := a.st.UserCount(); err == nil && n == 0 {
			target = "/setup"
		}
		http.Redirect(w, r, target, http.StatusFound)
	})
}

func (a *Auth) setSession(w http.ResponseWriter, email string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    a.sessionValue(email),
		Path:     "/",
		Expires:  time.Now().Add(sessionTTL),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *Auth) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/",
		MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

func (a *Auth) renderLogin(w http.ResponseWriter, status int, next, msg string) {
	data := baseData(a.st, "", PageLogin)
	data["Error"] = msg
	data["Next"] = next
	w.WriteHeader(status)
	if err := a.loginTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render login: %v", err)
	}
}

func (a *Auth) renderSetup(w http.ResponseWriter, status int, msg string) {
	data := baseData(a.st, "", PageSetup)
	data["Error"] = msg
	w.WriteHeader(status)
	if err := a.setupTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render setup: %v", err)
	}
}

// HandleSetupGET serves the first-run form; once an admin exists it is
// closed for good.
func (a *Auth) HandleSetupGET(w http.ResponseWriter, r *http.Request) {
	if n, err := a.st.UserCount(); err != nil || n > 0 {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	a.renderSetup(w, http.StatusOK, "")
}

// HandleSetupPOST creates the single admin (first INSERT wins) and logs in.
func (a *Auth) HandleSetupPOST(w http.ResponseWriter, r *http.Request) {
	if n, err := a.st.UserCount(); err != nil || n > 0 {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	email := normalizeEmail(r.FormValue("email"))
	pw := r.FormValue("password")
	if !validEmail(email) {
		a.renderSetup(w, http.StatusBadRequest, "A valid email is required.")
		return
	}
	if len(pw) < 10 {
		a.renderSetup(w, http.StatusBadRequest, "Password must be at least 10 characters.")
		return
	}
	hashed, err := store.HashPassword(pw)
	if err != nil {
		http.Error(w, "hash failure", http.StatusInternalServerError)
		return
	}
	created, err := a.st.CreateUserIfNone(email, hashed)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !created {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	log.Printf("first admin account created for %q — setup is now locked", email)
	a.setSession(w, email)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// HandleLoginGET redirects to setup while the account table is empty.
func (a *Auth) HandleLoginGET(w http.ResponseWriter, r *http.Request) {
	if n, err := a.st.UserCount(); err == nil && n == 0 {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	a.renderLogin(w, http.StatusOK, safeNext(r.URL.Query().Get("next")), "")
}

// HandleLoginPOST verifies credentials behind a per-ip+email failure
// throttle (5 per 15 min) so the sslip-published UI cannot be brute
// forced at line speed.
func (a *Auth) HandleLoginPOST(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	next := safeNext(r.FormValue("next"))
	email := normalizeEmail(r.FormValue("email"))
	pw := r.FormValue("password")
	key := clientIP(r) + "|" + email
	if a.throttled(key) {
		http.Error(w, "too many failed logins — wait 15 minutes", http.StatusTooManyRequests)
		return
	}
	ok, err := a.st.VerifyUser(email, pw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		a.registerFail(key)
		a.renderLogin(w, http.StatusUnauthorized, next, "Invalid email or password.")
		return
	}
	a.clearFails(key)
	a.setSession(w, email)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// HandleLogout clears the cookie AND revokes it server-side: the stateless
// token would otherwise stay bearer-valid until expiry.
func (a *Auth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if email, ok := a.sessionUser(r); ok {
		if err := a.st.RevokeSessions(email); err != nil {
			log.Printf("logout revoke for %q: %v", email, err)
		}
	}
	a.clearSession(w)
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (a *Auth) throttled(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.failers[key]
	if f == nil {
		return false
	}
	if time.Since(f.since) > loginFailWindow {
		delete(a.failers, key)
		return false
	}
	return f.count >= loginMaxFails
}

func (a *Auth) registerFail(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.failers) > 1024 {
		for k, f := range a.failers {
			if time.Since(f.since) > loginFailWindow {
				delete(a.failers, k)
			}
		}
	}
	f := a.failers[key]
	if f == nil || time.Since(f.since) > loginFailWindow {
		a.failers[key] = &failRecord{count: 1, since: time.Now()}
		return
	}
	f.count++
}

func (a *Auth) clearFails(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.failers, key)
}

// safeNext confines post-login redirects to same-origin paths: absolute
// URLs and protocol-relative "//host" values fall back to the dashboard.
func safeNext(n string) string {
	if n == "" || !strings.HasPrefix(n, "/") || strings.HasPrefix(n, "//") || strings.Contains(n, "\\\\") {
		return "/"
	}
	return n
}

func normalizeEmail(e string) string {
	return strings.ToLower(strings.TrimSpace(e))
}

func validEmail(e string) bool {
	at := strings.IndexByte(e, '@')
	return at > 0 && at == strings.LastIndexByte(e, '@') && at < len(e)-1 &&
		!strings.ContainsAny(e, " \t") && len(e) <= 254
}

// clientIP strips the port from RemoteAddr; behind Traefik the real IP is
// in X-Forwarded-For, but the monitor binds the docker network, so
// RemoteAddr is the proxy and that is acceptable for a coarse throttle.
func clientIP(r *http.Request) string {
	addr := r.RemoteAddr
	if h := r.Header.Get("X-Forwarded-For"); h != "" {
		if j := strings.IndexByte(h, ','); j > 0 {
			return h[:j]
		}
		return h
	}
	if i := strings.LastIndexByte(addr, ':'); i > 0 && !strings.Contains(addr[i+1:], "]") {
		return addr[:i]
	}
	return addr
}
