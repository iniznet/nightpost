package alerts

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Telegram posts through the Bot API's sendMessage. ChatID is required because a
// bot is in many chats and the channel config names exactly one; APIBase is
// empty for the public host and set when a gateway fronts it.
type Telegram struct {
	Token   string
	ChatID  string
	APIBase string
	Client  *http.Client
}

const (
	telegramAPIBase   = "https://api.telegram.org"
	telegramTextLimit = 4000 // the API rejects 4096 and above
)

func (t *Telegram) Send(title, body string) error {
	base := t.APIBase
	if base == "" {
		base = telegramAPIBase
	}
	payload, err := json.Marshal(map[string]any{
		"chat_id":                  t.ChatID,
		"text":                     clip(body, telegramTextLimit),
		"disable_web_page_preview": true,
	})
	if err != nil {
		return err
	}
	return postJSON(t.Client, strings.TrimRight(base, "/")+"/bot"+t.Token+"/sendMessage", payload, "telegram sendMessage")
}
