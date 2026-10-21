package ingest

import (
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// — ingest hardening for public ingress.
//
// The per-project token bucket is keyed on a path segment the CALLER chooses.
// An unauthenticated flood that varies {project} mints a fresh full-burst
// bucket per name, so the per-project budget never engages — and each name
// also auto-provisions a permanent projects row and a permanent gate entry.
// Distinct-name traffic is therefore never throttled and grows memory and the
// projects table without bound, while it saturates the single WAL writer that
// legitimate keyed tenants share.
//
// The guard is the attacker-independent ceiling that closes that hole: one
// global bucket plus one bucket per source address, both BOUNDED, evaluated
// BEFORE any key lookup or body read. It runs outermost on purpose — a name
// flood must be throttled whether or not it presents a valid key.
//
// The global ceiling is a DoS ceiling, not a fairness limit: it sits far above
// the platform's real aggregate volume, so a legitimate tenant never notices
// it while a name flood hits it immediately.

const (
	defaultGlobalRate  = 500
	defaultGlobalBurst = 1000
	defaultIPRate      = 100
	defaultIPBurst     = 200
	maxGuardIPs        = 4096
)

type IngressGuard struct {
	globalRate  float64
	globalBurst float64
	ipRate      float64
	ipBurst     float64
	maxIPs      int

	mu     sync.Mutex
	global tokenBucket
	ips    map[string]*tokenBucket

	globalRejected atomic.Int64
	ipRejected     atomic.Int64
}

// NewIngressGuard builds the coarse ceilings. A non-positive global value
// falls back to the shipped default; a non-positive ipRate DISABLES the
// per-address leg, which is the right setting behind a reverse proxy (every
// request then shares the proxy address, so the global bucket is the only
// meaningful ceiling).
func NewIngressGuard(globalPerSecond, globalBurst, ipPerSecond, ipBurst float64) *IngressGuard {
	g := &IngressGuard{
		globalRate:  globalPerSecond,
		globalBurst: globalBurst,
		ipRate:      ipPerSecond,
		ipBurst:     ipBurst,
		maxIPs:      maxGuardIPs,
		ips:         map[string]*tokenBucket{},
	}
	if g.globalRate <= 0 {
		g.globalRate = defaultGlobalRate
	}
	if g.globalBurst <= 0 {
		g.globalBurst = defaultGlobalBurst
	}
	if g.ipRate > 0 && g.ipBurst <= 0 {
		g.ipBurst = defaultIPBurst
	}
	return g
}

// Allow applies the coarse ceilings and reports which leg rejected.
func (g *IngressGuard) Allow(ip string) (bool, string) {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if !take(&g.global, g.globalRate, g.globalBurst, now) {
		g.globalRejected.Add(1)
		return false, "global"
	}
	if g.ipRate > 0 && ip != "" {
		b := g.ips[ip]
		if b == nil {
			g.evictIPsLocked()
			b = &tokenBucket{tokens: g.ipBurst, last: now}
			g.ips[ip] = b
		}
		if !take(b, g.ipRate, g.ipBurst, now) {
			g.ipRejected.Add(1)
			return false, "ip"
		}
	}
	return true, ""
}

// evictIPsLocked drops the least-recently-seen half of the address map once it
// reaches its cap, so a flood that varies its source address cannot grow it
// without limit.
func (g *IngressGuard) evictIPsLocked() {
	if len(g.ips) < g.maxIPs {
		return
	}
	type entry struct {
		key  string
		last time.Time
	}
	all := make([]entry, 0, len(g.ips))
	for k, b := range g.ips {
		all = append(all, entry{k, b.last})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].last.Before(all[j].last) })
	for i := 0; i < len(all)/2; i++ {
		delete(g.ips, all[i].key)
	}
}

// GlobalRejections and IPRejections report the two legs for the health strip.
func (g *IngressGuard) GlobalRejections() int64 { return g.globalRejected.Load() }
func (g *IngressGuard) IPRejections() int64     { return g.ipRejected.Load() }

// ClientIP resolves the source address the guard keys on. It deliberately
// IGNORES X-Forwarded-For: that header is caller-controlled, so honouring it
// would hand an attacker a fresh per-address bucket per request — the exact
// bypass the guard exists to close. Behind a reverse proxy every request
// shares the proxy address, which is why the global bucket is the primary
// ceiling and the per-address leg can be switched off.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// WithIngressGuard wraps a route with the coarse ceiling.
func WithIngressGuard(g *IngressGuard, next http.HandlerFunc) http.HandlerFunc {
	if g == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ok, leg := g.Allow(ClientIP(r))
		if !ok {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "ingest rate ceiling reached ("+leg+")", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}
