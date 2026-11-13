package ingest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/iniznet/nightpost/internal/store"
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

// v2 trace identity outcomes. They are recorded on every span row so the
// request page can say where a trace key came from: a minted key is ours, an
// adopted one was propagated by a producer we trust, and a replaced one was
// propagated by a producer we do not — the payload's claim was dropped.
const (
	TraceOriginMinted   = "minted"
	TraceOriginAdopted  = "adopted"
	TraceOriginReplaced = "replaced"
)

// TraceIDFromRequest resolves the trace id for an ingest request, most
// specific first: the payload's own value, then X-Monitor-Trace-Id, then a W3C
// traceparent. An empty result is valid — the row simply joins no trace — and
// is never invented, so a missing header cannot fabricate a trace key that
// collides with a real one.
func TraceIDFromRequest(r *http.Request, fromPayload string) string {
	if v := explicitTraceID(r, fromPayload); v != "" {
		return v
	}
	if id, ok := ParseTraceparent(r.Header.Get(HeaderTraceparent)); ok {
		return id
	}
	return ""
}

// explicitTraceID returns the trace id a producer STATED, as opposed to one
// recovered from the W3C header. Kept separate from the header because a
// stated id is a deliberate claim while a traceparent may or may not be
// trusted — see traceIdentity.
func explicitTraceID(r *http.Request, fromPayload string) string {
	if v := strings.TrimSpace(fromPayload); v != "" {
		return v
	}
	return strings.TrimSpace(r.Header.Get(HeaderTraceID))
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

// strictTraceparent validates the exact shape this server adopts:
// `00-<32 hex>-<16 hex>-01`, nothing else, no trailing fields.
//
// It is deliberately narrower than ParseTraceparent. Adopting an id means two
// requests are drawn as one waterfall for ever, so the banner the plan fixes —
// version 00, sampled flag 01 — is the only claim taken on trust; a future
// version, an unsampled trace or a padded field is minted past instead.
func strictTraceparent(header string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(header), "-")
	if len(parts) != 4 {
		return "", false
	}
	if parts[0] != "00" || parts[3] != "01" {
		return "", false
	}
	if len(parts[1]) != 32 || !isLowerHex(parts[1]) || parts[1] == strings.Repeat("0", 32) {
		return "", false
	}
	if len(parts[2]) != 16 || !isLowerHex(parts[2]) || parts[2] == strings.Repeat("0", 16) {
		return "", false
	}
	return parts[1], true
}

// traceIdentity resolves the trace id a span-carrying payload joins, and says
// how it was decided.
//
//  1. A stated id (payload or legacy header) is adopted: the producer built it,
//     and every v1 producer that already sends one keeps its traces.
//  2. A valid traceparent from a TRUSTED producer is adopted.
//  3. A valid traceparent from an untrusted one is replaced: a caller-chosen key
//     must not be able to meld two unrelated requests into one waterfall, and
//     there is no later pass that could separate them again.
//  4. Absent or malformed means minted here.
func traceIdentity(r *http.Request, fromPayload string, trusted bool) (id, origin string) {
	if stated := explicitTraceID(r, fromPayload); stated != "" {
		return truncate(stated, 64), TraceOriginAdopted
	}
	if propagated, ok := strictTraceparent(r.Header.Get(HeaderTraceparent)); ok {
		if trusted {
			return propagated, TraceOriginAdopted
		}
		return mintTraceID(), TraceOriginReplaced
	}
	return mintTraceID(), TraceOriginMinted
}

// mintTraceID returns a fresh 32-hex trace id. An entropy failure returns the
// empty string, which the schema already treats as "joins no trace" — the
// event is still stored, and a fabricated key would be worse than none.
func mintTraceID() string { return randomHex(16) }

// mintSpanID returns a fresh 16-hex span id, the W3C width.
func mintSpanID() string { return randomHex(8) }

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		log.Printf("trace id entropy: %v", err)
		return ""
	}
	return hex.EncodeToString(buf)
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

// spanPayload is one span item on the v2 wire.
//
// store.Span is embedded because the v1 array is the same JSON: name,
// duration_ms, start_ms and tags keep landing in profiles.spans while the v2
// fields join the trace waterfall. One struct, so a producer cannot send a
// payload this server reads two different ways.
//
// start_ms is an OFFSET from the payload's own start, never a wall clock: the
// callers run on different hosts, and a join on absolute time would need clock
// sync no install has (pre-mortem: cross-host skew).
type spanPayload struct {
	store.Span
	SpanID       string `json:"span_id"`
	ParentSpanID string `json:"parent_span_id"`
	Kind         string `json:"kind"`
	Status       string `json:"status"`
	Route        string `json:"route"`
	PeerService  string `json:"peer_service"`
}

const (
	// maxPayloadSpans bounds one payload's batch: the rows land in one
	// transaction on the single writer, so an unbounded batch is a lock hold.
	maxPayloadSpans = 200
	maxSpanName     = 200
)

// spanIDOrMint keeps a valid producer id and replaces an unusable one.
func spanIDOrMint(id string) string {
	if len(id) == 16 && isLowerHex(id) {
		return id
	}
	return mintSpanID()
}

// clipSpans bounds a batch and repairs the fields this server can repair. Every
// repair is logged with its JSON path; none of them refuses the payload, because
// a v2 span field must never turn a payload a v1 producer sent into a rejection.
func clipSpans(in []spanPayload, d *digest) []spanPayload {
	if len(in) == 0 {
		return nil
	}
	if len(in) > maxPayloadSpans {
		d.note("spans", "batch of %d clipped to %d", len(in), maxPayloadSpans)
		in = in[:maxPayloadSpans]
	}
	out := make([]spanPayload, 0, len(in))
	for i, sp := range in {
		path := fmt.Sprintf("spans[%d]", i)
		sp.Name = strings.TrimSpace(sp.Name)
		if sp.Name == "" {
			d.note(path+".name", "missing, span dropped")
			continue
		}
		if sp.SpanID != "" && spanIDOrMint(sp.SpanID) != sp.SpanID {
			d.note(path+".span_id", "not 16 lowercase hex digits, replaced")
			sp.SpanID = ""
		}
		if sp.StartMs < 0 {
			d.note(path+".start_ms", "negative, clamped to 0")
			sp.StartMs = 0
		}
		if sp.DurationMs < 0 {
			d.note(path+".duration_ms", "negative, clamped to 0")
			sp.DurationMs = 0
		}
		sp.Status = spanStatusOf(sp.Status, path, d)
		sp.Kind = spanKind(sp.Kind)
		sp.Name = truncate(sp.Name, maxSpanName)
		sp.Route = truncate(strings.TrimSpace(sp.Route), 255)
		sp.PeerService = truncate(strings.TrimSpace(sp.PeerService), maxServiceName)
		out = append(out, sp)
	}
	return out
}

// spanKind normalises a span kind. "internal" is the honest default: a producer
// that did not say whether a span crossed a boundary must not be drawn as one.
func spanKind(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" {
		return "internal"
	}
	return truncate(kind, 32)
}

// spanStatusOf keeps one of the statuses the waterfall draws and reports any
// other value instead of guessing what it meant.
func spanStatusOf(status, path string, d *digest) string {
	s := strings.ToLower(strings.TrimSpace(status))
	switch s {
	case "":
		return "ok"
	case "ok", "error", "unset":
		return s
	}
	d.note(path+".status", "%q is not ok|error|unset, stored as ok", s)
	return "ok"
}

// spanBatch is one payload's span set: the payload's OWN span — the root of its
// trace, with no parent — plus the child spans the producer reported.
type spanBatch struct {
	Project  string
	TraceID  string
	Origin   string
	Service  serviceRef
	Root     spanPayload
	Children []spanPayload
}

// write maps the batch onto store rows and inserts them, returning the root
// route id so the profile row can name the endpoint it stood for.
func (b spanBatch) write(st *store.Store, d *digest) (int64, error) {
	rootID := spanIDOrMint(b.Root.SpanID)
	rootRouteID := upsertRoute(st, b.Service.ID, b.Root.Route, b.Root.Kind, d)
	rows := make([]store.TraceSpan, 0, len(b.Children)+1)
	rows = append(rows, store.TraceSpan{
		TraceID:    b.TraceID,
		SpanID:     rootID,
		ServiceID:  b.Service.ID,
		RouteID:    rootRouteID,
		Kind:       spanKind(b.Root.Kind),
		Name:       truncate(b.Root.Name, maxSpanName),
		StartMs:    0,
		DurationMs: b.Root.DurationMs,
		Status:     "ok",
		Attributes: spanAttributes(b.Origin, ""),
	})
	for _, child := range b.Children {
		parent := child.ParentSpanID
		if parent == "" {
			// Inside one payload an unnamed parent is the payload's own request.
			parent = rootID
		}
		var peerID int64
		if child.PeerService != "" {
			peerID = resolveService(st, b.Project, child.PeerService, "", "span peer").ID
		}
		status := child.Status
		if status == "" {
			status = "ok"
		}
		rows = append(rows, store.TraceSpan{
			TraceID:       b.TraceID,
			SpanID:        spanIDOrMint(child.SpanID),
			ParentSpanID:  parent,
			ServiceID:     b.Service.ID,
			RouteID:       upsertRoute(st, b.Service.ID, child.Route, child.Kind, d),
			Kind:          spanKind(child.Kind),
			Name:          truncate(child.Name, maxSpanName),
			StartMs:       child.StartMs,
			DurationMs:    child.DurationMs,
			Status:        status,
			PeerServiceID: peerID,
			Attributes:    spanAttributes(b.Origin, child.PeerService),
		})
	}
	if err := st.InsertSpans(rows); err != nil {
		return rootRouteID, err
	}
	return rootRouteID, nil
}

// spanAttributes is the JSON trail on a span row. trace_origin is always
// present — that is the plan's provenance record — and peer_service names the
// callee so an unreported hop can be rendered with a name instead of a blank.
func spanAttributes(origin, peer string) string {
	attrs := map[string]string{"trace_origin": origin}
	if peer != "" {
		attrs["peer_service"] = peer
	}
	buf, err := json.Marshal(attrs)
	if err != nil {
		return ""
	}
	return string(buf)
}

// upsertRoute maps a producer's route PATTERN onto its row, which is what makes
// two requests comparable as a journey hop. method is empty: every current
// producer reports the pattern, not the verb, and a pattern with no verb is the
// shape that collapses /chapters/1 and /chapters/2 into one endpoint.
func upsertRoute(st *store.Store, serviceID int64, pattern, kind string, d *digest) int64 {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || serviceID == 0 || st == nil {
		return 0
	}
	id, err := st.UpsertRoute(serviceID, "", truncate(pattern, 255), spanKind(kind))
	if err != nil {
		d.note("route", "upsert %q: %v", pattern, err)
		return 0
	}
	return id
}
