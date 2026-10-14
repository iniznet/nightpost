package ingest

import (
	"crypto/subtle"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

// ProjectKeyStore is the store surface the gate needs: key lookup plus
// one-time admission of unknown producers as unsigned legacy projects.
type ProjectKeyStore interface {
	ProjectIngestKey(key string) (string, error)
	EnsureProject(key string) error
}

type keyEntry struct {
	key   string
	until time.Time
}

// KeyGate enforces per-project ingest keys. A project with no key keeps
// accepting unsigned (legacy) traffic; once a key is rotated in, every
// request must present it as "Authorization: Bearer <key>" or
// "X-Monitor-Key: <key>". Lookups are cached briefly to keep SQLite off
// the ingest hot path; Invalidate drops a cached entry so an in-process
// rotation takes effect immediately. On a lookup failure the gate serves
// the stale cached decision when one exists and otherwise admits the
// event with a warning: a transient read error must not blackhole
// producers, and the network perimeter is still the docker policy until
// dashboard auth lands.
//
// NewKeyGate with ttl <= 0 disables caching (every request consults the
// store) — the mode tests use to observe rotation instantly.
type KeyGate struct {
	store   ProjectKeyStore
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string]keyEntry
	seen    sync.Map

	rejected      atomic.Int64
	unknownAdmits atomic.Int64
}

// Rejections reports ingest requests answered 401 for a missing/wrong key.
func (g *KeyGate) Rejections() int64 { return g.rejected.Load() }

// UnknownAdmits reports first-sight auto-provisioned unsigned projects. A
// rising figure on a public ingress is the M6.10 attack signature.
func (g *KeyGate) UnknownAdmits() int64 { return g.unknownAdmits.Load() }

func NewKeyGate(st ProjectKeyStore, ttl time.Duration) *KeyGate {
	return &KeyGate{store: st, ttl: ttl, entries: map[string]keyEntry{}}
}

// Invalidate clears the cached key decision for a project.
func (g *KeyGate) Invalidate(project string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.entries, project)
}

func (g *KeyGate) lookup(project string) (string, error) {
	if g.ttl > 0 {
		g.mu.Lock()
		e, ok := g.entries[project]
		g.mu.Unlock()
		if ok && time.Now().Before(e.until) {
			return e.key, nil
		}
	}
	key, err := g.store.ProjectIngestKey(project)
	if err != nil {
		if g.ttl > 0 {
			g.mu.Lock()
			e, ok := g.entries[project]
			g.mu.Unlock()
			if ok {
				log.Printf("keygate: lookup %q failed (%v), serving stale decision", project, err)
				return e.key, nil
			}
		}
		return "", err
	}
	if g.ttl > 0 {
		g.mu.Lock()
		g.entries[project] = keyEntry{key: key, until: time.Now().Add(g.ttl)}
		g.mu.Unlock()
	}
	return key, nil
}

// PathProject resolves the project from the {project} path segment.
func PathProject(r *http.Request) string { return r.PathValue("project") }

// XHProfProject resolves the profiler routes' project: path segment, then
// HTTP Basic username, then the X-Monitor-Project header.
func XHProfProject(r *http.Request) string { return xhprofProject(r) }

// presentedKey extracts the offered ingest secret from the request headers.
func presentedKey(r *http.Request) string {
	if h := r.Header.Get("X-Monitor-Key"); h != "" {
		return h
	}
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	// URL-native senders (the xhprof extension POSTs straight from the
	// endpoint's userinfo and cannot set custom headers) authenticate as
	// HTTP Basic: the PASSWORD segment carries the ingest key, the username
	// stays the project name. Bearer/X-Monitor-Key remain the preferred
	// paths for every producer that controls its own headers.
	if h := r.Header.Get("Authorization"); len(h) > 6 && strings.EqualFold(h[:6], "basic ") {
		if _, pass, ok := r.BasicAuth(); ok {
			return pass
		}
	}
	return ""
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// WithKeyGate wraps an ingest handler; resolveProject mirrors the route's
// own project resolution so the gate never disagrees with the handler.
func WithKeyGate(g *KeyGate, resolveProject func(*http.Request) string, next http.HandlerFunc) http.HandlerFunc {
	if g == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		project := resolveProject(r)
		if project == "" {
			project = "default"
		}
		want, err := g.lookup(project)
		if err != nil {
			log.Printf("keygate: lookup %q failed (%v); admitting unsigned", project, err)
			next(w, r)
			return
		}
		if want != "" {
			presented := presentedKey(r)
			if presented == "" || !constantTimeEqual(want, store.HashIngestKey(presented)) {
				g.rejected.Add(1)
				http.Error(w, "invalid ingest key", http.StatusUnauthorized)
				return
			}
			next(w, r)
			return
		}
		if _, loaded := g.seen.LoadOrStore(project, true); !loaded {
			g.unknownAdmits.Add(1)
			if err := g.store.EnsureProject(project); err != nil {
				log.Printf("keygate: ensure project %q: %v", project, err)
			}
		}
		next(w, r)
	}
}
