package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iniznet/nightpost/internal/store"
)

// TestIngestHealthStripCountsEveryLeg pins the footer contract: each
// ingest ceiling has its own counter, so "why is my data missing?" can be
// answered per leg instead of with one opaque number.
func TestIngestHealthStripCountsEveryLeg(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	mux := http.NewServeMux()
	Register(mux, st, nil, nil, WithIngestHealth(IngestHealth{
		ShaperRejections:  func() int64 { return 11 },
		KeyGateRejections: func() int64 { return 22 },
		UnknownAdmits:     func() int64 { return 33 },
		GateRefusals:      func() int64 { return 44 },
		GuardGlobal:       func() int64 { return 55 },
		GuardIP:           func() int64 { return 66 },
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body := readAll(t, resp)
	for _, want := range []string{
		"<b>55</b> global 429",
		"<b>66</b> ip 429",
		"<b>11</b> project 429",
		"<b>22</b> key 401",
		"<b>44</b> refused",
		"<b>33</b> unsigned admits",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("health strip missing %q", want)
		}
	}
}
