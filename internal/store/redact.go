package store

// Secret redaction and the dashboard display transformation. Nothing here
// touches SQLite; the store, exports and API responses share these helpers.

import (
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

// WebhookEnv names the notification destination variable; its value is a secret.
const WebhookEnv = "OCTOMUS_NOTIFICATION_WEBHOOK_URL"

// ErrorMessage renders an error for an operator, with secrets scrubbed. Errors
// reach operators through saved records and API responses, so every stored
// error message is built here rather than formatted at each site.
func ErrorMessage(err error) string { return Redact(err.Error()) }

// Whitespace includes Unicode White_Space, TAB–CR and NEL.
// Go's \s is ASCII-only, so use the equivalent class in every whitespace match.
const tokenWhitespace = `\p{Z}\x{0009}-\x{000D}\x{0085}`

var tokenPattern = regexp.MustCompile(`(?i)(bearer[` + tokenWhitespace + `]+)[A-Za-z0-9._~+/=-]+|(?:gh[pousr]_|github_pat_|sk-)[A-Za-z0-9_-]{10,}|[a-z]+://[^` + tokenWhitespace + `/@]+:[^` + tokenWhitespace + `/@]+@`)

var (
	secretsOnce sync.Once
	secrets     []string
)

func environmentSecrets() []string {
	secretsOnce.Do(func() {
		for _, entry := range os.Environ() {
			key, value, ok := strings.Cut(entry, "=")
			if !ok || len(value) < 8 {
				continue
			}
			if key == WebhookEnv || strings.Contains(key, "TOKEN") || strings.Contains(key, "SECRET") || strings.Contains(key, "PASSWORD") || strings.Contains(key, "API_KEY") {
				secrets = append(secrets, value)
			}
		}
	})
	return secrets
}

// RedactSecrets scrubs tokens and secret-bearing environment values without any
// length limit. Persisted results must be bounded by the caller so shortening is
// always flagged.
func RedactSecrets(input string) string {
	s := tokenPattern.ReplaceAllString(input, "[redacted]")
	for _, value := range environmentSecrets() {
		s = strings.ReplaceAll(s, value, "[redacted]")
	}
	return s
}

// displayTextLimit bounds every string a dashboard display value can carry,
// counted in characters on a rune boundary.
const displayTextLimit = 16384

// boundDisplayText shortens text to the display limit, cutting on a rune
// boundary, and reports whether anything was dropped.
func boundDisplayText(s string) (string, bool) {
	count := 0
	for i := range s {
		if count == displayTextLimit {
			return s[:i], true
		}
		count++
	}
	return s, false
}

// displayString applies the display transformation to one string and reports
// the kinds applied: "redacted" for secret scrubbing, "shortened" for the
// display length bound.
func displayString(s string) (string, []string) {
	kinds := []string{}
	redacted := RedactSecrets(s)
	if redacted != s {
		kinds = append(kinds, "redacted")
	}
	display, shortened := boundDisplayText(redacted)
	if shortened {
		kinds = append(kinds, "shortened")
	}
	return display, kinds
}

// Redact scrubs secrets and bounds the text to the display character limit.
func Redact(input string) string {
	s, _ := displayString(input)
	return s
}

// DisplayTransform records every string inside one top-level field whose
// display value differs from the canonical saved value, so an operator can
// tell a display preview from the stored original. Each path is a structured
// segment list — strings for object keys, numbers for array indices — so
// callers walk it directly rather than re-parsing a formatted path.
type DisplayTransform struct {
	Field string   `json:"field"`
	Kinds []string `json:"kinds"`
	Paths [][]any  `json:"paths"`
}

// DisplayJSON returns the display-safe form of a generic JSON object: every
// string passes through the same redaction and length bound as RedactJSON,
// and each altered string is reported by field, kind and structured JSON
// path. The result is display data only; it must never be treated as
// canonical executable configuration.
func DisplayJSON(object map[string]any) (map[string]any, []DisplayTransform) {
	transforms := map[string]*DisplayTransform{}
	var walk func(value any, path []any, field string) any
	walk = func(value any, path []any, field string) any {
		switch v := value.(type) {
		case string:
			display, kinds := displayString(v)
			if len(kinds) == 0 {
				return display
			}
			entry := transforms[field]
			if entry == nil {
				entry = &DisplayTransform{Field: field, Kinds: []string{}, Paths: [][]any{}}
				transforms[field] = entry
			}
			for _, kind := range kinds {
				if !slices.Contains(entry.Kinds, kind) {
					entry.Kinds = append(entry.Kinds, kind)
				}
			}
			entry.Paths = append(entry.Paths, path)
			return display
		case []any:
			for i := range v {
				v[i] = walk(v[i], append(slices.Clone(path), i), field)
			}
			return v
		case map[string]any:
			keys := make([]string, 0, len(v))
			for key := range v {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				v[key] = walk(v[key], append(slices.Clone(path), key), field)
			}
			return v
		default:
			return value
		}
	}
	fields := make([]string, 0, len(object))
	for field := range object {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	for _, field := range fields {
		object[field] = walk(object[field], []any{field}, field)
	}
	result := []DisplayTransform{}
	for _, field := range fields {
		if entry := transforms[field]; entry != nil {
			// walk visits fields, map keys and array indices in sorted order,
			// so Paths are already ordered; only Kinds needs sorting.
			sort.Strings(entry.Kinds)
			result = append(result, *entry)
		}
	}
	return object, result
}

// RedactJSON scrubs every string inside a generic JSON value in place.
func RedactJSON(value any) any {
	switch v := value.(type) {
	case string:
		return Redact(v)
	case []any:
		for i := range v {
			v[i] = RedactJSON(v[i])
		}
		return v
	case map[string]any:
		for k := range v {
			v[k] = RedactJSON(v[k])
		}
		return v
	default:
		return value
	}
}

// RedactedValue serializes a typed export, decodes it as generic JSON (numbers
// kept verbatim) and scrubs every string in place. Exports use it so redaction
// happens after the facts are computed from the saved records.
func RedactedValue(value any) (map[string]any, error) {
	generic, err := wirejson.GenericMap(value)
	if err != nil {
		return nil, err
	}
	RedactJSON(generic)
	return generic, nil
}
