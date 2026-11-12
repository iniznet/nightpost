package alerts

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/iniznet/nightpost/internal/store"
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
// rule's channels under a per-(rule, target) cooldown and per-channel caps. A
// success starts the cooldown clock, which is what makes an alert exactly-once
// per window; a failure records nothing, so the target is retried once the
// window lapses.
type Dispatcher struct {
	st        *store.Store
	cfg       *Config
	cfgPath   string
	mtime     time.Time
	startedAt time.Time
	Senders   map[string]Sender
}

func NewDispatcher(st *store.Store, cfg *Config, cfgPath string) *Dispatcher {
	d := &Dispatcher{st: st, cfg: cfg, cfgPath: cfgPath, Senders: sendersFor(cfg), startedAt: time.Now()}
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
	d.Senders = sendersFor(cfg)
	log.Printf("alerts: config reloaded (%d channels, %d rules)", len(cfg.Channels), len(cfg.Rules))
}

// sendersFor builds one sender per configured channel. Boot and reload share it,
// so a hot-reloaded config can never gain a channel type the boot path knows and
// the reload path forgot.
func sendersFor(cfg *Config) map[string]Sender {
	senders := make(map[string]Sender, len(cfg.Channels))
	for _, ch := range cfg.Channels {
		switch ch.Type {
		case "discord":
			senders[ch.ID] = &Discord{Webhook: ch.Webhook}
		case "email":
			senders[ch.ID] = &Email{Cfg: ch}
		case "slack":
			senders[ch.ID] = &Slack{Webhook: ch.Webhook}
		case "telegram":
			senders[ch.ID] = &Telegram{Token: ch.BotToken, ChatID: ch.ChatID, APIBase: ch.APIBase}
		case "webhook":
			senders[ch.ID] = &Webhook{URL: ch.Webhook}
		}
	}
	return senders
}

// Tick evaluates every rule once and dispatches fresh events. Exported
// shape (now param + return) keeps the cooldown logic test-pinned.
func (d *Dispatcher) Tick(now time.Time) []Event {
	var sent []Event
	for _, r := range d.cfg.Rules {
		for _, ev := range d.evaluate(r, now) {
			// The cooldown is per (rule, target), so it is decided ONCE per event,
			// before the fan-out. Checking it inside the channel loop let the first
			// channel's own send record the window and then silence every channel
			// after it — a second channel received nothing, ever.
			if !d.cooledDown(r, ev, now) {
				continue
			}
			msg := ev.Title + "\n" + ev.Body
			if d.cfg.BaseURL != "" && ev.Link != "" {
				msg += "\n" + d.cfg.BaseURL + ev.Link
			}
			for _, chID := range r.Channels {
				if !d.channelAllows(chID, ev, now) {
					continue
				}
				sender := d.Senders[chID]
				if sender == nil {
					continue
				}
				if err := sender.Send(ev.Title, msg); err != nil {
					log.Printf("alerts: send to %q failed (retried once the window lapses): %v", chID, err)
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

// cooledDown is the per-(rule, target) window from the alerts_log ledger.
func (d *Dispatcher) cooledDown(r Rule, ev Event, now time.Time) bool {
	last, err := d.st.LastAlertSent(ev.Rule, ev.TargetKey)
	if err != nil {
		log.Printf("alerts: cooldown lookup: %v", err)
		return false
	}
	return now.Unix()-last >= int64(r.cooldownMinutes())*60
}

// channelAllows applies one channel's hourly cap and daily ceiling. The event is
// passed only so the drop is logged with the rule and target that lost it.
func (d *Dispatcher) channelAllows(chID string, ev Event, now time.Time) bool {
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
	case "heartbeat":
		mult := r.GraceMultiplier
		if mult <= 0 {
			mult = 2
		}
		need := r.CadenceSeconds * int64(mult)
		var out []Event
		for _, p := range r.Projects {
			lastOK, err := d.st.JobLastOKAt(p, "command", r.Command)
			if err != nil {
				log.Printf("alerts: heartbeat eval %s/%s: %v", p, r.Command, err)
				continue
			}
			ageFrom := lastOK
			if ageFrom == 0 {
				// Never observed: the clock starts at monitor boot so a
				// fresh deploy cannot false-fire before one full window.
				ageFrom = d.startedAt.Unix()
			}
			if now.Unix()-ageFrom < need {
				continue
			}
			body := fmt.Sprintf("last success %ds ago (expected within %ds)", now.Unix()-lastOK, need)
			if lastOK == 0 {
				body = fmt.Sprintf("no successful run recorded since monitor boot %ds ago (expected every %ds)", now.Unix()-d.startedAt.Unix(), r.CadenceSeconds)
			}
			out = append(out, Event{Rule: r.Type, TargetKey: p + "|" + r.Command,
				Title: fmt.Sprintf("Heartbeat stale in %s: %s", p, trunc(r.Command, 100)),
				Body:  body, Link: "/jobs?project=" + p})
		}
		return out
	case "missed_run":
		rows, err := d.st.MissedJobs(r.Projects)
		if err != nil {
			log.Printf("alerts: missed_run eval: %v", err)
			return nil
		}
		out := make([]Event, 0, len(rows))
		for _, m := range rows {
			out = append(out, Event{Rule: r.Type, TargetKey: m.ProjectKey + "|" + m.Kind + "|" + m.Name,
				Title: fmt.Sprintf("Missed run in %s: %s", m.ProjectKey, trunc(m.Name, 100)),
				Body: fmt.Sprintf("expected at %s (+%ds grace), last run %ds ago",
					time.Unix(m.ExpectedAt, 0).UTC().Format(time.RFC3339), m.GraceSeconds, now.Unix()-m.LastRunAt),
				Link: "/jobs?project=" + url.QueryEscape(m.ProjectKey)})
		}
		return out
	case "query_slow":
		rows, err := d.st.SlowQueries(now.Add(-time.Duration(r.windowMinutes())*time.Minute).Unix(), r.ThresholdMs, r.Projects)
		if err != nil {
			log.Printf("alerts: query_slow eval: %v", err)
			return nil
		}
		out := make([]Event, 0, len(rows))
		for _, q := range rows {
			out = append(out, Event{Rule: r.Type, TargetKey: q.ProjectKey + "|" + q.Fingerprint,
				Title: fmt.Sprintf("Slow query %.0fms in %s", q.MaxMs, q.ProjectKey),
				Body:  fmt.Sprintf("%s ran %d times, worst %.0fms inside the last %dm.", trunc(q.SQL, 200), q.Count, q.MaxMs, r.windowMinutes()),
				Link:  "/queries?project=" + url.QueryEscape(q.ProjectKey)})
		}
		return out
	case "span_unreported":
		rows, err := d.st.UnreportedJourneys(now.Add(-time.Duration(r.windowMinutes())*time.Minute).Unix(), r.Projects)
		if err != nil {
			log.Printf("alerts: span_unreported eval: %v", err)
			return nil
		}
		out := make([]Event, 0, len(rows))
		for _, g := range rows {
			coverage := 0.0
			if g.Hops > 0 {
				coverage = float64(g.Hops-g.Unreported) / float64(g.Hops) * 100
			}
			out = append(out, Event{Rule: r.Type, TargetKey: g.ProjectKey + "|" + g.PatternKey,
				Title: fmt.Sprintf("Unreported hop in %s: %s", g.ProjectKey, trunc(g.EntryPattern, 100)),
				Body:  fmt.Sprintf("%d of %d hops reported nothing on this path; coverage %.0f%%.", g.Unreported, g.Hops, coverage),
				Link:  "/performance?project=" + url.QueryEscape(g.ProjectKey)})
		}
		return out
	case "release_regression":
		points := r.Points
		if points <= 0 {
			points = 3
		}
		drop := r.DropPoints
		if drop <= 0 {
			drop = 5
		}
		rows, err := d.st.ReleaseCrashPoints(r.Projects, points+1)
		if err != nil {
			log.Printf("alerts: release_regression eval: %v", err)
			return nil
		}
		// The store returns newest-first per project, so the baseline is the rows
		// after the head; a project with fewer than points+1 releases has no
		// baseline to compare against and stays silent rather than firing on the
		// first deploy it ever sees.
		byProject := map[string][]store.ReleaseCrashPoint{}
		for _, p := range rows {
			byProject[p.ProjectKey] = append(byProject[p.ProjectKey], p)
		}
		projects := make([]string, 0, len(byProject))
		for key := range byProject {
			projects = append(projects, key)
		}
		sort.Strings(projects)
		var out []Event
		for _, project := range projects {
			releases := byProject[project]
			if len(releases) < points+1 {
				continue
			}
			latest := releases[0]
			baseline := 0.0
			for _, prev := range releases[1 : points+1] {
				baseline += prev.CrashFree
			}
			baseline /= float64(points)
			if latest.CrashFree*100 > baseline*100-drop {
				continue
			}
			out = append(out, Event{Rule: r.Type, TargetKey: project + "|" + latest.Version,
				Title: fmt.Sprintf("Crash-free down in %s: %s", project, trunc(latest.Version, 60)),
				Body: fmt.Sprintf("release %s is %.1f%% crash-free against a %.1f%% baseline over the previous %d release(s).",
					latest.Version, latest.CrashFree*100, baseline*100, points),
				Link: "/releases?project=" + url.QueryEscape(project)})
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
