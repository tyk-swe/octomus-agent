package redact

import (
	"cmp"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
)

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
			if strings.Contains(key, "WEBHOOK") || strings.Contains(key, "TOKEN") || strings.Contains(key, "SECRET") || strings.Contains(key, "PASSWORD") || strings.Contains(key, "API_KEY") {
				secrets = append(secrets, value)
			}
		}
	})
	return secrets
}

func Secrets(input string) string { return scrub(input, environmentSecrets()) }

// secretSpans finds all spans in the original text and merges overlaps, so
// replacing one secret never splits another.
func secretSpans(input string, values []string) [][2]int {
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
	return mergeSpans(spans)
}

func mergeSpans(spans [][2]int) [][2]int {
	slices.SortFunc(spans, func(a, b [2]int) int { return cmp.Compare(a[0], b[0]) })
	merged := spans[:0]
	for i := 0; i < len(spans); {
		start, end := spans[i][0], spans[i][1]
		for i++; i < len(spans) && spans[i][0] < end; i++ {
			end = max(end, spans[i][1])
		}
		merged = append(merged, [2]int{start, end})
	}
	return merged
}

func scrub(input string, values []string) string {
	return scrubParts([]string{input}, values)[0]
}

func scrubParts(parts []string, values []string) []string {
	input := strings.Join(parts, "")
	return replaceParts(parts, input, secretSpans(input, values))
}

func replaceParts(parts []string, input string, spans [][2]int) []string {
	if len(spans) == 0 {
		return parts
	}
	start := 0
	for i, part := range parts {
		end := start + len(part)
		last := start
		var out strings.Builder
		for len(spans) > 0 && spans[0][0] < end {
			span := spans[0]
			if span[0] >= start {
				out.WriteString(input[last:span[0]])
				out.WriteString("[redacted]")
			}
			last = min(span[1], end)
			if span[1] > end {
				break
			}
			spans = spans[1:]
		}
		out.WriteString(input[last:end])
		parts[i] = out.String()
		start = end
	}
	return parts
}

const displayTextLimit = 16384

// displayString scrubs s and bounds it to displayTextLimit characters, naming each transform it applied.
func displayString(s string) (string, []string) {
	kinds := []string{}
	display := Secrets(s)
	if display != s {
		kinds = append(kinds, "redacted")
	}
	count := 0
	for i := range display {
		if count == displayTextLimit {
			display, kinds = display[:i], append(kinds, "shortened")
			break
		}
		count++
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

// walk rewrites every string under value in place, in key order, through visit, which also sees the path from the
// root to that string.
func walk(value any, path []any, visit func(s string, path []any) string) any {
	switch v := value.(type) {
	case string:
		return visit(v, path)
	case []any:
		for i := range v {
			v[i] = walk(v[i], append(slices.Clone(path), i), visit)
		}
		return v
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(v)) {
			v[key] = walk(v[key], append(slices.Clone(path), key), visit)
		}
		return v
	default:
		return value
	}
}

func DisplayJSON(object map[string]any) (map[string]any, []DisplayTransform) {
	transforms := map[string]*DisplayTransform{}
	fields := slices.Sorted(maps.Keys(object))
	for _, field := range fields {
		object[field] = walk(object[field], []any{field}, func(s string, path []any) string {
			display, kinds := displayString(s)
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
		})
	}
	result := []DisplayTransform{}
	for _, field := range fields {
		if entry := transforms[field]; entry != nil {
			slices.Sort(entry.Kinds)
			result = append(result, *entry)
		}
	}
	return object, result
}

func JSON(value any) any {
	return walk(value, nil, func(s string, _ []any) string { return Text(s) })
}
