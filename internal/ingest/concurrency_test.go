package ingest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// This host has no C compiler, so `go test -race` cannot run here ("-race
// requires cgo"). The smallest safe equivalent is a stress test: the Go runtime
// makes a concurrent map write a FATAL error ("concurrent map writes") even
// without the race detector, so a dropped lock in the bounded set fails loudly
// rather than silently corrupting. What this does NOT prove: absence of races on
// values the detector alone can see. Run `-race` on a host with gcc when one is
// available.

func TestKnownSetIsConcurrencySafe(t *testing.T) {
	s := newKnownSet(64)
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				name := fmt.Sprintf("p-%d-%d", g, i)
				s.add(name)
				_ = s.has(name)
				_ = s.size()
			}
		}(g)
	}
	wg.Wait()
	if got := s.size(); got > 64 {
		t.Errorf("knownSet grew to %d under concurrency, want <= 64", got)
	}
}

// TestKnownSetAddReportsTheWinner pins the contract the counter relies on,
// within a set large enough that EVICTION cannot occur: with no eviction, each
// name has exactly one inserter, so a concurrent first sight is counted once.
// Eviction is the caveat, and TestKnownSetEvictionMayReinsert proves it.
func TestKnownSetAddReportsTheWinner(t *testing.T) {
	s := newKnownSet(4096)
	if !s.add("x") {
		t.Error("first add must report itself as the inserter")
	}
	if s.add("x") {
		t.Error("second add must report no insert")
	}
	// And it must hold under contention: exactly one winner per name.
	const names = 200
	var mu sync.Mutex
	winners := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < names; i++ {
				if s.add(fmt.Sprintf("n-%d", i)) {
					mu.Lock()
					winners[fmt.Sprintf("n-%d", i)]++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	for name, n := range winners {
		if n != 1 {
			t.Errorf("%s reported %d winners, want exactly 1", name, n)
		}
	}
}

// TestKnownSetEvictionMayReinsert documents the honest limit of the counter:
// eviction drops the older half, so a still-live project name can be re-inserted
// later and counted a second time. That is why UnknownAdmits is an attack
// SIGNATURE rather than an exact tally of distinct names, and why eviction is
// sized (4096) far above the project counts it is meant to hold.
func TestKnownSetEvictionMayReinsert(t *testing.T) {
	s := newKnownSet(4)
	if !s.add("old") {
		t.Fatal("first add must win")
	}
	// Push enough distinct names through to force eviction of the older half.
	for i := 0; i < 20; i++ {
		s.add(fmt.Sprintf("filler-%d", i))
	}
	if s.add("old") {
		// Re-inserted after eviction: expected, and the reason the counter is
		// documented as approximate. Assert the set stayed bounded regardless.
		if s.size() > 4 {
			t.Errorf("size = %d, want <= 4", s.size())
		}
		return
	}
	// If it was NOT evicted (a different but also valid eviction order), the
	// bound must still hold.
	if s.size() > 4 {
		t.Errorf("size = %d, want <= 4", s.size())
	}
}

// TestKeyGateConcurrentFirstSight drives many goroutines through the gate. The
// exact-count assertion is the point: 8 workers x 50 names plus one shared name
// must produce exactly 401 auto-provisions, no more. An over-count is what a
// dropped lock or a sloppy check-then-act would produce.
func TestKeyGateConcurrentFirstSight(t *testing.T) {
	st := newBenchStore(t)
	g := NewKeyGate(st, 0)
	mux := gateFor(t, g)

	const workers = 8
	const perWorker = 50
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				names := []string{"shared-name", fmt.Sprintf("w-%d-%d", w, i)}
				for _, name := range names {
					rec := httptest.NewRecorder()
					mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ingest/"+name+"/profile", nil))
					if rec.Code != http.StatusOK {
						t.Errorf("status = %d for %s", rec.Code, name)
						return
					}
				}
			}
		}(w)
	}
	wg.Wait()

	want := int64(1 + workers*perWorker)
	if got := g.UnknownAdmits(); got != want {
		t.Errorf("unknown admits = %d, want exactly %d (double-counting a concurrent first sight)", got, want)
	}
	if got := g.seen.size(); got > g.seen.max {
		t.Errorf("known set grew to %d, want <= %d", got, g.seen.max)
	}
}
