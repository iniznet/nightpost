package ingest

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// CORS for the browser reporter routes (M6.5).
//
// The allowlist used to be one global -cors-origin string applied to every
// browser route, which a multi-tenant install cannot express: each tenant's
// front end is a different origin. The allowlist now lives on the project row
// (empty falls back to the -cors-origin default), and it is checked against
// the request's own Origin instead of being echoed blindly.

// WithCORS decorates a first-party ingest handler with browser-CORS support so
// SvelteKit client-side hooks can report directly from the page origin.
// Disabled entirely when origin is empty (default): server-only senders need
// no CORS headers and the surface stays browser-hostile.
func WithCORS(origin string, next http.HandlerFunc) http.HandlerFunc {
	return WithCORSSource(func(*http.Request) string { return origin }, next)
}

// WithCORSSource resolves the allowlist PER REQUEST, so the project row decides
// which browser origins may post to it.
func WithCORSSource(origin func(*http.Request) string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list := parseOriginList(currentOrigin(origin, r))
		if len(list) == 0 {
			next(w, r)
			return
		}
		value, ok := corsValue(list, r.Header.Get("Origin"))
		if !ok {
			// A browser call from an origin the project did not allowlist is
			// refused out loud rather than answered without CORS headers, so
			// the failure is countable and diagnosable instead of silent.
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		writeCORSHeaders(w, value)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// PreflightHandler answers CORS preflights for routes registered POST-only in
// the pattern mux (an OPTIONS request never matches a POST pattern, so
// preflight needs its own registration). Without a configured origin the
// preflight is refused, keeping default behavior unchanged.
func PreflightHandler(origin string) http.HandlerFunc {
	return PreflightSource(func(*http.Request) string { return origin })
}

// PreflightSource is the per-project form of PreflightHandler.
func PreflightSource(origin func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list := parseOriginList(currentOrigin(origin, r))
		if len(list) == 0 {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		value, ok := corsValue(list, r.Header.Get("Origin"))
		if !ok {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		writeCORSHeaders(w, value)
		w.WriteHeader(http.StatusNoContent)
	}
}

// CORSResolver resolves a project's browser-origin allowlist, caching the
// answer briefly so a browser reporter does not pay a project lookup on every
// POST. An empty value on the row falls back to the -cors-origin default: the
// flag is the install-time seed, the row is the per-tenant truth.
type CORSResolver struct {
	fallback string
	lookup   func(project string) (string, error)
	ttl      time.Duration

	mu    sync.Mutex
	cache map[string]corsEntry
}

type corsEntry struct {
	value string
	at    time.Time
}

// NewCORSResolver builds the resolver; a nil lookup or ttl <= 0 just disables
// caching and always reads the fallback.
func NewCORSResolver(fallback string, lookup func(string) (string, error), ttl time.Duration) *CORSResolver {
	return &CORSResolver{fallback: fallback, lookup: lookup, ttl: ttl, cache: map[string]corsEntry{}}
}

// OriginFor resolves the allowlist from the {project} path value.
func (c *CORSResolver) OriginFor(r *http.Request) string {
	if c == nil {
		return ""
	}
	project := PathProject(r)
	if project == "" {
		return c.fallback
	}
	c.mu.Lock()
	if e, ok := c.cache[project]; ok && c.ttl > 0 && time.Since(e.at) < c.ttl {
		c.mu.Unlock()
		if e.value != "" {
			return e.value
		}
		return c.fallback
	}
	c.mu.Unlock()

	value := ""
	if c.lookup != nil {
		if v, err := c.lookup(project); err == nil {
			value = v
		}
	}
	// Bound the map: project names arrive from the ingest path, so the cache
	// must never be a growth vector (the M6.10 rule applied to a new map).
	c.mu.Lock()
	if len(c.cache) >= maxCORSCache {
		c.evictLocked()
	}
	c.cache[project] = corsEntry{value: value, at: time.Now()}
	c.mu.Unlock()

	if value != "" {
		return value
	}
	return c.fallback
}

// Invalidate drops a cached decision, so an allowlist edit takes effect on the
// next request instead of waiting out the TTL.
func (c *CORSResolver) Invalidate(project string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.cache, project)
	c.mu.Unlock()
}

const maxCORSCache = 4096

func (c *CORSResolver) evictLocked() {
	type entry struct {
		key string
		at  time.Time
	}
	all := make([]entry, 0, len(c.cache))
	for k, v := range c.cache {
		all = append(all, entry{k, v.at})
	}
	for i := 0; i < len(all)/2; i++ {
		oldest := i
		for j := i + 1; j < len(all); j++ {
			if all[j].at.Before(all[oldest].at) {
				oldest = j
			}
		}
		all[i], all[oldest] = all[oldest], all[i]
		delete(c.cache, all[i].key)
	}
}

// currentOrigin tolerates a nil resolver func (treat as "no CORS").
func currentOrigin(fn func(*http.Request) string, r *http.Request) string {
	if fn == nil {
		return ""
	}
	return fn(r)
}

// parseOriginList splits and normalizes a comma-separated allowlist.
func parseOriginList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimRight(strings.TrimSpace(part), "/"); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// corsValue picks the header value for this request. The second result reports
// whether the caller is allowed at all: an Origin that is present but absent
// from the list is a refusal, never a silent pass.
//
// A request with NO Origin header is not a browser cross-origin call (a server
// sender, curl, or the same-origin page), so it proceeds; with a single-entry
// list the legacy behaviour of publishing that origin is preserved.
func corsValue(list []string, reqOrigin string) (string, bool) {
	if len(list) == 0 {
		return "", true
	}
	if reqOrigin == "" {
		if len(list) == 1 {
			return list[0], true
		}
		return "", true
	}
	for _, allowed := range list {
		if strings.EqualFold(allowed, reqOrigin) {
			return reqOrigin, true
		}
	}
	return "", false
}

func writeCORSHeaders(w http.ResponseWriter, value string) {
	if value != "" {
		w.Header().Set("Access-Control-Allow-Origin", value)
	}
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Max-Age", "600")
}
