package alerts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Discord posts alert text to an incoming webhook. All outbound HTTP stays
// under the 30s request ceiling rule (#139 class) with an explicit timeout.
type Discord struct {
	Webhook string
	Client  *http.Client
}

func (d *Discord) Send(title, body string) error {
	content := body
	if len(content) > 1900 {
		content = content[:1900]
	}
	payload, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return err
	}
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Post(d.Webhook, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("discord webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("discord webhook: status %d: %s", resp.StatusCode, msg)
	}
	return nil
}
