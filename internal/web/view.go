package web

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// OccView is a UI-shaped occurrence: the handler layer extracts stacks,
// fields and raw JSON from the stored payload so the templates never walk
// raw JSON structures.
type Frame struct {
	Func string
	File string
	Line string
}

type KV struct {
	K string
	V string
}

type OccView struct {
	When   string
	Kind   string
	Title  string
	Fields []KV
	Stack  []Frame
	Raw    string
}

var jsFrameRe = regexp.MustCompile(`^(?:at\s+)?(.+?)\s*\((.+?):(\d+)(?::\d+)?\)$`)
var jsFrameRe2 = regexp.MustCompile(`^(?:at\s+)?(.+?):(\d+)(?::\d+)?$`)

// buildOccViews converts store.Occurrences rows into renderable views.
func buildOccViews(entries []map[string]any) []OccView {
	out := make([]OccView, 0, len(entries))
	for _, e := range entries {
		v := OccView{}
		v.When, _ = e["seen_at"].(string)
		payload, _ := e["payload"].(map[string]any)
		if payload != nil {
			v.Raw = prettyJSON(payload)
		}
		switch {
		case hasException(payload):
			v.Kind = "exception"
			fillSentry(payload, &v)
		case hasKey(payload, "stack"):
			v.Kind = "js-error"
			fillJSError(payload, &v)
		case hasKey(payload, "channel"):
			v.Kind = "log"
			fillLog(payload, &v)
		default:
			v.Kind = "payload"
			v.Fields = flatten(payload, 15)
		}
		out = append(out, v)
	}
	return out
}

func hasException(p map[string]any) bool {
	exc, ok := p["exception"].(map[string]any)
	if !ok {
		return false
	}
	vals, ok := exc["values"].([]any)
	return ok && len(vals) > 0
}

func hasKey(p map[string]any, k string) bool {
	if p == nil {
		return false
	}
	_, ok := p[k]
	return ok
}

// fillSentry extracts the stored Sentry event: primary exception summary,
// request context fields and the in-app stack frames (SDK frames are
// oldest-first; the UI shows the throwing frame first).
func fillSentry(event map[string]any, v *OccView) {
	exc, _ := event["exception"].(map[string]any)
	values, _ := exc["values"].([]any)
	if len(values) > 0 {
		if first, ok := values[len(values)-1].(map[string]any); ok {
			class, _ := first["type"].(string)
			value, _ := first["value"].(string)
			v.Title = truncate(class+": "+value, 120)
			trace, _ := first["stacktrace"].(map[string]any)
			frames, _ := trace["frames"].([]any)
			for i := len(frames) - 1; i >= 0 && len(v.Stack) < 40; i-- {
				f, ok := frames[i].(map[string]any)
				if !ok {
					continue
				}
				fn, _ := f["function"].(string)
				if fn == "" {
					fn, _ = f["module"].(string)
				}
				file, _ := f["filename"].(string)
				line := fmtVal(f["lineno"])
				if fn == "" && file == "" {
					continue
				}
				v.Stack = append(v.Stack, Frame{Func: fn, File: file, Line: line})
			}
		}
	}
	for _, k := range []string{"level", "environment", "release", "transaction", "server_name"} {
		if s, ok := event[k].(string); ok && s != "" {
			v.Fields = append(v.Fields, KV{K: k, V: s})
		}
	}
	if req, ok := event["request"].(map[string]any); ok {
		if u, ok := req["url"].(string); ok && u != "" {
			v.Fields = append(v.Fields, KV{K: "url", V: u})
		}
		if m, ok := req["method"].(string); ok && m != "" {
			v.Fields = append(v.Fields, KV{K: "method", V: m})
		}
	}
}

// fillJSError extracts the first-party JS error report.
func fillJSError(p map[string]any, v *OccView) {
	msg, _ := p["message"].(string)
	v.Title = truncate(msg, 120)
	stack, _ := p["stack"].(string)
	for _, line := range strings.Split(stack, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Error") {
			continue
		}
		if len(v.Stack) >= 40 {
			break
		}
		v.Stack = append(v.Stack, parseJSFrame(line))
	}
	for _, k := range []string{"url", "route", "environment", "release"} {
		if s, ok := p[k].(string); ok && s != "" {
			v.Fields = append(v.Fields, KV{K: k, V: s})
		}
	}
}

func parseJSFrame(line string) Frame {
	if m := jsFrameRe.FindStringSubmatch(line); m != nil {
		return Frame{Func: m[1], File: m[2], Line: m[3]}
	}
	if m := jsFrameRe2.FindStringSubmatch(line); m != nil {
		return Frame{File: m[1], Line: m[2]}
	}
	return Frame{Func: line}
}

// fillLog extracts the Monolog record shape.
func fillLog(p map[string]any, v *OccView) {
	channel, _ := p["channel"].(string)
	if channel != "" {
		v.Fields = append(v.Fields, KV{K: "channel", V: channel})
	}
	if ctx, ok := p["context"].(map[string]any); ok {
		v.Fields = append(v.Fields, flatten(ctx, 15)...)
	}
}

// flatten renders one map level as key/value rows, sorted for stable order.
func flatten(m map[string]any, cap int) []KV {
	if m == nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]KV, 0, len(keys))
	for _, k := range keys {
		if len(out) >= cap {
			break
		}
		val := m[k]
		if s, ok := val.(string); ok && s == "" {
			continue
		}
		if val == nil {
			continue
		}
		out = append(out, KV{K: k, V: fmtVal(val)})
	}
	return out
}

// fmtVal renders JSON-decoded values for display: scalars inline, nested
// containers as compact JSON.
func fmtVal(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case []any:
		if b, err := json.Marshal(t); err == nil {
			return string(b)
		}
	case map[string]any:
		if b, err := json.Marshal(t); err == nil {
			return string(b)
		}
	default:
		return fmt.Sprintf("%v", t)
	}
	return fmt.Sprintf("%v", v)
}

func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
