package redact

import (
	"cmp"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const TokenEnv = "OCTOMUS_TOKEN"

const WebhookEnv = "OCTOMUS_NOTIFICATION_WEBHOOK_URL"

func Error(err error) string { return Text(err.Error()) }

const tokenWhitespace = `\p{Z}\x{0009}-\x{000D}\x{0085}`

var tokenPattern = regexp.MustCompile(`(?i)(bearer[` + tokenWhitespace + `]+)[A-Za-z0-9._~+/=-]+|(?:gh[pousr]_|github_pat_)[A-Za-z0-9_-]{10,}|[a-z]+://[^` + tokenWhitespace + `/@]+:[^` + tokenWhitespace + `/@]+@`)

var keyPattern = regexp.MustCompile(`(?i)(?:^|[^A-Za-z]|\\(?:u[0-9A-Fa-f]{4}|x[0-9A-Fa-f]{2}|[A-Za-z])|%[0-9A-Fa-f]{2}|(?:\x1b|\\(?:u001b|x1b|e|033))(?:\[[0-9:;<=>?]*[\x20-\x2f]*[A-Za-z]|[\x20-\x2f]*[\x30-\x7e])|\[[0-9;]*m)(sk-[A-Za-z0-9_-]{10,})`)

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

func Secrets(input string) string { return scrub(input, environmentSecrets()) }

func TrimCutSecretEnd(text string) string { return trimCutSecretEnd(text, environmentSecrets()) }

func trimCutSecretEnd(text string, values []string) string {
	cut := 0
	for _, value := range values {
		for i, r := range value {
			if i > cut && unicode.IsSpace(r) && strings.HasSuffix(text, value[:i]) {
				cut = i
			}
		}
	}
	return text[:len(text)-cut]
}

func TrimCutSecretStart(text string) string {
	return trimCutSecretStart(text, environmentSecrets())
}

func trimCutSecretStart(text string, values []string) string {
	cut := 0
	for _, value := range values {
		for i, r := range value {
			if !unicode.IsSpace(r) {
				continue
			}
			if rest := value[i+utf8.RuneLen(r):]; len(rest) > cut && strings.HasPrefix(text, rest) {
				cut = len(rest)
			}
		}
	}
	return text[cut:]
}

// scrub finds all spans in the original text and merges overlaps, so replacing one secret never splits another.
func scrub(input string, values []string) string {
	var spans [][2]int
	for _, match := range tokenPattern.FindAllStringIndex(input, -1) {
		spans = append(spans, [2]int{match[0], match[1]})
	}
	for _, match := range keyPattern.FindAllStringSubmatchIndex(input, -1) {
		spans = append(spans, [2]int{match[2], match[3]})
	}
	for _, value := range values {
		if value == "" {
			continue
		}
		first := len(spans)
		for from := 0; ; {
			i := strings.Index(input[from:], value)
			if i < 0 {
				break
			}
			start, end := from+i, from+i+len(value)
			if last := len(spans) - 1; last >= first && start < spans[last][1] {
				spans[last][1] = end
			} else {
				spans = append(spans, [2]int{start, end})
			}
			from = start + 1
		}
	}
	if len(spans) == 0 {
		return input
	}
	slices.SortFunc(spans, func(a, b [2]int) int { return cmp.Compare(a[0], b[0]) })
	var out strings.Builder
	last := 0
	for i := 0; i < len(spans); {
		start, end := spans[i][0], spans[i][1]
		for i++; i < len(spans) && spans[i][0] < end; i++ {
			end = max(end, spans[i][1])
		}
		out.WriteString(input[last:start])
		out.WriteString("[redacted]")
		last = end
	}
	out.WriteString(input[last:])
	return out.String()
}

const displayTextLimit = 16384

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

func displayString(s string) (string, []string) {
	kinds := []string{}
	redacted := Secrets(s)
	if redacted != s {
		kinds = append(kinds, "redacted")
	}
	display, shortened := boundDisplayText(redacted)
	if shortened {
		kinds = append(kinds, "shortened")
	}
	return display, kinds
}

func Text(input string) string {
	s, _ := displayString(input)
	return s
}

type DisplayTransform struct {
	Field string   `json:"field"`
	Kinds []string `json:"kinds"`
	Paths [][]any  `json:"paths"`
}

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
			sort.Strings(entry.Kinds)
			result = append(result, *entry)
		}
	}
	return object, result
}

func JSON(value any) any {
	switch v := value.(type) {
	case string:
		return Text(v)
	case []any:
		for i := range v {
			v[i] = JSON(v[i])
		}
		return v
	case map[string]any:
		for k := range v {
			v[k] = JSON(v[k])
		}
		return v
	default:
		return value
	}
}
