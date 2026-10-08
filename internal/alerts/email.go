package alerts

import (
	"crypto/tls"
	"fmt"
	"net/smtp"
	"strings"
)

// Email delivers alerts over SMTP with opportunistic STARTTLS; credentials
// come from the config file (mounted secret), never from the UI (#157).
type Email struct {
	Cfg Channel
}

func (e *Email) Send(title, body string) error {
	host := e.Cfg.SMTPHost
	serverName := host
	if i := strings.IndexByte(host, ':'); i > 0 {
		serverName = host[:i]
	}
	msg := "From: " + e.Cfg.From + "\r\n" +
		"To: " + strings.Join(e.Cfg.To, ", ") + "\r\n" +
		"Subject: " + title + "\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n"

	var auth smtp.Auth
	if e.Cfg.SMTPUser != "" {
		auth = smtp.PlainAuth(e.Cfg.SMTPUser, e.Cfg.SMTPUser, e.Cfg.SMTPPass, serverName)
	}

	if e.Cfg.StartTLS || strings.HasSuffix(host, ":465") {
		return e.sendTLS(serverName, host, msg, auth)
	}
	if err := smtp.SendMail(host, auth, e.Cfg.From, e.Cfg.To, []byte(msg)); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	return nil
}

func (e *Email) sendTLS(serverName, host, msg string, auth smtp.Auth) error {
	tc := &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	if strings.HasSuffix(host, ":465") {
		conn, err := tls.Dial("tcp", host, tc)
		if err != nil {
			return fmt.Errorf("smtps: %w", err)
		}
		cli, err := smtp.NewClient(conn, serverName)
		if err != nil {
			conn.Close()
			return err
		}
		defer cli.Close()
		return e.mailVia(cli, msg, auth)
	}
	cli, err := smtp.Dial(host)
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	defer cli.Close()
	if err := cli.StartTLS(tc); err != nil {
		return fmt.Errorf("smtp starttls: %w", err)
	}
	return e.mailVia(cli, msg, auth)
}

func (e *Email) mailVia(cli *smtp.Client, msg string, auth smtp.Auth) error {
	if auth != nil {
		if ok, _ := cli.Extension("AUTH"); ok {
			if err := cli.Auth(auth); err != nil {
				return fmt.Errorf("smtp auth: %w", err)
			}
		}
	}
	if err := cli.Mail(e.Cfg.From); err != nil {
		return err
	}
	for _, to := range e.Cfg.To {
		if err := cli.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := cli.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	return w.Close()
}
