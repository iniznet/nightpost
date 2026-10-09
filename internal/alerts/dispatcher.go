package alerts

import (
	"context"
	"fmt"
	"log"
	"os"
	"net/url"
	"strings"
	"time"

	"github.com/iniznet/fenrirealm-monitor/internal/store"
)

// Sender delivers one alert to one channel.
type Sender interface {
	Send(title, body string) error
}

// Event is one rule match awaiting dispatch.
type Event struct {
	Rule      string
	TargetKey string
	Title     string
	Body      string
	Link      string // path appended to cfg.BaseURL for the deep link
}

// Dispatcher evaluates rules on a tick and fans matched events out to the
// rule's channels under per-target cooldowns and per-channel caps. A send
// failure records nothing, so the same match is retried next tick; a
// success starts the cooldown clock — exactly-once-per-window semantics.
type Dispatcher struct {
	st      *store.Store
	cfg     *Config
	cfgPath string
	mtime   time.Time
	Senders map[string]Sender
}

func NewDispatcher(st *store.Store, cfg *Config, cfgPath string) *Dispatcher {
	d := &Dispatcher{st: st, cfg: cfg, cfgPath: cfgPath, Senders: map[string]Sender{}}
	for _, ch := range cfg.Channels {
		switch ch.Type {
		case "discord":
			d.Senders[ch.ID] = &Discord{Webhook: ch.Webhook}
		case "email":
			d.Senders[ch.ID] = &Email{Cfg: ch}
		}
	}
	if cfgPath != "" {
		if fi, err := os.Stat(cfgPath); err == nil {
			d.mtime = fi.ModTime()
		}
	}
	return d
}

// Run ticks until ctx is cancelled; -alerts-config unset never starts it.
func (d *Dispatcher) Run(ctx context.Context) {
	tick := time.Duration(d.cfg.TickSecs) * time.Second
	if tick <= 0 {
		tick = 60 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.reloadIfChanged()
			d.Tick(time.Now())
		}
	}
}

func (d *Dispatcher) reloadIfChanged() {
	if d.cfgPath == "" {
		return
	}
	fi, err := os.Stat(d.cfgPath)
	if err != nil || !fi.ModTime().After(d.mtime) {
		return
	}
	cfg, err := LoadConfig(d.cfgPath)
	if err != nil {
		log.Printf("alerts: reload failed (keeping previous config): %v", err)
		return
	}
	d.cfg = cfg
	d.mtime = fi.ModTime()
	for _, ch := range cfg.Channels {
		switch ch.Type {
		case "discord":
			d.Senders[ch.ID] = &Discord{Webhook: ch.Webhook}
		case "email":
			d.Senders[ch.ID] = &Email{Cfg: ch}
		}
	}
	log.Printf("alerts: config reloaded (%d channels, %d rules)", len(cfg.Channels), len(cfg.Rules))
}

// Tick evaluates every rule once and dispatches fresh events. Exported
// shape (now param + return) keeps the cooldown logic test-pinned.
func (d *Dispatcher) Tick(now time.Time) []Event {
	var sent []Event
	for _, r := range d.cfg.Rules {
		for _, ev := range d.evaluate(r, now) {
			for _, chID := range r.Channels {
				if !d.allowed(r, ev, chID, now) {
					continue
				}
				sender := d.Senders[chID]
				if sender == nil {
					continue
				}
				msg := ev.Title + "\n" + ev.Body
				if d.cfg.BaseURL != "" && ev.Link != "" {
					msg += "\n" + d.cfg.BaseURL + ev.Link
				}
				if err := sender.Send(ev.Title, msg); err != nil {
					log.Printf("alerts: send to %q failed (will retry next tick): %v", chID, err)
					continue
				}
				if err := d.st.RecordAlert(chID, ev.Rule, ev.TargetKey, now.Unix()); err != nil {
					log.Printf("alerts: record: %v", err)
				}
				sent = append(sent, ev)
			}
		}
	}
	return sent
}

// allowed applies per-target cooldown then per-channel caps.
func (d *Dispatcher) allowed(r Rule, ev Event, chID string, now time.Time) bool {
	last, err := d.st.LastAlertSent(ev.Rule, ev.TargetKey)
	if err != nil {
		log.Printf("alerts: cooldown lookup: %v", err)
		return false
	}
	if now.Unix()-last < int64(r.cooldownMinutes())*60 {
		return false
	}
	ch := d.cfg.Channel(chID)
	if ch == nil {
		return false
	}
	if ch.HourlyCap > 0 {
		n, err := d.st.ChannelAlertCount(chID, now.Add(-time.Hour).Unix())
		if err != nil {
			return false
		}
		if n >= int64(ch.HourlyCap) {
			log.Printf("alerts: channel %q hourly cap reached, dropping %s/%s", chID, ev.Rule, ev.TargetKey)
			return false
		}
	}
	if ch.DailyCeiling > 0 {
		n, err := d.st.ChannelAlertCount(chID, now.Add(-24*time.Hour).Unix())
		if err != nil {
			return false
		}
		if n >= int64(ch.DailyCeiling) {
			return false
		}
	}
	return true
}

func (d *Dispatcher) evaluate(r Rule, now time.Time) []Event {
	projects := strings.Join(r.Projects, ",")
	switch r.Type {
	case "new-issue":
		rows, err := d.st.NewIssueEvents(now.Add(-time.Duration(r.windowMinutes())*time.Minute).Unix(), r.Projects)
		if err != nil {
			log.Printf("alerts: new-issue eval: %v", err)
			return nil
		}
		out := make([]Event, 0, len(rows))
		for _, iss := range rows {
			out = append(out, Event{Rule: r.Type, TargetKey: iss.ProjectKey + "|" + iss.Fingerprint,
				Title: fmt.Sprintf("New issue in %s: %s", iss.ProjectKey, trunc(iss.Title, 120)),
				Body:  fmt.Sprintf("First seen %ds ago (projects: %s)", now.Unix()-iss.FirstSeenAt, projects),
				Link:  fmt.Sprintf("/issues/%d", iss.ID)})
		}
		return out
	case "regression":
		rows, err := d.st.Regressions(r.Projects)
		if err != nil {
			log.Printf("alerts: regression eval: %v", err)
			return nil
		}
		out := make([]Event, 0, len(rows))
		for _, iss := range rows {
			out = append(out, Event{Rule: r.Type, TargetKey: iss.ProjectKey + "|" + iss.Fingerprint,
				Title: fmt.Sprintf("Regressed in %s: %s", iss.ProjectKey, trunc(iss.Title, 120)),
				Body:  "A resolved issue fired again — reopened automatically.",
				Link:  fmt.Sprintf("/issues/%d", iss.ID)})
		}
		return out
	case "count-window":
		rows, err := d.st.CountBreaches(now.Add(-time.Duration(r.windowMinutes())*time.Minute).Unix(), r.Threshold, r.Projects)
		if err != nil {
			log.Printf("alerts: count eval: %v", err)
			return nil
		}
		out := make([]Event, 0, len(rows))
		for _, cb := range rows {
			out = append(out, Event{Rule: r.Type, TargetKey: cb.ProjectKey + "|" + cb.Fingerprint,
				Title: fmt.Sprintf("%d occurrences in %dm: %s", cb.N, r.windowMinutes(), trunc(cb.Title, 120)),
				Body:  fmt.Sprintf("Project %s crossed the count-window threshold of %d.", cb.ProjectKey, r.Threshold),
				Link:  "/issues?status=all"})
		}
		return out
	case "p95-latency":
		hours := r.windowMinutes() / 60
		if hours < 1 {
			hours = 1
		}
		var out []Event
		for _, p := range r.Projects {
			routes, err := d.st.ProfileRouteStats(p, store.ProfileTypeRequest, hours, 100)
			if err != nil {
				log.Printf("alerts: p95 eval: %v", err)
				continue
			}
			for _, rt := range routes {
				if rt.P95Ms < r.ThresholdMs {
					continue
				}
				out = append(out, Event{Rule: r.Type, TargetKey: p + "|" + rt.Name,
					Title: fmt.Sprintf("p95 %.0fms on %s: %s", rt.P95Ms, p, trunc(rt.Name, 100)),
					Body:  fmt.Sprintf("%d requests in the last %dh crossed the %.0fms threshold.", rt.Count, hours, r.ThresholdMs),
					Link:  "/profiles?project=" + url.QueryEscape(p) + "&type=request"})
			}
		}
		return out
	}
	return nil
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
