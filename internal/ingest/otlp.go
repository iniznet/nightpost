package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"

	"github.com/iniznet/nightpost/internal/store"
)

// OpenTelemetry ingest: POST /v1/traces and POST /v1/logs, JSON only.
//
// A collector configured with the `otlphttp` exporter points at one of these two
// routes, and any OTel-instrumented service then joins a journey without a
// Nightpost SDK. JSON only, never protobuf: protobuf would add a generated-code
// module dependency for data these structs already carry, and the plan locks
// that trade ("OTLP | JSON only").
//
// Neither route reads traceparent. OTLP STATES its own trace id on every span,
// so adopting the header instead would rewrite ids the producer already handed
// to its children — the children would then land under a different trace key and
// the waterfall would show two half traces.
const (
	// otlpMaxBody bounds one collector batch. A collector posts on a timer with a
	// bounded queue, so a body past this ceiling is a misconfiguration or an
	// attack; refusing it costs nothing the producer cannot resend.
	otlpMaxBody = 8 << 20

	// OTLP carries ids as hex of the W3C width, 32 for a trace and 16 for a span.
	otlpTraceIDLen = 32
	otlpSpanIDLen  = 16

	// Raw-envelope route labels, matching the path segment so /ingest-log reads
	// "traces" beside the other routes' "profile" and "error".
	rawRouteTraces = "traces"
	rawRouteLogs   = "logs"
)

// The named rejections. An id this server cannot verify refuses the WHOLE
// envelope rather than dropping one span: a half-written trace is worse than a
// rejected one, because the hops that did land draw a waterfall with a gap that
// no reader can tell from a callee that never reported.
var (
	ErrOTLPTraceID      = errors.New("otlp: malformed trace id")
	ErrOTLPSpanID       = errors.New("otlp: malformed span id")
	ErrOTLPParentSpanID = errors.New("otlp: malformed parent span id")
)

// OTLPProject resolves the collector routes' project key and pins it onto the
// request.
//
// The collector is configured with ONE endpoint, so it cannot name a project in
// the path the way /ingest/{project}/... does: ?project= is what a
// multi-project install pins, and the first configured project is the single
// install's only answer. The pin exists so the shaper — which resolves its own
// project from the path — charges the bucket the gate admitted; without it a
// ?project= payload would be admitted as one project and budgeted as another.
func OTLPProject(st *store.Store) func(*http.Request) string {
	return func(r *http.Request) string {
		p := otlpProject(r, st)
		r.SetPathValue("project", p)
		return p
	}
}

// otlpProject is the shared resolution behind OTLPProject and the two handlers.
// "default" is the last resort, matching WithKeyGate and the Monolog leg, so a
// first event carrying no project anywhere still lands instead of erroring.
func otlpProject(r *http.Request, st *store.Store) string {
	if p := strings.TrimSpace(r.PathValue("project")); p != "" {
		return p
	}
	if p := strings.TrimSpace(r.URL.Query().Get("project")); p != "" {
		return p
	}
	if st != nil {
		// Every project EVER SEEN, sorted; the first is the install's project in
		// the one-install shape this server ships for.
		keys, err := st.ProjectsInOrg(0)
		if err != nil {
			log.Printf("otlp project lookup: %v", err)
		} else if len(keys) > 0 {
			return keys[0]
		}
	}
	return "default"
}

// otlpAttribute is one key/value pair. The value is a proto3 JSON oneof, so
// exactly one of otlpValue's fields is set and every one is optional.
type otlpAttribute struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

// otlpValue carries the JSON encoding of an OTel AnyValue. IntValue is a
// json.Number because proto3 JSON sends int64 as a STRING while a hand-rolled
// client sends a number, and the decoder accepts both into this type.
type otlpValue struct {
	StringValue *string         `json:"stringValue"`
	BoolValue   *bool           `json:"boolValue"`
	IntValue    json.Number     `json:"intValue"`
	DoubleValue *float64        `json:"doubleValue"`
	ArrayValue  *otlpArrayValue `json:"arrayValue"`
	KVListValue *otlpKeyValues  `json:"kvlistValue"`
	BytesValue  *string         `json:"bytesValue"`
}

type otlpArrayValue struct {
	Values []otlpValue `json:"values"`
}

type otlpKeyValues struct {
	Values []otlpAttribute `json:"values"`
}

// plain flattens an AnyValue into a value the store can marshal. The shapes are
// kept (array, kvlist) rather than stringified: a context field that arrived as
// a list should be readable as a list on the event page.
func (v otlpValue) plain() any {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.BoolValue != nil:
		return *v.BoolValue
	case v.IntValue != "":
		if n, err := v.IntValue.Int64(); err == nil {
			return n
		}
		return v.IntValue.String()
	case v.DoubleValue != nil:
		return *v.DoubleValue
	case v.ArrayValue != nil:
		out := make([]any, 0, len(v.ArrayValue.Values))
		for _, item := range v.ArrayValue.Values {
			out = append(out, item.plain())
		}
		return out
	case v.KVListValue != nil:
		return attrsToMap(v.KVListValue.Values)
	case v.BytesValue != nil:
		return *v.BytesValue
	}
	return ""
}

// message renders a log body as the string a Monolog record would carry at the
// same position. A non-string body is JSON-rendered: printing a Go map would
// make the message column unsearchable and unreadable.
func (v otlpValue) message() string {
	if v.StringValue != nil {
		return *v.StringValue
	}
	flat := v.plain()
	if s, ok := flat.(string); ok {
		return s
	}
	buf, err := json.Marshal(flat)
	if err != nil {
		return ""
	}
	return string(buf)
}

// attrsToMap flattens an attribute list onto a plain map. A repeated key keeps
// the LAST value: OTel forbids duplicates, and last-wins is what every other
// attribute reader in this repo does with a conflicting pair.
func attrsToMap(attrs []otlpAttribute) map[string]any {
	out := make(map[string]any, len(attrs))
	for _, a := range attrs {
		key := strings.TrimSpace(a.Key)
		if key == "" {
			continue
		}
		out[key] = a.Value.plain()
	}
	return out
}

// firstString returns the first non-empty string among keys. Attribute names are
// the OTel semantic conventions, which renamed fields between stable versions:
// service.name is stable, deployment.environment became
// deployment.environment.name, so both spellings are read.
func firstString(attrs map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := attrs[key].(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	}
	return ""
}

type otlpResource struct {
	Attributes []otlpAttribute `json:"attributes"`
}

type otlpScope struct {
	Name string `json:"name"`
}

type otlpStatus struct {
	Code int `json:"code"`
}

// otlpSpan is one OTLP span as the JSON encoding carries it. startTimeUnixNano
// and endTimeUnixNano are the producer's own clock and are used only to measure
// offsets inside one trace — see traceOrigins.
type otlpSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	ParentSpanID      string          `json:"parentSpanId"`
	Name              string          `json:"name"`
	Kind              int             `json:"kind"`
	StartTimeUnixNano json.Number     `json:"startTimeUnixNano"`
	EndTimeUnixNano   json.Number     `json:"endTimeUnixNano"`
	Status            otlpStatus      `json:"status"`
	Attributes        []otlpAttribute `json:"attributes"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpTracesEnvelope struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpLogRecord struct {
	TimeUnixNano         json.Number     `json:"timeUnixNano"`
	ObservedTimeUnixNano json.Number     `json:"observedTimeUnixNano"`
	SeverityText         string          `json:"severityText"`
	SeverityNumber       int             `json:"severityNumber"`
	Body                 otlpValue       `json:"body"`
	Attributes           []otlpAttribute `json:"attributes"`
}

type otlpScopeLogs struct {
	Scope      otlpScope       `json:"scope"`
	LogRecords []otlpLogRecord `json:"logRecords"`
}

type otlpResourceLogs struct {
	Resource  otlpResource    `json:"resource"`
	ScopeLogs []otlpScopeLogs `json:"scopeLogs"`
}

type otlpLogsEnvelope struct {
	ResourceLogs []otlpResourceLogs `json:"resourceLogs"`
}

// otlpSpanGroup is one span plus the resource identity it inherited. The service
// lives on the RESOURCE, so it has to travel with each span: one envelope
// legitimately carries several services, because a collector batches whatever
// its pipeline held at flush time.
type otlpSpanGroup struct {
	service string
	env     string
	span    otlpSpan
}

// otlpLogGroup is one log record plus the identity and JSON path it came from.
// The path is carried so a dropped or repaired record names where it was.
type otlpLogGroup struct {
	service string
	env     string
	scope   string
	path    string
	record  otlpLogRecord
}

// TracesHandler accepts one OTLP JSON traces envelope on POST /v1/traces.
//
// Every span becomes a row in the v2 spans table, which is the same table the
// first-party producers write, so the journey materialiser never learns a second
// input shape.
func TracesHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project := otlpProject(r, st)
		body, err := io.ReadAll(io.LimitReader(r.Body, otlpMaxBody))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var env otlpTracesEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			captureRaw(st, project, rawRouteTraces, body, false, "body: not an otlp json envelope")
			http.Error(w, "invalid otlp traces envelope", http.StatusBadRequest)
			return
		}
		groups := flattenSpans(env)
		// Ids are checked BEFORE any store write, so a malformed envelope leaves
		// no service row, no route row and no span behind.
		if err := validateOTLPSpans(groups); err != nil {
			captureRaw(st, project, rawRouteTraces, body, false, err.Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !chargeItemBatch(w, r, ItemSpans, len(groups)) {
			captureRaw(st, project, rawRouteTraces, body, false, "span budget exhausted")
			return
		}

		d := newDigest(st, project, rawRouteTraces)
		origins := traceOrigins(groups)
		services := map[string]serviceRef{}
		stored := map[string]map[string]bool{}
		rows := make([]store.TraceSpan, 0, len(groups))
		duplicates := 0
		for _, g := range groups {
			svc, ok := services[g.service]
			if !ok {
				svc = resolveService(st, project, g.service, g.env, "otlp")
				services[g.service] = svc
			}
			seen, ok := stored[g.span.TraceID]
			if !ok {
				seen = storedSpanIDs(st, g.span.TraceID)
				stored[g.span.TraceID] = seen
			}
			// A collector retries a batch it did not get a 2xx for, and a second
			// row for the same span would count one hop twice in the journey.
			// Marking it here also dedupes a span the envelope repeated.
			if seen[g.span.SpanID] {
				duplicates++
				continue
			}
			seen[g.span.SpanID] = true
			rows = append(rows, g.row(st, project, svc, origins, d))
		}

		if len(rows) > 0 {
			if err := st.InsertSpans(rows); err != nil {
				log.Printf("otlp spans: %v", err)
				captureRaw(st, project, rawRouteTraces, body, false, "store: "+err.Error())
				http.Error(w, "store", http.StatusInternalServerError)
				return
			}
		}
		captureRaw(st, project, rawRouteTraces, body, true, "")

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"spans":      len(rows),
			"duplicates": duplicates,
			"trace_ids":  traceIDs(groups),
		})
	}
}

// row maps one OTLP span onto a store row.
func (g otlpSpanGroup) row(st *store.Store, project string, svc serviceRef, origins map[string]int64, d *digest) store.TraceSpan {
	span := g.span
	attrs := attrsToMap(span.Attributes)
	start, hasStart := unixNano(span.StartTimeUnixNano)
	end, hasEnd := unixNano(span.EndTimeUnixNano)

	startMs := 0.0
	if hasStart {
		startMs = float64(start-origins[span.TraceID]) / 1e6
	}
	durationMs := 0.0
	switch {
	case hasStart && hasEnd && end > start:
		durationMs = float64(end-start) / 1e6
	case hasStart && hasEnd:
		d.note("span."+span.SpanID+".endTimeUnixNano", "ends before it starts, duration stored as 0")
	}
	if startMs < 0 {
		// Only reachable when the trace has no start time of its own, because the
		// origin IS the earliest start; clamping keeps the waterfall ordered.
		startMs = 0
	}

	kind := otlpSpanKind(span.Kind)
	peer := firstString(attrs, "peer.service", "net.peer.name")
	peerID := int64(0)
	if peer != "" {
		peerID = resolveService(st, project, peer, "", "otlp peer").ID
	}
	route := firstString(attrs, "http.route", "http.target", "url.path")
	return store.TraceSpan{
		TraceID:       span.TraceID,
		SpanID:        span.SpanID,
		ParentSpanID:  otlpParentID(span.ParentSpanID),
		ServiceID:     svc.ID,
		RouteID:       upsertRoute(st, svc.ID, route, kind, d),
		Kind:          kind,
		Name:          truncate(derefOr(strings.TrimSpace(span.Name), kind), maxSpanName),
		StartMs:       startMs,
		DurationMs:    durationMs,
		Status:        otlpSpanStatus(span.Status.Code),
		PeerServiceID: peerID,
		// The producer stated this trace id, so its provenance is "adopted" —
		// the same word every other route uses for an id it did not mint.
		Attributes: spanAttributes(TraceOriginAdopted, peer),
	}
}

// flattenSpans walks the envelope's three levels. A resource with no
// service.name leaves the identity empty, which resolveService labels with the
// project key: the v1 rollout path, applied to a resource that said nothing.
func flattenSpans(env otlpTracesEnvelope) []otlpSpanGroup {
	var out []otlpSpanGroup
	for _, rs := range env.ResourceSpans {
		attrs := attrsToMap(rs.Resource.Attributes)
		service := firstString(attrs, "service.name")
		envName := firstString(attrs, "deployment.environment", "deployment.environment.name")
		for _, ss := range rs.ScopeSpans {
			for _, span := range ss.Spans {
				out = append(out, otlpSpanGroup{service: service, env: envName, span: span})
			}
		}
	}
	return out
}

// validateOTLPSpans refuses the envelope on the first id that cannot be trusted.
func validateOTLPSpans(groups []otlpSpanGroup) error {
	for i, g := range groups {
		if !validOTLPID(g.span.TraceID, otlpTraceIDLen) {
			return fmt.Errorf("%w: span %d traceId %q", ErrOTLPTraceID, i, g.span.TraceID)
		}
		if !validOTLPID(g.span.SpanID, otlpSpanIDLen) {
			return fmt.Errorf("%w: span %d spanId %q", ErrOTLPSpanID, i, g.span.SpanID)
		}
		parent := otlpParentID(g.span.ParentSpanID)
		if parent != "" && !validOTLPID(parent, otlpSpanIDLen) {
			return fmt.Errorf("%w: span %d parentSpanId %q", ErrOTLPParentSpanID, i, g.span.ParentSpanID)
		}
	}
	return nil
}

// validOTLPID is exact length, lowercase hex, and not the all-zero sentinel. The
// sentinel is the W3C's "no id": storing it would key every anonymous span of
// every project onto one trace.
func validOTLPID(id string, n int) bool {
	return len(id) == n && isLowerHex(id) && strings.Trim(id, "0") != ""
}

// otlpParentID normalises a parent. OTLP pads an absent parent with zeros rather
// than omitting it, and the materialiser reads an empty parent as the trace root.
func otlpParentID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" || strings.Trim(id, "0") == "" {
		return ""
	}
	return id
}

// traceOrigins is the per-trace zero that makes start_ms relative: the earliest
// span start in that trace.
//
// It exists because a span's absolute time is a value from ITS host's clock, and
// two hosts disagree (pre-mortem: cross-host clock skew). The journeys
// materialiser and the request waterfall read only this relative offset, so no
// part of this server compares a stored start_ms against a wall clock — that is
// what keeps the hop order stable when a fleet's clocks are not.
func traceOrigins(groups []otlpSpanGroup) map[string]int64 {
	origins := map[string]int64{}
	seen := map[string]bool{}
	for _, g := range groups {
		start, ok := unixNano(g.span.StartTimeUnixNano)
		if !ok {
			continue
		}
		id := g.span.TraceID
		if !seen[id] || start < origins[id] {
			origins[id] = start
			seen[id] = true
		}
	}
	return origins
}

// unixNano reads a proto3 JSON int64 nanosecond count. OTLP JSON sends it as a
// STRING; a hand-rolled client sends a number, so both decode. A value this
// function cannot read is reported as absent rather than defaulted to zero: zero
// is the epoch, which would sort every such span first.
func unixNano(n json.Number) (int64, bool) {
	if n == "" {
		return 0, false
	}
	if v, err := n.Int64(); err == nil {
		return v, true
	}
	f, err := n.Float64()
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return int64(f), true
}

// otlpSpanKind maps OTLP's numeric kind onto the store's string kind. 2 (server)
// and 3 (client) are the two the materialiser branches on: a client span is the
// caller's record of an outbound call, everything else is a leg the reporting
// service served itself. An unknown number falls back to internal, the honest
// "we cannot tell whether this crossed a boundary".
func otlpSpanKind(kind int) string {
	switch kind {
	case 2:
		return "server"
	case 3:
		return "client"
	case 4:
		return "producer"
	case 5:
		return "consumer"
	}
	return "internal"
}

// otlpSpanStatus maps status.code onto the waterfall's vocabulary: 2 is error,
// everything else is ok. 0 (unset) is stored as ok because unset is a producer
// saying nothing, not a failure, and the gap rows are what name silence.
func otlpSpanStatus(code int) string {
	if code == 2 {
		return "error"
	}
	return "ok"
}

// storedSpanIDs is the idempotency guard's read. It is bounded by ONE trace, so a
// resent batch costs a single indexed lookup instead of a table scan.
func storedSpanIDs(st *store.Store, traceID string) map[string]bool {
	out := map[string]bool{}
	if st == nil {
		return out
	}
	spans, err := st.TraceSpans(traceID)
	if err != nil {
		log.Printf("otlp: read trace %s: %v", traceID, err)
		return out
	}
	for _, span := range spans {
		out[span.SpanID] = true
	}
	return out
}

// traceIDs lists the envelope's distinct traces, sorted so two identical
// envelopes answer the same bytes.
func traceIDs(groups []otlpSpanGroup) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		if seen[g.span.TraceID] {
			continue
		}
		seen[g.span.TraceID] = true
		out = append(out, g.span.TraceID)
	}
	sort.Strings(out)
	return out
}

// chargeItemBatch charges n events of one item dimension and answers 429 with
// Retry-After when the batch does not fit.
//
// The profile route DROPS an oversized span batch because the error or profile
// around it is the primary record. Here the batch IS the payload, so dropping it
// while answering 200 would tell the collector it was stored and lose it for
// good; a 429 lets the exporter retry it.
func chargeItemBatch(w http.ResponseWriter, r *http.Request, item string, n int) bool {
	if n <= 0 {
		return true
	}
	a, _ := r.Context().Value(admissionKey{}).(*admission)
	if a == nil || a.shaper == nil {
		return true
	}
	ok, retry := a.shaper.allowItems(a.project, item, n)
	if ok {
		return true
	}
	a.shaper.rejected.Add(1)
	writeRetryAfter(w, retry)
	http.Error(w, "ingest "+item+" budget exhausted", http.StatusTooManyRequests)
	return false
}

// LogsHandler accepts one OTLP JSON logs envelope on POST /v1/logs.
//
// Each record lands through store.RecordLog, the exact path a Monolog line takes,
// so an OTel-native service and a Laravel service share one log stream, one
// fingerprinting rule and one level facet. Nothing here writes a second log
// table or a parallel payload shape.
func LogsHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project := otlpProject(r, st)
		body, err := io.ReadAll(io.LimitReader(r.Body, otlpMaxBody))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var env otlpLogsEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			captureRaw(st, project, rawRouteLogs, body, false, "body: not an otlp json envelope")
			http.Error(w, "invalid otlp logs envelope", http.StatusBadRequest)
			return
		}
		records := flattenLogs(env)
		if !chargeItemBatch(w, r, ItemLogs, len(records)) {
			captureRaw(st, project, rawRouteLogs, body, false, "log budget exhausted")
			return
		}

		d := newDigest(st, project, rawRouteLogs)
		// The services table is the install's producer inventory, so a collector
		// posting logs is admitted as a producer identity even though a log row
		// carries no service column. One upsert per distinct name per request.
		admits := map[string]serviceRef{}
		stored := 0
		for _, g := range records {
			if _, ok := admits[g.service]; !ok {
				admits[g.service] = resolveService(st, project, g.service, g.env, "otlp logs")
			}
			message := strings.TrimSpace(g.record.Body.message())
			if message == "" {
				d.note(g.path+".body", "empty, record dropped")
				continue
			}
			attrs := attrsToMap(g.record.Attributes)
			// The channel is what the logs view filters on. Monolog names it
			// explicitly; an OTel producer names the instrumentation scope, which
			// is the closest thing the envelope carries to a log channel.
			channel := firstString(attrs, "log.channel")
			if channel == "" {
				channel = truncate(g.scope, 64)
			}
			if channel == "" {
				channel = "default"
			}
			delete(attrs, "log.channel")
			if _, err := st.RecordLog(project, channel, otlpLogLevel(g.record), truncate(message, 4<<10), attrs, g.env); err != nil {
				log.Printf("otlp logs: %v", err)
				captureRaw(st, project, rawRouteLogs, body, false, "store: "+err.Error())
				http.Error(w, "store", http.StatusInternalServerError)
				return
			}
			stored++
		}
		captureRaw(st, project, rawRouteLogs, body, true, "")

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"logs": stored})
	}
}

// flattenLogs walks resource -> scope -> record. The JSON path is built as it
// walks so a refused or repaired record can name its own position.
func flattenLogs(env otlpLogsEnvelope) []otlpLogGroup {
	var out []otlpLogGroup
	for i, rl := range env.ResourceLogs {
		attrs := attrsToMap(rl.Resource.Attributes)
		service := firstString(attrs, "service.name")
		envName := firstString(attrs, "deployment.environment", "deployment.environment.name")
		for j, sl := range rl.ScopeLogs {
			for k, record := range sl.LogRecords {
				out = append(out, otlpLogGroup{
					service: service,
					env:     envName,
					scope:   sl.Scope.Name,
					path:    fmt.Sprintf("resourceLogs[%d].scopeLogs[%d].logRecords[%d]", i, j, k),
					record:  record,
				})
			}
		}
	}
	return out
}

// otlpLogLevel reads the severity a Monolog record would carry: severityText
// lower-cased (the wire vectors' own spelling), and severityNumber as the
// fallback for a producer that sent only the number. "info" is the same default
// the Monolog leg applies to a line that named no level.
func otlpLogLevel(record otlpLogRecord) string {
	if text := strings.ToLower(strings.TrimSpace(record.SeverityText)); text != "" {
		return truncate(text, 16)
	}
	switch {
	case record.SeverityNumber >= 21:
		return "fatal"
	case record.SeverityNumber >= 17:
		return "error"
	case record.SeverityNumber >= 13:
		return "warning"
	case record.SeverityNumber >= 9:
		return "info"
	case record.SeverityNumber >= 5:
		return "debug"
	case record.SeverityNumber >= 1:
		return "trace"
	}
	return "info"
}

func derefOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
