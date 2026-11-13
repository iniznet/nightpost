package alerts

import (
	"encoding/json"
	"net/http"
)

// Webhook is the generic JSON POST. The body is this server's own shape (title
// plus body), so an integration never has to parse Discord or Slack markup it did
// not ask for.
type Webhook struct {
	URL    string
	Client *http.Client
}

// webhookPayload is the documented body of the generic channel.
type webhookPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func (w *Webhook) Send(title, body string) error {
	payload, err := json.Marshal(webhookPayload{Title: title, Body: body})
	if err != nil {
		return err
	}
	return postJSON(w.Client, w.URL, payload, "webhook")
}
