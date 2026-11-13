package alerts

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// One test per channel, each asserting the PAYLOAD the receiver gets rather than
// that some function was called: the wire shape is the contract an operator's
// Slack app, bot or endpoint is written against.

// captureServer stands in for a webhook receiver and hands back the request body
// and path it saw.
func captureServer(t *testing.T, body *string, path *string, header *http.Header) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		*body = string(raw)
		if path != nil {
			*path = r.URL.Path
		}
		if header != nil {
			*header = r.Header.Clone()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func decodeJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("payload is not JSON (%v): %q", err, raw)
	}
	return out
}

func TestChannelDiscordPayloadShape(t *testing.T) {
	var body string
	srv := captureServer(t, &body, nil, nil)
	if err := (&Discord{Webhook: srv.URL}).Send("Slow route", "GET /page p95 2400ms"); err != nil {
		t.Fatalf("send: %v", err)
	}
	payload := decodeJSON(t, body)
	if len(payload) != 1 || payload["content"] != "GET /page p95 2400ms" {
		t.Fatalf("discord payload = %v, want {content: body}", payload)
	}
}

func TestChannelSlackPayloadShape(t *testing.T) {
	var body string
	srv := captureServer(t, &body, nil, nil)
	if err := (&Slack{Webhook: srv.URL}).Send("Slow route", "GET /page p95 2400ms"); err != nil {
		t.Fatalf("send: %v", err)
	}
	payload := decodeJSON(t, body)
	if len(payload) != 1 || payload["text"] != "GET /page p95 2400ms" {
		t.Fatalf("slack payload = %v, want {text: body}", payload)
	}
}

func TestChannelTelegramPayloadShape(t *testing.T) {
	var body, path string
	srv := captureServer(t, &body, &path, nil)
	tg := &Telegram{Token: "123456:secret", ChatID: "-100200", APIBase: srv.URL}
	if err := tg.Send("Slow route", "GET /page p95 2400ms"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if path != "/bot123456:secret/sendMessage" {
		t.Fatalf("telegram path = %q, want the Bot API sendMessage call", path)
	}
	payload := decodeJSON(t, body)
	if payload["chat_id"] != "-100200" {
		t.Errorf("chat_id = %v, want -100200", payload["chat_id"])
	}
	if payload["text"] != "GET /page p95 2400ms" {
		t.Errorf("text = %v, want the alert body", payload["text"])
	}
	if payload["disable_web_page_preview"] != true {
		t.Errorf("disable_web_page_preview = %v, want true", payload["disable_web_page_preview"])
	}
}

func TestChannelWebhookPayloadShape(t *testing.T) {
	var body string
	var header http.Header
	srv := captureServer(t, &body, nil, &header)
	if err := (&Webhook{URL: srv.URL}).Send("Slow route", "GET /page p95 2400ms"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if ct := header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q, want application/json", ct)
	}
	payload := decodeJSON(t, body)
	if len(payload) != 2 || payload["title"] != "Slow route" || payload["body"] != "GET /page p95 2400ms" {
		t.Fatalf("webhook payload = %v, want {title, body}", payload)
	}
}

// fakeSMTP is a one-connection SMTP server that keeps the DATA block. The email
// channel talks the protocol, so asserting the received message is the only
// honest test of it.
type fakeSMTP struct {
	ln   net.Listener
	msgs chan string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeSMTP{ln: ln, msgs: make(chan string, 1)}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	reply := func(line string) {
		w.WriteString(line + "\r\n")
		w.Flush()
	}
	reply("220 nightpost.test ESMTP")
	var body strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			// No STARTTLS and no AUTH offered: this test asserts the message, not
			// the TLS handshake.
			w.WriteString("250-nightpost.test\r\n250 HELP\r\n")
			w.Flush()
		case strings.HasPrefix(cmd, "MAIL FROM"), strings.HasPrefix(cmd, "RCPT TO"):
			reply("250 ok")
		case cmd == "DATA":
			reply("354 end with <CRLF>.<CRLF>")
			for {
				dataLine, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dataLine, "\r\n") == "." {
					break
				}
				body.WriteString(dataLine)
			}
			reply("250 queued")
		case cmd == "QUIT":
			reply("221 bye")
			f.msgs <- body.String()
			return
		default:
			reply("250 ok")
		}
	}
}

func TestChannelEmailMessageShape(t *testing.T) {
	fake := newFakeSMTP(t)
	mailer := &Email{Cfg: Channel{
		ID: "ops-mail", Type: "email", SMTPHost: fake.ln.Addr().String(),
		From: "alerts@nightpost.test", To: []string{"ops@nightpost.test"},
	}}
	if err := mailer.Send("Slow route", "GET /page p95 2400ms"); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case msg := <-fake.msgs:
		for _, want := range []string{
			"From: alerts@nightpost.test",
			"To: ops@nightpost.test",
			"Subject: Slow route",
			"Content-Type: text/plain; charset=utf-8",
			"GET /page p95 2400ms",
		} {
			if !strings.Contains(msg, want) {
				t.Fatalf("email is missing %q:\n%s", want, msg)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message reached the SMTP server")
	}
}
