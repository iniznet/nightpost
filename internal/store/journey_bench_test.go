package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkRecomputeJourneys100k measures ONE materialisation pass over 100,000
// spans: 20,000 traces of a five-span reader request, spread over eight entry
// routes so the pass writes a realistic handful of journeys rather than one.
//
// It is a benchmark and not a test on purpose — the plan asks for a reported
// number, not a gate that fails on a loaded machine.
func BenchmarkRecomputeJourneys100k(b *testing.B) {
	st, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	defer st.Close()

	svcID, err := st.UpsertService("bench", "front", "node", "production", false)
	if err != nil {
		b.Fatalf("service: %v", err)
	}
	peerID, err := st.UpsertService("bench", "backend", "php", "production", false)
	if err != nil {
		b.Fatalf("peer service: %v", err)
	}
	entries := make([]int64, 8)
	callees := make([]int64, 8)
	for i := range entries {
		if entries[i], err = st.UpsertRoute(svcID, "GET", fmt.Sprintf("GET /page/%d/{slug}", i), "server"); err != nil {
			b.Fatalf("entry route: %v", err)
		}
		if callees[i], err = st.UpsertRoute(svcID, "GET", fmt.Sprintf("GET /api/%d/{id}", i), "server"); err != nil {
			b.Fatalf("callee route: %v", err)
		}
	}

	type row struct {
		trace, span, parent string
		routeID             int64
		kind                string
		start, dur          float64
		peer                int64
	}
	const traces = 20000
	rows := make([]row, 0, traces*5)
	for i := 0; i < traces; i++ {
		trace := fmt.Sprintf("t-%06d", i)
		entry := entries[i%8]
		callee := callees[i%8]
		root := fmt.Sprintf("%016d", i*5)
		client := fmt.Sprintf("%016d", i*5+1)
		child := fmt.Sprintf("%016d", i*5+2)
		rows = append(rows,
			row{trace: trace, span: root, kind: "server", routeID: entry, dur: 812},
			row{trace: trace, span: client, parent: root, kind: "client", dur: 300, peer: peerID},
			row{trace: trace, span: child, parent: client, kind: "server", routeID: callee, start: 40, dur: 280},
			row{trace: trace, span: fmt.Sprintf("%016d", i*5+3), parent: root, kind: "internal", start: 400, dur: 40},
			row{trace: trace, span: fmt.Sprintf("%016d", i*5+4), parent: root, kind: "internal", start: 700, dur: 60},
		)
	}

	tx, err := st.wdb.Begin()
	if err != nil {
		b.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO spans
		(trace_id, span_id, parent_span_id, service_id, route_id, kind, name,
		 start_ms, duration_ms, status, peer_service_id, attributes, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'ok', ?, '', ?)`)
	if err != nil {
		b.Fatalf("prepare: %v", err)
	}
	now := time.Now()
	for _, r := range rows {
		var routeID, peer any
		if r.routeID != 0 {
			routeID = r.routeID
		}
		if r.peer != 0 {
			peer = r.peer
		}
		if _, err := stmt.Exec(r.trace, r.span, r.parent, svcID, routeID, r.kind, r.trace,
			r.start, r.dur, peer, now.Unix()); err != nil {
			b.Fatalf("insert span: %v", err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit: %v", err)
	}

	from, to := now.Add(-journeysTrailingBench), now.Add(time.Minute)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := st.RecomputeJourneys(from, to); err != nil {
			b.Fatalf("recompute: %v", err)
		}
	}
}

// journeysTrailingBench is the production trailing window the benchmark rebuilds.
const journeysTrailingBench = 15 * time.Minute
