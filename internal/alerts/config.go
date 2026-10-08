package alerts

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Config is the -alerts-config JSON document, hot-reloaded on mtime change.
// No alert-configuration UI in M4: the file is the product surface (#157).
type Config struct {
	BaseURL   string    `json:"base_url"`
	TickSecs  int       `json:"tick_seconds"`
	Channels  []Channel `json:"channels"`
	Rules     []Rule    `json:"rules"`
}

type Channel struct {
	ID           string   `json:"id"`
	Type         string   `json:"type"` // "discord" | "email"
	Webhook      string   `json:"webhook"`
	SMTPHost     string   `json:"smtp_host"`
	SMTPUser     string   `json:"smtp_user"`
	SMTPPass     string   `json:"smtp_pass"`
	From         string   `json:"from"`
	To           []string `json:"to"`
	StartTLS     bool     `json:"starttls"`
	HourlyCap    int      `json:"hourly_cap"`
	DailyCeiling int      `json:"daily_ceiling"`
}

type Rule struct {
	Type            string   `json:"type"` // new-issue | regression | count-window | p95-latency
	Projects        []string `json:"projects"`
	Channels        []string `json:"channels"`
	WindowMinutes   int      `json:"window_minutes"`
	Threshold       int64    `json:"threshold"`     // count-window: occurrences
	ThresholdMs     float64  `json:"threshold_ms"`  // p95-latency
	CooldownMinutes int      `json:"cooldown_minutes"`
}

var ruleTypes = map[string]bool{"new-issue": true, "regression": true, "count-window": true, "p95-latency": true}

func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("alerts config: %w", err)
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("alerts config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.TickSecs < 0 {
		return fmt.Errorf("alerts config: tick_seconds must be positive")
	}
	seen := map[string]bool{}
	for i, ch := range c.Channels {
		if ch.ID == "" || seen[ch.ID] {
			return fmt.Errorf("alerts config: channel %d: id empty or duplicate", i)
		}
		seen[ch.ID] = true
		switch ch.Type {
		case "discord":
			if !strings.HasPrefix(ch.Webhook, "https://") && !strings.HasPrefix(ch.Webhook, "http://") {
				return fmt.Errorf("alerts config: channel %q: webhook must be an http(s) URL", ch.ID)
			}
		case "email":
			if ch.SMTPHost == "" || ch.From == "" || len(ch.To) == 0 {
				return fmt.Errorf("alerts config: channel %q: smtp_host, from and to are required", ch.ID)
			}
		default:
			return fmt.Errorf("alerts config: channel %q: unknown type %q", ch.ID, ch.Type)
		}
	}
	for i, r := range c.Rules {
		if !ruleTypes[r.Type] {
			return fmt.Errorf("alerts config: rule %d: unknown type %q", i, r.Type)
		}
		if len(r.Channels) == 0 {
			return fmt.Errorf("alerts config: rule %d (%s): no channels", i, r.Type)
		}
		for _, ch := range r.Channels {
			if !seen[ch] {
				return fmt.Errorf("alerts config: rule %d (%s): unknown channel %q", i, r.Type, ch)
			}
		}
		switch r.Type {
		case "count-window":
			if r.Threshold <= 0 {
				return fmt.Errorf("alerts config: rule %d: threshold must be positive", i)
			}
		case "p95-latency":
			if r.ThresholdMs <= 0 {
				return fmt.Errorf("alerts config: rule %d: threshold_ms must be positive", i)
			}
		}
	}
	return nil
}

func (c *Config) Channel(id string) *Channel {
	for i := range c.Channels {
		if c.Channels[i].ID == id {
			return &c.Channels[i]
		}
	}
	return nil
}

func (r Rule) cooldownMinutes() int {
	if r.CooldownMinutes > 0 {
		return r.CooldownMinutes
	}
	return 60
}

func (r Rule) windowMinutes() int {
	if r.WindowMinutes > 0 {
		return r.WindowMinutes
	}
	return 15
}
