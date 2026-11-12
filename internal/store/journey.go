package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"path"
	"sort"
	"strings"
	"time"
)

// Journey is the materialised cross-service path: one row per distinct
// (entry_kind, ordered route pattern list). It is recomputed from spans, never
// written by ingest directly, so a producer's reporting order cannot split one
// path into duplicate journeys.
type Journey struct {
	ID           int64
	PatternKey   string
	EntryKind    string
	EntryRouteID int64
	HopCount     int64
	Samples      int64
	P50Ms        float64
	P95Ms        float64
	FirstSeenAt  time.Time
	LastSeenAt   time.Time
}

// JourneyHop is one step of a journey: the route at a position plus its
// latency share. An outbound span with no matching child row materialises as
// status "unreported" here, so a gap is a named hop, never a blank.
type JourneyHop struct {
	JourneyID int64
	Position  int
	RouteID   int64
	Status    string
	P95Ms     float64
	Share     float64
}

// UpsertJourney replaces one journey row and its hops atomically. pattern_key
// is the natural key: a recompute that lands the same path again updates the
// aggregates instead of minting a duplicate, and the hop list is rewritten
// wholesale so positions never drift out of order between recomputes.
func (s *Store) UpsertJourney(j Journey, hops []JourneyHop) (int64, error) {
	now := time.Now().Unix()
	first := j.FirstSeenAt
	if first.IsZero() {
		first = time.Unix(now, 0)
	}
	last := j.LastSeenAt
	if last.IsZero() {
		last = time.Unix(now, 0)
	}
	tx, err := s.wdb.Begin()
	if err != nil {
		return 0, fmt.Errorf("upsert journey: begin: %w", err)
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRow(`INSERT INTO journeys
		(pattern_key, entry_kind, entry_route_id, hop_count, samples, p50_ms, p95_ms, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(pattern_key) DO UPDATE SET
			entry_kind = excluded.entry_kind,
			entry_route_id = excluded.entry_route_id,
			hop_count = excluded.hop_count,
			samples = excluded.samples,
			p50_ms = excluded.p50_ms,
			p95_ms = excluded.p95_ms,
			last_seen_at = excluded.last_seen_at
		RETURNING id`,
		j.PatternKey, j.EntryKind, nullableID(j.EntryRouteID), j.HopCount, j.Samples,
		j.P50Ms, j.P95Ms, first.Unix(), last.Unix()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert journey: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM journey_hops WHERE journey_id = ?`, id); err != nil {
		return 0, fmt.Errorf("upsert journey: clear hops: %w", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO journey_hops (journey_id, position, route_id, status, p95_ms, share)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("upsert journey: prepare hops: %w", err)
	}
	defer stmt.Close()
	for _, h := range hops {
		status := h.Status
		if status == "" {
			status = "ok"
		}
		if _, err := stmt.Exec(id, h.Position, nullableID(h.RouteID), status, h.P95Ms, h.Share); err != nil {
			return 0, fmt.Errorf("upsert journey: insert hop %d: %w", h.Position, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("upsert journey: commit: %w", err)
	}
	return id, nil
}

// Journeys lists a project's materialised paths, most painful first, by
// joining the entry route back to its owning service. entryKind "" means
// every kind.
func (s *Store) Journeys(projectKey, entryKind string, limit int) ([]Journey, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.rdb.Query(`SELECT j.id, j.pattern_key, j.entry_kind, j.entry_route_id,
			j.hop_count, j.samples, j.p50_ms, j.p95_ms, j.first_seen_at, j.last_seen_at
		FROM journeys j
		JOIN routes r ON r.id = j.entry_route_id
		JOIN services s ON s.id = r.service_id
		WHERE (? = '' OR s.project_key = ?) AND (? = '' OR j.entry_kind = ?)
		ORDER BY j.p95_ms DESC
		LIMIT ?`,
		projectKey, projectKey, entryKind, entryKind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Journey
	for rows.Next() {
		var j Journey
		var entryRouteID, first, last sql.NullInt64
		if err := rows.Scan(&j.ID, &j.PatternKey, &j.EntryKind, &entryRouteID, &j.HopCount, &j.Samples,
			&j.P50Ms, &j.P95Ms, &first, &last); err != nil {
			return nil, err
		}
		j.EntryRouteID = entryRouteID.Int64
		if first.Valid {
			j.FirstSeenAt = time.Unix(first.Int64, 0)
		}
		if last.Valid {
			j.LastSeenAt = time.Unix(last.Int64, 0)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// JourneyHops returns one journey's hops in position order.
func (s *Store) JourneyHops(journeyID int64) ([]JourneyHop, error) {
	rows, err := s.rdb.Query(`SELECT journey_id, position, route_id, status, p95_ms, share
		FROM journey_hops WHERE journey_id = ? ORDER BY position`, journeyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JourneyHop
	for rows.Next() {
		var h JourneyHop
		var routeID sql.NullInt64
		var status sql.NullString
		if err := rows.Scan(&h.JourneyID, &h.Position, &routeID, &status, &h.P95Ms, &h.Share); err != nil {
			return nil, err
		}
		h.RouteID = routeID.Int64
		h.Status = status.String
		out = append(out, h)
	}
	return out, rows.Err()
}

// The derived layer: what this server computes FROM spans instead of reading
// from a producer. Every function here is idempotent, because the ticker re-runs
// each of them over a trailing window and a second pass must land the same rows.

// Entry kinds a journey is filed under. "api" is a request a server answered,
// "page" is a document a browser rendered. They stay apart because their budgets
// and their owners differ, and a page p95 that mixed in a backend route would be
// a number nobody can act on.
const (
	EntryKindAPI  = "api"
	EntryKindPage = "page"
)

// Span kinds the materialiser branches on. These are the values ingest writes
// (spanKind in internal/ingest), so they are the ones that reach this table. A
// client span is the caller's record of an outbound call; every other kind is a
// leg the reporting service served itself.
const (
	spanKindClient   = "client"
	spanKindBrowser  = "browser"
	spanKindDocument = "document"
)

// Hop statuses. "unreported" is the gap: an outbound call whose callee recorded
// nothing, drawn as a named row instead of a blank. "error" keeps a failed hop
// distinguishable from a fast one, and neither counts as a coverage gap.
const (
	hopStatusOK         = "ok"
	hopStatusError      = "error"
	hopStatusUnreported = "unreported"
)

// journeyPatternKeyLen is the hex prefix the plan fixes for pattern_key.
const journeyPatternKeyLen = 32

// routeRef is the two facts one route id carries into a journey: the pattern
// that names the hop, and the project that owns it through its service. Loaded
// once per pass, because routes are a bounded set while spans are not.
type routeRef struct {
	Pattern string
	Project string
}

// traceHop is one step of one trace, before it is folded into a journey.
type traceHop struct {
	routeID    int64
	pattern    string
	status     string
	durationMs float64
}

// traceJourney is one trace reduced to the shape a journey row needs.
type traceJourney struct {
	project      string
	entryKind    string
	entryRouteID int64
	durationMs   float64
	firstSeen    int64
	lastSeen     int64
	patterns     []string
	hops         []traceHop
}

// RecomputeJourneys materialises every trace whose spans were stored inside
// [from, to] into journeys and journey_hops. The window is the only bound on the
// read: the trailing tick window is what keeps the pass finite, so no row cap is
// layered on top — a cap would cut a trace in half and materialise a journey the
// fleet never ran.
//
// pattern_key is the natural key of the result, so the pass is idempotent: the
// same window twice updates the same rows instead of minting duplicates. The
// window is also the interval the aggregates describe, so the row always reports
// the most recent materialisation, never an unbounded running total.
func (s *Store) RecomputeJourneys(from, to time.Time) error {
	spans, err := s.spansBetween(from, to)
	if err != nil {
		return err
	}
	if len(spans) == 0 {
		return nil
	}
	routes, services, err := s.journeyIndex()
	if err != nil {
		return err
	}

	aggs := make(map[string]*journeyAgg)
	for i := 0; i < len(spans); {
		j := i + 1
		for j < len(spans) && spans[j].TraceID == spans[i].TraceID {
			j++
		}
		if tj, ok := buildTraceJourney(spans[i:j], routes, services); ok {
			key := journeyPatternKey(tj.project, tj.entryKind, tj.patterns)
			agg := aggs[key]
			if agg == nil {
				agg = newJourneyAgg(tj)
				aggs[key] = agg
			}
			agg.add(tj)
		}
		i = j
	}

	// Sorted keys make two passes write in the same order, which is what lets a
	// test compare the whole table between them.
	keys := make([]string, 0, len(aggs))
	for key := range aggs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		agg := aggs[key]
		if _, err := s.UpsertJourney(agg.journey(key), agg.hops()); err != nil {
			return err
		}
	}
	return nil
}

// spansBetween reads the window in waterfall order. ORDER BY trace_id walks the
// (trace_id) index, so one materialisation pass groups a trace's spans without a
// second lookup, and start_ms inside that trace is the order the hops happened.
func (s *Store) spansBetween(from, to time.Time) ([]TraceSpan, error) {
	rows, err := s.rdb.Query(`SELECT trace_id, span_id, parent_span_id, service_id, route_id, kind, name,
			start_ms, duration_ms, status, peer_service_id, attributes, created_at
		FROM spans WHERE created_at >= ? AND created_at <= ?
		ORDER BY trace_id ASC, start_ms ASC, span_id ASC`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TraceSpan
	for rows.Next() {
		var sp TraceSpan
		var serviceID, routeID, peerID sql.NullInt64
		var attrs sql.NullString
		var created sql.NullInt64
		if err := rows.Scan(&sp.TraceID, &sp.SpanID, &sp.ParentSpanID, &serviceID, &routeID, &sp.Kind, &sp.Name,
			&sp.StartMs, &sp.DurationMs, &sp.Status, &peerID, &attrs, &created); err != nil {
			return nil, err
		}
		sp.ServiceID = serviceID.Int64
		sp.RouteID = routeID.Int64
		sp.PeerServiceID = peerID.Int64
		sp.Attributes = attrs.String
		if created.Valid {
			sp.CreatedAt = time.Unix(created.Int64, 0)
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// journeyIndex loads the two lookups a materialisation pass needs: route id to
// pattern and owning project, and service id to name (for an unreported hop's
// peer).
func (s *Store) journeyIndex() (map[int64]routeRef, map[int64]string, error) {
	routes := make(map[int64]routeRef)
	rows, err := s.rdb.Query(`SELECT r.id, r.pattern, svc.project_key
		FROM routes r JOIN services svc ON svc.id = r.service_id`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id int64
		var ref routeRef
		if err := rows.Scan(&id, &ref.Pattern, &ref.Project); err != nil {
			rows.Close()
			return nil, nil, err
		}
		routes[id] = ref
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()

	services := make(map[int64]string)
	srows, err := s.rdb.Query(`SELECT id, name FROM services`)
	if err != nil {
		return nil, nil, err
	}
	defer srows.Close()
	for srows.Next() {
		var id int64
		var name string
		if err := srows.Scan(&id, &name); err != nil {
			return nil, nil, err
		}
		services[id] = name
	}
	return routes, services, srows.Err()
}

// buildTraceJourney reduces one trace's ordered spans to its entry, its hop
// chain and its end-to-end duration. ok=false means the trace has no hop this
// server can name (no route anywhere), which is a payload the journey table has
// nothing to say about.
func buildTraceJourney(spans []TraceSpan, routes map[int64]routeRef, services map[int64]string) (traceJourney, bool) {
	var tj traceJourney
	if len(spans) == 0 {
		return tj, false
	}
	// A span is "answered" when some other span claims it as its parent: that is
	// what separates a hop that was reported from one that went silent.
	hasChild := make(map[string]bool, len(spans))
	for _, sp := range spans {
		if sp.ParentSpanID != "" {
			hasChild[sp.ParentSpanID] = true
		}
	}
	root := spans[0]
	for _, sp := range spans {
		if sp.ParentSpanID == "" {
			root = sp
			break
		}
	}
	tj.entryKind = entryKindOf(root.Kind)
	tj.entryRouteID = root.RouteID
	tj.project = routes[root.RouteID].Project
	tj.firstSeen, tj.lastSeen = root.CreatedAt.Unix(), root.CreatedAt.Unix()
	for _, sp := range spans {
		when := sp.CreatedAt.Unix()
		if when < tj.firstSeen {
			tj.firstSeen = when
		}
		if when > tj.lastSeen {
			tj.lastSeen = when
		}
		hop, ok := hopOf(sp, hasChild[sp.SpanID], routes, services)
		if !ok {
			continue
		}
		tj.hops = append(tj.hops, hop)
		tj.patterns = append(tj.patterns, hop.pattern)
	}
	if len(tj.hops) == 0 {
		return tj, false
	}
	// The entry span measured the request, so its duration IS the journey's
	// latency. A producer that reported none falls back to the span extent, so a
	// journey is never filed as a 0ms path while its hops show work.
	tj.durationMs = root.DurationMs
	if tj.durationMs <= 0 {
		tj.durationMs = spanExtent(spans)
	}
	return tj, true
}

// hopOf maps one span onto a hop, and drops the spans that are not hops: internal
// work with no route, and a REPORTED outbound call — the callee's own span sits
// below it, so drawing both would count the same wait twice.
//
// An outbound call with no child is a gap whether or not the caller named its
// peer: the peer names the callee, the route names the address it called, and
// either one is enough to say which leg went silent. Only a span that names
// neither is dropped, because there would be nothing to report about it.
func hopOf(sp TraceSpan, hasChild bool, routes map[int64]routeRef, services map[int64]string) (traceHop, bool) {
	if sp.Kind == spanKindClient {
		if hasChild {
			return traceHop{}, false
		}
		// The callee never reported. Name the hop after the route the caller
		// recorded if it named one, and after the peer service otherwise, so a gap
		// reads as "backend went silent" rather than as an anonymous blank.
		pattern := routes[sp.RouteID].Pattern
		if pattern == "" {
			pattern = services[sp.PeerServiceID]
		}
		if pattern == "" {
			return traceHop{}, false
		}
		return traceHop{routeID: sp.RouteID, pattern: pattern, status: hopStatusUnreported, durationMs: sp.DurationMs}, true
	}
	ref, ok := routes[sp.RouteID]
	if !ok || ref.Pattern == "" {
		return traceHop{}, false
	}
	status := hopStatusOK
	if sp.Status == hopStatusError {
		status = hopStatusError
	}
	return traceHop{routeID: sp.RouteID, pattern: ref.Pattern, status: status, durationMs: sp.DurationMs}, true
}

// spanExtent is the wall time the trace covers: the last span to finish minus the
// first to start. The offsets are relative to the payload's own start, so this
// needs no clock agreement between hosts.
func spanExtent(spans []TraceSpan) float64 {
	start, end := spans[0].StartMs, spans[0].StartMs+spans[0].DurationMs
	for _, sp := range spans {
		if sp.StartMs < start {
			start = sp.StartMs
		}
		if finish := sp.StartMs + sp.DurationMs; finish > end {
			end = finish
		}
	}
	if end <= start {
		return 0
	}
	return end - start
}

// entryKindOf maps a root span's kind onto the two kinds a journeys page filters
// by. A document a browser rendered is a page; everything else is an api entry.
// The fallback is deliberate: a producer that names no kind is stored as
// "internal", and inventing a third entry kind would hide it from both filters.
func entryKindOf(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case spanKindBrowser, spanKindDocument, "page", "navigation":
		return EntryKindPage
	}
	return EntryKindAPI
}

// journeyPatternKey is hex(sha256(project | entry_kind | patterns joined))[:32].
// The plan's input is entry_kind plus the route patterns joined by ">"; the
// project is PREFIXED to it because pattern_key is the unique upsert key: without
// the project, two tenants whose route names coincide — every install has
// "GET /" — would overwrite each other's row, and one project's page would
// report the other's samples.
func journeyPatternKey(project, entryKind string, patterns []string) string {
	sum := sha256.Sum256([]byte(project + "|" + entryKind + "|" + strings.Join(patterns, ">")))
	return hex.EncodeToString(sum[:])[:journeyPatternKeyLen]
}

// journeyAgg folds every trace that shares one pattern key into the numbers the
// journey row carries.
type journeyAgg struct {
	entryKind    string
	entryRouteID int64
	samples      int64
	firstSeen    int64
	lastSeen     int64
	totals       []float64
	hopDurations [][]float64
	hopRoutes    []int64
	hopStatuses  []string
}

func newJourneyAgg(tj traceJourney) *journeyAgg {
	agg := &journeyAgg{
		entryKind:    tj.entryKind,
		entryRouteID: tj.entryRouteID,
		firstSeen:    tj.firstSeen,
		lastSeen:     tj.lastSeen,
		hopRoutes:    make([]int64, len(tj.hops)),
		hopStatuses:  make([]string, len(tj.hops)),
		hopDurations: make([][]float64, len(tj.hops)),
	}
	for i, hop := range tj.hops {
		agg.hopRoutes[i] = hop.routeID
		agg.hopStatuses[i] = hop.status
	}
	return agg
}

func (a *journeyAgg) add(tj traceJourney) {
	a.samples++
	if tj.firstSeen < a.firstSeen {
		a.firstSeen = tj.firstSeen
	}
	if tj.lastSeen > a.lastSeen {
		a.lastSeen = tj.lastSeen
	}
	a.totals = append(a.totals, tj.durationMs)
	for i, hop := range tj.hops {
		if i < len(a.hopDurations) {
			a.hopDurations[i] = append(a.hopDurations[i], hop.durationMs)
		}
	}
}

func (a *journeyAgg) journey(key string) Journey {
	p50, p95 := percentileOf(a.totals)
	return Journey{
		PatternKey:   key,
		EntryKind:    a.entryKind,
		EntryRouteID: a.entryRouteID,
		HopCount:     int64(len(a.hopRoutes)),
		Samples:      a.samples,
		P50Ms:        p50,
		P95Ms:        p95,
		FirstSeenAt:  time.Unix(a.firstSeen, 0),
		LastSeenAt:   time.Unix(a.lastSeen, 0),
	}
}

// hops renders the hop chain. share is the hop's own p95 as a fraction of the
// journey's p95 — a share of the whole, never a partition of it: nested hops each
// claim their own wait, so the shares may add up past 1, and that is the honest
// picture of a chain where a caller waits on its callee.
func (a *journeyAgg) hops() []JourneyHop {
	_, p95 := percentileOf(a.totals)
	out := make([]JourneyHop, 0, len(a.hopRoutes))
	for i := range a.hopRoutes {
		_, hopP95 := percentileOf(a.hopDurations[i])
		share := 0.0
		if p95 > 0 {
			share = math.Min(1, hopP95/p95)
		}
		out = append(out, JourneyHop{
			Position: i,
			RouteID:  a.hopRoutes[i],
			Status:   a.hopStatuses[i],
			P95Ms:    hopP95,
			Share:    share,
		})
	}
	return out
}

// percentileOf returns p50 and p95 of an unsorted sample set.
func percentileOf(values []float64) (p50, p95 float64) {
	if len(values) == 0 {
		return 0, 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return percentileValue(sorted, 50), percentileValue(sorted, 95)
}

// percentileValue reads the p-th percentile of an ASCENDING slice with the index
// rule ProfileRouteStats already uses (index n*p/100, clamped to the last row).
// One convention across the store is what makes a journey's p95 and a route's p95
// comparable numbers instead of two near-misses.
func percentileValue(sorted []float64, pct int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := len(sorted) * pct / 100
	if idx > len(sorted)-1 {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// JourneyCoverage counts a journey's reported hops against its total. It is the
// number that says whether a p95 describes the whole path or only the part the
// fleet bothered to report.
type JourneyCoverage struct {
	Reported int64
	Total    int64
}

// Pct is 100 for a fully reported chain and 0 for a journey with no hops.
func (c JourneyCoverage) Pct() float64 {
	if c.Total == 0 {
		return 0
	}
	return float64(c.Reported) / float64(c.Total) * 100
}

// Complete reports whether every hop was actually reported.
func (c JourneyCoverage) Complete() bool { return c.Total > 0 && c.Reported == c.Total }

// String renders the "2/3" form the journeys table prints.
func (c JourneyCoverage) String() string { return fmt.Sprintf("%d/%d", c.Reported, c.Total) }

// JourneyCoverage reads one journey's hop statuses.
func (s *Store) JourneyCoverage(journeyID int64) (JourneyCoverage, error) {
	var c JourneyCoverage
	var unreported int64
	if err := s.rdb.QueryRow(`SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN status = '`+hopStatusUnreported+`' THEN 1 ELSE 0 END), 0)
		FROM journey_hops WHERE journey_id = ?`, journeyID).Scan(&c.Total, &unreported); err != nil {
		return c, err
	}
	c.Reported = c.Total - unreported
	return c, nil
}

// HotspotFrameScope selects which frames a hotspot aggregate keeps.
type HotspotFrameScope int

const (
	// HotspotAppFrames keeps application frames only — the default, because a
	// framework's own hot function is not a defect this team can fix, and a
	// dependency version bump would otherwise rewrite the whole leaderboard.
	HotspotAppFrames HotspotFrameScope = iota
	// HotspotAllFrames keeps every frame: the ?frames=all disclosure.
	HotspotAllFrames
)

// HotspotAttribution is the hotspot leaderboard with the frame scope the request
// page asks for. Filtering after aggregation is exact rather than approximate:
// the aggregate groups by (file, function), so a frame is either application code
// in every sample or in none of them.
func (s *Store) HotspotAttribution(projectKey string, scope HotspotFrameScope, sampleLimit int) ([]HotspotAgg, error) {
	all, err := s.SlowestCode(projectKey, sampleLimit)
	if err != nil {
		return nil, err
	}
	if scope == HotspotAllFrames {
		return all, nil
	}
	out := make([]HotspotAgg, 0, len(all))
	for _, h := range all {
		if hotspotFrameIgnored(h.Function, h.File) {
			continue
		}
		out = append(out, h)
	}
	return out, nil
}

// hotspotFrameIgnored applies the grouper's own policy (defaultStackIgnore) to a
// profile frame, so "application code" means the same thing on the issue list and
// on the performance page. A frame with neither a file nor a function cannot be
// classified and is dropped: that is a producer bug, not attribution.
func hotspotFrameIgnored(inFunction, inFile string) bool {
	fn := strings.TrimSpace(inFunction)
	file := strings.ReplaceAll(inFile, "\\", "/")
	if fn == "" && file == "" {
		return true
	}
	if strings.HasPrefix(fn, "runtime.") || strings.HasPrefix(fn, "runtime/") {
		return true
	}
	if strings.Contains(file, "/runtime/") || strings.HasPrefix(file, "runtime/") {
		return true
	}
	for _, raw := range defaultStackIgnore {
		pattern := strings.TrimSpace(raw)
		if pattern == "" {
			continue
		}
		if strings.ContainsAny(pattern, "*?[") {
			if ok, err := path.Match(pattern, file); err == nil && ok {
				return true
			}
			continue
		}
		if file != "" && strings.Contains(file, pattern) {
			return true
		}
	}
	return false
}

// MissedJob is one scheduled row whose promised next run never arrived.
type MissedJob struct {
	ProjectKey   string
	Kind         string
	Name         string
	ExpectedAt   int64
	GraceSeconds int64
	LastRunAt    int64
}

// MarkMissedJobs stamps every scheduled run whose next_expected_at plus its own
// grace window has passed, and returns only the rows it just stamped. The stamp
// is the idempotency: a row already marked 'missed' no longer matches the
// candidate query, so a second sweep inside the same window returns nothing and
// the missed_run rule cannot fire twice for one absence.
func (s *Store) MarkMissedJobs(now time.Time) ([]MissedJob, error) {
	rows, err := s.rdb.Query(`SELECT project_key, kind, name,
			COALESCE(next_expected_at, 0), COALESCE(grace_seconds, 0), COALESCE(last_run_at, 0)
		FROM job_stats
		WHERE next_expected_at IS NOT NULL AND next_expected_at > 0
		  AND last_status <> 'missed'
		  AND next_expected_at + COALESCE(grace_seconds, 0) < ?
		ORDER BY next_expected_at ASC LIMIT 500`, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("missed sweep: %w", err)
	}
	var candidates []MissedJob
	for rows.Next() {
		var m MissedJob
		if err := rows.Scan(&m.ProjectKey, &m.Kind, &m.Name, &m.ExpectedAt, &m.GraceSeconds, &m.LastRunAt); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	marked := make([]MissedJob, 0, len(candidates))
	for _, m := range candidates {
		res, err := s.wdb.Exec(`UPDATE job_stats SET last_status = 'missed'
			WHERE project_key = ? AND kind = ? AND name = ? AND last_status <> 'missed'`,
			m.ProjectKey, m.Kind, m.Name)
		if err != nil {
			return marked, fmt.Errorf("mark missed: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return marked, err
		}
		if n == 1 {
			marked = append(marked, m)
		}
	}
	return marked, nil
}

// MissedJobs lists the rows a sweep marked missed and that have not recovered
// since — the missed_run rule's input. A run that lands clears last_status, so
// recovery is removal from this list rather than a second write.
func (s *Store) MissedJobs(projects []string) ([]MissedJob, error) {
	clause, args := projectClause(projects)
	rows, err := s.rdb.Query(`SELECT project_key, kind, name,
			COALESCE(next_expected_at, 0), COALESCE(grace_seconds, 0), COALESCE(last_run_at, 0)
		FROM job_stats WHERE last_status = 'missed'`+clause+`
		ORDER BY next_expected_at ASC LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MissedJob
	for rows.Next() {
		var m MissedJob
		if err := rows.Scan(&m.ProjectKey, &m.Kind, &m.Name, &m.ExpectedAt, &m.GraceSeconds, &m.LastRunAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetJobSchedule records the promise a scheduled job makes: the next run it
// expects and the grace it asked for. It lives beside the sweep because a
// schedule nothing compares against now is a column nobody reads.
func (s *Store) SetJobSchedule(projectKey, kind, name, cron string, nextExpectedAt, graceSeconds int64) error {
	res, err := s.wdb.Exec(`UPDATE job_stats SET schedule_cron = ?, next_expected_at = ?, grace_seconds = ?, registered_at = ?
		WHERE project_key = ? AND kind = ? AND name = ?`,
		cron, nextExpectedAt, graceSeconds, time.Now().Unix(), projectKey, kind, name)
	if err != nil {
		return fmt.Errorf("set job schedule: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("set job schedule %s/%s/%s: no such job", projectKey, kind, name)
	}
	return nil
}

// SlowQuery is one statement shape whose worst sample crossed a threshold.
type SlowQuery struct {
	ProjectKey  string
	Fingerprint string
	SQL         string
	Count       int64
	MaxMs       float64
	LastSeenAt  int64
}

// SlowQueries lists the statement shapes whose worst sample in the window is at
// or above thresholdMs — the query_slow rule's input. max_ms is the column to
// alert on: a shape that is usually fast and occasionally seconds long is a
// user-visible stall, and its average hides exactly that.
func (s *Store) SlowQueries(since int64, thresholdMs float64, projects []string) ([]SlowQuery, error) {
	clause, args := projectClause(projects)
	q := append([]any{since, thresholdMs}, args...)
	rows, err := s.rdb.Query(`SELECT project_key, fingerprint, sql, count, max_ms, last_seen_at
		FROM query_stats WHERE last_seen_at >= ? AND max_ms >= ?`+clause+`
		ORDER BY max_ms DESC LIMIT 10`, q...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SlowQuery
	for rows.Next() {
		var sq SlowQuery
		if err := rows.Scan(&sq.ProjectKey, &sq.Fingerprint, &sq.SQL, &sq.Count, &sq.MaxMs, &sq.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, sq)
	}
	return out, rows.Err()
}

// JourneyGap is a materialised journey carrying at least one unreported hop — the
// span_unreported rule's input.
type JourneyGap struct {
	ProjectKey   string
	PatternKey   string
	EntryPattern string
	Hops         int64
	Unreported   int64
	LastSeenAt   int64
}

// UnreportedJourneys lists the journeys with a named gap inside the window. It
// reads the materialised hops rather than the raw spans on purpose: the rule fires
// on what the performance page draws, so an alert and the page can never disagree
// about whether a chain is covered.
func (s *Store) UnreportedJourneys(since int64, projects []string) ([]JourneyGap, error) {
	clause, args := projectClause(projects)
	q := append([]any{since}, args...)
	rows, err := s.rdb.Query(`SELECT svc.project_key, j.pattern_key, COALESCE(r.pattern, ''),
			COALESCE(j.hop_count, 0), COUNT(h.position), COALESCE(j.last_seen_at, 0)
		FROM journeys j
		JOIN journey_hops h ON h.journey_id = j.id AND h.status = '`+hopStatusUnreported+`'
		JOIN routes r ON r.id = j.entry_route_id
		JOIN services svc ON svc.id = r.service_id
		WHERE COALESCE(j.last_seen_at, 0) >= ?`+clause+`
		GROUP BY j.id
		ORDER BY j.last_seen_at DESC LIMIT 10`, q...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JourneyGap
	for rows.Next() {
		var g JourneyGap
		if err := rows.Scan(&g.ProjectKey, &g.PatternKey, &g.EntryPattern, &g.Hops, &g.Unreported, &g.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ReleaseCrashPoint is one release's crash-free ratio, computed from session
// outcomes.
type ReleaseCrashPoint struct {
	ProjectKey string
	Version    string
	Sessions   int64
	Crashed    int64
	CrashFree  float64
	StartedAt  int64
}

// ReleaseCrashPoints returns each project's newest releases with their crash-free
// ratio, newest first, at most points rows per project.
//
// It aggregates sessions instead of reading releases.crash_free because that
// column is written by nothing yet: a rule that alerted on a null column would be
// silent for the whole life of the install and would look like a healthy fleet.
func (s *Store) ReleaseCrashPoints(projects []string, points int) ([]ReleaseCrashPoint, error) {
	if points <= 0 || points > 20 {
		points = 5
	}
	clause, args := projectClause(projects)
	rows, err := s.rdb.Query(`SELECT project_key, release, COUNT(*),
			SUM(CASE WHEN status IN ('crashed', 'abnormal') THEN 1 ELSE 0 END),
			COALESCE(MAX(started_at), 0)
		FROM sessions
		WHERE release <> ''`+clause+`
		GROUP BY project_key, release
		ORDER BY MAX(started_at) DESC, release DESC
		LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	kept := make(map[string]int)
	var out []ReleaseCrashPoint
	for rows.Next() {
		var p ReleaseCrashPoint
		if err := rows.Scan(&p.ProjectKey, &p.Version, &p.Sessions, &p.Crashed, &p.StartedAt); err != nil {
			return nil, err
		}
		if kept[p.ProjectKey] >= points {
			continue
		}
		kept[p.ProjectKey]++
		if p.Sessions > 0 {
			p.CrashFree = 1 - float64(p.Crashed)/float64(p.Sessions)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
