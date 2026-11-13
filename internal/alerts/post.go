package alerts

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"time"
)

// postJSON is the one outbound POST the webhook-shaped channels share: an
// explicit timeout (the request-ceiling rule) and a bounded error read, so a slow
// or chatty endpoint can neither wedge a tick nor fill the log.
func postJSON(client *http.Client, target string, payload []byte, what string) error {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Post(target, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("%s: status %d: %s", what, resp.StatusCode, msg)
	}
	return nil
}

// clip bounds an alert body to a receiver's own limit, counted in runes so a
// multi-byte message is never cut mid-character.
func clip(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit])
}
