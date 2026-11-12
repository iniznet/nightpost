package alerts

import (
	"encoding/json"
	"net/http"
)

// Slack posts to an incoming webhook. The payload is Slack's own flat document:
// an operator's existing Slack app accepts it with no translator, and trimming to
// Slack's block limit happens here rather than in the dispatcher, because the
// limit belongs to the receiver.
type Slack struct {
	Webhook string
	Client  *http.Client
}

const slackTextLimit = 3000

func (s *Slack) Send(title, body string) error {
	payload, err := json.Marshal(map[string]string{"text": clip(body, slackTextLimit)})
	if err != nil {
		return err
	}
	return postJSON(s.Client, s.Webhook, payload, "slack webhook")
}
