// Command nightpost-go demonstrates the reference client against a real
// server. Run it with NIGHTPOST_ENDPOINT pointed at an instance.
package main

import (
	"context"
	"log"

	"time"
)

func main() {
	c := NewFromEnv()
	if c == nil {
		log.Fatal("set NIGHTPOST_ENDPOINT (or MONITOR_ENDPOINT) — an unconfigured client is inert by design")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A deploy marker, so a regression later has a timestamp to line up with.
	if err := c.Release(ctx, "demo", "2026.10.9-a1b2c3", "production"); err != nil {
		log.Printf("release: %v", err)
	}
	// One timed request with a child span.
	start := time.Now()
	// ...do the work...
	if err := c.Profile(ctx, "demo", Profile{
		Name:        "GET /demo",
		DurationMs:  float64(time.Since(start).Microseconds()) / 1000,
		Environment: "production",
		Spans:       []Span{{Name: "work", DurationMs: 1.5}},
	}); err != nil {
		log.Printf("profile: %v", err)
	}
}
