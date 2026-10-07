package ingest

import (
    "encoding/json"
    "io"
    "log"
    "net/http"
    "strings"

    "github.com/iniznet/fenrirealm-monitor/internal/store"
)

const maxEnvelopeBytes = 10 << 20 // 10 MiB

// SentryHandler accepts a Sentry envelope POST — the wire protocol
// sentry/sentry-laravel speaks when its DSN points at this server. Body is
// newline-delimited JSON: one event header line, then repeating
// (item header, payload) line pairs.
func SentryHandler(st *store.Store) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        projectKey := r.PathValue("project")
        if projectKey == "" {
            http.Error(w, "missing project", http.StatusBadRequest)
            return
        }

        body, err := io.ReadAll(io.LimitReader(r.Body, maxEnvelopeBytes))
        if err != nil {
            http.Error(w, "read body", http.StatusBadRequest)
            return
        }
        lines := strings.Split(strings.TrimSpace(string(body)), "\n")
        if len(lines) < 2 {
            http.Error(w, "invalid envelope", http.StatusBadRequest)
            return
        }

        var header struct {
            EventID     string `json:"event_id"`
            Environment string `json:"environment"`
        }
        if err := json.Unmarshal([]byte(lines[0]), &header); err != nil || header.EventID == "" {
            http.Error(w, "invalid envelope header", http.StatusBadRequest)
            return
        }

        recorded := 0
        for i := 1; i+1 < len(lines); i += 2 {
            var itemHeader struct {
                Type string `json:"type"`
            }
            if err := json.Unmarshal([]byte(lines[i]), &itemHeader); err != nil {
                continue
            }
            if itemHeader.Type != "event" {
                continue
            }
            var event map[string]any
            if err := json.Unmarshal([]byte(lines[i+1]), &event); err != nil {
                continue
            }
            recorded += recordEvent(st, projectKey, header.Environment, event)
        }

        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusOK)
        _ = json.NewEncoder(w).Encode(map[string]any{"recorded": recorded})
    }
}

func recordEvent(st *store.Store, projectKey, environment string, event map[string]any) int {
    level, _ := event["level"].(string)
    if level == "" {
        level = "error"
    }

    recorded := 0
    if excValues, ok := event["exception"].(map[string]any); ok {
        if values, ok := excValues["values"].([]any); ok {
            for _, v := range values {
                exc, ok := v.(map[string]any)
                if !ok {
                    continue
                }
                class, _ := exc["type"].(string)
                message, _ := exc["value"].(string)
                if class == "" && message == "" {
                    continue
                }
                if class == "" {
                    class = "Exception"
                }
                if message == "" {
                    message = "unknown exception"
                }
                fp := store.ForException(class, message, topFrame(exc))
                if _, err := st.RecordError(projectKey, class, message, fp, environment, level, event); err != nil {
                    log.Printf("record error: %v", err)
                    continue
                }
                recorded++
            }
        }
    }

    if recorded == 0 {
        message, _ := event["message"].(string)
        if message == "" {
            message = "unknown event"
        }
        fp := store.ForException("Message", message, "")
        if _, err := st.RecordError(projectKey, "Message", message, fp, environment, level, event); err != nil {
            log.Printf("record message event: %v", err)
            return 0
        }
        recorded = 1
    }

    return recorded
}

// topFrame picks the deepest in-app frame for grouping; Sentry orders frames
// oldest-first, so the last in_app frame is the one that threw.
func topFrame(exc map[string]any) string {
    trace, ok := exc["stacktrace"].(map[string]any)
    if !ok {
        return ""
    }
    frames, ok := trace["frames"].([]any)
    if !ok || len(frames) == 0 {
        return ""
    }
    for i := len(frames) - 1; i >= 0; i-- {
        frame, ok := frames[i].(map[string]any)
        if !ok {
            continue
        }
        class, _ := frame["module"].(string)
        if class == "" {
            class, _ = frame["filename"].(string)
        }
        fn, _ := frame["function"].(string)
        if class != "" || fn != "" {
            return class + ":" + fn
        }
    }
    return ""
}