package alerts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "alerts.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigValid(t *testing.T) {
	p := writeCfg(t, `{
		"base_url": "http://monitor.local",
		"tick_seconds": 60,
		"channels": [{"id": "ops", "type": "discord", "webhook": "https://discord.com/api/webhooks/1/x", "hourly_cap": 6, "daily_ceiling": 20}],
		"rules": [{"type": "new-issue", "channels": ["ops"], "projects": ["front"], "cooldown_minutes": 30}]
	}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Channels) != 1 || len(cfg.Rules) != 1 || cfg.BaseURL != "http://monitor.local" {
		t.Fatalf("bad parse: %+v", cfg)
	}
	if cfg.Rules[0].cooldownMinutes() != 30 || cfg.Rules[0].windowMinutes() != 15 {
		t.Fatal("defaults/overrides wrong")
	}
}

func TestLoadConfigRejects(t *testing.T) {
	cases := map[string]string{
		"unknown field":     `{"channels": [], "rules": [], "typo": 1}`,
		"dup channel id":    `{"channels": [{"id":"a","type":"discord","webhook":"https://x"},{"id":"a","type":"discord","webhook":"https://y"}], "rules": []}`,
		"bad webhook":       `{"channels": [{"id":"a","type":"discord","webhook":"ftp://x"}], "rules": []}`,
		"unknown rule":      `{"channels": [{"id":"a","type":"discord","webhook":"https://x"}], "rules": [{"type":"nope","channels":["a"]}]}`,
		"unknown channel":   `{"channels": [{"id":"a","type":"discord","webhook":"https://x"}], "rules": [{"type":"regression","channels":["b"]}]}`,
		"threshold missing": `{"channels": [{"id":"a","type":"discord","webhook":"https://x"}], "rules": [{"type":"count-window","channels":["a"],"threshold":0}]}`,
		"email incomplete":  `{"channels": [{"id":"a","type":"email","smtp_host":"h","from":"f@x","to":[]}], "rules": []}`,
	}
	for name, body := range cases {
		if _, err := LoadConfig(writeCfg(t, body)); err == nil {
			t.Fatalf("%s: must reject", name)
		} else if !strings.Contains(err.Error(), "alerts config") {
			t.Fatalf("%s: wrong error %v", name, err)
		}
	}
}
