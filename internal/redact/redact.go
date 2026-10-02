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

// Part is text from a capture, optionally cut inside its first or last line.
// Prefix is included only when text remains after trimming the cut lines.
type Part struct {
	Text             string
	Prefix           string
	CutStart, CutEnd bool
}

// Parts scrubs the concatenation after trimming capture cuts, retaining the part
// boundaries. A secret spanning parts is replaced once, in the part where it starts.
func Parts(parts ...Part) []string {
	values := environmentSecrets()
	return scrubParts(partTexts(parts, values), values)
}

// Streams returns first's parts, a separating newline, and second's parts after
// capture-cut normalization and joint redaction. Both adjacent and separated
// views are matched before replacement. Discarded capture fragments still inform
// redaction of retained bytes, without changing which cut lines are displayed.
func Streams(first, second []Part) []string {
	return streams(first, second, environmentSecrets())
}

func partTexts(parts []Part, values []string) []string {
	texts := make([]string, len(parts))
	for i, part := range parts {
		text, _ := partSlice(part, values)
		if text != "" {
			texts[i] = part.Prefix + text
		}
	}
	return texts
}

func partSlice(part Part, values []string) (text string, start int) {
	text = part.Text
	if part.CutStart {
		text = cutFragment(text, TailLineCut, values)
		start = len(part.Text) - len(text)
	}
	if part.CutEnd {
		text = cutFragment(text, HeadLineCut, values)
	}
	return text, start
}

func streams(first, second []Part, values []string) []string {
	parts := append(slices.Clone(first), Part{Text: "\n"})
	parts = append(parts, second...)
	texts, raw := make([]string, len(parts)), make([]string, len(parts))
	// Each retained slice maps original capture bytes to their display offsets.
	type keptSlice struct{ rawStart, rawEnd, textStart int }
	var kept []keptSlice
	var rawOffset, textOffset, rawBoundary, textBoundary int
	for i, part := range parts {
		if i == len(first) {
			rawBoundary, textBoundary = rawOffset, textOffset
		}
		if part.Text != "" {
			raw[i] = part.Prefix + part.Text
		}
		text, start := partSlice(part, values)
		if text != "" {
			texts[i] = part.Prefix + text
			// Anchor projected replacements in retained capture bytes, never in
			// display-only prefixes or the separator that callers may omit.
			if i != len(first) {
				from := rawOffset + len(part.Prefix) + start
				kept = append(kept, keptSlice{from, from + len(text), textOffset + len(part.Prefix)})
			}
		}
		rawOffset += len(raw[i])
		textOffset += len(texts[i])
	}
	input := strings.Join(texts, "")
	spans := streamSpans(input, textBoundary, values)
	original := strings.Join(raw, "")
	if original == input {
		return replaceParts(texts, input, spans)
	}
	for _, span := range streamSpans(original, rawBoundary, values) {
		from, to := -1, 0
		for _, slice := range kept {
			start, end := max(span[0], slice.rawStart), min(span[1], slice.rawEnd)
			if start < end {
				if from < 0 {
					from = slice.textStart + start - slice.rawStart
				}
				to = slice.textStart + end - slice.rawStart
			}
		}
		if from >= 0 {
			spans = append(spans, [2]int{from, to})
		}
	}
	return replaceParts(texts, input, mergeSpans(spans))
}

func streamSpans(input string, boundary int, values []string) [][2]int {
	spans := secretSpans(input, values)
	adjacent := input[:boundary] + input[boundary+1:]
	for _, span := range secretSpans(adjacent, values) {
		// Map back across the display-only newline. A spanning match includes
		// that newline; a match starting in the second stream leaves it alone.
		if span[0] >= boundary {
			span[0]++
		}
		if span[1] > boundary {
			span[1]++
		}
		spans = append(spans, span)
	}
	return mergeSpans(spans)
}

type FragmentKind uint8

const (
	HeadLineCut FragmentKind = iota
	HeadWordCut
	TailLineCut
	TailTwoWordsCut
)

func Fragment(input string, kind FragmentKind) string {
	values := environmentSecrets()
	return scrub(cutFragment(input, kind, values), values)
}

func cutFragment(text string, kind FragmentKind, values []string) string {
	switch kind {
	case HeadLineCut:
		if i := strings.LastIndexByte(text, '\n'); i >= 0 {
			return trimCutSecretEnd(text[:i], values)
		}
		if i := strings.LastIndexFunc(text, unicode.IsSpace); i >= 0 {
			return trimCutSecretEnd(text[:i], values)
		}
		return ""
	case HeadWordCut:
		return trimCutSecretEnd(beforeLastWord(scrub(text, values)), values)
	case TailLineCut:
		return tailLineStart(text, values)
	case TailTwoWordsCut:
		return tailTwoWordsStart(scrub(text, values), values)
	}
	return ""
}

func beforeLastWord(text string) string {
	if i := strings.LastIndexFunc(text, unicode.IsSpace); i >= 0 {
		return text[:i]
	}
	return ""
}

func afterWord(text string) string {
	if i := strings.IndexFunc(text, unicode.IsSpace); i >= 0 {
		return text[i:]
	}
	return ""
}

func tailTwoWordsStart(text string, values []string) string {
	var rest string
	if _, after, found := strings.Cut(text, "\n"); found {
		rest = after
	} else {
		rest = afterWord(strings.TrimLeftFunc(afterWord(text), unicode.IsSpace))
	}
	return trimCutSecretStart(strings.TrimLeftFunc(rest, unicode.IsSpace), values)
}

const escapeIntermediates = " !\"#$%&'()*+,-./"

var cutEscapeKey = regexp.MustCompile(`(?i)^[a-z]sk-[a-z0-9_-]{10}`)

func mayEndBearerPrefix(text string) bool {
	const prefix = "bearer"
	n := min(len(text), len(prefix))
	return (n == len(text) || n == len(prefix)) && strings.EqualFold(text[len(text)-n:], prefix[len(prefix)-n:])
}

func tailLineStart(text string, values []string) string {
	dropped, rest, found := strings.Cut(text, "\n")
	if !found {
		i := strings.IndexFunc(text, unicode.IsSpace)
		if i < 0 {
			return ""
		}
		dropped, rest = text[:i], text[i:]
	}
	run := 0
	for {
		rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
		at := len(text) - len(rest)
		if at >= run {
			run = at + len(rest) - len(strings.TrimLeft(rest, escapeIntermediates))
		}
		from := -1
		if key := cutEscapeKey.FindStringIndex(text[run:]); key != nil {
			from = run - at + key[1]
		} else if mayEndBearerPrefix(strings.TrimRightFunc(dropped, unicode.IsSpace)) {
			from = 0
		} else if trimmed := trimCutSecretStart(rest, values); len(trimmed) < len(rest) {
			from = len(rest) - len(trimmed)
		}
		if from < 0 {
			return rest
		}
		i := strings.IndexFunc(rest[from:], unicode.IsSpace)
		if i < 0 {
			return ""
		}
		dropped, rest = rest[:from+i], rest[from+i:]
	}
}

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
