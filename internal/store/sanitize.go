package store

import "strings"

var secretKeyFragments = []string{
	"password", "passwd", "secret", "token", "authorization", "cookie",
	"credit", "card", "api_key", "apikey", "auth",
}

const (
	redactedMarker  = "[REDACTED]"
	truncatedMarker = "[TRUNCATED]"
	maxDepth        = 8
	maxStringLength = 2000
)

// Clean recursively redacts values under secret-shaped keys and truncates
// oversized strings so one hostile payload cannot balloon a row.
func Clean(value any) any {
	return clean(value, 0)
}

func clean(value any, depth int) any {
	if depth > maxDepth {
		return truncatedMarker
	}
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			if isSecretKey(k) {
				out[k] = redactedMarker
				continue
			}
			out[k] = clean(item, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = clean(item, depth+1)
		}
		return out
	case string:
		if len(v) > maxStringLength {
			return v[:maxStringLength] + "…"
		}
		return v
	default:
		return value
	}
}

func isSecretKey(key string) bool {
	lower := strings.ToLower(key)
	for _, fragment := range secretKeyFragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}