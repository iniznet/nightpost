package ingest

import (
	"math"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Shaper bounds ingest per project with a token bucket: senders are
// first-party and sampled by contract , so a misbehaving sender must
// never translate into unbounded store writes. Requests past the budget get
// a cheap 429 before any body read or DB work.
type Shaper struct {
	// rate and burst are LIVE. SetBudget rewrites them while ingest is
	// serving, so an operator's saved setting applies without a restart;
	// atomics keep the admission path free of a second lock.
	rateBits   atomic.Uint64 // float64: tokens per second
	burstBits  atomic.Uint64 // float64: burst ceiling
	maxBuckets int

	rejected atomic.Int64

	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

// maxShaperBuckets bounds the per-project bucket map. The map is keyed
// on a name the caller chooses, so without a cap a distinct-name flood grows
// it without limit even while every request is being throttled.
const maxShaperBuckets = 4096

// take refills a token bucket and consumes one token.
func take(b *tokenBucket, rate, burst float64, now time.Time) bool {
	b.tokens += now.Sub(b.last).Seconds() * rate
	b.last = now
	if b.tokens > burst {
		b.tokens = burst
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Rejections reports how many ingest events were answered 429 — the footer
// ingest-health strip surfaces it so "why is my data missing?" has an answer.
func (sh *Shaper) Rejections() int64 { return sh.rejected.Load() }

type tokenBucket struct {
	tokens float64
	last   time.Time
}

// NewShaper builds a per-project limiter at perSecond tokens with burst
// ceiling; zero/negative inputs fall back to the defaults (50/s, burst 100).
func NewShaper(perSecond, burst float64) *Shaper {
	sh := &Shaper{maxBuckets: maxShaperBuckets, buckets: map[string]*tokenBucket{}}
	sh.SetBudget(perSecond, burst)
	return sh
}

// Budget reports the live per-project budget (tokens/s, burst ceiling).
func (sh *Shaper) Budget() (rate, burst float64) {
	return math.Float64frombits(sh.rateBits.Load()), math.Float64frombits(sh.burstBits.Load())
}

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
	return perSecond, burst
}

// Allow reports whether one ingest event for the project fits its budget.
func (sh *Shaper) Allow(project string) bool {
	rate, burst := sh.Budget()
	now := time.Now()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	b := sh.buckets[project]
	if b == nil {
		sh.evictBucketsLocked()
		b = &tokenBucket{tokens: burst, last: now}
		sh.buckets[project] = b
	}
	return take(b, rate, burst, now)
}

// evictBucketsLocked drops the least-recently-seen half of the bucket map once
// it reaches its cap, so a distinct-name flood cannot grow it without limit.
func (sh *Shaper) evictBucketsLocked() {
	if len(sh.buckets) < sh.maxBuckets {
		return
	}
	type entry struct {
		key  string
		last time.Time
	}
	all := make([]entry, 0, len(sh.buckets))
	for k, b := range sh.buckets {
		all = append(all, entry{k, b.last})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].last.Before(all[j].last) })
	for i := 0; i < len(all)/2; i++ {
		delete(sh.buckets, all[i].key)
	}
}

// BucketCount reports the live bucket-map size; the test asserts the bound.
func (sh *Shaper) BucketCount() int {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return len(sh.buckets)
}

// WithShaper rejects requests whose project has exhausted its ingest budget
// before the wrapped handler reads the body. Project resolution matches the
// xhprof routes, where the key lives in HTTP Basic auth instead of the path.
func WithShaper(sh *Shaper, next http.HandlerFunc) http.HandlerFunc {
	if sh == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		project := r.PathValue("project")
		if project == "" {
			project = xhprofProject(r)
		}
		if !sh.Allow(project) {
			sh.rejected.Add(1)
			w.Header().Set("Retry-After", "1")
			http.Error(w, "ingest budget exhausted", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}
