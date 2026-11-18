// Package redact performs best-effort credential redaction and removes terminal controls.
// It complements whitelisted collection; it cannot recognize every application secret.
package redact

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const Redacted = "[REDACTED]"

var (
	sensitiveKey   = regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|authorization|credential|api[-_]?key|access[-_]?key|private[-_]?key)`)
	privateKey     = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	auth           = regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9._~+/=-]+`)
	keyValue       = regexp.MustCompile(`(?i)(["']?(?:password|passwd|pwd|secret|token|authorization|credential|api[-_]?key|access[-_]?key|client[-_]?secret)["']?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;&}\]]+)`)
	urlCredentials = regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://)[^\s/@]+:[^\s/@]+@`)
	jwt            = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)
	ansi           = regexp.MustCompile("\x1b(?:\\[[0-?]*[ -/]*[@-~]|\\][^\x07]*(?:\x07|\x1b\\\\))")
)

func Text(value string, maxChars int) string {
	value = Sanitize(value, -1)
	value = privateKey.ReplaceAllString(value, Redacted)
	value = auth.ReplaceAllString(value, "${1} "+Redacted)
	value = keyValue.ReplaceAllString(value, "${1}"+Redacted)
	value = urlCredentials.ReplaceAllString(value, "${1}"+Redacted+"@")
	value = jwt.ReplaceAllString(value, Redacted)
	return bound(value, maxChars)
}

// Sanitize removes terminal controls and bounds already-redacted display text.
// It preserves quoting in serialized formats; raw evidence must use Text or Value.
func Sanitize(value string, maxChars int) string {
	value = ansi.ReplaceAllString(value, "")
	value = strings.Map(func(r rune) rune {
		if (r < 32 && r != '\n' && r != '\t') || (r >= 127 && r <= 159) {
			return -1
		}
		return r
	}, value)
	return bound(value, maxChars)
}

func bound(value string, maxChars int) string {
	runes := []rune(value)
	if maxChars >= 0 && len(runes) > maxChars {
		return string(runes[:maxChars]) + "\n[TRUNCATED]"
	}
	return value
}

func Value(value any, maxChars int) any {
	// Normalize typed Kubernetes structs/slices without retaining mutable aliases.
	data, err := json.Marshal(value)
	if err != nil {
		return Text(fmt.Sprint(value), maxChars)
	}
	var normalized any
	if json.Unmarshal(data, &normalized) != nil {
		return nil
	}
	return walk(normalized, maxChars, 0)
}

func walk(value any, maxChars, depth int) any {
	if depth >= 12 {
		return "[MAX_DEPTH]"
	}
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > 100 {
			keys = keys[:100]
		}
		result := make(map[string]any, len(keys))
		for _, k := range keys {
			if sensitiveKey.MatchString(k) {
				result[Text(k, 200)] = Redacted
			} else {
				result[Text(k, 200)] = walk(v[k], maxChars, depth+1)
			}
		}
		return result
	case []any:
		if len(v) > 100 {
			v = v[:100]
		}
		result := make([]any, len(v))
		for i, item := range v {
			result[i] = walk(item, maxChars, depth+1)
		}
		return result
	case string:
		return Text(v, maxChars)
	default:
		return v
	}
}
