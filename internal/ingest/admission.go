package ingest

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Item-type budgets. One chatty producer class must not spend another class's
// allowance: a browser error storm and a profile sample share a project but not
// a budget, so a front-end incident cannot blind the request waterfall.
const (
	ItemErrors   = "errors"
	ItemProfiles = "profiles"
	ItemLogs     = "logs"
	ItemSpans    = "spans"
)

// The admission dimensions an operator can retune without a restart. They sit
// beside ingest_rate/ingest_burst and are read through the same Settings source
// on the same refresh tick, so one poller applies every ceiling.
const (
	SettingIngestHourly       = "ingest_hourly"
	SettingIngestRateErrors   = "ingest_rate_errors"
	SettingIngestRateProfiles = "ingest_rate_profiles"
	SettingIngestRateLogs     = "ingest_rate_logs"
	SettingIngestRateSpans    = "ingest_rate_spans"
)

// Settings is the live knob surface admission reads. *store.Store satisfies it;
// tests use a map so a reload is provable without a database.
type Settings interface {
	Setting(key string) (string, error)
	SettingFloat(key string, fallback float64) float64
}

// ShaperOption configures a Shaper at construction. Options exist so the live
// settings source is injected explicitly rather than reached for globally.
type ShaperOption func(*Shaper)

// WithSettings makes the extra admission dimensions reload with the budget: the
// poller calls SetBudget on its usual interval, and SetBudget re-reads these on
// the same tick.
func WithSettings(src Settings) ShaperOption {
	return func(sh *Shaper) { sh.src = src }
}

// Shaper bounds ingest per project with a token bucket: senders are
// first-party and sampled by contract , so a misbehaving sender must never
// translate into unbounded store writes. Requests past the budget get a cheap
// 429 before any body read or DB work.
//
// Three dimensions sit on top of that bucket, and every one of them is live
// from settings so an operator needs no restart to change a ceiling:
//   - a per-item-type budget (errors, profiles, logs, spans), each its own
//     bucket, so one producer class cannot starve another in the same project;
//   - an hourly ceiling beside the burst rate, because a burst ceiling alone
//     admits a slow steady flood for ever;
//   - a Retry-After on every 429, computed from the dimension that refused it.
type Shaper struct {
	// rate and burst are LIVE. SetBudget rewrites them while ingest is
	// serving, so an operator's saved setting applies without a restart;
	// atomics keep the admission path free of a second lock.
	rateBits   atomic.Uint64 // float64: tokens per second
	burstBits  atomic.Uint64 // float64: burst ceiling
	hourlyBits atomic.Uint64 // float64: events per hour per project (0 = off)
	items      itemRates
	maxBuckets int
	src        Settings

	rejected atomic.Int64

	mu          sync.Mutex
	buckets     map[string]*tokenBucket
	itemBuckets map[string]*tokenBucket
	hours       map[string]*hourWindow
}

// itemRates holds one refill rate per item dimension. Zero disables that
// dimension, which is the shipped default: the aggregate bucket alone behaves
// exactly like v1 until an operator asks for more.
type itemRates struct {
	errors   atomic.Uint64
	profiles atomic.Uint64
	logs     atomic.Uint64
	spans    atomic.Uint64
}

func (r *itemRates) get(item string) float64 {
	switch item {
	case ItemErrors:
		return math.Float64frombits(r.errors.Load())
	case ItemProfiles:
		return math.Float64frombits(r.profiles.Load())
	case ItemSpans:
		return math.Float64frombits(r.spans.Load())
	}
	return math.Float64frombits(r.logs.Load())
}

func (r *itemRates) set(item string, rate float64) {
	switch item {
	case ItemErrors:
		r.errors.Store(math.Float64bits(rate))
	case ItemProfiles:
		r.profiles.Store(math.Float64bits(rate))
	case ItemSpans:
		r.spans.Store(math.Float64bits(rate))
	default:
		r.logs.Store(math.Float64bits(rate))
	}
}

// maxShaperBuckets bounds the per-project bucket maps. The maps are keyed on a
// name the caller chooses, so without a cap a distinct-name flood grows them
// without limit even while every request is being throttled.
const maxShaperBuckets = 4096

// take refills a token bucket and consumes one token.
func take(b *tokenBucket, rate, burst float64, now time.Time) bool {
	return takeN(b, rate, burst, 1, now)
}

// takeN refills a token bucket and consumes n tokens. A batch that does not fit
// is refused WHOLE: charging it in part would silently halve a payload that the
// caller believed was admitted.
func takeN(b *tokenBucket, rate, burst float64, n int, now time.Time) bool {
	b.tokens += now.Sub(b.last).Seconds() * rate
	b.last = now
	if b.tokens > burst {
		b.tokens = burst
	}
	need := float64(n)
	if b.tokens < need {
		return false
	}
	b.tokens -= need
	return true
}

// Rejections reports how many ingest events were answered 429 — the footer
// ingest-health strip surfaces it so "why is my data missing?" has an answer.
func (sh *Shaper) Rejections() int64 { return sh.rejected.Load() }

type tokenBucket struct {
	tokens float64
	last   time.Time
}

// hourWindow counts one project's admissions inside a fixed clock hour. A
// fixed window, not a sliding one, because the ceiling is a coarse "stop the
// flood" number and a fixed window costs one int instead of a ring buffer.
type hourWindow struct {
	hour  int64
	count int
}

// NewShaper builds a per-project limiter at perSecond tokens with burst
// ceiling; zero/negative inputs fall back to the defaults (50/s, burst 100).
func NewShaper(perSecond, burst float64, opts ...ShaperOption) *Shaper {
	sh := &Shaper{
		maxBuckets:  maxShaperBuckets,
		buckets:     map[string]*tokenBucket{},
		itemBuckets: map[string]*tokenBucket{},
		hours:       map[string]*hourWindow{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(sh)
		}
	}
	sh.SetBudget(perSecond, burst)
	return sh
}

// Budget reports the live per-project budget (tokens/s, burst ceiling).
func (sh *Shaper) Budget() (rate, burst float64) {
	return math.Float64frombits(sh.rateBits.Load()), math.Float64frombits(sh.burstBits.Load())
}

// HourlyCeiling reports the live per-project hourly ceiling (0 = off).
func (sh *Shaper) HourlyCeiling() float64 {
	return math.Float64frombits(sh.hourlyBits.Load())
}

// ItemBudget reports the live refill rate for one item dimension (0 = off).
func (sh *Shaper) ItemBudget(item string) float64 { return sh.items.get(item) }

// SetBudget replaces the live per-project budget and reports what it applied.
// The settings poller calls it, so an operator's change at /settings (or an
// operator API write) never needs a restart. Safe while ingest serves: readers
// take the two atomics once per event, and an existing bucket clamps to the new
// burst on its next take.
//
// Zero or negative inputs fall back to the shipped defaults, exactly like
// NewShaper — a corrupted settings row must not stop admission entirely.
func (sh *Shaper) SetBudget(perSecond, burst float64) (float64, float64) {
	if perSecond <= 0 {
		perSecond = 50
	}
	if burst <= 0 {
		burst = 100
	}
	sh.rateBits.Store(math.Float64bits(perSecond))
	sh.burstBits.Store(math.Float64bits(burst))
	sh.reload()
	return perSecond, burst
}

// reload applies the item budgets and the hourly ceiling from the settings
// source. It rides SetBudget because that is the tick an operator already
// relies on: a saved ingest_rate and a saved ingest_hourly must land together,
// or the pages would show one applied and the other pending.
func (sh *Shaper) reload() {
	if sh.src == nil {
		return
	}
	sh.hourlyBits.Store(math.Float64bits(nonNegative(sh.src.SettingFloat(SettingIngestHourly, 0))))
	for _, item := range []string{ItemErrors, ItemProfiles, ItemLogs, ItemSpans} {
		sh.items.set(item, nonNegative(sh.src.SettingFloat(settingForItem(item), 0)))
	}
}

// nonNegative clamps a stored rate: a negative ceiling is a corrupt row, and
// reading it as zero (off) is safer than reading it as a refusal of everything.
func nonNegative(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

// settingForItem maps an item dimension onto its settings key.
func settingForItem(item string) string {
	switch item {
	case ItemErrors:
		return SettingIngestRateErrors
	case ItemProfiles:
		return SettingIngestRateProfiles
	case ItemSpans:
		return SettingIngestRateSpans
	}
	return SettingIngestRateLogs
}

// Allow reports whether one ingest event for the project fits its budget.
func (sh *Shaper) Allow(project string) bool {
	ok, _ := sh.admit(project)
	return ok
}

// admit applies the aggregate bucket and the hourly ceiling, and reports the
// wait a refused caller should honour.
func (sh *Shaper) admit(project string) (bool, int) {
	rate, burst := sh.Budget()
	now := time.Now()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	b := sh.bucketLocked(sh.buckets, project, burst, now)
	if ceiling := sh.HourlyCeiling(); !hourAllows(sh.hours[project], ceiling, now) {
		return false, secondsUntilNextHour(now)
	}
	if !take(b, rate, burst, now) {
		return false, retryAfterSeconds(b.tokens, rate)
	}
	sh.hourRecordLocked(project, now)
	return true, 0
}

// AllowItem reports whether one event of that item type fits its own budget.
func (sh *Shaper) AllowItem(project, item string) bool {
	ok, _ := sh.allowItems(project, item, 1)
	return ok
}

// allowItems charges n events of one item dimension.
func (sh *Shaper) allowItems(project, item string, n int) (bool, int) {
	if n <= 0 {
		return true, 0
	}
	rate := sh.items.get(item)
	if rate <= 0 {
		return true, 0
	}
	now := time.Now()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	b := sh.bucketLocked(sh.itemBuckets, project+"\x00"+item, float64(n), now)
	if !takeN(b, rate, float64(n), n, now) {
		return false, retryAfterSeconds(b.tokens, rate)
	}
	return true, 0
}

// bucketLocked returns the bucket for key, creating it with a full burst.
func (sh *Shaper) bucketLocked(m map[string]*tokenBucket, key string, burst float64, now time.Time) *tokenBucket {
	b := m[key]
	if b == nil {
		sh.evictLocked(m)
		b = &tokenBucket{tokens: burst, last: now}
		m[key] = b
	}
	return b
}

// hourAllows reports whether the project is under its ceiling for this hour.
func hourAllows(w *hourWindow, ceiling float64, now time.Time) bool {
	if ceiling <= 0 {
		return true
	}
	if w == nil || w.hour != now.Unix()/3600 {
		return true
	}
	return float64(w.count) < ceiling
}

// hourRecordLocked counts one admitted event, rolling the window on a new hour.
func (sh *Shaper) hourRecordLocked(project string, now time.Time) {
	if sh.HourlyCeiling() <= 0 {
		return
	}
	hour := now.Unix() / 3600
	w := sh.hours[project]
	if w == nil || w.hour != hour {
		if len(sh.hours) >= sh.maxBuckets {
			sh.evictHoursLocked(hour)
		}
		w = &hourWindow{hour: hour}
		sh.hours[project] = w
	}
	w.count++
}

// secondsUntilNextHour is the honest wait when the hourly ceiling is the leg
// that refused: no token refill can admit the request before the window rolls.
func secondsUntilNextHour(now time.Time) int {
	next := now.Truncate(time.Hour).Add(time.Hour)
	secs := int(math.Ceil(next.Sub(now).Seconds()))
	if secs < 1 {
		return 1
	}
	return secs
}

// retryAfterSeconds reports how long until one token is available.
func retryAfterSeconds(tokens, rate float64) int {
	if rate <= 0 {
		return 1
	}
	wait := math.Ceil((1 - tokens) / rate)
	if wait < 1 {
		return 1
	}
	if wait > 3600 {
		return 3600
	}
	return int(wait)
}

// evictLocked drops the least-recently-seen half of a bucket map once it reaches
// its cap, so a distinct-name flood cannot grow it without limit.
func (sh *Shaper) evictLocked(m map[string]*tokenBucket) {
	if len(m) < sh.maxBuckets {
		return
	}
	type entry struct {
		key  string
		last time.Time
	}
	all := make([]entry, 0, len(m))
	for k, b := range m {
		all = append(all, entry{k, b.last})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].last.Before(all[j].last) })
	for i := 0; i < len(all)/2; i++ {
		delete(m, all[i].key)
	}
}

// evictHoursLocked drops every window from an earlier hour, which is bounded by
// the number of distinct projects seen in the current hour.
func (sh *Shaper) evictHoursLocked(hour int64) {
	for k, w := range sh.hours {
		if w.hour != hour {
			delete(sh.hours, k)
		}
	}
}

// BucketCount reports the live project-bucket count; the test asserts the bound.
func (sh *Shaper) BucketCount() int {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return len(sh.buckets)
}

// admission is what WithShaper hands the handler through the request context:
// the span batch inside a payload can only be charged AFTER the body is read,
// and the handler is the only place that knows how many spans arrived.
type admission struct {
	shaper  *Shaper
	project string
}

type admissionKey struct{}

// admitSpans charges the spans budget for one payload's span batch. A span
// batch that does not fit is DROPPED rather than answered 429: the profile or
// error in the same payload is the primary record, and throwing it away to
// enforce a secondary budget would lose the very signal the sender came for.
func admitSpans(r *http.Request, n int) bool {
	if n <= 0 {
		return true
	}
	a, _ := r.Context().Value(admissionKey{}).(*admission)
	if a == nil || a.shaper == nil {
		return true
	}
	ok, _ := a.shaper.allowItems(a.project, ItemSpans, n)
	if !ok {
		a.shaper.rejected.Add(1)
	}
	return ok
}

// itemForPath maps an ingest path onto its item budget. The Sentry SDK
// compatibility paths (/api/{project}/store, /envelope) are error events, and
// the profiler compatibility paths are profiles — the mapping follows the kind
// of event, not the shape of the URL.
func itemForPath(path string) string {
	switch {
	case strings.Contains(path, "/profiler/"):
		return ItemProfiles
	case strings.HasSuffix(path, "/profile"), strings.HasSuffix(path, "/xhprof"):
		return ItemProfiles
	case strings.HasSuffix(path, "/sentry"), strings.HasSuffix(path, "/error"),
		strings.HasSuffix(path, "/envelope"), strings.HasSuffix(path, "/store"),
		strings.HasSuffix(path, "/store/"):
		return ItemErrors
	}
	return ItemLogs
}

// WithShaper rejects requests whose project has exhausted its budget before the
// wrapped handler reads the body. Project resolution matches the xhprof routes,
// where the key lives in HTTP Basic auth instead of the path.
func WithShaper(sh *Shaper, next http.HandlerFunc) http.HandlerFunc {
	if sh == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		project := r.PathValue("project")
		if project == "" {
			project = xhprofProject(r)
		}
		item := itemForPath(r.URL.Path)
		ok, retry := sh.admit(project)
		if ok {
			itemOK, itemRetry := sh.allowItems(project, item, 1)
			if !itemOK {
				ok, retry = false, itemRetry
			}
		}
		if !ok {
			sh.rejected.Add(1)
			writeRetryAfter(w, retry)
			http.Error(w, "ingest budget exhausted", http.StatusTooManyRequests)
			return
		}
		ctx := context.WithValue(r.Context(), admissionKey{}, &admission{shaper: sh, project: project})
		next(w, r.WithContext(ctx))
	}
}
