package ingest

import (
	"net/http"
	"strings"
)

// Cross-service trace context.
//
// A producer with a Nightpost client carries its trace id in the payload. An
// OpenTelemetry-instrumented service (Go, Python, Rust, …) has neither our SDK
// nor a payload field, but it DOES propagate the W3C `traceparent` header —
// reading that header is the cheapest way for such a service to join a trace,
// and it needs no server change beyond this file.
const (
	// HeaderTraceID is the explicit legacy header.
	HeaderTraceID = "X-Monitor-Trace-Id"
	// HeaderTraceparent is the W3C Trace Context header.
	HeaderTraceparent = "traceparent"
)

// TraceIDFromRequest resolves the trace id for an ingest request, most
// specific first: the payload's own value, then X-Monitor-Trace-Id, then a W3C
// traceparent. An empty result is valid — the row simply joins no trace — and
// is never invented, so a missing header cannot fabricate a trace key that
// collides with a real one.
func TraceIDFromRequest(r *http.Request, fromPayload string) string {
	if v := strings.TrimSpace(fromPayload); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get(HeaderTraceID)); v != "" {
		return v
	}
	if id, ok := ParseTraceparent(r.Header.Get(HeaderTraceparent)); ok {
		return id
	}
	return ""
}

// ParseTraceparent validates a W3C traceparent header and returns its trace id.
//
// Shape: version "-" trace-id "-" parent-id "-" flags, e.g.
// "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01". The version is two
// hex digits and `ff` is invalid; the trace id is 32 lowercase hex digits and
// must not be all zero; the parent id is 16 hex digits; the flags are two hex
// digits. Any further fields belong to a future version and are ignored.
//
// A malformed header is REJECTED rather than salvaged: guessing at a broken
// value would mint a trace key that silently groups unrelated requests.
func ParseTraceparent(header string) (string, bool) {
	h := strings.TrimSpace(header)
	if h == "" {
		return "", false
	}
	parts := strings.Split(h, "-")
	if len(parts) < 4 {
		return "", false
	}
	version, traceID, parentID, flags := parts[0], parts[1], parts[2], parts[3]
	if len(version) != 2 || !isLowerHex(version) || version == "ff" {
		return "", false
	}
	if len(traceID) != 32 || !isLowerHex(traceID) || traceID == strings.Repeat("0", 32) {
		return "", false
	}
	if len(parentID) != 16 || !isLowerHex(parentID) {
		return "", false
	}
	if len(flags) != 2 || !isLowerHex(flags) {
		return "", false
	}
	return traceID, true
}

// isLowerHex reports whether s is non-empty and made only of 0-9a-f. The spec
// requires lowercase, and accepting uppercase would key the same trace twice.
func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
