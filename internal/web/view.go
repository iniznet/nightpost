package web

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iniznet/nightpost/internal/store"
)

// OccView is a UI-shaped occurrence: the handler layer extracts stacks,
// fields and raw JSON from the stored payload so the templates never walk
// raw JSON structures.
type Frame struct {
	Func string
	File string
	Line string
}

type KV struct {
	K string
	V string
}

type OccView struct {
	When   string
	Kind   string
	Title  string
	Fields []KV
	Stack  []Frame
	Raw    string
}

var jsFrameRe = regexp.MustCompile(`^(?:at\s+)?(.+?)\s*\((.+?):(\d+)(?::\d+)?\)$`)
var jsFrameRe2 = regexp.MustCompile(`^(?:at\s+)?(.+?):(\d+)(?::\d+)?$`)

// buildOccViews converts store.Occurrences rows into renderable views.
func buildOccViews(entries []map[string]any) []OccView {
	out := make([]OccView, 0, len(entries))
	for _, e := range entries {
		v := OccView{}
		v.When, _ = e["seen_at"].(string)
		payload, _ := e["payload"].(map[string]any)
		if payload != nil {
			v.Raw = prettyJSON(payload)
		}
		switch {
		case hasException(payload):
			v.Kind = "exception"
			fillSentry(payload, &v)
		case hasKey(payload, "stack"):
			v.Kind = "js-error"
			fillJSError(payload, &v)
		case hasKey(payload, "channel"):
			v.Kind = "log"
			fillLog(payload, &v)
		default:
			v.Kind = "payload"
			v.Fields = flatten(payload, 15)
		}
		out = append(out, v)
	}
	return out
}

func hasException(p map[string]any) bool {
	exc, ok := p["exception"].(map[string]any)
	if !ok {
		return false
	}
	vals, ok := exc["values"].([]any)
	return ok && len(vals) > 0
}

func hasKey(p map[string]any, k string) bool {
	if p == nil {
		return false
	}
	_, ok := p[k]
	return ok
}

// fillSentry extracts the stored Sentry event: primary exception summary,
// request context fields and the in-app stack frames (SDK frames are
// oldest-first; the UI shows the throwing frame first).
func fillSentry(event map[string]any, v *OccView) {
	exc, _ := event["exception"].(map[string]any)
	values, _ := exc["values"].([]any)
	if len(values) > 0 {
		if first, ok := values[len(values)-1].(map[string]any); ok {
			class, _ := first["type"].(string)
			value, _ := first["value"].(string)
			v.Title = truncate(class+": "+value, 120)
			trace, _ := first["stacktrace"].(map[string]any)
			frames, _ := trace["frames"].([]any)
			for i := len(frames) - 1; i >= 0 && len(v.Stack) < 40; i-- {
				f, ok := frames[i].(map[string]any)
				if !ok {
					continue
				}
				fn, _ := f["function"].(string)
				if fn == "" {
					fn, _ = f["module"].(string)
				}
				file, _ := f["filename"].(string)
				line := fmtVal(f["lineno"])
				if fn == "" && file == "" {
					continue
				}
				v.Stack = append(v.Stack, Frame{Func: fn, File: file, Line: line})
			}
		}
	}
	for _, k := range []string{"level", "environment", "release", "transaction", "server_name"} {
		if s, ok := event[k].(string); ok && s != "" {
			v.Fields = append(v.Fields, KV{K: k, V: s})
		}
	}
	if req, ok := event["request"].(map[string]any); ok {
		if u, ok := req["url"].(string); ok && u != "" {
			v.Fields = append(v.Fields, KV{K: "url", V: u})
		}
		if m, ok := req["method"].(string); ok && m != "" {
			v.Fields = append(v.Fields, KV{K: "method", V: m})
		}
	}
}

// fillJSError extracts the first-party JS error report.
func fillJSError(p map[string]any, v *OccView) {
	msg, _ := p["message"].(string)
	v.Title = truncate(msg, 120)
	stack, _ := p["stack"].(string)
	for _, line := range strings.Split(stack, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Error") {
			continue
		}
		if len(v.Stack) >= 40 {
			break
		}
		v.Stack = append(v.Stack, parseJSFrame(line))
	}
	for _, k := range []string{"url", "route", "environment", "release"} {
		if s, ok := p[k].(string); ok && s != "" {
			v.Fields = append(v.Fields, KV{K: k, V: s})
		}
	}
}

func parseJSFrame(line string) Frame {
	if m := jsFrameRe.FindStringSubmatch(line); m != nil {
		return Frame{Func: m[1], File: m[2], Line: m[3]}
	}
	if m := jsFrameRe2.FindStringSubmatch(line); m != nil {
		return Frame{File: m[1], Line: m[2]}
	}
	return Frame{Func: line}
}

// fillLog extracts the Monolog record shape.
func fillLog(p map[string]any, v *OccView) {
	channel, _ := p["channel"].(string)
	if channel != "" {
		v.Fields = append(v.Fields, KV{K: "channel", V: channel})
	}
	if ctx, ok := p["context"].(map[string]any); ok {
		v.Fields = append(v.Fields, flatten(ctx, 15)...)
	}
}

// flatten renders one map level as key/value rows, sorted for stable order.
func flatten(m map[string]any, cap int) []KV {
	if m == nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]KV, 0, len(keys))
	for _, k := range keys {
		if len(out) >= cap {
			break
		}
		val := m[k]
		if s, ok := val.(string); ok && s == "" {
			continue
		}
		if val == nil {
			continue
		}
		out = append(out, KV{K: k, V: fmtVal(val)})
	}
	return out
}

// fmtVal renders JSON-decoded values for display: scalars inline, nested
// containers as compact JSON.
func fmtVal(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case []any:
		if b, err := json.Marshal(t); err == nil {
			return string(b)
		}
	case map[string]any:
		if b, err := json.Marshal(t); err == nil {
			return string(b)
		}
	default:
		return fmt.Sprintf("%v", t)
	}
	return fmt.Sprintf("%v", v)
}

func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---------------------------------------------------------------------------
// Performance surface views.
//
// These builders only shape store rows into the rows a template prints. They
// are pure on purpose: the hop chain has exactly one spelling across the six
// pages, and the golden test can pin that spelling without an HTTP round trip.
// ---------------------------------------------------------------------------

// hopUnreported is the store's gap status. Spelled here because three builders
// compare a hop against it and a typo would silently drop the dashed row.
const hopUnreported = "unreported"

// routeInfo is the three facts a hop chip prints about the route it names: the
// pattern, the service that owns it, and that service's runtime (which selects
// the producer guide a gap chip links to). Journeys carry route ids only, so
// this is loaded once per page.
type routeInfo struct {
	Pattern string
	Method  string
	Service string
	Project string
	Runtime string
}

// producerDocPath maps a producer runtime onto the guide that tells that stack
// how to report a hop. docs/producers/webhook.md is the generic path: any stack
// that speaks the wire format is documented there, so a gap always has a link.
func producerDocPath(runtime string) string {
	switch strings.ToLower(strings.TrimSpace(runtime)) {
	case "php", "laravel":
		return "/docs/producers/laravel.md"
	case "node", "nodejs", "sveltekit", "bun", "deno":
		return "/docs/producers/sveltekit.md"
	}
	return "/docs/producers/webhook.md"
}

// HopChip is one link of a journey's chain: a route chip, or a dashed gap chip
// naming the producer that owes the hop.
type HopChip struct {
	Position int
	RouteID  int64
	Label    string
	Route    string
	Service  string
	Status   string
	P95Ms    float64
	SharePct float64
	Gap      bool
	Doc      string
}

// hopChip shapes one stored hop. It never returns a blank label and never
// returns a bare dash: an unreported hop is a named absence with a link, which
// is the difference between "we did not measure this" and "this page is broken".
func hopChip(h store.JourneyHop, routes map[int64]routeInfo) HopChip {
	ref, known := routes[h.RouteID]
	chip := HopChip{Position: h.Position + 1, RouteID: h.RouteID, Status: h.Status, P95Ms: h.P95Ms, SharePct: h.Share * 100}
	if h.Status == hopUnreported {
		chip.Gap = true
		if known && ref.Pattern != "" {
			// The caller recorded the route it called, so the gap is nameable
			// down to the address and the service that owes it.
			chip.Route, chip.Service, chip.Label = ref.Pattern, ref.Service, ref.Pattern
			chip.Doc = producerDocPath(ref.Runtime)
		} else {
			// The caller recorded only that it waited on another service, and
			// journey_hops stores the route id without the peer name. Say that in
			// words and point at the generic producer guide: a blank chip reads as
			// a rendering fault rather than as missing telemetry.
			chip.Label = "peer service did not report"
			chip.Doc = producerDocPath("")
		}
		return chip
	}
	if known && ref.Pattern != "" {
		chip.Route, chip.Service, chip.Label = ref.Pattern, ref.Service, ref.Pattern
	} else {
		chip.Label = "unattributed hop"
	}
	return chip
}

// JourneyRow is one row of the Journeys table: the entry, the chain, and the
// four numbers a reader can act on. P95 is the emphasised number; there is no
// total column here because a total belongs to the route a reader opened.
type JourneyRow struct {
	RouteID      int64
	Project      string
	Entry        string
	EntryService string
	EntryKind    string
	Hops         []HopChip
	Requests     int64
	P50Ms        float64
	P95Ms        float64
	Slowest      HopChip
	Covered      int64
	Total        int64
	Coverage     string
	Spark        []float64
}

// journeyInput is one materialised journey with its hops and coverage, read by
// the handler so the builder stays pure.
type journeyInput struct {
	Journey  store.Journey
	Hops     []store.JourneyHop
	Coverage store.JourneyCoverage
}

// buildJourneyRows turns materialised journeys into table rows. spark is the
// row's own hop-p95 series: it is the only per-row series the store holds, and
// the visual system allows exactly one 40 px sparkline beside the numbers.
func buildJourneyRows(in []journeyInput, routes map[int64]routeInfo) []JourneyRow {
	out := make([]JourneyRow, 0, len(in))
	for _, ji := range in {
		row := JourneyRow{
			RouteID:   ji.Journey.EntryRouteID,
			EntryKind: ji.Journey.EntryKind,
			Requests:  ji.Journey.Samples,
			P50Ms:     ji.Journey.P50Ms,
			P95Ms:     ji.Journey.P95Ms,
			Covered:   ji.Coverage.Reported,
			Total:     ji.Coverage.Total,
			Coverage:  ji.Coverage.String(),
			Slowest:   HopChip{SharePct: -1},
		}
		if row.EntryKind == "" {
			row.EntryKind = store.EntryKindAPI
		}
		if ref, ok := routes[ji.Journey.EntryRouteID]; ok {
			row.Entry, row.EntryService, row.Project = ref.Pattern, ref.Service, ref.Project
		} else {
			row.Entry = "unnamed entry"
		}
		for _, h := range ji.Hops {
			chip := hopChip(h, routes)
			row.Hops = append(row.Hops, chip)
			if chip.SharePct >= 0 && chip.P95Ms > row.Slowest.P95Ms {
				row.Slowest = chip
			}
			row.Spark = append(row.Spark, chip.P95Ms)
		}
		if row.Slowest.SharePct < 0 {
			row.Slowest = HopChip{}
		}
		out = append(out, row)
	}
	return out
}

// journeyHasService reports whether a chain touches one service, which is what
// the bar's service dimension narrows a journey to: a hop owner, not the entry.
func journeyHasService(row JourneyRow, service string) bool {
	if service == "" {
		return true
	}
	if row.EntryService == service {
		return true
	}
	for _, h := range row.Hops {
		if h.Service == service {
			return true
		}
	}
	return false
}

// RouteRow is one row of the Routes table. Profile-derived latency is used
// where a producer profiled the route; the chain facts (who calls it, how many
// journeys walk it, whether they reported every hop) come from the materialised
// journeys, so a route that arrived through traces is still visible.
type RouteRow struct {
	RouteID   int64
	Service   string
	Project   string
	Pattern   string
	Method    string
	Requests  int64
	P50Ms     float64
	P95Ms     float64
	MaxMs     float64
	TotalMs   float64
	CalledBy  []string
	Calls     int64
	Covered   int64
	Expected  int64
	Coverage  string
	Profiled  bool
	HopP95Ms  float64
}

type routeAcc struct {
	row     RouteRow
	callers map[string]bool
	hopP95  []float64
}

// buildRouteRows folds journeys and profile rows into one table keyed by
// (project, pattern). A route appears once, whether it arrived as a hop, as a
// profile, or as both.
func buildRouteRows(journeys []JourneyRow, profiles []store.RouteIndexRow, routes map[int64]routeInfo) []RouteRow {
	key := func(project, pattern string) string { return project + "|" + pattern }
	// The id a row links to comes from the route table, so a row that arrived as
	// a profile and never as a hop still opens its own detail page.
	idAt := make(map[string]int64, len(routes))
	for id, ref := range routes {
		idAt[key(ref.Project, ref.Pattern)] = id
		if ref.Method != "" {
			idAt[key(ref.Project, ref.Method+" "+ref.Pattern)] = id
		}
	}
	byKey := map[string]*routeAcc{}
	accOf := func(project, pattern, service string) *routeAcc {
		k := key(project, pattern)
		a := byKey[k]
		if a == nil {
			a = &routeAcc{callers: map[string]bool{}, row: RouteRow{
				RouteID: idAt[k], Project: project, Pattern: pattern, Service: service,
			}}
			byKey[k] = a
		}
		if a.row.Service == "" {
			a.row.Service = service
		}
		return a
	}

	for _, j := range journeys {
		// A route named twice in one chain is still one request through it.
		unique := map[string]HopChip{}
		order := make([]string, 0, len(j.Hops))
		for _, h := range j.Hops {
			if h.Route == "" {
				continue
			}
			if _, dup := unique[h.Route]; !dup {
				unique[h.Route] = h
				order = append(order, h.Route)
			}
		}
		full := j.Total > 0 && j.Covered == j.Total
		for _, pattern := range order {
			h := unique[pattern]
			a := accOf(j.Project, pattern, h.Service)
			a.row.Calls++
			a.row.Requests += j.Requests
			a.row.Expected++
			if full {
				a.row.Covered++
			}
			a.hopP95 = append(a.hopP95, h.P95Ms)
			// "Called by" is the service that ENTERED the chain, not the service
			// that owns the hop: a route nothing calls reads as an entry.
			if h.Route != j.Entry && j.EntryService != "" {
				a.callers[j.EntryService] = true
			}
		}
	}

	// The producer's own spelling of a route is the pattern, or the method
	// prefixed to it; both are accepted so one route never shows up twice.
	profileAt := make(map[string]store.RouteIndexRow, len(profiles))
	for _, p := range profiles {
		profileAt[key(p.Project, p.Name)] = p
	}
	for _, a := range byKey {
		p, ok := profileAt[key(a.row.Project, a.row.Pattern)]
		if !ok && a.row.Method != "" {
			p, ok = profileAt[key(a.row.Project, a.row.Method+" "+a.row.Pattern)]
		}
		if ok {
			a.row.Profiled = true
			a.row.Requests = p.Count
			a.row.P50Ms, a.row.P95Ms, a.row.MaxMs = p.P50Ms, p.P95Ms, p.MaxMs
			a.row.TotalMs = p.TotalMs
		}
	}
	// A route the fleet profiled but no journey has walked yet is still a route.
	for _, p := range profiles {
		if _, seen := byKey[key(p.Project, p.Name)]; seen {
			continue
		}
		k := key(p.Project, p.Name)
		id := idAt[k]
		byKey[k] = &routeAcc{
			callers: map[string]bool{},
			row: RouteRow{Project: p.Project, Pattern: p.Name, Requests: p.Count,
				RouteID: id, Service: routes[id].Service, Method: routes[id].Method,
				P50Ms: p.P50Ms, P95Ms: p.P95Ms, MaxMs: p.MaxMs, TotalMs: p.TotalMs, Profiled: true},
		}
	}

	out := make([]RouteRow, 0, len(byKey))
	for _, a := range byKey {
		row := a.row
		if !row.Profiled && len(a.hopP95) > 0 {
			// Without profiles the only latency the store holds for a hop is each
			// journey's hop p95, so the row shows the worst of them instead of
			// inventing a second percentile convention inside the view layer.
			row.P95Ms = a.hopP95[0]
			for _, v := range a.hopP95[1:] {
				if v > row.P95Ms {
					row.P95Ms = v
				}
			}
			row.HopP95Ms = row.P95Ms
		}
		row.Coverage = strconv.FormatInt(row.Covered, 10) + "/" + strconv.FormatInt(row.Expected, 10)
		names := make([]string, 0, len(a.callers))
		for name := range a.callers {
			names = append(names, name)
		}
		sort.Strings(names)
		row.CalledBy = names
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].P95Ms != out[j].P95Ms {
			return out[i].P95Ms > out[j].P95Ms
		}
		if out[i].Pattern != out[j].Pattern {
			return out[i].Pattern < out[j].Pattern
		}
		return out[i].Project < out[j].Project
	})
	return out
}

// HopSlot is one position of the route-detail distribution: how many journeys
// passed the route at this position and what that position cost.
type HopSlot struct {
	Position int
	Journeys int64
	P95Ms    float64
	SharePct float64
}

// buildHopSlots answers "where in the chain does this route sit, and what does
// that position cost" for the route-detail distribution section.
func buildHopSlots(journeys []JourneyRow, pattern string) []HopSlot {
	byPos := map[int]*HopSlot{}
	for _, j := range journeys {
		for _, h := range j.Hops {
			if h.Route != pattern {
				continue
			}
			slot := byPos[h.Position]
			if slot == nil {
				slot = &HopSlot{Position: h.Position}
				byPos[h.Position] = slot
			}
			slot.Journeys++
			if h.P95Ms > slot.P95Ms {
				slot.P95Ms = h.P95Ms
			}
			if h.SharePct > slot.SharePct {
				slot.SharePct = h.SharePct
			}
		}
	}
	out := make([]HopSlot, 0, len(byPos))
	for _, slot := range byPos {
		out = append(out, *slot)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out
}

// TraceSample is one request a reader can open from the route detail.
type TraceSample struct {
	TraceID  string
	TotalMs  float64
	Hops     int
	Services int
}

// WaterfallRow is one span on the request page's single time axis. left/width
// are percentages of the trace's own extent, so the whole walk shares one
// scale and a nested span is drawn inside its parent rather than beside it.
type WaterfallRow struct {
	SpanID     string
	Name       string
	Service    string
	Kind       string
	Status     string
	StartMs    float64
	DurationMs float64
	Left       float64
	Width      float64
	Depth      int
	Gap        bool
	Peer       string
	Doc        string
}

// HostFrame is one row of the attribution table: an application frame and the
// share of the attributed self time it owns.
type HostFrame struct {
	Function string
	File     string
	SelfMs   float64
	SharePct float64
	Calls    int64
}

// RequestView is the request page: one header line, one axis, one attribution
// table, and every gap the trace contains named.
type RequestView struct {
	TraceID  string
	Services int
	Hops     int
	TotalMs  float64
	Rows     []WaterfallRow
	Frames   []HostFrame
	Gaps     int
}

// buildRequestView walks one trace's spans. It is the only place the waterfall
// geometry is computed, so the axis, the nesting and the gap rows cannot
// disagree with each other.
func buildRequestView(spans []store.TraceSpan, services map[int64]store.Service, hotspots []store.HotspotAgg) RequestView {
	v := RequestView{TraceID: spans[0].TraceID, Hops: len(spans)}
	if len(spans) == 0 {
		return v
	}
	start, end := spans[0].StartMs, spans[0].StartMs+spans[0].DurationMs
	for _, sp := range spans {
		if sp.StartMs < start {
			start = sp.StartMs
		}
		if finish := sp.StartMs + sp.DurationMs; finish > end {
			end = finish
		}
	}
	extent := end - start
	if extent <= 0 {
		extent = 1
	}
	v.TotalMs = extent

	// A span is answered when another span in the same trace claims it as its
	// parent; an outbound call with no child is the gap the dashed row names.
	hasChild := make(map[string]bool, len(spans))
	byID := make(map[string]store.TraceSpan, len(spans))
	for _, sp := range spans {
		if sp.ParentSpanID != "" {
			hasChild[sp.ParentSpanID] = true
		}
		byID[sp.SpanID] = sp
	}
	seenService := map[int64]bool{}
	for _, sp := range spans {
		if sp.ServiceID != 0 {
			seenService[sp.ServiceID] = true
		}
		row := WaterfallRow{
			SpanID:     sp.SpanID,
			Name:       sp.Name,
			Kind:       sp.Kind,
			Status:     sp.Status,
			StartMs:    sp.StartMs,
			DurationMs: sp.DurationMs,
			Left:       (sp.StartMs - start) / extent * 100,
			Width:      sp.DurationMs / extent * 100,
			Depth:      spanDepth(sp, byID, 0),
		}
		if svc, ok := services[sp.ServiceID]; ok {
			row.Service = svc.Name
		}
		if row.Status == "" {
			row.Status = "ok"
		}
		if sp.Kind == "client" && !hasChild[sp.SpanID] {
			row.Gap = true
			v.Gaps++
			if svc, ok := services[sp.PeerServiceID]; ok {
				row.Peer = svc.Name
				row.Doc = producerDocPath(svc.Runtime)
			} else {
				row.Peer = "peer service"
				row.Doc = producerDocPath("")
			}
		}
		v.Rows = append(v.Rows, row)
	}
	v.Services = len(seenService)

	if len(hotspots) > 0 {
		maxSelf := 0.0
		for _, h := range hotspots {
			if h.TotalSelfMs > maxSelf {
				maxSelf = h.TotalSelfMs
			}
		}
		for _, h := range hotspots {
			frame := HostFrame{Function: h.Function, File: h.File, SelfMs: h.TotalSelfMs, Calls: int64(h.Samples)}
			if maxSelf > 0 {
				frame.SharePct = h.TotalSelfMs / maxSelf * 100
			}
			v.Frames = append(v.Frames, frame)
		}
	}
	return v
}

// spanDepth counts how many parents stand between a span and the trace root,
// so the waterfall indents instead of serialising a tree into one column.
func spanDepth(sp store.TraceSpan, byID map[string]store.TraceSpan, depth int) int {
	if depth > 8 || sp.ParentSpanID == "" {
		return depth
	}
	parent, ok := byID[sp.ParentSpanID]
	if !ok {
		return depth
	}
	return spanDepth(parent, byID, depth+1)
}

// ReleaseRow is one row of the Releases table: which deploy, when it first
// appeared, how many reader sessions it served, and how many of those crashed.
type ReleaseRow struct {
	Project   string
	Version   string
	FirstSeen time.Time
	Sessions  int64
	Crashed   int64
	CrashFree float64
	Adoption  float64
}

// buildReleaseRows folds the release crash points into rows and computes
// adoption as the share of the project's listed sessions this release served.
// No sessions is reported as fully crash-free, the same reading the store uses,
// so a brand-new deploy never flashes red before its first beacon.
func buildReleaseRows(points []store.ReleaseCrashPoint) []ReleaseRow {
	total := map[string]int64{}
	for _, p := range points {
		total[p.ProjectKey] += p.Sessions
	}
	out := make([]ReleaseRow, 0, len(points))
	for _, p := range points {
		row := ReleaseRow{
			Project: p.ProjectKey, Version: p.Version,
			FirstSeen: time.Unix(p.StartedAt, 0),
			Sessions:  p.Sessions, Crashed: p.Crashed, CrashFree: p.CrashFree,
		}
		if total[p.ProjectKey] > 0 {
			row.Adoption = float64(p.Sessions) / float64(total[p.ProjectKey]) * 100
		}
		out = append(out, row)
	}
	return out
}

// JobRow is one row of the Jobs table. status is the worst fact known: a missed
// run outranks a failed one, because a job that never started is a bigger
// silence than a job that started and broke.
type JobRow struct {
	Project      string
	Kind         string
	Name         string
	Queue        string
	Runs         int64
	Failures     int64
	LastRun      time.Time
	LastMs       float64
	Cron         string
	ExpectedNext time.Time
	GraceSeconds int64
	Status       string
}

// buildJobRows unions the runs the fleet reported with the runs a sweep marked
// missed, because a missed job has by definition stopped reporting: listing
// only leaderboard rows would hide it exactly when it matters.
func buildJobRows(jobs []store.JobAgg, missed []store.MissedJob) []JobRow {
	key := func(project, kind, name string) string { return project + "|" + kind + "|" + name }
	missedAt := make(map[string]store.MissedJob, len(missed))
	for _, m := range missed {
		missedAt[key(m.ProjectKey, m.Kind, m.Name)] = m
	}
	out := make([]JobRow, 0, len(jobs)+len(missed))
	seen := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		k := key(j.ProjectKey, j.Kind, j.Name)
		seen[k] = true
		row := JobRow{
			Project: j.ProjectKey, Kind: j.Kind, Name: j.Name, Queue: j.Queue,
			Runs: j.Runs, Failures: j.Failures, LastRun: j.LastRunAt, LastMs: j.LastMs,
			Cron: j.Cron, GraceSeconds: j.GraceSeconds,
			Status: "ok",
		}
		if j.ExpectedNextAt > 0 {
			row.ExpectedNext = time.Unix(j.ExpectedNextAt, 0)
		}
		if j.LastStatus == "failed" {
			row.Status = "failed"
		}
		if m, ok := missedAt[k]; ok {
			row.Status = "missed"
			row.ExpectedNext = time.Unix(m.ExpectedAt, 0)
			row.GraceSeconds = m.GraceSeconds
		}
		out = append(out, row)
	}
	for _, m := range missed {
		if seen[key(m.ProjectKey, m.Kind, m.Name)] {
			continue
		}
		out = append(out, JobRow{
			Project: m.ProjectKey, Kind: m.Kind, Name: m.Name,
			LastRun:      time.Unix(m.LastRunAt, 0),
			ExpectedNext: time.Unix(m.ExpectedAt, 0),
			GraceSeconds: m.GraceSeconds,
			Status:       "missed",
		})
	}
	return out
}

// sparkBox renders an SVG polyline for a series in a w x h box. It is the
// sized form of sparkPoints: the journeys table allows one 40 px sparkline, and
// a 96 px trend box would be the second chart the visual system refuses.
func sparkBox(points []float64, w, h float64) string {
	if len(points) < 2 {
		return ""
	}
	max := points[0]
	for _, p := range points[1:] {
		if p > max {
			max = p
		}
	}
	if max <= 0 {
		max = 1
	}
	pts := make([]string, 0, len(points))
	for i, p := range points {
		x := float64(i) * w / float64(len(points)-1)
		y := h - 2 - p/max*(h-4)
		pts = append(pts, fmt.Sprintf("%.1f,%.1f", x, y))
	}
	return strings.Join(pts, " ")
}

