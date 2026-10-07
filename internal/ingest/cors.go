package ingest

import "net/http"

// WithCORS decorates a first-party ingest handler with browser-CORS support
// so SvelteKit client-side hooks can report directly from the page origin.
// Disabled entirely when origin is empty (default): server-only senders need
// no CORS headers and the surface stays browser-hostile.
func WithCORS(origin string, next http.HandlerFunc) http.HandlerFunc {
	if origin == "" {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "600")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// PreflightHandler answers CORS preflights for routes registered POST-only
// in the pattern mux (an OPTIONS request never matches a POST pattern, so
// preflight needs its own registration). Without a configured origin the
// preflight is refused, keeping default behavior unchanged.
func PreflightHandler(origin string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if origin == "" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
	}
}
