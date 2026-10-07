package store

import (
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strings"
)

var (
	uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	hexRe  = regexp.MustCompile(`[0-9a-fA-F]{16,}`)
	numRe  = regexp.MustCompile(`[0-9]+`)
	strRe  = regexp.MustCompile(`"[^"]*"|'[^']*'`)
)

// ForException builds the per-defect grouping key: exception class + top
// frame + the message with volatile tokens (numbers, uuids, hex ids, quoted
// strings) blanked, so "user 42 not found" and "user 43 not found" collapse
// into one issue while distinct defects stay distinct.
func ForException(exceptionClass, message, topFrame string) string {
	return hash(exceptionClass + "|" + topFrame + "|" + normalize(message))
}

// ForLog groups log records by channel + level + normalized message.
func ForLog(channel, level, message string) string {
	return hash("log|" + channel + "|" + level + "|" + normalize(message))
}

func normalize(message string) string {
	s := uuidRe.ReplaceAllString(message, "<uuid>")
	s = hexRe.ReplaceAllString(s, "<hex>")
	s = strRe.ReplaceAllString(s, "<s>")
	s = numRe.ReplaceAllString(s, "<n>")
	return strings.TrimSpace(s)
}

func hash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}